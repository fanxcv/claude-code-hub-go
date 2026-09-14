import { extractAnthropicEffortInfo } from "@/lib/utils/anthropic-effort";
import { extractCodexReasoningEffortInfo } from "@/lib/utils/codex-reasoning-effort";
import { extractOpenAIReasoningEffortFromSpecialSettings } from "@/lib/utils/openai-reasoning-effort";
import {
  extractThinkingEffortConversion,
  sourceForProtocol,
  type ThinkingEffortConversion,
} from "@/lib/utils/thinking-effort-forwarded";
import type { SpecialSetting } from "@/types/special-settings";

/** 思考强度审计来源：Codex 的 reasoning.effort、OpenAI chat/completions 或 Anthropic 的 output_config.effort。 */
export type ThinkingEffortSource = "codex" | "openai" | "anthropic";

/** 任意模型统一后的思考强度展示信息，供列表列与请求详情共用。 */
export interface ThinkingEffortInfo {
  source: ThinkingEffortSource;
  /** 客户端请求声明的思考强度；历史记录可能缺失。 */
  requestedEffort: string | null;
  /**
   * 实际转发给上游的思考强度。
   *
   * 有协议转换探针时**以探针为准**（那是转换器产物里的事实，可与上游实收 body 对照）；
   * 否则回退到「供应商覆写后的值」，再否则等于客户端请求值。被移除时为 null。
   */
  effectiveEffort: string | null;
  /** 请求值与实际转发值不同（供应商覆写或协议转换改写）——界面据此画 `x → y`。 */
  isOverridden: boolean;
  /**
   * 供应商级参数覆写是否命中。
   *
   * 与 `isOverridden` 分开的原因：文案不同。把**协议转换改写**说成「已被供应商覆写」是错误归因
   * （两条来源本来就不是一回事），故消费方必须分开取用：箭头用 `isOverridden`，覆写说明用本字段。
   */
  isProviderOverridden: boolean;
  /** 协议转换探针；探针上线前的历史行、或本次没有转换事实时为 null。 */
  conversion: ThinkingEffortConversion | null;
}

/**
 * 从 specialSettings 中提取任意模型的思考强度。
 *
 * 复用 Codex、OpenAI chat/completions 与 Anthropic 三个客户端侧提取器并统一返回结构：
 * Codex 审计优先，其次 OpenAI，最后回退到 Anthropic effort，三者都无则返回 null。
 * OpenAI chat/completions 目前无供应商级覆写机制，isOverridden 恒为 false。
 *
 * **协议转换探针（Go 侧新增）会覆盖 effective 的取值**：列要显示「转换之后、即将发给供应商的
 * 值」，而客户端侧审计只说明「客户端要了什么」。两者都留，各自可辨：
 *   - `requestedEffort` 取客户端侧审计（缺失时回退到探针里的 requested）；
 *   - `effectiveEffort` 取探针的 `forwardedEffort`；
 *   - `isOverridden` 在探针显示「转换把值改写了」时为真（画 `x → y`）；
 *   - `conversion.dropped` 为真表示**转换把该字段丢了**，界面必须显式提示，不得静默成 `-`。
 */
export function extractThinkingEffortInfo(
  specialSettings: SpecialSetting[] | null | undefined
): ThinkingEffortInfo | null {
  const clientInfo = extractClientAuditInfo(specialSettings);
  const conversion = extractThinkingEffortConversion(specialSettings);

  if (!conversion) {
    return clientInfo ? { ...clientInfo, conversion: null } : null;
  }

  const source =
    clientInfo?.source ??
    sourceForProtocol(conversion.clientProtocol) ??
    sourceForProtocol(conversion.targetProtocol) ??
    "anthropic";

  const requestedEffort = clientInfo?.requestedEffort ?? null;
  // 有探针时以探针为 effective 的唯一真源；但两种「探针没有值」要分开：
  //   - dropped=true：转换确实把字段丢了 → effective 就是 null（界面显式提示，不得回退显示旧值）；
  //   - 未丢弃但探针读不到（例如原生直通且字段路径取不到）：不臆断，回退到客户端侧的覆写结果。
  const effectiveEffort = conversion.dropped
    ? null
    : (conversion.forwardedEffort ?? clientInfo?.effectiveEffort ?? null);
  // 「改写」的两条来源互不排斥：供应商级覆写（clientInfo.isOverridden）与协议转换改写都可能为真，
  // 二者任一成立就要画 `x → y`（丢弃不算改写：没有「之后的值」可指）。
  const conversionRewrote =
    !conversion.dropped &&
    conversion.forwardedEffort != null &&
    clientInfo?.effectiveEffort != null &&
    conversion.forwardedEffort !== clientInfo.effectiveEffort;

  return {
    source,
    requestedEffort,
    effectiveEffort,
    isOverridden: (clientInfo?.isOverridden ?? false) || conversionRewrote,
    isProviderOverridden: clientInfo?.isOverridden ?? false,
    conversion,
  };
}

/** 客户端侧三提取器的统一封装（不含转换探针）。 */
function extractClientAuditInfo(
  specialSettings: SpecialSetting[] | null | undefined
): Omit<ThinkingEffortInfo, "conversion"> | null {
  const codexInfo = extractCodexReasoningEffortInfo(specialSettings);
  if (codexInfo) {
    return {
      source: "codex",
      requestedEffort: codexInfo.requestedEffort,
      effectiveEffort: codexInfo.effectiveEffort,
      isOverridden: codexInfo.isOverridden,
      isProviderOverridden: codexInfo.isOverridden,
    };
  }

  const openaiInfo = extractOpenAIReasoningEffortFromSpecialSettings(specialSettings);
  if (openaiInfo) {
    return {
      source: "openai",
      requestedEffort: openaiInfo.effort,
      effectiveEffort: openaiInfo.effort,
      isOverridden: false,
      isProviderOverridden: false,
    };
  }

  const anthropicInfo = extractAnthropicEffortInfo(specialSettings);
  if (anthropicInfo) {
    return {
      source: "anthropic",
      requestedEffort: anthropicInfo.originalEffort,
      effectiveEffort: anthropicInfo.isOverridden
        ? anthropicInfo.overriddenEffort
        : anthropicInfo.originalEffort,
      isOverridden: anthropicInfo.isOverridden,
      isProviderOverridden: anthropicInfo.isOverridden,
    };
  }

  return null;
}
