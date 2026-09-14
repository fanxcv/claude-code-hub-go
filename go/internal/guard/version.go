package guard

import (
	"encoding/json"
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件复刻 ProxyVersionGuard 与切片的 rateLimit 步骤。
//
// 版本检查是 fail-open 的软开关：功能默认关闭，任何一步出错都放行——它的目的只是提示
// 用户升级，不该成为新的可用性单点。

// clientUpgradePayload 是版本过旧时的抢答体。
type clientUpgradePayload struct {
	Error struct {
		Type              string `json:"type"`
		Message           string `json:"message"`
		CurrentVersion    string `json:"current_version"`
		RequiredVersion   string `json:"required_version"`
		ClientType        string `json:"client_type"`
		ClientDisplayName string `json:"client_display_name"`
	} `json:"error"`
}

// versionStep 复刻 ProxyVersionGuard.ensure。
func (d Deps) versionStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		if d.Settings == nil || d.Versions == nil {
			// 缝隙缺失即视为功能不可用：记一条 debug 后放行，不阻断请求。
			d.logger().Debug("guard.version.dependency_missing", nil)
			return nil, nil
		}

		settings, err := d.Settings.FindSystemSettings(d.runContext(ctx))
		if err != nil {
			d.logger().Error("guard.version.settings_failed", map[string]any{"error": err.Error()})
			return nil, nil
		}
		if !settings.EnableClientVersionCheck {
			return nil, nil
		}

		auth, ok := authState(ctx)
		if !ok || auth.UserID == 0 {
			return nil, nil
		}

		agent := userAgent(ctx)
		client, ok := d.Versions.ParseUserAgent(agent)
		if !ok {
			d.logger().Debug("guard.version.ua_unparsed", map[string]any{"userAgent": agent})
			return nil, nil
		}

		// Node 侧是 fire-and-forget。Go 侧同步调用但忽略错误：这儿的 goroutine 会变成
		// 每请求一次的无界派生，是否入队交给实现方决定。
		if err := d.Versions.UpdateUserVersion(d.runContext(ctx), auth.UserID, client); err != nil {
			d.logger().Error("guard.version.update_failed", map[string]any{"error": err.Error()})
		}

		needsUpgrade, gaVersion, err := d.Versions.ShouldUpgrade(d.runContext(ctx), client)
		if err != nil {
			d.logger().Error("guard.version.check_failed", map[string]any{"error": err.Error()})
			return nil, nil
		}
		if !needsUpgrade {
			return nil, nil
		}

		displayName := d.Versions.DisplayName(client.ClientType)
		d.logger().Warn("guard.version.outdated", map[string]any{
			"userId":          auth.UserID,
			"clientType":      client.ClientType,
			"currentVersion":  client.Version,
			"requiredVersion": gaVersion,
		})

		payload := clientUpgradePayload{}
		payload.Error.Type = "client_upgrade_required"
		payload.Error.Message = "Your " + displayName + " (v" + client.Version +
			") is outdated. Please upgrade to v" + gaVersion +
			" or later to continue using this service."
		payload.Error.CurrentVersion = client.Version
		payload.Error.RequiredVersion = gaVersion
		payload.Error.ClientType = client.ClientType
		payload.Error.ClientDisplayName = displayName

		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}

		// content-type 对齐 Node 侧此处不带 charset 的写法。
		headers := http.Header{}
		headers.Set("content-type", "application/json")
		return NewResponse(400, headers, body), nil
	}
}

// rateLimitStep 复刻 guard-pipeline.ts 的 rateLimit 步骤。
//
// 判定属限流包（多维额度、成本窗口、并发会话），本步骤只负责把判定翻译成响应并早退。
// 缝隙缺失时留一条 warn 继续——这是显式的接线缺口，不是一个默认放行策略。
func (d Deps) rateLimitStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		if d.RateLimit == nil {
			d.logger().Warn("guard.rate_limit.missing", map[string]any{
				"note": "限流缝隙未接线，本步骤跳过",
			})
			return nil, nil
		}

		block, err := d.RateLimit.Check(d.runContext(ctx), ctx)
		if err != nil {
			return nil, err
		}
		if block == nil {
			return nil, nil
		}

		// 限流响应必须用 Node 同形的七字段信封与 X-RateLimit-* 头（见 BuildRateLimitError）。
		return BuildRateLimitError(*block), nil
	}
}
