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
	// SettingsErr 非空表示系统设置读取失败，已回落到出厂默认（总闸开、模式为会话优先）。
	SettingsErr string
	// IgnoreClientSessionID 是 affinityIgnoreClientSessionId 的生效值（**模式开关**）。
	//
	// 它为真时强制前缀指纹粘性（会话绑定层跳过）；为假时会话绑定优先、前缀作兜底。
	// 它**不管**亲和开不开——那是 Enabled。2026-09-21 之前两者挤在同一字段，
	// 于是「让会话粘性生效」与「保持亲和可用」不可兼得（见 drizzle/0132 迁移注释）。
	IgnoreClientSessionID bool
}

// affinityBootSnapshotNote 是 /readyz 与启动日志里那半「取自启动时系统设置」的标注。
//
// 提为包级常量是为了让钉子能引用同一串：测试里另抄一份会在改文案时静默变绿。
const affinityBootSnapshotNote = "；注：来源与模式取自启动时的系统设置（启动快照），运行时改设置后选路即生效、本行不更新；window/ttl 取自 env，本就是启动常量"

// describe 是给 /readyz 与日志用的一行结论，**自带启动快照标注**。
//
// 为何必须标注：Enabled / Source / IgnoreClientSessionID 三者取自启动时读到的系统设置，
// 而它们对应的选路已改为**逐请求读**快照（dataplane 的 affinitySwitchesFor），运行时改设置
// **立即**生效、本行却不随之更新。不标注就会让 /readyz 读起来像「随时可查的活值」——
// 与「接口返 200 而行为不变」是同一族误导，只是方向相反。
//
// 为何 Window / TTLSeconds **不**并入标注：它们取自 env（cfg.Env.PrefixAffinityWindow /
// PrefixAffinityTTLSeconds），env 本就是启动常量，报快照是对的。
func (s affinityStatus) describe() string {
	if !s.Enabled {
		return "disabled（env 未开且系统设置未开）" + affinityBootSnapshotNote
	}
	mode := "会话粘性（前缀兜底）"
	if s.IgnoreClientSessionID {
		mode = "前缀指纹粘性（忽略会话 ID）"
	}
	text := fmt.Sprintf("enabled（来源 %s，window %d，ttl %ds，%s）", s.Source, s.Window, s.TTLSeconds, mode)
	if s.SettingsErr != "" {
		text += "；系统设置读取失败，已按出厂默认（总闸开、模式为会话优先）处理"
	}
	return text + affinityBootSnapshotNote
}

// affinityDecision 把两路总闸开关合成结论，优先级逐字对齐 Node：`env || settings`。
//
// **两个开关各自独立**（2026-09-21 拆分，见 drizzle/0132）：
//   - 总闸 (Enabled)：env `ENABLE_PREFIX_AFFINITY` 覆写，或系统设置 `affinity_enabled`（默认开）。
//   - 模式 (IgnoreClientSessionID)：只取自系统设置 `affinity_ignore_client_session_id`，
//     真 = 强制前缀粘性（会话绑定层跳过），假 = 会话优先、前缀兜底。
//
// 拆开的原因：原先该字段既当总闸又当模式，翻它的默认值会把整套亲和关掉，
// 而保持 true 又无法让会话粘性生效——两个诉求不可兼得。
//
// 系统设置读取失败时按出厂默认处理（总闸开、模式为会话优先），而不是保守关掉
// ——两侧不一致才是缺陷。
func affinityDecision(envEnabled bool, settings *store.SystemSettings, settingsErr error) affinityStatus {
	settingsEnabled := true
	ignoreClientSession := false
	status := affinityStatus{}
	switch {
	case settingsErr != nil:
		status.SettingsErr = settingsErr.Error()
	case settings != nil:
		settingsEnabled = settings.AffinityEnabled
		ignoreClientSession = settings.AffinityIgnoreClientSessionID
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
	// 模式只由系统设置决定：它只管「会话绑定层跳不跳」，不管亲和开不开。
	status.IgnoreClientSessionID = ignoreClientSession
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
//   - 开关关闭时**仍然建存储**（见下方 return 前的注释）：总闸与模式已改成逐请求读，
//     按启动时的开关决定建不建，会让运行时打开得到「报 enabled、实际不生效」的假象。
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

	// 存储**无条件**建：总闸与模式都已改成逐请求读（见 route.Options.AffinitySwitches 与
	// dataplane 的 affinitySwitchesFor），若仍按启动时的开关决定建不建，运行时把它打开就只会
	// 得到「报 enabled、实际不生效」——与接口返 200 的假象同类。
	// 关着时的零开销由选路层保证：总闸为假即整层不进，一次 Redis 都不发。
	if !status.Enabled {
		logger.Info("affinity_disabled", map[string]any{
			"source": status.Source,
			"note":   "开关逐请求读、存储已建，运行时打开即生效（无需重启）",
		})
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
