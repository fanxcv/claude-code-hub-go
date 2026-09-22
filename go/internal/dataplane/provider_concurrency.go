package dataplane

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件是供应商并发名额的**生产接线面**：把 limit 包现成的 provider 维度机制
// （键 provider:{id}:active_sessions / provider:{id}:active_session_refs + 三个 Lua 脚本）
// 接到转发路径的**唯一拨号口**上（缝的契约见 forward.Deps.ProviderInFlight）。
//
// 为什么必须接：该机制此前**全套就位但零调用**——键、三个 Lua 脚本、三个 Go 方法
// （CheckAndTrackProviderSession / ReleaseProviderSession / ForceTerminateProviderSession）
// 都在，但全仓调用点只有测试；生产路径上对该 ZSET 的唯一动作是 session 包的**删**。
// 后果就是用户看到的两个现象：providers.limit_concurrent_sessions 设了不生效，
// 渠道并发 session 数恒为 0（展示面一直在读这个恒空的集合）。
//
// 为什么不落在守卫链的限流步骤：那一步排在 provider 步**之前**（见 guard.go 的预设顺序），
// 此刻还不知道本次会落到哪一家；而名额是按供应商计的，只有拨号点才既知道渠道、
// 又能在同一次原子调用里「判定 + 占名额」。详见 forward.Deps.ProviderInFlight 的注释。

// providerInFlightReleaseTimeout 是释放名额的时间上限。
//
// 为什么用独立上下文而不是请求上下文：客户端中断、竞速败者被取消、上游静默超时这三类结局
// 恰恰都发生在请求上下文已取消之后（与终态写入同一个坑，见 settleTimeout 的说明）。
// 用请求上下文会让释放必然失败，名额只能等 Lua 的 TTL 过期——那正是「泄漏」的定义。
const providerInFlightReleaseTimeout = 5 * time.Second

// providerConcurrencyGate 是并发名额的生产实现。
//
// 一个进程一份（建在装配期），每请求只多一个闭包。
type providerConcurrencyGate struct {
	tracker *limit.SessionTracker
	// trackingEnabled 是全局统计开关（逐请求调用）；nil 表示未接线。
	//
	// 为什么必须逐请求读而不是构造期快照：构造期快照会让「管理面改了开关、进程重启才生效」，
	// 而本仓对同类开关（affinitySwitchesFor / wsEligibility）已定下逐请求读的口径。
	//
	// 它**只管统计（页面显示）**，不管执法：执法只看渠道自己有没有设并发数
	// （见 acquire 的闸门说明）。故它关着也不会让「配了上限的渠道」漏判。
	trackingEnabled func(ctx context.Context) bool
	logger          *logx.Logger
}

// inFlight 给出本请求的登记缝。
//
// sessionID 在构造期捕获：它是每请求事实（守卫链的会话步骤赋值，见 RequestState.sessionID），
// 而 forward.Deps 跨请求共享（dataplane 每请求拷贝一份再填 Facts，这里同理）。
func (g *providerConcurrencyGate) inFlight(
	state *RequestState,
) func(context.Context, int64, int) forward.ProviderInFlightResult {
	return func(ctx context.Context, providerID int64, providerLimit int) forward.ProviderInFlightResult {
		return g.acquire(ctx, providerID, providerLimit, state.sessionID)
	}
}

