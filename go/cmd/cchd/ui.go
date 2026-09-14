package main

// 本文件把 UI 静态面（embed 产物）接进进程启动：`CCH_EGRESS_PAGES=embed` 时页面面由
// internal/uiapp 直接作答，不再回退 Node（Node 下线后的终态）。
//
// 三条纪律：
//
//  1. **产物缺失即 fail fast**（与数据面同向，与「管理面装不起来只降级」相反）：页面面没有
//     别的落点，静默服务一个空站点比起不来更难排查。
//  2. **不装 embed 时行为一字不变**：`off` 档仍由本进程自答（不服务页面面），
//     本文件只在 embed 档被调用。
//  3. **会话只消费管理面已解析的结论**：壳注入里的会话快照由 internal/adminapi 的只读解析
//     入口给出（见 uiSessionResolver），本文件不自带解析逻辑——两份解析迟早分叉。

import (
	"context"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/appversion"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/uiapp"
)

// uiOptions 是 UI 面装配缝的参数。
type uiOptions struct {
	Logger *logx.Logger
	// Pools 是与数据面共用的同一套连接池（由 boot 开与关）；nil 时元数据取不到，
	// 壳只注入会话与 locale。
	Pools *store.Pools
	// Sessions 是管理面守卫的只读会话解析入口（boot 从管理面装配处回填）；
	// nil 表示管理面没装起来（无 DSN），壳对任何凭据都按未登录处理。
	Sessions adminapi.SessionResolver
}

// uiStatus 是 UI 面的装配结论，供启动日志与 /readyz 如实报告。
//
// 为什么必须报：`CCH_EGRESS_PAGES` 决定「页面面到底谁在答」，两个取值都不报错、只表现成
// 行为不同（自答 / embed）。启动日志会被滚动掉，/readyz 是随时可查的事实。
type uiStatus struct {
	// Mode 是生效的页面面档位（off / embed）。
	Mode config.EgressPages
	// Build 是 uiapp 的产物结论；非 embed 档为零值。
	Build uiapp.Status
}

// describe 给出一行结论。
func (s uiStatus) describe() string {
	if s.Mode == config.EgressPagesEmbed {
		return "embed（" + s.Build.Describe() + "）"
	}
	return "off（页面面未装配，非 embed 产物）"
}

// openUIPlane 装配 UI 静态面处理器。
func openUIPlane(options uiOptions) (http.Handler, uiStatus, error) {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	now := time.Now
	handler, err := uiapp.New(uiapp.Options{
		Logger:   logger,
		Meta:     uiMetaSource(options.Pools),
		Sessions: newUISessionResolver(options.Sessions),
		Now:      now,
		// 已注册 locale 取 guard 的那一份（与 src/i18n/config.ts 同集），不从产物目录推断。
		Locales: guard.SupportedLocales,
	})
	if err != nil {
		return nil, uiStatus{}, err
	}
	status := uiStatus{Mode: config.EgressPagesEmbed, Build: handler.Status()}
	return handler, status, nil
}

// uiMetaSource 读壳注入用的站点元数据：站点标题取 system_settings，时区走
// 「system_settings.timezone → 环境变量 TZ → UTC」三级取值（与 internal/adminapi 同规则）。
//
// 时区取值必须走 config.ResolveLocationFromEnv，不得自读环境变量：自读会丢掉 TZ 的默认值
// （Asia/Shanghai，与 Node 的 env.schema.ts:179 一致），退化成「库里没设时区就按 UTC 算」，
// 壳里的 timeZone 与 UI 的时间显示随之偏离 Node。
//
// 取不到时返回零值元数据 + nil 错误：壳是页面入口，元数据缺失由前端兜底，不该让整页失败。
// 只有真正的依赖故障才返回 error——uiapp 据此记一次 warn 并沿用上一次的值。
func uiMetaSource(pools *store.Pools) uiapp.MetaSource {
	return func(ctx context.Context) (uiapp.Meta, error) {
		meta := uiapp.Meta{Version: uiAppVersion(os.Getenv)}
		if pools == nil {
			meta.TimeZone = config.ResolveLocationFromEnv(nil).String()
			return meta, nil
		}

		settings, settingsErr := pools.FindSystemSettings(ctx)
		if settingsErr == nil && settings != nil {
			meta.SiteTitle = strings.TrimSpace(settings.SiteTitle)
		}
		raw, timezoneErr := pools.AdminSystemTimezone(ctx)
		meta.TimeZone = config.ResolveLocationFromEnv(raw).String()

		// 元数据能取多少算多少，但仍把依赖故障报上去（调用方记 warn 并沿用缓存值）。
		if settingsErr != nil {
			return meta, settingsErr
		}
		return meta, timezoneErr
	}
}

// uiAppVersion 给出壳注入用的版本号（展示形态，带小写 v 前缀）。
//
// 取值的唯一实现在 `internal/appversion`（注入的 APP_VERSION 为真源），本函数只把 os.Getenv
// 递进去。旧版在这里又自建了一条「env → VERSION 文件 → 自己的常量」的链，与 /api/version、
// /api/health 各说各话：同一镜像里壳写 v0.9.5、端点报 v0.9.0（见 appversion 包注释）。
func uiAppVersion(getenv func(string) string) string {
	return appversion.Resolve(getenv)
}

// uiSessionResolver 把管理面守卫的只读解析结论翻译成 UI 壳的会话快照。
//
// 为什么是适配器而不是自己解析：会话解析（裸 ADMIN_TOKEN → 签名令牌 → 不透明会话 → legacy/dual
// 裸 Key 四条分支）的唯一实现在 internal/adminapi 的 AuthGuard。本文件的上一版自带了一份
// 「只认 ADMIN_TOKEN 两条分支」的解析器，用户的不透明会话与裸 Key 一律落到未登录，前端只能
// 回退到 `/api/v1/me/metadata` 探针——那条路拿不到角色，于是 shell 对 admin 页不做角色判定。
// 现在解析口径只有一份，壳拿到的是真实角色。接线见 boot.go（管理面把守卫回填给 UI 面）。
type uiSessionResolver struct {
	source adminapi.SessionResolver
}

// newUISessionResolver 把解析入口适配成 uiapp 的契约。
func newUISessionResolver(source adminapi.SessionResolver) uiapp.SessionResolver {
	return uiSessionResolver{source: source}
}

// ResolveSession 实现 uiapp.SessionResolver。
//
// 三个返回值的语义由 uiapp 定：ok 表示「是个已登录会话」，err 表示依赖故障（uiapp 记一次
// warn 后按未登录处理，与 resolveToken 对 Redis 故障的处置同向）。
func (r uiSessionResolver) ResolveSession(
	ctx context.Context,
	request *http.Request,
) (uiapp.Session, bool, error) {
	if r.source == nil {
		// 管理面没装起来（无 DSN）：没有解析入口，按未登录。
		return uiapp.Session{}, false, nil
	}
	snapshot, err := r.source.ResolveSession(ctx, request)
	if err != nil {
		return uiapp.Session{}, false, err
	}
	if snapshot == nil {
		return uiapp.Session{}, false, nil
	}
	return uiapp.Session{
		User: uiapp.UserSnapshot{ID: snapshot.UserID, Name: snapshot.UserName, Role: snapshot.Role},
		Key:  &uiapp.KeySnapshot{CanLoginWebUI: snapshot.CanLoginWebUI},
	}, true, nil
}
