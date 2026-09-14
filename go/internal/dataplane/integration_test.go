package dataplane

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是数据面的真实依赖集成测试（env 门控）：真 PG + 真 Redis 键空间 + httptest 假上游。
//
// 它检验 acceptance 里那四条断言：状态码、SSE 逐事件可读、message_request 落一行且终态正确、
// usage 与假上游一致。数据库语义（终态谓词、账本触发器）由 store / terminal 各自的集成测试
// 覆盖，本文件只断言「数据面这条链确实把该写的写进去了」。
//
// 门控：未设置 CCH_TEST_DSN 时整组跳过（仓库要求无依赖环境也能跑绿）。

// 测试固定用量：假上游在流与非流里都报这些数字，测试断言它们原样落进 usage 列。
const (
	testInputTokens  = 11
	testOutputTokens = 7
)

// provisioning 是一次集成测试的库内前置数据。
type provisioning struct {
	pools      *store.Pools
	providerID int64
	apiKey     string
	userID     int64
	// watermark 是**夹具建立那一刻** message_request 的最大 id。
	//
	// 取行一律带上 `id > watermark`：共享测试库里有别的包/轮次先写的行，不设水位就会把
	// 它们当成本次请求的行。
	watermark int64
}

// integrationStore 建池；未设置门控变量时跳过。
func integrationStore(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-dataplane-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// providerTypeFor 给出与路由同协议的供应商类型。
//
// 必须同线：选路的格式兼容过滤会拒掉跨线且未开协议转换的供应商——用 claude 供应商跑
// /v1/chat/completions 会得到「无可用供应商」。
func providerTypeFor(requestPath string) string {
	switch requestPath {
	case "/v1/chat/completions":
		return "openai-compatible"
	case "/v1/responses":
		return "codex"
	default:
		return "claude"
	}
}

// priorityLeftoverTier 是历史上夹具用过的固定档（-1000000），也是本次插入的起始值。
//
// 夹具不能就停在这个固定档位：选路的 selectTopPriority 只保留最小优先级那一档，而
// 被信号杀掉的旧运行会把同档的僵尸夹具（名字前缀 dataplane-it-，指向已关闭的假上游）留在
// 共享库里。同档的候选一多，每个请求就在死端口之间加权随机，故障转移一路排除到请求预算
// 耗尽——表现为 /v1/messages 的 504 与断线风暴用例的行数不足（见本文件末尾的
// TestIntegrationFixtureWinsOverLeftoverTier）。
const priorityLeftoverTier = -1_000_000

// pinAsTopCandidate 把夹具钉成「比库里其余所有供应商都更靠前」的唯一候选。
//
// 取「其余最小优先级 - 1」而不是某个固定值：僵尸夹具的档位无法预设（它们来自被杀的运行，
// 值各不相同），但此刻库里真正存在的值可以直接读。一条语句完成比较与写入，避免读-写之间
// 又插进一个新的候选。
//
// 还必须把**保留档也一起压住**（LEAST(min-1, 保留档-1)）：本文件的用例是「先钉夹具、再插一批
// 同档僵尸」，只比较「此刻库里已有的行」会漏掉还没插进来的那批。干净库上库里最小档远高于
// 保留档（例如 0），守卫 `p.priority >= sub.value` 不成立就什么都不做，夹具留在保留档上，
// 随即被用例自己插的 40 个僵尸拖进同一档——请求在两个死端口之间加权随机，前几轮侥幸命中、
// 随后 504（TestIntegrationFixtureWinsOverLeftoverTier 的原始故障）。压住保留档后，夹具无论
// 库是空是脏、也无论先钉后插还是先插后钉，都严格优于保留档。
//
// 溢出不设防：要撞到 int4 下界（-2147483648）得让库里先出现该值的行。
func pinAsTopCandidate(ctx context.Context, writer *store.Pool, providerID int64) error {
	// LEAST 在 SQL 里忽略 NULL，故「其余供应商为空」时取保留档 - 1，而不是不动。
	_, err := writer.Exec(ctx, `
		UPDATE providers AS p
		SET priority = sub.value
		FROM (SELECT LEAST(min(priority) - 1, $2) AS value FROM providers WHERE id <> $1) AS sub
		WHERE p.id = $1 AND p.priority >= sub.value`,
		providerID, int64(priorityLeftoverTier)-1)
	return err
}

// provision 插入一个专用供应商（钉成最高优先级，确保被选中）与取出一个可用密钥。
//
// 用真库现有用户/密钥而不是自造：密钥的启用态、有效期、分组与配额列都有现实形状，
// 自造一套反而会在别处漏掉语义。测试结束后删除自造供应商与本轮产生的请求行。
func provision(t *testing.T, pools *store.Pools, upstreamURL string, providerType string) provisioning {
	t.Helper()
	ctx := context.Background()

	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	var userID int64
	var apiKey string
	if err := writer.QueryRow(ctx, `
		SELECT k.user_id, k.key
		FROM keys k
		JOIN users u ON u.id = k.user_id
		WHERE k.is_enabled = true AND u.is_enabled = true
		  AND (k.expires_at IS NULL OR k.expires_at > now())
		ORDER BY k.id ASC
		LIMIT 1`,
	).Scan(&userID, &apiKey); err != nil {
		t.Skipf("库里没有可用的启用态密钥，跳过集成测试: %v", err)
	}

	// priority 先用历史档位插入，紧接着由 pinAsTopCandidate 钉成唯一最靠前的候选。
	var providerID int64
	if err := writer.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier)
		VALUES ($1, $2, $3, $4, true, 1, $5, 1.0)
		RETURNING id`,
		"dataplane-it-"+time.Now().Format("150405.000000"), upstreamURL, "fake-upstream-key", providerType,
		priorityLeftoverTier,
	).Scan(&providerID); err != nil {
		t.Fatalf("插入测试供应商失败: %v", err)
	}
	if err := pinAsTopCandidate(ctx, writer, providerID); err != nil {
		t.Fatalf("钉住测试供应商优先级失败: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = writer.Exec(cleanupCtx, `DELETE FROM message_request WHERE provider_id = $1`, providerID)
		_, _ = writer.Exec(cleanupCtx, `DELETE FROM providers WHERE id = $1`, providerID)
	})

	return provisioning{
		pools:      pools,
		providerID: providerID,
		apiKey:     apiKey,
		userID:     userID,
		watermark:  requestWatermark(t, writer),
	}
}

// requestWatermark 读当前 message_request 的最大 id（空表为 0），作为「本夹具之前的行」的分界。
func requestWatermark(t *testing.T, pool *store.Pool) int64 {
	t.Helper()
	var watermark int64
	if err := pool.QueryRow(context.Background(),
		`SELECT coalesce(max(id), 0) FROM message_request`).Scan(&watermark); err != nil {
		t.Fatalf("读取请求行水位失败: %v", err)
	}
	return watermark
}

// newIntegrationHandler 用真实 store 装配数据面。
func newIntegrationHandler(t *testing.T, provisioned provisioning) http.Handler {
	t.Helper()
	assembly, err := NewStoreBacked(StoreOptions{
		Pools:  provisioned.pools,
		Logger: logx.New(nil),
	})
	if err != nil {
		t.Fatalf("数据面装配失败: %v", err)
	}
	return assembly.Handler
}

// lastRequestRow 读回该供应商最近一行请求日志。
func lastRequestRow(t *testing.T, provisioned provisioning) map[string]any {
	t.Helper()
	reader, err := provisioned.pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	// 按夹具身份（provider_id，它是本用例新建的）+ 水位取行：只认本次夹具建立之后落库的行。
	rows, err := reader.Query(context.Background(), `
		SELECT row_to_json(t)::text FROM (
			SELECT * FROM message_request
			WHERE provider_id = $1 AND id > $2
			ORDER BY id DESC LIMIT 1
		) t`, provisioned.providerID, provisioned.watermark)
	if err != nil {
		t.Fatalf("读取请求日志失败: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("本次请求没有落任何 message_request 行")
	}
	var payload string
	if err := rows.Scan(&payload); err != nil {
		t.Fatalf("扫描请求日志失败: %v", err)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		t.Fatalf("解析请求日志失败: %v", err)
	}
	return row
}

// countRequestRows 统计该供应商的请求日志行数。
func countRequestRows(t *testing.T, provisioned provisioning) int {
	t.Helper()
	reader, err := provisioned.pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var count int
	if err := reader.QueryRow(context.Background(),
		`SELECT count(*) FROM message_request WHERE provider_id = $1`, provisioned.providerID,
	).Scan(&count); err != nil {
		t.Fatalf("统计请求日志失败: %v", err)
	}
	return count
}

// waitForFinalizedRow 等终态写入落地。
//
// 流式终态是异步的（结算发生在流终态协程里，客户端读到 EOF 时它可能仍在写库），
// 因此读到「未终态的行」并不等于结算失败：必须等。等不到才是失败。
func waitForFinalizedRow(t *testing.T, provisioned provisioning) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		row := lastRequestRow(t, provisioned)
		if row["status_code"] != nil {
			return row
		}
		if time.Now().After(deadline) {
			t.Fatalf("终态未在 5s 内落地：行仍是未终态（%v）", row)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// streamFramesFor 按路由方言给出假上游的 SSE 帧（含决定性内容帧与用量帧）。
func streamFramesFor(requestPath string) []string {
	switch requestPath {
	case "/v1/chat/completions":
		return []string{
			// openai-chat 的 chunk 无事件名，决定性字段是 choices[].delta.content。
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"甲\"}}]}\n\n",
			fmt.Sprintf("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d}}\n\n", testInputTokens, testOutputTokens),
			"data: [DONE]\n\n",
		}
	case "/v1/responses":
		return []string{
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"甲\"}\n\n",
			fmt.Sprintf("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d}}}\n\n", testInputTokens, testOutputTokens),
		}
	default:
		return []string{
			fmt.Sprintf("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":%d}}}\n\n", testInputTokens),
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"甲\"}}\n\n",
			fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n", testOutputTokens),
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
	}
}

// expectedEventsFor 是各路由应当原样交付给客户端的事件名序列。
func expectedEventsFor(requestPath string) []string {
	switch requestPath {
	case "/v1/chat/completions":
		// openai-chat 的 chunk 无事件名：交付的是 data 行，故这里只断言 data 计数。
		return nil
	case "/v1/responses":
		return []string{"response.output_text.delta", "response.completed"}
	default:
		return []string{"message_start", "content_block_delta", "message_delta", "message_stop"}
	}
}

// numberOrZero 把 row_to_json 出来的数值列读成 float64（NULL 记 0）。
func numberOrZero(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case string:
		var parsed float64
		_, _ = fmt.Sscanf(typed, "%g", &parsed)
		return parsed
	default:
		return 0
	}
}

// 非流式：三条 Go 承载的路由各跑一轮，断言状态码、正文透传与「只落一行」。
func TestIntegrationNonStreamWritesTerminalRow(t *testing.T) {
	for _, requestPath := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(requestPath, func(t *testing.T) {
			runNonStreamCase(t, requestPath)
		})
	}
}

func runNonStreamCase(t *testing.T, requestPath string) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-4-5",`+
			`"usage":{"input_tokens":%d,"output_tokens":%d},"content":[]}`, testInputTokens, testOutputTokens)
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor(requestPath))
	handler := newIntegrationHandler(t, provisioned)

	request := httptest.NewRequest(http.MethodPost, requestPath,
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"你好"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "msg_1") {
		t.Fatalf("正文应来自假上游，收到 %q", recorder.Body.String())
	}
	if got := countRequestRows(t, provisioned); got != 1 {
		t.Fatalf("应只落一行请求日志，实际 %d", got)
	}
	row := waitForFinalizedRow(t, provisioned)
	if status := int(numberOrZero(row["status_code"])); status != http.StatusOK {
		t.Errorf("终态状态码应为 200，收到 %d", status)
	}
	if model, _ := row["model"].(string); model != "claude-sonnet-4-5" {
		t.Errorf("model 列应为 claude-sonnet-4-5，收到 %v", row["model"])
	}
	// duration_ms 只断言「写进去了」：假上游在本机毫秒级返回，非要 >0 会把时钟分辨率
	// 当成需求。
	if row["duration_ms"] == nil {
		t.Error("duration_ms 不应为 NULL：到达过上游的请求必须写耗时")
	}
	// Node 的同口径兜底：非流式没有首字节记录，故 ttfb_ms 与 first_byte_ms 都落 duration
	// （`response-handler.ts:3212-3213`）。两列不写会让 TPS（要 first_byte_ms 非空）
	// 与排行榜 tok/s 榜（`first_byte_ms IS NOT NULL`）双双缺数。
	if duration := int(numberOrZero(row["duration_ms"])); duration > 0 {
		if got := int(numberOrZero(row["first_byte_ms"])); got != duration {
			t.Errorf("非流式 first_byte_ms 应等于 duration_ms（Node 的 `?? duration` 口径）：first_byte_ms=%d duration_ms=%d", got, duration)
		}
		if got := int(numberOrZero(row["ttfb_ms"])); got != duration {
			t.Errorf("非流式 ttfb_ms 应等于 duration_ms（Node 的 `?? duration` 口径）：ttfb_ms=%d duration_ms=%d", got, duration)
		}
	}
	// 链上 reason 必须是与 Node 同词的结局原因（`success` 这类自造词会让消费者把成功算成失败）。
	if reason := lastChainReason(t, row); reason != "request_success" {
		t.Errorf("非流式成功链的 reason 应为 request_success，实际 %q", reason)
	}
	// 消费者对账（可用率路径）：用 Node 移植来的分类器判该行是不是成功。
	// 改前 Go 写 `success`——不在 SUCCESS_REASONS 里，于是每次成功都被算成失败、可用率恒 0。
	assertClassifierSeesSuccess(t, row)
	// routing_trace 同流式口径（非流式走另一个结算入口，故各判一次）。
	assertRoutingTraceRecorded(t, row)
}

