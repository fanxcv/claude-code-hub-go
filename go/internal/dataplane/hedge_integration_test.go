package dataplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是竞速接线的真实依赖集成测试（env 门控：未设置 CCH_TEST_DSN 时整组跳过）。
//
// 两条在无数据库环境里无法证伪的断言：
//  1. 流式请求的 provider_chain 真的会落库（此前流式路径不写该列，竞速原因无处留痕）；
//  2. 输家成本经**真实 SQL 的幂等谓词**只累加一次（重复投递同一输家不重复计费）。
//
// 竞速的时间语义（阈值、胜负、引流）由 forward 的 hedge_test 与 dataplane 的 hedge_test
// 覆盖；本文件不重复造第二个竞速夹具。

// TestIntegrationStreamWritesProviderChain 钉住流式终态写 provider_chain。
func TestIntegrationStreamWritesProviderChain(t *testing.T) {
	pools := integrationStore(t)
	upstream := newIntegrationStreamUpstream(t, http.StatusOK)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))
	handler, _ := newIntegrationHandlerWithPools(t, pools, upstream.URL)
	_ = handler

	// 直接打一条流式请求：终态由流终态协程写，必须等它落地。
	status, _ := integrationStreamRequest(t, pools, provisioned, "/v1/messages")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	row := waitForFinalizedRow(t, provisioned)
	chain := providerChainOf(t, row)
	if len(chain) == 0 {
		t.Fatalf("流式请求的 provider_chain 不应为空：%v", row["provider_chain"])
	}
	if chain[0]["id"] != float64(provisioned.providerID) {
		t.Fatalf("链首供应商应为夹具 %d，收到 %v", provisioned.providerID, chain[0]["id"])
	}
	if reason, _ := chain[0]["reason"].(string); reason == "" {
		t.Fatalf("链首缺少原因：%v", chain[0])
	}
}

// TestIntegrationHedgeLoserBilledOnce 钉住输家成本经真实 SQL 只累加一次。
//
// 用真实的 store.Pools（幂等谓词在 SQL 里）与真实价格：同一 (provider, attempt) 投递两次，
// cost_usd 只应增加一次，hedge_losers 只应有一条。
func TestIntegrationHedgeLoserBilledOnce(t *testing.T) {
	pools := integrationStore(t)
	upstream := newIntegrationStreamUpstream(t, http.StatusOK)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	// 先造一条真实的请求行（终态已落地，故 cost_usd 的基线可读）。
	status, _ := integrationStreamRequest(t, pools, provisioned, "/v1/messages")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}
	row := waitForFinalizedRow(t, provisioned)
	requestID := int64(row["id"].(float64))
	baseline := numberOrZero(row["cost_usd"])

	// 价格表里给夹具用的模型一份价格，其余一律查不到。
	resolver, _, _ := newTestResolver(t, "redirected", map[string]string{
		integrationModel: `{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}`,
	}, nil)
	biller := &hedgeLoserBiller{
		wiring: HedgeWiring{Costs: resolver, Pools: pools},
		state:  &RequestState{Model: integrationModel},
		logger: logx.New(nil),
	}
	input, output := 11.0, 7.0
	bill := forward.HedgeLoserBill{
		RequestID:          requestID,
		ProviderID:         provisioned.providerID,
		ProviderName:       "竞速输家夹具",
		Sequence:           1,
		UpstreamStatusCode: http.StatusOK,
		Usage:              convert.Usage{InputTokens: &input, OutputTokens: &output},
		Model:              integrationModel,
		DrainComplete:      true,
		At:                 time.Now(),
	}
	ctx := context.Background()
	// 投递两次：第二次必须被幂等谓词挡住（同一供应商的同一 attempt）。
	if err := biller.BillLoser(ctx, bill); err != nil {
		t.Fatalf("首次计费失败: %v", err)
	}
	if err := biller.BillLoser(ctx, bill); err != nil {
		t.Fatalf("重复计费失败: %v", err)
	}

	after := lastRequestRow(t, provisioned)
	if after["cost_usd"] == nil {
		t.Fatal("输家计费后 cost_usd 不该为空")
	}
	delta := numberOrZero(after["cost_usd"]) - baseline
	want := 11*0.000001 + 7*0.000002
	if diff := delta - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost_usd 只应增加一次 %.6f，实际增加 %.6f（基线 %.6f）", want, delta, baseline)
	}
	losers := hedgeLosersOf(t, after)
	if len(losers) != 1 {
		t.Fatalf("同一输家重复投递后应只有一条记录，实际 %d 条：%s", len(losers), after["hedge_losers"])
	}
	if losers[0]["providerId"] != float64(provisioned.providerID) {
		t.Fatalf("输家记录应指向 %d，收到 %v", provisioned.providerID, losers[0]["providerId"])
	}
}

