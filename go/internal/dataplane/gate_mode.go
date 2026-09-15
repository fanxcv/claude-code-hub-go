package dataplane

import (
	"context"
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// 本文件解析流式内容门控模式（`system_settings.stream_gate_mode`）。
//
// Node 的取法（stream-content-gate.ts:161-167）是「系统设置快照优先，无快照回退 env，
// 兜底异常按 off」。另有两支强制 enforce（forwarder.ts:2034、5334）：codex 强制流式与
// 「本请求是回放 owner」——前者由 forward 的 ForceGate 承载，后者由 gateModeForRequest 补。

// streamGateModeEnv 是 env 回退值的进程内一次性解析。
//
// 逐请求调 config.LoadEnv 是白读环境变量（env 在装载期已校验过），故只解析一次。
var streamGateModeEnv = sync.OnceValue(func() gate.Mode {
	env, err := config.LoadEnv(config.LookupFromOS())
	if err != nil {
		// Node 的 catch 分支同样返回 "off"：配置读不到时不门控。
		return gate.ModeOff
	}
	mode, ok := gate.ParseMode(env.StreamGateMode)
	if !ok {
		return gate.ModeOff
	}
	return mode
})

// resolveStreamGateMode 复刻 Node 的 resolveStreamGateMode：快照优先，env 兜底。
//
// 快照里的取值非法（空串或未知值）时也回退 env：Node 的快照侧有枚举校验，Go 侧读的是
// 数据库原文，故「非法值」必须有一个与 Node 同向的落点，而不是静默按 enforce 门控。
func (h *Handler) resolveStreamGateMode(ctx context.Context) gate.Mode {
	if h.options.Base.Settings != nil {
		settings, err := h.options.Base.Settings.FindSystemSettings(ctx)
		if err == nil && settings != nil {
			if mode, ok := gate.ParseMode(settings.StreamGateMode); ok {
				return mode
			}
		}
	}
	return streamGateModeEnv()
}

// gateModeForRequest 解析本次请求的门控模式，并补上 Node 的「回放 owner 强制门控」。
//
// 为什么 owner 必须门控：回放 spool 依赖门控的 precommit 语义（只有首个有效内容帧之后
// 才向客户端提交），Node 的判据里 `session.replayState?.role === "owner"` 正是这一支。
// 若在 off/shadow 下照配置跳过门控，spool 会把「中性前缀」也算成已交付内容。
func (h *Handler) gateModeForRequest(ctx context.Context, state *RequestState) gate.Mode {
	if state != nil && state.replay != nil && state.replay.holdsOwnership() {
		return gate.ModeEnforce
	}
	return h.resolveStreamGateMode(ctx)
}
