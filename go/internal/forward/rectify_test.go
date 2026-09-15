package forward

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/rectify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// recordingServer 记录每次上游收到的正文，并按序号给出响应。
type recordingServer struct {
	*httptest.Server
	mu       sync.Mutex
	bodies   []string
	statuses []int
	replies  []string
	hits     int32
}

func newRecordingServer(t *testing.T, statuses []int, replies []string) *recordingServer {
	t.Helper()
	recorder := &recordingServer{statuses: statuses, replies: replies}
	recorder.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		index := int(atomic.AddInt32(&recorder.hits, 1)) - 1
		recorder.mu.Lock()
		recorder.bodies = append(recorder.bodies, string(body))
		recorder.mu.Unlock()

		w.Header().Set("content-type", "application/json")
		status := http.StatusOK
		reply := `{"id":"msg_1","content":[{"type":"text","text":"ok"}]}`
		if index < len(statuses) {
			status = statuses[index]
		}
		if index < len(replies) && replies[index] != "" {
			reply = replies[index]
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(recorder.Close)
	return recorder
}

func (r *recordingServer) received(t *testing.T) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bodies...)
}

// auditCollector 收集整流器交接出的审计条目（生产里由数据面按请求收，终态追加落库）。
type auditCollector struct {
	mu      sync.Mutex
	entries []map[string]any
}

func (c *auditCollector) record(entry map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, entry)
}

func (c *auditCollector) all() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.entries...)
}

// auditTypes 取出收集到的审计条目类型。
func auditTypes(t *testing.T, collector *auditCollector) []string {
	t.Helper()
	entries := collector.all()
	types := make([]string, 0, len(entries))
	for _, entry := range entries {
		entryType, _ := entry["type"].(string)
		types = append(types, entryType)
	}
	return types
}

// TestForwardReactiveRectifierRetriesSameProviderWithRectifiedBody 断言被动整流的完整闭环：
// 上游 400 命中触发词 → 整流客户端正文 → **同一供应商**立即重试一次 → 第二次成功。
func TestForwardReactiveRectifierRetriesSameProviderWithRectifiedBody(t *testing.T) {
	trigger := `{"error":{"message":"thinking options type cannot be disabled when reasoning_effort is set"}}`
	server := newRecordingServer(t, []int{http.StatusBadRequest, http.StatusOK}, []string{trigger, ""})

	body := `{"model":"claude-sonnet-4-5-20250929","max_tokens":1024,"thinking":{"type":"disabled"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"ping"}]}`
	collector := &auditCollector{}
	deps := Deps{
		Dial:           newTestDial(t),
		Facts:          PlanFacts{Client: newClaudeRequest(body)},
		Limits:         Limits{RetryDelay: time.Millisecond},
		RectifierAudit: collector.record,
	}

	_, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 3), deps)
	if err != nil {
		t.Fatalf("整流后应成功: %v", err)
	}

	received := server.received(t)
	if len(received) != 2 {
		t.Fatalf("上游只应被调用 2 次（原发 + 整流重试），实际 %d 次", len(received))
	}
	if !strings.Contains(received[0], `"output_config":{"effort":"max"}`) {
		t.Fatalf("首次请求应带上客户端原始正文: %s", received[0])
	}
	if strings.Contains(received[1], "output_config") || strings.Contains(received[1], "reasoning_effort") {
		t.Fatalf("重试的正文应已剥掉 effort 载体与顶层 reasoning_effort: %s", received[1])
	}
	if !strings.Contains(received[1], `"thinking":{"type":"disabled"}`) {
		t.Fatalf("thinking 的关闭状态必须保留: %s", received[1])
	}

	entries := collector.all()
	if len(entries) != 1 || entries[0]["type"] != specialsettings.TypeThinkingEffortConflictRectifier {
		t.Fatalf("审计条目不对: %v", auditTypes(t, collector))
	}
	entry := entries[0]
	if entry["hit"] != true || entry["scope"] != "request" {
		t.Fatalf("审计条目缺少 hit/scope: %#v", entry)
	}
	if entry["trigger"] != "thinking_disabled_with_reasoning_effort" {
		t.Fatalf("触发词不对: %#v", entry["trigger"])
	}
	if entry["providerId"] != int64(1) && entry["providerId"] != 1 {
		t.Fatalf("providerId 不对: %#v", entry["providerId"])
	}
	if entry["attemptNumber"] != 1 || entry["retryAttemptNumber"] != 2 {
		t.Fatalf("attempt 语义不对（Node 记「当前失败尝试」与其下一次）: %#v / %#v", entry["attemptNumber"], entry["retryAttemptNumber"])
	}
	if entry["removedOutputConfigEffort"] != true || entry["effort"] != "max" {
		t.Fatalf("整流器特有字段不对: %#v", entry)
	}
}