// 流式：三条路由各跑一轮，SSE 逐事件可读、终态与用量落库。
//
// 三条线的首帧形态不同，但都必须含「决定性内容帧」——门控只在看到真实内容或错误时才提交，
// 只发中性帧（如 message_start）会让客户端一直等不到响应头。
func TestIntegrationStreamWritesTerminalRowWithUsage(t *testing.T) {
	for _, requestPath := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(requestPath, func(t *testing.T) {
			runStreamCase(t, requestPath)
		})
	}
}

func runStreamCase(t *testing.T, requestPath string) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for index, frame := range streamFramesFor(requestPath) {
			_, _ = io.WriteString(w, frame)
			// 首帧之后故意拖一段：让 duration_ms 与 first_byte_ms 拉开足够差距，
			// 从而能断言排行榜的 tok/s 谓词（要求生成窗口 >= 100ms）真的命中。
			// 不拖时两者只差毫秒，该谓词永远不成立——而线上 TPS 就是靠它算的。
			if index == 0 {
				flusher.Flush()
				time.Sleep(streamTailDelay)
			}
		}
		flusher.Flush()
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor(requestPath))
	handler := newIntegrationHandler(t, provisioned)

	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+requestPath,
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"你好"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("内容类型应为 SSE，收到 %q", contentType)
	}

	// 逐事件可读：客户端按行读到每个 event/data 对，而不是收到一坨。
	scanner := bufio.NewScanner(response.Body)
	events := make([]string, 0, 8)
	dataLines := 0
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
		if strings.HasPrefix(line, "data: ") {
			dataLines++
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("读取流失败: %v", err)
	}
	if expected := expectedEventsFor(requestPath); expected != nil {
		if strings.Join(events, ",") != strings.Join(expected, ",") {
			t.Fatalf("SSE 事件序列应为 %v，收到 %v", expected, events)
		}
	} else if dataLines == 0 {
		t.Fatalf("openai-chat 线应至少交付一个 data 行，实际 0")
	}

	row := waitForFinalizedRow(t, provisioned)
	if status := int(numberOrZero(row["status_code"])); status != http.StatusOK {
		t.Errorf("终态状态码应为 200，收到 %d", status)
	}
	if input := int(numberOrZero(row["input_tokens"])); input != testInputTokens {
		t.Errorf("input_tokens 应为 %d，收到 %d", testInputTokens, input)
	}
	if output := int(numberOrZero(row["output_tokens"])); output != testOutputTokens {
		t.Errorf("output_tokens 应为 %d，收到 %d", testOutputTokens, output)
	}
	if ttfb := int(numberOrZero(row["ttfb_ms"])); ttfb <= 0 {
		t.Errorf("ttfb_ms 应为正数，收到 %d", ttfb)
	}
	// first_byte_ms = 上游首个非空 chunk 的时刻（Node 的 recordFirstByte(attempt.firstByteAt)），
	// 它同时是公开页 TPS 的分母起点；缺失即 TPS/排行榜 tok/s 缺数。
	firstByte := int(numberOrZero(row["first_byte_ms"]))
	duration := int(numberOrZero(row["duration_ms"]))
	if firstByte <= 0 {
		t.Errorf("流式 first_byte_ms 应为正数（未记录会让公开页 TPS 恒缺），收到 %d", firstByte)
	}
	if duration > 0 && firstByte > duration {
		t.Errorf("first_byte_ms 不得大于 duration_ms：first_byte_ms=%d duration_ms=%d", firstByte, duration)
	}
	// 消费者对账（排行榜 tok/s 榜）：Node 的谓词要求 first_byte_ms 非空、严格小于
	// duration_ms 且生成窗口 >= 100ms（`admin_leaderboard.go`）。假上游首帧后刻意拖了
	// streamTailDelay，因此这条谓词必须命中——改前（NULL）它永远不成立。
	if generation := duration - firstByte; generation < 100 {
		t.Errorf("生成窗口应 >= 100ms（否则排行榜 tok/s 谓词不命中）：%dms（first_byte=%d duration=%d）", generation, firstByte, duration)
	}
	if count := countLeaderboardTpsRows(t, provisioned); count != 1 {
		t.Errorf("排行榜 tok/s 谓词应命中该行，实际命中 %d 行", count)
	}
	if reason := lastChainReason(t, row); reason != "request_success" && reason != "hedge_winner" {
		t.Errorf("流式成功链的 reason 应为 request_success/hedge_winner，实际 %q", reason)
	}
	// routing_trace：本次**真实跑过**的路径必须落库（Node 侧由 discovery/竞速子系统写）。
	// 缺它 → 详情页决策链视图恒空（审计 B2-2）。Go 只会写 single_upstream/legacy_hedge：
	// 它没有 discovery 子系统，声称 discovery 才是错的。
	assertRoutingTraceRecorded(t, row)
}

