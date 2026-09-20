package pctx

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// countingBody 记录读取次数，用于断言「构造期不读体」。
type countingBody struct {
	reads  atomic.Int32
	closes atomic.Int32
	data   string
	offset int
}

func (b *countingBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	if b.offset >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.offset:])
	b.offset += n
	return n, nil
}

func (b *countingBody) Close() error {
	b.closes.Add(1)
	return nil
}

func newTestContext(t *testing.T, init Init) *Context {
	t.Helper()
	if init.Method == "" {
		init.Method = "POST"
	}
	if init.Path == "" {
		init.Path = "/v1/messages"
	}
	ctx, err := New(init)
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return ctx
}

func TestNewNormalizesEntryFacts(t *testing.T) {
	ctx, err := New(Init{Method: "  post ", Headers: http.Header{"X-A": {"1"}}})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	if ctx.Method() != "POST" {
		t.Fatalf("method = %q, want POST", ctx.Method())
	}
	if ctx.Path() != "/" {
		t.Fatalf("空路径应归一为 /，得到 %q", ctx.Path())
	}

	if _, err := New(Init{Method: "   "}); !errors.Is(err, ErrInvalidInit) {
		t.Fatalf("空 method 应返回 ErrInvalidInit，得到 %v", err)
	}
}

func TestNewDoesNotReadBody(t *testing.T) {
	body := &countingBody{data: "hello"}
	ctx := newTestContext(t, Init{Body: body})

	if got := body.reads.Load(); got != 0 {
		t.Fatalf("构造期读体 %d 次，应当为 0", got)
	}
	if !ctx.HasBody() {
		t.Fatal("HasBody 应为 true")
	}

	reader, err := ctx.TakeBody()
	if err != nil {
		t.Fatalf("TakeBody 失败: %v", err)
	}
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("读体失败: %v", err)
	}
	if got := body.reads.Load(); got == 0 {
		t.Fatal("交付后应能读到正文")
	}
	if ctx.HasBody() {
		t.Fatal("正文被消费后 HasBody 应为 false")
	}
}

func TestHeadersAreSnapshottedAtConstruction(t *testing.T) {
	external := http.Header{"X-Tenant": {"acme"}}
	ctx := newTestContext(t, Init{Headers: external})

	external.Set("X-Tenant", "mutated")
	external.Set("X-Injected", "1")

	if got := ctx.Headers().Get("X-Tenant"); got != "acme" {
		t.Fatalf("上下文 headers 被外部改动影响: %q", got)
	}
	if ctx.Headers().Has("X-Injected") {
		t.Fatal("外部新增的键不应出现在上下文里")
	}
}

func TestTakeBodyIsOneShot(t *testing.T) {
	body := &countingBody{data: "payload"}
	ctx := newTestContext(t, Init{Body: body})

	first, err := ctx.TakeBody()
	if err != nil {
		t.Fatalf("首次 TakeBody 失败: %v", err)
	}
	if first == nil {
		t.Fatal("首次 TakeBody 应返回读取器")
	}

	if _, err := ctx.TakeBody(); !errors.Is(err, ErrBodyAlreadyTaken) {
		t.Fatalf("二次 TakeBody 应返回 ErrBodyAlreadyTaken，得到 %v", err)
	}

	empty := newTestContext(t, Init{})
	if _, err := empty.TakeBody(); !errors.Is(err, ErrNoBody) {
		t.Fatalf("无正文应返回 ErrNoBody，得到 %v", err)
	}
}

func TestOwnerIsDecidedOnce(t *testing.T) {
	var buf strings.Builder
	ctx := newTestContext(t, Init{Logger: logx.New(&buf)})

	if _, ok := ctx.Owner(); ok {
		t.Fatal("未决定时 Owner 不应有值")
	}
	if err := ctx.SetOwner(egress.OwnerGo); err != nil {
		t.Fatalf("首次 SetOwner 失败: %v", err)
	}
	owner, ok := ctx.Owner()
	if !ok || owner != egress.OwnerGo {
		t.Fatalf("Owner = %q/%v, want go/true", owner, ok)
	}

	if err := ctx.SetOwner(egress.OwnerNode); !errors.Is(err, ErrOwnerAlreadySet) {
		t.Fatalf("改判应返回 ErrOwnerAlreadySet，得到 %v", err)
	}
	if err := ctx.SetOwner(egress.OwnerGo); !errors.Is(err, ErrOwnerAlreadySet) {
		t.Fatalf("同值重设也应返回 ErrOwnerAlreadySet，得到 %v", err)
	}
	if owner, _ := ctx.Owner(); owner != egress.OwnerGo {
		t.Fatalf("改判被静默接受: %q", owner)
	}

	logged := buf.String()
	for _, want := range []string{"pctx.owner.rejected", `"attempt":"node"`, `"existing":"go"`} {
		if !strings.Contains(logged, want) {
			t.Fatalf("日志缺少 %s，实际: %s", want, logged)
		}
	}
}

func TestOwnerRejectsUnknownValues(t *testing.T) {
	ctx := newTestContext(t, Init{})
	for _, owner := range []egress.Owner{"", "k8s", "GO"} {
		if err := ctx.SetOwner(owner); !errors.Is(err, ErrUnknownOwner) {
			t.Fatalf("非法归属 %q 应返回 ErrUnknownOwner，得到 %v", owner, err)
		}
	}
	if _, ok := ctx.Owner(); ok {
		t.Fatal("非法归属不应写入上下文")
	}
	if err := ctx.SetOwner(egress.OwnerNode); err != nil {
		t.Fatalf("非法尝试后仍应允许设置合法归属: %v", err)
	}
}

