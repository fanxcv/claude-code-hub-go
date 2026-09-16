import type { SpecialSetting } from "@/types/special-settings";

type BuildUnifiedSpecialSettingsParams = {
  /**
   * 已有 specialSettings（通常来自 DB special_settings 或 Session Redis 缓存）
   */
  existing?: SpecialSetting[] | null;
  /**
   * 拦截类型（如 warmup / sensitive_word）
   */
  blockedBy?: string | null;
  /**
   * 拦截原因（通常为 JSON 字符串）
   */
  blockedReason?: string | null;
  /**
   * HTTP 状态码（用于补齐守卫拦截信息）
   */
  statusCode?: number | null;
  /**
   * Cache TTL 实际应用值（用于展示 TTL/标头覆写命中）
   */
  cacheTtlApplied?: string | null;
  /**
   * 1M 上下文是否应用（保留参数用于兼容调用方；不再自动派生 header 覆写审计）
   */
  context1mApplied?: boolean | null;
};

function buildSettingKey(setting: SpecialSetting): string {
  switch (setting.type) {
    case "provider_parameter_override":
      return JSON.stringify([
        setting.type,
        setting.providerId ?? null,
        setting.providerType ?? null,
        setting.hit,
        setting.changed,
        [...setting.changes]
          .map((change) => [change.path, change.before, change.after, change.changed] as const)
          .sort((a, b) => a[0].localeCompare(b[0])),
      ]);
    case "response_fixer":
      return JSON.stringify([
        setting.type,
        setting.hit,
        [...setting.fixersApplied]
          .map((fixer) => [fixer.fixer, fixer.applied] as const)
          .sort((a, b) => a[0].localeCompare(b[0])),
      ]);
    case "guard_intercept":
      return JSON.stringify([setting.type, setting.guard, setting.action, setting.statusCode]);
    case "anthropic_effort":
      return JSON.stringify([setting.type, setting.hit, setting.effort]);
    case "codex_reasoning_effort":
      return JSON.stringify([setting.type, setting.hit, setting.effort]);
    case "openai_reasoning_effort":
      return JSON.stringify([setting.type, setting.hit, setting.effort, setting.source]);
    case "protocol_conversion":
      // 同一请求可能多次尝试（重试/故障转移）都发生转换，按协议对去重，跨线组合不同则分别保留。
      return JSON.stringify([setting.type, setting.clientProtocol, setting.targetProtocol]);
    case "protocol_conversion_failed":
      // 失败条目的判别元组：阶段 + 协议对 + 原因。同一处失败在重试中重复出现时会被折叠，
      // 而不同阶段/不同原因（例如先路径不可映射、后正文不可编码）各自保留——它们是不同的事实。
      return JSON.stringify([
        setting.type,
        setting.phase ?? null,
        setting.clientProtocol ?? null,
        setting.targetProtocol ?? null,
        setting.reason ?? null,
      ]);
    case "protocol_conversion_loss":
      // 损失条目：协议对 + 未聚合总数 + 分组（按能力/动作字典序，与后端固定顺序一致）。
      // 注：Go 读侧对该类型走兜底编码（整对象字典序，见 usage_logs_rows.go 的 default 分支），
      // 两侧键形不同，但对同一行的重复条目去重结果一致，故不强行对齐字符串。
      return JSON.stringify([
        setting.type,
        setting.clientProtocol,
        setting.targetProtocol,
        setting.total,
        [...setting.groups]
          .map((group) => [group.capability, group.action, group.count] as const)
          .sort((a, b) => a[0].localeCompare(b[0]) || a[1].localeCompare(b[1])),
      ]);
    case "thinking_effort_forwarded":
      // Go 侧新增的转换探针：同一条链路上「请求值 + 转发值 + 是否丢弃」就唯一确定一条记录。
      // 注：Go 读侧对该类型走兵底编码（整对象字典序，见 usage_logs_rows.go 的 default 分支），
      // 两侧键形不同，但对同一行的重复条目去重结果一致，故不强行对齐字符串。
      return JSON.stringify([
        setting.type,
        setting.requestedEffort ?? null,
        setting.forwardedEffort ?? null,
        setting.dropped,
        setting.converted,
        setting.clientProtocol ?? null,
        setting.targetProtocol ?? null,
      ]);
    case "anthropic_cache_ttl_header_override":
      return JSON.stringify([setting.type, setting.ttl]);
    case "anthropic_context_1m_header_override":
      return JSON.stringify([setting.type, setting.header, setting.flag]);
    case "long_context_pricing":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.pricingScope ?? null,
        setting.thresholdTokens ?? null,
      ]);
    case "thinking_signature_rectifier":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.providerId ?? null,
        setting.trigger,
        setting.attemptNumber,
        setting.retryAttemptNumber,
        setting.removedThinkingBlocks,
        setting.removedRedactedThinkingBlocks,
        setting.removedSignatureFields,
      ]);
    case "thinking_effort_conflict_rectifier":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.providerId ?? null,
        setting.trigger,
        setting.attemptNumber,
        setting.retryAttemptNumber,
        setting.removedOutputConfigEffort,
        setting.removedReasoningEffort,
        setting.thinkingType,
        setting.effort,
      ]);
    case "codex_session_id_completion":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.action,
        setting.source,
        setting.sessionId,
      ]);
    case "claude_metadata_user_id_injection":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.action,
        setting.reason,
        setting.keyId,
        setting.sessionId,
      ]);
    case "thinking_budget_rectifier":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.providerId ?? null,
        setting.trigger,
        setting.attemptNumber,
        setting.retryAttemptNumber,
        setting.before.maxTokens,
        setting.before.thinkingBudgetTokens,
        setting.after.maxTokens,
        setting.after.thinkingBudgetTokens,
      ]);
    case "billing_header_rectifier":
      return JSON.stringify([setting.type, setting.hit, setting.removedCount]);
    case "gemini_function_id_rectifier":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.providerId ?? null,
        setting.trigger,
        setting.attemptNumber,
        setting.retryAttemptNumber,
        setting.strippedFunctionCallIds,
        setting.strippedFunctionResponseIds,
      ]);
    case "gemini_google_search_override":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.providerId ?? null,
        setting.action,
        setting.preference,
        setting.hadGoogleSearchInRequest,
      ]);
    case "pricing_resolution":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.modelName,
        setting.resolvedModelName,
        setting.resolvedPricingProviderKey,
        setting.source,
      ]);
    case "codex_service_tier_result":
      return JSON.stringify([
        setting.type,
        setting.hit,
        setting.requestedServiceTier,
        setting.actualServiceTier,
        setting.billingSourcePreference ?? null,
        setting.resolvedFrom ?? null,
        setting.effectivePriority,
      ]);
    case "response_input_rectifier":
      return JSON.stringify([setting.type, setting.hit, setting.action, setting.originalType]);
    case "thinking_placeholder_signature_rectifier":
      // 主动型剥离：同一处剥离的块数就唯一确定一条记录（无 trigger/attempt 维度）。
      return JSON.stringify([setting.type, setting.hit, setting.removedPlaceholderThinkingBlocks]);
    case "thinking_signature_model_detection":
      return JSON.stringify([
        setting.type,
        setting.source,
        setting.signatureFound,
        setting.thinkingEnabled,
        setting.extractedModel,
        setting.requestedModel,
      ]);
    default: {
      // 兜底：保证即使未来扩展类型也不会导致运行时崩溃
      const _exhaustive: never = setting;
      return JSON.stringify(_exhaustive);
    }
  }
}

