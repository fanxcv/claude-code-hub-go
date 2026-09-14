package main

import (
	"context"
	"fmt"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件装配前缀亲和的**存储与开关**。两件事必须一起做对，否则亲和会静默失效：
//
//  1. 存储：`route.Selector` 的 `Options.Affinity` 为 nil 时不查找也不提名（见 select.go 的注释），
//     于是亲和既不路由也不写回——2026-09-12 之前的生产装配正是这个状态。
//  2. 开关：Node 的判定是「env 强制 或 系统设置」（src/app/v1/_lib/proxy/affinity/config.ts:11-13），
//     且系统设置默认开（src/lib/config/system-settings-cache.ts:168,290）。只看 env 会让
//     「靠系统设置开亲和」的部署在切到 Go 后静默变成关闭。

// affinityStatus 是亲和的开关结论，供启动日志与 /readyz 如实报告。
//
// 「静默关闭」是最难发现的一类问题：路由粘性退化，没有任何错误，只表现为请求在多供应商间抖动。
type affinityStatus struct {
	// Enabled 为真表示本次装配启用了亲和。
	Enabled bool
	// Source 说明是谁打开的：env / system_setting / env+system_setting / disabled。
	Source string
	// Window 与 TTLSeconds 是生效的窗口与滑动 TTL，便于与 Node 对齐核对。
	Window     int
	TTLSeconds int
	// SettingsErr 非空表示系统设置读取失败，已回落到 Node 的默认值（默认开）。
	SettingsErr string
	// IgnoreClientSessionID 是 affinityIgnoreClientSessionId 的生效值（Node 默认 true，
	// `src/lib/config/system-settings-cache.ts:168` 的 DEFAULT_SETTINGS）。
	//
	// 它不参与选路判定（选路只看亲和是否装配），只决定请求日志的 session_identity_kind：
	// 为真且请求可指纹化时记 prefix_affinity（跨会话复用同一渠道），否则记 session_id。
	IgnoreClientSessionID bool
}

// describe 是给 /readyz 与日志用的一行结论。
func (s affinityStatus) describe() string {
	if !s.Enabled {
		return "disabled（env 未开且系统设置未开）"
	}
	text := fmt.Sprintf("enabled（来源 %s，window %d，ttl %ds）", s.Source, s.Window, s.TTLSeconds)
	if s.SettingsErr != "" {
		text += "；系统设置读取失败，已按 Node 默认值（开）处理"
	}
	return text
}

// affinityDecision 把两路开关合成结论，优先级逐字对齐 Node：`env || settings`。
//
// 系统设置读取失败时按 Node 的默认值处理（affinityIgnoreClientSessionId 默认 true，
// 见 proxy-runtime.ts:53,61 的两个 fallback 分支与 system-settings-cache.ts:168），
// 而不是保守关掉——两侧不一致才是缺陷。
func affinityDecision(envEnabled bool, settings *store.SystemSettings, settingsErr error) affinityStatus {
	settingsEnabled := true
	status := affinityStatus{}
	switch {
	case settingsErr != nil:
		status.SettingsErr = settingsErr.Error()
	case settings != nil:
		settingsEnabled = settings.AffinityIgnoreClientSessionID
	}
	switch {
	case envEnabled && settingsEnabled:
		status.Source = "env+system_setting"
	case envEnabled:
		status.Source = "env"
	case settingsEnabled:
		status.Source = "system_setting"
	default:
		status.Source = "disabled"
	}
	status.Enabled = envEnabled || settingsEnabled
	// 日志身份形制取设置的原值（不进 env 的或）：Node 里 skipSessionBinding 只由
	// `affinityIgnoreClientSessionId && fingerprintable` 决定，ENABLE_PREFIX_AFFINITY
	// 只是「要不要做亲和」，不改变身份形制。
	status.IgnoreClientSessionID = settingsEnabled
	return status
}

// affinitySetup 是装配出的亲和存储与其专用 Redis 连接。
type affinitySetup struct {
	// store 为 nil 表示本次不启用亲和（开关关闭或没有 Redis）。
	store  *route.AffinityStore
	client redis.UniversalClient
	status affinityStatus
}

// close 释放专用连接。
func (s affinitySetup) close() {
	if s.client != nil {
		_ = s.client.Close()
	}
}

// openAffinity 建亲和存储。
//
//   - 未配置 REDIS_URL 时返回零值：存储为 nil，选择器完全不启用亲和（既不查找也不提名）。
//     这与「Node 读到坏绑定后 fail-open 回落加权随机」是两件事：没有 Redis 就没有亲和。
//   - 开关关闭时同样不建存储，并立即释放刚建的连接（省掉每请求的 Redis 往返）。
//   - 系统设置行读取失败不阻断启动：按 Node 的默认值处理并如实记录（见 affinityDecision）。
func openAffinity(ctx context.Context, cfg config.Config, pools *store.Pools, logger *logx.Logger) affinitySetup {
	if cfg.RedisURL == "" {
		logger.Warn("affinity_absent", map[string]any{"reason": "未配置 REDIS_URL"})
		return affinitySetup{status: affinityStatus{Source: "disabled"}}
	}

	options, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		// 错误信息可能回显 URL，故只报类型不回显原文（与 openSubscriber 同款纪律）。
		logger.Warn("affinity_absent", map[string]any{
			"reason": fmt.Sprintf("解析 REDIS_URL 失败（值已隐去）: %T", err),
		})
		return affinitySetup{status: affinityStatus{Source: "disabled"}}
	}

	// 亲和自己的命令连接：刻意不复用 cfgsync 的订阅连接——订阅连接被 Pub/Sub 长期占用，
	// 且其关闭时机归 cfgsync，复用会让「谁先关谁背锅」。连接池开销可忽略。
	client := redis.NewClient(options)

	settings, settingsErr := pools.FindSystemSettings(ctx)
	status := affinityDecision(cfg.Env.EnablePrefixAffinity, settings, settingsErr)
	status.Window = route.AffinityWindow(cfg.Env.PrefixAffinityWindow)
	status.TTLSeconds = cfg.Env.PrefixAffinityTTLSeconds

	if !status.Enabled {
		_ = client.Close()
		logger.Info("affinity_disabled", map[string]any{"source": status.Source})
		return affinitySetup{status: status}
	}
	return affinitySetup{
		store: route.NewAffinityStore(route.AffinityOptions{
			Redis:             client,
			Window:            cfg.Env.PrefixAffinityWindow,
			SlidingTTLSeconds: cfg.Env.PrefixAffinityTTLSeconds,
		}),
		client: client,
		status: status,
	}
}