// streamTailDelay 是首帧后的拖延时长：它把生成窗口拉到 100ms 以上，
// 使排行榜 tok/s 谓词可被观测（见 runStreamCase 的说明）。
const streamTailDelay = 150 * time.Millisecond

// countLeaderboardTpsRows 用**排行榜自己的谓词**数行（与 admin_leaderboard.go 同源）。
func countLeaderboardTpsRows(t *testing.T, provisioned provisioning) int {
	t.Helper()
	reader, err := provisioned.pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	var count int
	if err := reader.QueryRow(context.Background(), `
		SELECT count(*) FROM message_request
		WHERE provider_id = $1
		  AND first_byte_ms IS NOT NULL
		  AND first_byte_ms < duration_ms
		  AND (duration_ms - first_byte_ms) >= 100`, provisioned.providerID,
	).Scan(&count); err != nil {
		t.Fatalf("排行榜谓词查询失败: %v", err)
	}
	return count
}

// assertClassifierSeesSuccess 用公开状态投影的分类器（Node 的 classifyProviderChainItemOutcome
// 的 Go 镜像）判**链项**是不是「成功」——它就是可用率的分母/分子口径。
//
// 为何要按链项而不是行：链项里**没有状态码字段**（`route.ChainItem` 无 StatusCode），
// 分类器只能靠 reason 词判——所以“写错一个词”在这里就会变成“每次成功都计失败”。
// 行级状态码会在 `IsSuccessStatusCode` 分支把它救回来，用行判会把这个缺陷掩掉。
func assertClassifierSeesSuccess(t *testing.T, row map[string]any) {
	t.Helper()
	item := lastChainItem(t, row)
	taxonomy, ok := pubstatus.ClassifyProviderChainItemOutcome(item)
	if !ok {
		t.Fatalf("分类器未给出判决（reason=%v）：该链项会被公开页归为不可判定", item.Reason)
	}
	if taxonomy.Outcome != pubstatus.OutcomeSuccess {
		t.Errorf("链项应被判为成功，实际 %s（reason=%v）——这正是可用率被拉低的形式",
			taxonomy.Outcome, item.Reason)
	}
}