func TestSlotsAreCopiedNotAliased(t *testing.T) {
	ctx := newTestContext(t, Init{})

	auth := AuthState{KeyID: 7, UserID: 3, KeyName: "loadtest", APIKey: "sk-secret"}
	ctx.SetAuth(auth)
	auth.KeyName = "mutated"
	stored, ok := ctx.Auth()
	if !ok || stored.KeyName != "loadtest" || stored.KeyID != 7 {
		t.Fatalf("鉴权槽位应按值保存，得到 %+v", stored)
	}

	selection := ProviderSelection{ProviderID: 11, Name: "mock-upstream", Type: "codex"}
	ctx.SetProvider(selection)
	selection.Name = "mutated"
	if got, _ := ctx.Provider(); got.Name != "mock-upstream" {
		t.Fatalf("选路槽位应按值保存，得到 %+v", got)
	}

	if _, ok := ctx.Auth(); !ok {
		t.Fatal("Auth 应有值")
	}
	empty := newTestContext(t, Init{})
	if _, ok := empty.Auth(); ok {
		t.Fatal("未鉴权时 Auth 不应有值")
	}
	if _, ok := empty.Provider(); ok {
		t.Fatal("未选路时 Provider 不应有值")
	}
}

func TestSettlementIsRecordedOnce(t *testing.T) {
	frozen := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	ctx := newTestContext(t, Init{Now: func() time.Time { return frozen }})

	if _, ok := ctx.Settlement(); ok {
		t.Fatal("未结算时 Settlement 不应有值")
	}
	if !ctx.MarkSettled(Settlement{StatusCode: 200, Success: true}) {
		t.Fatal("首次结算应返回 true")
	}
	settled, ok := ctx.Settlement()
	if !ok || settled.StatusCode != 200 || !settled.Success {
		t.Fatalf("结算结果 = %+v", settled)
	}
	if !settled.At.Equal(frozen) {
		t.Fatalf("未提供时刻时应取注入时钟，得到 %v", settled.At)
	}

	if ctx.MarkSettled(Settlement{StatusCode: 502, Success: false}) {
		t.Fatal("重复结算应返回 false")
	}
	if again, _ := ctx.Settlement(); again.StatusCode != 200 {
		t.Fatalf("重复结算改写了既有结果: %+v", again)
	}
}

func TestDebugArtifactsDefaultOff(t *testing.T) {
	ctx := newTestContext(t, Init{})
	if ctx.ShouldPersistDebugArtifacts() {
		t.Fatal("调试工件默认必须关闭")
	}
	ctx.SetPersistDebugArtifacts(true)
	if !ctx.ShouldPersistDebugArtifacts() {
		t.Fatal("显式开启后应为 true")
	}
	ctx.SetPersistDebugArtifacts(false)
	if ctx.ShouldPersistDebugArtifacts() {
		t.Fatal("显式关闭后应为 false")
	}
}

func TestClientIPAndProtocolFrom(t *testing.T) {
	ctx := newTestContext(t, Init{ClientIP: "203.0.113.7", ProtocolFrom: egress.FamilyAnthropicMessages})
	if ctx.ClientIP() != "203.0.113.7" {
		t.Fatalf("client ip = %q", ctx.ClientIP())
	}
	if ctx.ProtocolFrom() != egress.FamilyAnthropicMessages {
		t.Fatalf("protocol from = %q", ctx.ProtocolFrom())
	}
	ctx.SetClientIP("198.51.100.9")
	ctx.SetProtocolFrom(egress.FamilyOpenAIResponses)
	if ctx.ClientIP() != "198.51.100.9" || ctx.ProtocolFrom() != egress.FamilyOpenAIResponses {
		t.Fatalf("写入后读回不一致: %q %q", ctx.ClientIP(), ctx.ProtocolFrom())
	}
}

func TestConcurrentAccessIsRaceFreeAndSingleWinner(t *testing.T) {
	body := &countingBody{data: "payload"}
	ctx := newTestContext(t, Init{Body: body, Headers: http.Header{"X-A": {"1"}}})

	const goroutines = 16
	var (
		bodyWinners  atomic.Int32
		ownerWinners atomic.Int32
		settleWinner atomic.Int32
		wg           sync.WaitGroup
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx.SetHeader("X-Worker", "1")
			_ = ctx.Headers().Values("X-Worker")
			ctx.SetAuth(AuthState{KeyID: int64(index)})
			ctx.SetProvider(ProviderSelection{ProviderID: int64(index)})
			ctx.SetClientIP("127.0.0.1")

			if _, err := ctx.TakeBody(); err == nil {
				bodyWinners.Add(1)
			}
			if err := ctx.SetOwner(egress.OwnerGo); err == nil {
				ownerWinners.Add(1)
			}
			if ctx.MarkSettled(Settlement{StatusCode: 200, Success: true}) {
				settleWinner.Add(1)
			}
		}(i)
	}
	wg.Wait()

	if got := bodyWinners.Load(); got != 1 {
		t.Fatalf("正文赢家应为 1，得到 %d", got)
	}
	if got := ownerWinners.Load(); got != 1 {
		t.Fatalf("归属赢家应为 1，得到 %d", got)
	}
	if got := settleWinner.Load(); got != 1 {
		t.Fatalf("结算赢家应为 1，得到 %d", got)
	}
	if _, ok := ctx.Owner(); !ok {
		t.Fatal("并发后归属应当已写入")
	}
}