// TestForwardReactiveRectifierStopsAfterOneRetry 断言「同供应商只重试一次」：
// 上游持续返回同一触发词时，第二次失败归为不可重试的客户端错误并立即终止（不切换、不继续重试）。
func TestForwardReactiveRectifierStopsAfterOneRetry(t *testing.T) {
	trigger := `{"error":{"message":"thinking options type cannot be disabled when reasoning_effort is set"}}`
	server := newRecordingServer(t, []int{http.StatusBadRequest, http.StatusBadRequest, http.StatusBadRequest}, []string{trigger, trigger, trigger})

	body := `{"model":"claude-sonnet-4-5-20250929","thinking":{"type":"disabled"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"ping"}]}`
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{Client: newClaudeRequest(body)},
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(context.Context, []int64) (*Candidate, error) {
			t.Fatal("整流失败后不得切换供应商")
			return nil, nil
		},
	}

	result, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 5), deps)
	if err == nil {
		t.Fatal("全部尝试失败时应返回错误")
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("错误应为 *Failure: %v", err)
	}
	if failure.Category != CategoryNonRetryableClientError {
		t.Fatalf("已重试过时应归不可重试的客户端错误，实际 %v", failure.Category)
	}
	if hits := len(server.received(t)); hits != 2 {
		t.Fatalf("应只整流重试一次（共 2 次上游调用），实际 %d 次", hits)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("尝试留痕 = %d 条", len(result.Attempts))
	}
	if result.Attempts[0].ProviderID != 1 {
		t.Fatalf("留痕供应商 = %d", result.Attempts[0].ProviderID)
	}
}

// TestForwardBillingHeaderRectifierStripsSystemBeforeSend 断言主动型 billing header 整流：
// 只按供应商类型（claude 系）门控，不等上游报错，发送前剥离 system 里的计费头文本块。
func TestForwardBillingHeaderRectifierStripsSystemBeforeSend(t *testing.T) {
	server := newRecordingServer(t, []int{http.StatusOK}, []string{""})

	body := `{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"system":[{"type":"text","text":"you are helpful"},{"type":"text","text":"x-anthropic-billing-header: v1"}],"messages":[{"role":"user","content":"ping"}]}`
	collector := &auditCollector{}
	deps := Deps{
		Dial:           newTestDial(t),
		Facts:          PlanFacts{Client: newClaudeRequest(body)},
		Limits:         Limits{RetryDelay: time.Millisecond},
		RectifierAudit: collector.record,
	}

	if _, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 1), deps); err != nil {
		t.Fatalf("转发失败: %v", err)
	}

	received := server.received(t)
	if len(received) != 1 {
		t.Fatalf("主动整流不产生额外尝试，实际 %d 次", len(received))
	}
	if strings.Contains(received[0], "x-anthropic-billing-header") {
		t.Fatalf("计费头必须被剥离: %s", received[0])
	}
	if !strings.Contains(received[0], "you are helpful") {
		t.Fatalf("其余 system 块必须保留: %s", received[0])
	}

	entries := collector.all()
	if len(entries) != 1 || entries[0]["type"] != specialsettings.TypeBillingHeaderRectifier {
		t.Fatalf("审计条目不对: %v", auditTypes(t, collector))
	}
	entry := entries[0]
	if entry["removedCount"] != 1 {
		t.Fatalf("removedCount 不对: %#v", entry["removedCount"])
	}
	values, ok := entry["extractedValues"].([]string)
	if !ok || len(values) != 1 || values[0] != "x-anthropic-billing-header: v1" {
		t.Fatalf("extractedValues 不对: %#v", entry["extractedValues"])
	}
	if _, hasProvider := entry["providerId"]; hasProvider {
		t.Fatalf("主动型条目不应带 provider 上下文（Node 如此）: %#v", entry)
	}
}