// lastChainItem 把 provider_chain 末项解码成分类器输入（与公开状态投影同形）。
func lastChainItem(t *testing.T, row map[string]any) pubstatus.ProviderChainItem {
	t.Helper()
	chain, ok := row["provider_chain"].([]any)
	if !ok || len(chain) == 0 {
		t.Fatalf("provider_chain 应为非空数组，实际 %v", row["provider_chain"])
	}
	raw, err := json.Marshal(chain[len(chain)-1])
	if err != nil {
		t.Fatalf("重编码链项失败: %v", err)
	}
	var item pubstatus.ProviderChainItem
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("解码链项失败: %v", err)
	}
	return item
}

// lastChainReason 取 provider_chain 末项的 reason（终态原因就是最后一条尝试的结局）。
//
// 断言走**落库后的真实列**而不是内存里的留痕：本任务的两个缺陷（自造 reason、缺 first_byte_ms）
// 都只在「写进列」这一步体现，内存断言看不见。
func lastChainReason(t *testing.T, row map[string]any) string {
	t.Helper()
	chain, ok := row["provider_chain"].([]any)
	if !ok || len(chain) == 0 {
		t.Fatalf("provider_chain 应为非空数组，实际 %v", row["provider_chain"])
	}
	last, ok := chain[len(chain)-1].(map[string]any)
	if !ok {
		t.Fatalf("provider_chain 末项应为对象，实际 %v", chain[len(chain)-1])
	}
	reason, _ := last["reason"].(string)
	return reason
}

