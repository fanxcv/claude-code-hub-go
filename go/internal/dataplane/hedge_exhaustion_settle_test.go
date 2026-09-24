package dataplane

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住竞速耗尽（D2）的终态留痕：全部尝试失败时，终态链必须照写。
//
// 生产证据（修复前）：16 行 provider_id=167 / status_code=524 的记录，provider_chain=[]、
// routing_trace=null——竞速的 maybeFinishLocked 只送 hedgeResult{err}，Result 为 nil，
// 于是终态链（只在 len(Attempts) > 0 时才写）整段落空。
//
// 反证：把 maybeFinishLocked 的 exhaustedResultLocked() 换回 nil，本文件两条用例转红。

// failingUpstream 回 500，让竞速的全部尝试都失败。
func failingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// exhaustedHedgeHandler 造一个「竞速开启 + 上游必败 + 无更多候选」的数据面。
func exhaustedHedgeHandler(t *testing.T) (*Handler, *terminalCaptureWriter, *divertRecorder) {
	t.Helper()
	return newTerminalCaptureHandler(t, terminalCaptureAssembly{
		upstreamURL: failingUpstream(t).URL,
		provider: chainEntryProvider{
			selection: pctx.ProviderSelection{ProviderID: 156, Name: "当选者", Type: "claude"},
			entry:     slowDivertChainEntry(t, 167, 156),
		},
		// 首字节阈值 > 0 是竞速前置；备选由 Failover 返回 nil（无更多候选）⇒ 耗尽。
		firstByteMS:  60000,
		hedgeEnabled: true,
		settings:     hedgeTestSettings{settings: store.SystemSettings{LegacyHedgeMaxInFlight: 2}},
	})
}

// TestHedgeExhaustedWritesProviderChainAndRoutingTrace 竞速全失败时 provider_chain 非空、
// routing_trace 非空——这是生产 16 行空链记录的直接回归。
func TestHedgeExhaustedWritesProviderChainAndRoutingTrace(t *testing.T) {
	handler, writer, _ := exhaustedHedgeHandler(t)

	status := terminalCaptureRequest(t, handler, true)
	if status == http.StatusOK {
		t.Fatalf("上游全失败时不应回 200，实际 %d", status)
	}
	patch, ok := writer.lastPatch()
	if !ok {
		t.Fatal("竞速耗尽必须留下终态 patch")
	}
	if len(patch.ProviderChain) == 0 {
		t.Fatalf("竞速耗尽时 provider_chain 不得为空（修复前恒为空数组）：%+v", patch)
	}
	if len(patch.RoutingTrace) == 0 {
		t.Fatalf("竞速耗尽时 routing_trace 不得为 null（修复前恒为 null）：%+v", patch)
	}
	items := decodeChainItems(t, patch.ProviderChain)
	if len(items) == 0 {
		t.Fatalf("provider_chain 应是带尝试留痕的数组，实际 %s", patch.ProviderChain)
	}
	if reason, _ := items[0]["reason"].(string); reason == "" {
		t.Fatalf("链上首条尝试应带失败原因，实际 %v", items[0])
	}
}

// TestHedgeExhaustedSettlesOnce 钉住「只结算一次」：竞速耗尽的终态不得由 forward 与数据面各写一遍。
func TestHedgeExhaustedSettlesOnce(t *testing.T) {
	handler, writer, _ := exhaustedHedgeHandler(t)

	terminalCaptureRequest(t, handler, true)
	if got := writer.patchCount(); got != 1 {
		t.Fatalf("竞速耗尽的终态只应写一次，实际 %d 次", got)
	}
}
