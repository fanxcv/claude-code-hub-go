package dataplane

import (
	"context"
	"fmt"
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件实现 `system_settings.verbose_provider_error` 打开时的详细 503 体。
//
// Node 的出处是 provider-selector.ts:490-558：状态码恒为 503，但详细模式会按被剔除的
// 理由给出可读文案（全限流 / 全熔断 / 混合），并把被剔除条目（**只带 id 与理由**，
// 不带供应商名称）放进 details。
//
// 关闭时是逐字节固定的 `{"error":{"message":"No available providers",
// "type":"no_available_providers"}}`——Go 改造前的行为正是这一支，故默认行为不变
// （生产 system_settings.verbose_provider_error 也是 false）。

// verboseNoProviderResponse 按被剔除理由构造详细 503 响应。
//
// 与 Node 的两点刻意差异（都在报告里登记）：
//  1. `totalAttempts` / `excludedCount` 恒为 0：Node 的两个数来自**带排除列表重选**的那条
//     路径，而 Go 的选路在守卫链内只发生一次，没有对应的重试计数。Node 在未重试时的取值
//     同样是 0，故形状一致。
//  2. `excludedProviders.length > 0` 那一支（`All providers unavailable (tried N providers)`）
//     在 Go 不可达，理由同上：本次选路不携带排除列表，故不产出 `all_providers_failed`。
func verboseNoProviderResponse(diagnostic guard.NoProviderDiagnostic) *guard.Response {
	counts := map[string]int{}
	for _, entry := range diagnostic.Filtered {
		counts[entry.Reason]++
	}
	rateLimited := counts[string(route.ReasonRateLimited)]
	circuitOpen := counts[string(route.ReasonCircuitOpen)]
	disabled := counts[string(route.ReasonDisabled)]
	modelNotAllowed := counts[string(route.ReasonModelNotAllowed)]
	clientRestricted := counts[string(route.ReasonClientRestriction)]
	filteredTotal := len(diagnostic.Filtered)

	message := "No available providers"
	errorType := "no_available_providers"
	if filteredTotal > 0 {
		// 判据逐条对齐 Node：unavailableCount 只算限流与熔断，totalEnabled 要扣掉
		// 「禁用 / 模型不允许 / 客户端名单拒绝」三类——它们不是「暂时不可用」，
		// 归入「全限流」或「全熔断」会误导排障方向。
		unavailable := rateLimited + circuitOpen
		totalEnabled := filteredTotal - disabled - modelNotAllowed - clientRestricted
		switch {
		case rateLimited > 0 && circuitOpen == 0 && unavailable == totalEnabled:
			message = fmt.Sprintf("All providers rate limited (%d providers)", rateLimited)
			errorType = "rate_limit_exceeded"
		case circuitOpen > 0 && rateLimited == 0 && unavailable == totalEnabled:
			message = fmt.Sprintf("All providers circuit breaker open (%d providers)", circuitOpen)
			errorType = "circuit_breaker_open"
		case rateLimited > 0 && circuitOpen > 0:
			message = fmt.Sprintf("All providers unavailable (%d rate limited, %d circuit open)",
				rateLimited, circuitOpen)
			errorType = "mixed_unavailable"
		}
	}

	details := map[string]any{
		"totalAttempts": 0,
		"excludedCount": 0,
	}
	if filteredTotal > 0 {
		filteredProviders := make([]map[string]any, 0, filteredTotal)
		restrictedProviders := make([]map[string]any, 0, clientRestricted)
		for _, entry := range diagnostic.Filtered {
			item := map[string]any{"id": entry.ID, "reason": entry.Reason}
			filteredProviders = append(filteredProviders, item)
			if entry.Reason == string(route.ReasonClientRestriction) {
				restrictedProviders = append(restrictedProviders, item)
			}
		}
		details["filteredProviders"] = filteredProviders
		if len(restrictedProviders) > 0 {
			details["clientRestrictedProviders"] = restrictedProviders
		}
	}
	return guard.BuildErrorWithDetails(http.StatusServiceUnavailable, message, errorType, details, "")
}

// verboseProviderError 读 `system_settings.verbose_provider_error`。
//
// 读不到时按 false：简洁模式是 Node 的默认，也是「不把内部事实暴露给客户端」的方向。
func (h *Handler) verboseProviderError(ctx context.Context) bool {
	if h.options.Base.Settings == nil {
		return false
	}
	settings, err := h.options.Base.Settings.FindSystemSettings(ctx)
	if err != nil || settings == nil {
		h.logger.Warn("dataplane.verbose_provider_error_lookup_failed", map[string]any{"error": errorText(err)})
		return false
	}
	return settings.VerboseProviderError
}