// 非流式的用量与耗时：三条线各跑一轮，断言 token 列与真实到达耗时。
//
// 假上游故意晚若干毫秒再回答：本机毫秒级返回时「耗时」与时钟分辨率分不开，
// 只有让上游真的慢下来，`duration_ms` 才能区分「量的是这一次请求」与「量的是 0」。
// 同时它盖住了两个真实缺陷：正文里的用量永不入账、duration_ms 恒为 0。
func TestIntegrationNonStreamWritesUsageAndDuration(t *testing.T) {
	for _, requestPath := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(requestPath, func(t *testing.T) {
			runNonStreamUsageCase(t, requestPath)
		})
	}
}

// 假上游的思考时长（ms）。断言只需「至少这么多」，不接受整秒级阈值：本机调度抖动
// 不该把功能验收变成计时竞赛。
const nonStreamUpstreamDelay = 12

func runNonStreamUsageCase(t *testing.T, requestPath string) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(nonStreamUpstreamDelay * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, nonStreamBodyFor(requestPath))
	}))
	defer upstream.Close()

	pools := integrationStore(t)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor(requestPath))
	handler := newIntegrationHandler(t, provisioned)

	request := httptest.NewRequest(http.MethodPost, requestPath,
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"你好"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", provisioned.apiKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	row := waitForFinalizedRow(t, provisioned)

	// 用量：来自上游正文，按上游协议线归一。
	if input := int(numberOrZero(row["input_tokens"])); input != testInputTokens {
		t.Errorf("input_tokens 应为 %d，收到 %d（行：%v）", testInputTokens, input, row)
	}
	if output := int(numberOrZero(row["output_tokens"])); output != testOutputTokens {
		t.Errorf("output_tokens 应为 %d，收到 %d", testOutputTokens, output)
	}
	// 实际模型：上游回报的模型名，用于区分「请求的模型」与「真实回答的模型」。
	if actual, _ := row["actual_response_model"].(string); actual == "" {
		t.Errorf("actual_response_model 不应为空，收到 %v", row["actual_response_model"])
	}
	// 耗时：必须盖住上游的思考时长。
	if duration := int(numberOrZero(row["duration_ms"])); duration < nonStreamUpstreamDelay {
		t.Errorf("duration_ms 应至少 %dms（上游真耗时），收到 %d", nonStreamUpstreamDelay, duration)
	}
}

