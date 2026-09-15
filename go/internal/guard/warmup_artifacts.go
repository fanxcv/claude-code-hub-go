package guard

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件是 warmup 抢答的会话工件落地（Node `warmup-guard.ts:44-75` 那一段）。
//
// 为什么值得落：抢答响应是**本进程自己造**的，上游根本没有这次请求，故会话详情页里
// 除它之外没有任何响应侧痕迹——不写这四条工件，用户看到的就是「会话里有这一次请求、
// 但既没有请求体也没有响应」。四条键分别是响应正文、响应头、上游请求元信息与上游响应
// 元信息，都由 warmupArtifactStore 的读取侧对应（见 adminapi 的会话详情接口）。

// WarmupUpstreamMetaURL 是抢答请求在「上游元信息」里的占位地址（Node 同常量）。
//
// 取这个不可路由的路径而不是留空：元信息列非空本身是事实（这次请求确实没发往上游），
// 空值会被详情页当成「未采集」。
const WarmupUpstreamMetaURL = "/__cch__/warmup"

// storeWarmupArtifacts 落抢答响应的四条工件。
//
// 两个前置条件与 Node 同判：有会话 id、允许落调试工件（高并发模式下不落）。
// 失败只记 warn：抢答已经决定，工件是旁路。
func (d Deps) storeWarmupArtifacts(ctx *pctx.Context, keyID int64, responseText string) {
	if d.WarmupArtifacts == nil || !ctx.ShouldPersistDebugArtifacts() {
		return
	}
	bound, ok := d.sessionIdentity(ctx)
	if !ok || bound.SessionID == "" {
		return
	}
	request := WarmupArtifactRequest{
		SessionID:  bound.SessionID,
		Sequence:   bound.Sequence,
		KeyID:      keyID,
		Method:     ctx.Method(),
		Body:       responseText,
		Headers:    map[string]string{"content-type": "application/json; charset=utf-8"},
		StatusCode: 200,
	}
	if err := d.WarmupArtifacts.StoreWarmupResponse(d.runContext(ctx), request); err != nil {
		d.logger().Warn("guard.warmup.artifacts_failed", map[string]any{"error": err.Error()})
	}
}

// sessionIdentity 取本次请求的会话身份；未接线（SessionLookup 为 nil）时返回 false。
func (d Deps) sessionIdentity(ctx *pctx.Context) (SessionResult, bool) {
	if d.SessionLookup == nil {
		return SessionResult{}, false
	}
	return d.SessionLookup(ctx)
}
