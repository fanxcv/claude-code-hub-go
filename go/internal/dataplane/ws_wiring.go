package dataplane

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件是上游 WS 在**数据面侧的接线**：资格判定与「跳过」的可观测面。
//
// 为什么从 assemble.go 里抽成具名构造函数：这两样都是「生产怎么接」的事实，而装配函数
// 需要真库、真 Redis 才跑得起来——抽出来之后端到端用例（ws_upstream_e2e_test.go）用的是
// **同一份**资格判定，而不是测试自己重写一遍「我认为的资格」。后者会让接线分叉
// （测试全绿、生产永不生效），正是 2026-09-19 那次排障的形态。

// wsEligibleFunc 是上游 WS 的资格判定缝（与 forward.Deps.WSEligible 同签名）。
type wsEligibleFunc func(ctx context.Context, pc *pctx.Context, provider forward.Provider) bool

// wsEligibility 构造资格判定：逐条对齐 Node 的三条件。
//
// 每请求判定而不是构造期一次：设置开关读的是快照（cfgsync，60s + 失效广播），
// 供应商类型来自本次候选。
//
// 三条全真才走上游 WS；任一条不真即回落 HTTP SSE 隧道，客户端可见协议不变。
func wsEligibility(settings guard.SettingsSource) wsEligibleFunc {
	return func(ctx context.Context, pc *pctx.Context, provider forward.Provider) bool {
		if !forward.IsWebSocketClientRequest(pc) {
			return false
		}
		if provider.Type != convert.ProviderCodex {
			return false
		}
		snapshot, err := settings.FindSystemSettings(ctx)
		return err == nil && snapshot != nil && snapshot.EnableOpenAIResponsesWebsocket
	}
}

// wsNoticeKeyLimit 是「跳过上游 WS」去重表的键数上界。
const wsNoticeKeyLimit = 512

// newWSNotice 构造「跳过上游 WS」的上报面。
//
// 去重维度是「原因 + 供应商 + 供应商类型」：跳过是每请求发生的，而可读的信号是组合
// （例如「客户端是 WS、这家是 codex、却因开关关闭没走」与「这家不是 codex」是两件事）。
// 每个组合只记一条 warn，故日志量与供应商数同阶，不需要限频器。
//
// 为什么不是逐条：热路径上逐条会淹掉日志；而为什么不是完全静默：跳过路径不写链
// （链词表冻结），日志是唯一能回答「WS 为什么没生效」的地方。
func newWSNotice(logger *logx.Logger) func(forward.WSSkip) {
	if logger == nil {
		return nil
	}
	var mu sync.Mutex
	seen := map[string]struct{}{}
	enabled := true
	return func(skip forward.WSSkip) {
		key := strings.Join([]string{string(skip.Cause), skip.ProviderType, strconv.FormatInt(skip.ProviderID, 10)}, "|")
		mu.Lock()
		if !enabled {
			mu.Unlock()
			return
		}
		if _, ok := seen[key]; ok {
			mu.Unlock()
			return
		}
		// 键表上界：原因 4 种、类型个位数、供应商数十家，正常规模远低于此；到顶即停止上报
		// （宁可少记，也不让一张无界的表跟着进程长）。判据在**写入前**，故表长度严格不超过
		// wsNoticeKeyLimit——否则这条注释与实际容量对不上，后来者会按错的数字估算内存。
		if len(seen) >= wsNoticeKeyLimit {
			enabled = false
			mu.Unlock()
			return
		}
		seen[key] = struct{}{}
		mu.Unlock()
		logger.Warn("forward.ws_skip", map[string]any{
			"cause":         string(skip.Cause),
			"provider_id":   skip.ProviderID,
			"provider_type": skip.ProviderType,
			// 端点 URL 先脱敏：供应商地址里可能带凭据（与拨号层、upws 同口径）。
			"endpoint_url": dial.RedactURL(skip.EndpointURL),
			"effect":       "upstream_websocket_not_used",
		})
	}
}