// nonStreamBodyFor 按上游协议线给出假上游的非流式正文（含用量）。
//
// 必须与请求路径同线：数据面把上游正文原样转给客户端，跨线形状虽不报错，
// 但那样就证实不了「按上游线取用量」。
func nonStreamBodyFor(requestPath string) string {
	switch requestPath {
	case "/v1/chat/completions":
		return fmt.Sprintf(`{"id":"c1","object":"chat.completion","model":"gpt-5.6",`+
			`"choices":[{"index":0,"message":{"role":"assistant","content":"甲"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`, testInputTokens, testOutputTokens)
	case "/v1/responses":
		return fmt.Sprintf(`{"id":"r1","object":"response","model":"gpt-5.6","status":"completed","output":[],`+
			`"usage":{"input_tokens":%d,"output_tokens":%d}}`, testInputTokens, testOutputTokens)
	default:
		return fmt.Sprintf(`{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","content":[],`+
			`"usage":{"input_tokens":%d,"output_tokens":%d}}`, testInputTokens, testOutputTokens)
	}
}

// TestIntegrationFixtureWinsOverLeftoverTier 钉住夹具的选路隔离：库里存在同档的僵尸供应商时，
// 请求仍必须落到本次夹具。
//
// 回归的是一处真实故障：夹具原用固定 priority=-1000000，而被信号杀掉的旧运行会把同档僵尸
// 夹具（名字前缀 dataplane-it-、指向早已关闭的假上游端口）留在共享库里。于是 top 档有几十个
// 候选，每个请求在死端口之间加权随机，故障转移一路排除直到请求预算耗尽——/v1/messages 的
// 流式与非流式用例收到 504，断线风暴用例表现为落库行数不足。三条红是同一个根因。
//
// 假上游端口用 127.0.0.1:1（必然拒绝连接）：僵尸夹具的病根就是「连不上」，不必真起服务。
func TestIntegrationFixtureWinsOverLeftoverTier(t *testing.T) {
	const (
		leftoverFixtureCount = 40
		rounds               = 4
	)
	pools := integrationStore(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","content":[],`+
			`"usage":{"input_tokens":%d,"output_tokens":%d}}`, testInputTokens, testOutputTokens)
	}))
	defer upstream.Close()

	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))
	insertLeftoverTierProviders(t, pools, leftoverFixtureCount, providerTypeFor("/v1/messages"))
	handler := newIntegrationHandler(t, provisioned)

	for round := 1; round <= rounds; round++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages",
			strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"你好"}]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("x-api-key", provisioned.apiKey)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)

		// 命中僵尸夹具时这里是 504（gateway_timeout），正文会点出那个死端口。
		if recorder.Code != http.StatusOK {
			t.Fatalf("第 %d 轮应命中本次夹具（200），收到 %d（%s）", round, recorder.Code, recorder.Body.String())
		}
	}
	if got := countRequestRows(t, provisioned); got != rounds {
		t.Fatalf("应全部落在本次夹具上，实际落库 %d 行（期望 %d）", got, rounds)
	}
}