/**
 * 构建“统一特殊设置”展示数据
 *
 * 目标：把 DB 字段（blockedBy/cacheTtlApplied/context1mApplied）与既有 special_settings 合并，
 * 统一在以下位置展示：日志列表/日志详情弹窗/Session 详情页。
 */
export function buildUnifiedSpecialSettings(
  params: BuildUnifiedSpecialSettingsParams
): SpecialSetting[] | null {
  const base = params.existing ?? [];
  const derived: SpecialSetting[] = [];

  if (params.blockedBy) {
    const guard = params.blockedBy;
    const action = guard === "warmup" ? "intercept_response" : "block_request";

    derived.push({
      type: "guard_intercept",
      scope: "guard",
      hit: true,
      guard,
      action,
      statusCode: params.statusCode ?? null,
      reason: params.blockedReason ?? null,
    });
  }

  if (params.cacheTtlApplied) {
    derived.push({
      type: "anthropic_cache_ttl_header_override",
      scope: "request_header",
      hit: true,
      ttl: params.cacheTtlApplied,
    });
  }

  if (base.length === 0 && derived.length === 0) {
    return null;
  }

  const seen = new Set<string>();
  const result: SpecialSetting[] = [];
  for (const item of [...base, ...derived]) {
    const key = buildSettingKey(item);
    if (seen.has(key)) continue;
    seen.add(key);
    result.push(item);
  }

  return result.length > 0 ? result : null;
}

export function hasPriorityServiceTierSpecialSetting(
  specialSettings?: SpecialSetting[] | null
): boolean {
  if (!Array.isArray(specialSettings) || specialSettings.length === 0) {
    return false;
  }

  const codexServiceTierResult = specialSettings.find(
    (setting): setting is Extract<SpecialSetting, { type: "codex_service_tier_result" }> =>
      setting.type === "codex_service_tier_result"
  );
  if (codexServiceTierResult) {
    if (
      codexServiceTierResult.billingSourcePreference == null &&
      codexServiceTierResult.resolvedFrom == null &&
      codexServiceTierResult.actualServiceTier != null
    ) {
      return codexServiceTierResult.actualServiceTier === "priority";
    }
    return codexServiceTierResult.effectivePriority;
  }

  return specialSettings.some(
    (setting) =>
      setting.type === "provider_parameter_override" &&
      setting.providerType === "codex" &&
      setting.changes.some(
        (change) => change.path === "service_tier" && change.after === "priority"
      )
  );
}

export function getPriorityServiceTierSpecialSetting(
  specialSettings?: SpecialSetting[] | null
): Extract<SpecialSetting, { type: "codex_service_tier_result" }> | null {
  if (!Array.isArray(specialSettings) || specialSettings.length === 0) {
    return null;
  }

  return (
    specialSettings.find(
      (setting): setting is Extract<SpecialSetting, { type: "codex_service_tier_result" }> =>
        setting.type === "codex_service_tier_result"
    ) ?? null
  );
}

export function getPricingResolutionSpecialSetting(
  specialSettings?: SpecialSetting[] | null
): Extract<SpecialSetting, { type: "pricing_resolution" }> | null {
  if (!Array.isArray(specialSettings) || specialSettings.length === 0) {
    return null;
  }

  return (
    specialSettings.find(
      (setting): setting is Extract<SpecialSetting, { type: "pricing_resolution" }> =>
        setting.type === "pricing_resolution"
    ) ?? null
  );
}

export function getThinkingSignatureModelDetectionSpecialSetting(
  specialSettings?: SpecialSetting[] | null
): Extract<SpecialSetting, { type: "thinking_signature_model_detection" }> | null {
  if (!Array.isArray(specialSettings) || specialSettings.length === 0) {
    return null;
  }

  return (
    specialSettings.find(
      (
        setting
      ): setting is Extract<SpecialSetting, { type: "thinking_signature_model_detection" }> =>
        setting.type === "thinking_signature_model_detection"
    ) ?? null
  );
}