// TestIntegrationCandidateCarriesFirstByteTimeout 钉住「首字节阈值确实由库列投影到候选」。
//
// 这条映射漏了的话，单测手填字段照样绿，而真实进程里竞速永远不会开（阈值恒为 0）——
// 正是本波先在真机上撞到的缺口，故用真实库把它钉住。
func TestIntegrationCandidateCarriesFirstByteTimeout(t *testing.T) {
	pools := integrationStore(t)
	upstream := newIntegrationStreamUpstream(t, http.StatusOK)
	provisioned := provision(t, pools, upstream.URL, providerTypeFor("/v1/messages"))

	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	ctx := context.Background()
	if _, err := writer.Exec(ctx,
		`UPDATE providers SET first_byte_timeout_streaming_ms = 7 WHERE id = $1`,
		provisioned.providerID,
	); err != nil {
		t.Fatalf("改夹具首字节阈值失败: %v", err)
	}

	source := newCandidateSource(pools, nil, nil, logx.New(nil))
	candidate, _, err := source.Candidate(ctx, pctx.ProviderSelection{
		ProviderID: provisioned.providerID,
		Name:       "夹具",
		Type:       providerTypeFor("/v1/messages"),
	}, SelectionFacts{})
	if err != nil {
		t.Fatalf("取候选失败: %v", err)
	}
	if candidate == nil {
		t.Fatal("候选不应为空")
	}
	if candidate.Provider.FirstByteTimeoutStreamingMS != 7 {
		t.Fatalf("候选的首字节阈值应为 7，实际 %d", candidate.Provider.FirstByteTimeoutStreamingMS)
	}
}

// integrationModel 是夹具模型名（与 nonStreamBodyFor / streamFramesFor 的模型一致）。
const integrationModel = "claude-sonnet-4-5"

// newIntegrationStreamUpstream 是一条固定 SSE 的假上游（与既有集成夹具同形）。
func newIntegrationStreamUpstream(t *testing.T, status int) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		flusher, _ := w.(http.Flusher)
		for _, frame := range integrationStreamFrames() {
			_, _ = io.WriteString(w, frame)
		}
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// integrationStreamFrames 返回一条含用量与终止标记的 claude 流。
func integrationStreamFrames() []string {
	return []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"model":"claude-sonnet-4-5",` +
			fmt.Sprintf(`"usage":{"input_tokens":%d}}}`, testInputTokens) + "\n\n",
		`event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"甲"}}` + "\n\n",
		`event: message_delta` + "\n" +
			fmt.Sprintf(`data: {"type":"message_delta","usage":{"output_tokens":%d}}`, testOutputTokens) + "\n\n",
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
}

// newIntegrationHandlerWithPools 用给定的池装配数据面（夹具可能读池与写池不同）。
func newIntegrationHandlerWithPools(t *testing.T, pools *store.Pools, _ string) (http.Handler, *store.Pools) {
	t.Helper()
	assembly, err := NewStoreBacked(StoreOptions{Pools: pools, Logger: logx.New(nil)})
	if err != nil {
		t.Fatalf("数据面装配失败: %v", err)
	}
	return assembly.Handler, pools
}

// integrationStreamRequest 打一条流式请求并读尽响应体，返回状态码与正文。
func integrationStreamRequest(
	t *testing.T,
	_ *store.Pools,
	provisioned provisioning,
	requestPath string,
) (int, string) {
	t.Helper()
	handler, _ := newIntegrationHandlerWithPools(t, provisioned.pools, "")
	server := httptest.NewServer(handler)
	defer server.Close()
	body := fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		integrationModel)
	request, err := http.NewRequest(http.MethodPost, server.URL+requestPath, strings.NewReader(body))
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
	payload, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Fatalf("读响应失败: %v", readErr)
	}
	return response.StatusCode, string(payload)
}

// providerChainOf 取行里的 provider_chain 数组。
func providerChainOf(t *testing.T, row map[string]any) []map[string]any {
	t.Helper()
	raw, ok := row["provider_chain"]
	if !ok || raw == nil {
		t.Fatalf("行里没有 provider_chain：%v", row)
	}
	return decodeObjectArray(t, raw, "provider_chain")
}

// hedgeLosersOf 取行里的 hedge_losers 数组。
func hedgeLosersOf(t *testing.T, row map[string]any) []map[string]any {
	t.Helper()
	raw, ok := row["hedge_losers"]
	if !ok || raw == nil {
		return nil
	}
	return decodeObjectArray(t, raw, "hedge_losers")
}

// decodeObjectArray 把 jsonb 列解成对象数组（row_to_json 已把 jsonb 解成 go 值）。
func decodeObjectArray(t *testing.T, raw any, column string) []map[string]any {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("重编码 %s 失败: %v", column, err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("解析 %s 失败: %v（原文 %s）", column, err, encoded)
	}
	return decoded
}

// decodeObjectArrayOrNil 是 decodeObjectArray 的**宽松版**：形状不符时返回 ok=false 而不 Fatal。
//
// 它专用于「从共享库的若干候选行里挑自己的那一行」的场景：别的夹具行形状可能是对象或标量，
// 遇到就跳过即可。**调用方仍必须在一条都找不到时失败**——宽松不等于静默变绿。
func decodeObjectArrayOrNil(raw any) ([]map[string]any, bool) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var decoded []map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return nil, false
	}
	return decoded, true
}