// insertLeftoverTierProviders 造出「同档僵尸夹具」：固定档位、指向必然拒绝连接的端口、已启用。
//
// 一次 INSERT 成批插入、一次 DELETE 成批回收：共享库上任何「先列后复读」的测试都会撞上并发的
// 增删窗口（internal/store 的 TestIntegrationReadProviderRoutingColumns 就是在两次查询之间被删掉行
// 而报 no rows in result set）。批量语句把两个窗口压到最小。
func insertLeftoverTierProviders(t *testing.T, pools *store.Pools, count int, providerType string) {
	t.Helper()
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	rows, err := writer.Query(context.Background(), `
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier)
		SELECT $1 || '-' || g, 'http://127.0.0.1:1/v1/messages',
		       'leftover-tier-key', $2, true, 1, $3, 1.0
		FROM generate_series(1, $4) AS g
		RETURNING id`, fmt.Sprintf("dataplane-it-leftover-%d", os.Getpid()), providerType, priorityLeftoverTier, count)
	if err != nil {
		t.Fatalf("插入同档僵尸供应商失败: %v", err)
	}
	defer rows.Close()
	ids := make([]int64, 0, count)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("读取僵尸供应商 id 失败: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("枚举僵尸供应商失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := writer.Exec(cleanupCtx, `DELETE FROM providers WHERE id = ANY($1)`, ids); err != nil {
			t.Logf("回收僵尸供应商失败（共享库会留下 %d 行）: %v", len(ids), err)
		}
	})
}