// acquire 登记一次在飞占用，并按上限判定是否放行。
//
// 闸门（两条都不满足时**连一次 Redis 命令都不发**）：
//
//  1. 该渠道配了并发上限（providerLimit > 0）——**必须判定，与统计开关无关**。
//     执法只取决于渠道自己有没有设并发数（用户 2026-09-22 明确：「渠道设置大于 0 的并发数，
//     就需要控制这个渠道的并发情况，跟我开不开统计开关有啥关系」）。把判定挂在展示面开关上
//     会让「配了上限」继续不生效——那正是本次要修的 bug。
//  2. 全局统计开关开启——必须登记，但**不判上限**（Lua 的 `limit > 0` 闸门关着）：
//     这一半纯粹为页面显示（读面四态与前端 5s 轮询都由它门控）。
//
// 故「关上完全不占用资源」的准确含义：**没有任何渠道设上限**且开关关着时零 Redis 命令、
// 零 429、页面不显示；而只要某渠道设了上限，执法所必需的那次登记照发（开关状态不影响）。
//
// 判定与统计共用同一次 Lua 调用（check-and-track-session.lua 本来就是「检查 + 追踪」一体），
// 故两者不会互相放大开销。
func (g *providerConcurrencyGate) acquire(
	ctx context.Context,
	providerID int64,
	providerLimit int,
	sessionID string,
) forward.ProviderInFlightResult {
	if g.tracker == nil || providerID <= 0 {
		return forward.ProviderInFlightResult{Allowed: true}
	}
	// 会话身份去空白后再判：空身份会把所有这类请求压成同一个成员，反而造成假满。
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return forward.ProviderInFlightResult{Allowed: true}
	}
	tracking := g.trackingEnabled != nil && g.trackingEnabled(ctx)
	if !tracking && providerLimit <= 0 {
		return forward.ProviderInFlightResult{Allowed: true}
	}

	result, err := g.tracker.CheckAndTrackProviderSession(ctx, providerID, sessionID, providerLimit)
	if err != nil {
		// limit 层已把 Redis 故障 Fail Open 成 Allowed=true；这里再兜一层并留痕，
		// 保证「登记失败」永远不会变成客户端可见的 429（限流判定的既有口径）。
		g.logger.Warn("dataplane.provider_in_flight_failed", map[string]any{
			"provider_id": providerID,
			"error":       err.Error(),
		})
		return forward.ProviderInFlightResult{Allowed: true}
	}
	if !result.Allowed {
		return forward.ProviderInFlightResult{Current: result.Count}
	}
	if !result.Referenced {
		// Lua 的 referenced=0 表示本次没有拿到释放引用（既有成员且引用已清零等形态）：
		// 不得安排释放，否则会把别的在飞尝试的引用多减一次。
		return forward.ProviderInFlightResult{Allowed: true, Current: result.Count}
	}
	registered := sync.OnceFunc(func() { g.release(providerID, sessionID) })
	return forward.ProviderInFlightResult{
		Allowed: true,
		Current: result.Count,
		// 幂等在实现侧收口（不只靠 body 包装的 once）：正文会被多条路径 Close
		// （正常读完、错误分支的 defer、竞速裁决时的强制关闭），多减一次引用会把
		// **另一个**在飞尝试的名额提前抹掉。
		Release: registered,
	}
}

// release 归还一次名额引用（一个引用归零才真正从并发集合里摘除，见 release-provider-session.lua）。
func (g *providerConcurrencyGate) release(providerID int64, sessionID string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), providerInFlightReleaseTimeout)
	defer cancel()
	if _, _, err := g.tracker.ReleaseProviderSession(ctx, providerID, sessionID); err != nil {
		g.logger.Warn("dataplane.provider_in_flight_release_failed", map[string]any{
			"provider_id": providerID,
			"error":       err.Error(),
		})
	}
}

// providerConcurrencyForRequest 取本请求的登记缝；未接线时返回 nil（forward 整段跳过）。
//
// 为何不能在这里按统计开关短路：执法与开关无关（见 acquire 的闸门说明），而本函数只知道
// 「本请求」、不知道「本次会落到哪家渠道配没配上限」——在这里跳过会让配了上限的渠道漏判。
func (h *Handler) providerConcurrencyForRequest(state *RequestState) func(context.Context, int64, int) forward.ProviderInFlightResult {
	gate := h.options.providerConcurrency
	if gate == nil {
		return nil
	}
	return gate.inFlight(state)
}
