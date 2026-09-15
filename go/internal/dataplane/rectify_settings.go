package dataplane

import (
	"context"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/rectify"
)

// rectifySwitches 取六个整流器开关（system_settings 的 enable_*_rectifier）。
//
// 读不到设置时回落 Node 默认（全开）：Node 一律写作 `settings.x ?? true`，把「读不到」当成
// 「关闭」会让整流器在生产静默失效，方向正好相反。此处用的是 cfgsync 的设置快照
// （60s 缓存 + 失效广播），不是每请求一次真库查询。
func rectifySwitches(ctx context.Context, source guard.SettingsSource, logger *logx.Logger) rectify.Switches {
	if source == nil {
		return rectify.NodeDefaults()
	}
	settings, err := source.FindSystemSettings(ctx)
	if err != nil || settings == nil {
		logger.Warn("dataplane.rectify_settings_unavailable", map[string]any{"error": errorText(err)})
		return rectify.NodeDefaults()
	}
	return rectify.Switches{
		ThinkingEffortConflict: settings.EnableThinkingEffortConflictRectifier,
		ThinkingSignature:      settings.EnableThinkingSignatureRectifier,
		ThinkingBudget:         settings.EnableThinkingBudgetRectifier,
		GeminiFunctionID:       settings.EnableGeminiFunctionIDRectifier,
		BillingHeader:          settings.EnableBillingHeaderRectifier,
		ResponseInput:          settings.EnableResponseInputRectifier,
	}
}