// TestForwardRectifierSwitchesOffRestoresLegacyRetry 断言开关关闭时整流器完全不上手：
// 同一触发词按普通供应商故障处理（重试到上限），且不产生审计条目。
func TestForwardRectifierSwitchesOffRestoresLegacyRetry(t *testing.T) {
	trigger := `{"error":{"message":"thinking options type cannot be disabled when reasoning_effort is set"}}`
	server := newRecordingServer(t, []int{http.StatusBadRequest, http.StatusBadRequest, http.StatusBadRequest}, []string{trigger, trigger, trigger})

	body := `{"model":"claude-sonnet-4-5-20250929","thinking":{"type":"disabled"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"ping"}]}`
	collector := &auditCollector{}
	deps := Deps{
		Dial:            newTestDial(t),
		Facts:           PlanFacts{Client: newClaudeRequest(body)},
		Limits:          Limits{RetryDelay: time.Millisecond},
		RectifySwitches: func(context.Context) rectify.Switches { return rectify.Switches{} },
		RectifierAudit:  collector.record,
	}

	_, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 3), deps)
	if err == nil {
		t.Fatal("全部尝试失败时应返回错误")
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("错误应为 *Failure: %v", err)
	}
	if failure.Category != CategoryProviderError {
		t.Fatalf("开关关闭时应保持原分类（供应商故障），实际 %v", failure.Category)
	}
	if hits := len(server.received(t)); hits != 3 {
		t.Fatalf("开关关闭时应重试到上限 3 次，实际 %d 次", hits)
	}
	if entries := collector.all(); len(entries) != 0 {
		t.Fatalf("开关关闭时不应产生审计: %d 条", len(entries))
	}
}

// TestForwardRectifierIgnoresUnrelatedProviderTypes 断言分组门控：非 claude/gemini 供应商不整流。
func TestForwardRectifierIgnoresUnrelatedProviderTypes(t *testing.T) {
	trigger := `{"error":{"message":"thinking options type cannot be disabled when reasoning_effort is set"}}`
	server := newRecordingServer(t, []int{http.StatusBadRequest, http.StatusBadRequest}, []string{trigger, trigger})

	body := `{"model":"gpt-5","thinking":{"type":"disabled"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":"ping"}]}`
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{Client: newClaudeRequest(body)},
		Limits: Limits{RetryDelay: time.Millisecond},
	}
	candidate := newTestCandidate(1, "供应商甲", server.URL, 2)
	candidate.Provider.Type = convert.ProviderCodex

	if _, err := Forward(context.Background(), nil, candidate, deps); err == nil {
		t.Fatal("全部尝试失败时应返回错误")
	}
	if hits := len(server.received(t)); hits != 2 {
		t.Fatalf("codex 供应商不整流，应重试到上限 2 次，实际 %d 次", hits)
	}
}

// TestRectifierErrorMessageComposition 钉住检测输入文案的拼装口径（Node getDetailedErrorMessage）。
func TestRectifierErrorMessageComposition(t *testing.T) {
	provider := Provider{ID: 7, Name: "供应商甲"}
	failure := &Failure{StatusCode: 400, Message: "boom", Body: `{"error":{"message":"boom"}}`}

	want := `Provider 供应商甲 returned 400: boom | Upstream: {"error":{"message":"boom"}}`
	if got := rectifierErrorMessage(provider, failure); got != want {
		t.Fatalf("拼装文案不对:\n got %s\nwant %s", got, want)
	}

	failure.Body = ""
	if got, want := rectifierErrorMessage(provider, failure), "Provider 供应商甲 returned 400: boom"; got != want {
		t.Fatalf("无正文时不应带 Upstream 段: %s", got)
	}
}

// TestForwardRectifierAuditIsJSONSerializable 断言审计条目可直接进 special_settings（jsonb）。
func TestForwardRectifierAuditIsJSONSerializable(t *testing.T) {
	body := `{"thinking":{"type":"enabled","budget_tokens":100},"max_tokens":1000,"messages":[]}`
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{Client: newClaudeRequest(body)},
		Limits: Limits{RetryDelay: time.Millisecond},
	}
	state := rectifierState{client: deps.Facts.Client}
	collector := &auditCollector{}
	state.sink = collector.record
	result := deps.applyReactiveRectifier(
		context.Background(),
		Provider{ID: 3, Name: "供应商乙", Type: convert.ProviderClaude},
		&Failure{StatusCode: 400, Message: "thinking.enabled.budget_tokens: Input should be greater than or equal to 1024"},
		&state,
		1,
	)
	if !result.Applied {
		t.Fatalf("预算整流应成立: %#v", result)
	}
	encoded, err := json.Marshal(collector.all())
	if err != nil {
		t.Fatalf("审计条目不可序列化: %v", err)
	}
	if !strings.Contains(string(encoded), `"before":{"maxTokens":1000`) {
		t.Fatalf("预算整流器的 before 快照不对: %s", encoded)
	}
}