// assertRoutingTraceRecorded 断言终态行的 routing_trace 是**真实**的路径记录。
//
// 判据（与 Node 的 RoutingTraceV1 同形，且不许声称 Go 没有的机制）：
//   - version=1；mode 只能是 single_upstream / legacy_hedge（Go 无 discovery 子系统）；
//   - discoveryEnabled/eligible 恒 false；
//   - 至少有 request_started 与 request_finished，且时间戳非零（只记实测时刻）；
//   - 若链上有真实尝试，则必须出现对应的 attempt 事件（带真实 provider id）。
func assertRoutingTraceRecorded(t *testing.T, row map[string]any) {
	t.Helper()
	trace, ok := row["routing_trace"].(map[string]any)
	if !ok || trace == nil {
		t.Fatalf("routing_trace 应为对象（缺它则详情页决策链恒空），实际 %v", row["routing_trace"])
	}
	if version := int(numberOrZero(trace["version"])); version != 1 {
		t.Errorf("routing_trace.version 应为 1，收到 %d", version)
	}
	mode, _ := trace["mode"].(string)
	if mode != "single_upstream" && mode != "legacy_hedge" {
		t.Errorf("routing_trace.mode 应为 single_upstream/legacy_hedge（Go 无 discovery），收到 %q", mode)
	}
	if enabled, _ := trace["discoveryEnabled"].(bool); enabled {
		t.Errorf("Go 无 discovery 子系统，discoveryEnabled 不得为 true")
	}
	if eligible, _ := trace["eligible"].(bool); eligible {
		t.Errorf("Go 无 discovery 子系统，eligible 不得为 true")
	}
	events, _ := trace["events"].([]any)
	if len(events) == 0 {
		t.Fatalf("routing_trace.events 不应为空（请求确实转发过）")
	}
	seen := map[string]bool{}
	attemptProviders := 0
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		if event == nil {
			t.Fatalf("事件应为对象，实际 %v", raw)
		}
		kind, _ := event["type"].(string)
		seen[kind] = true
		if at := numberOrZero(event["at"]); at <= 0 {
			t.Errorf("事件 %s 缺实测时间戳（at=%v）", kind, event["at"])
		}
		if kind == "attempt_finished" {
			provider, _ := event["provider"].(map[string]any)
			if provider == nil || numberOrZero(provider["id"]) <= 0 {
				t.Errorf("attempt_finished 应带真实供应商 id，实际 %v", event["provider"])
			}
			attemptProviders++
		}
	}
	if !seen["request_started"] || !seen["request_finished"] {
		t.Errorf("routing_trace 应含 request_started 与 request_finished，实际 %v", seen)
	}
	if attemptProviders == 0 {
		t.Errorf("链上有真实尝试时 routing_trace 应记 attempt_finished，实际事件 %v", seen)
	}
}
