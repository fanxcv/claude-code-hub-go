"use client";

import { ArrowRight } from "lucide-react";
import { useTranslations } from "next-intl";
import { ThinkingEffortBadge } from "@/components/customs/thinking-effort-badge";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { extractThinkingEffortInfo } from "@/lib/utils/thinking-effort";
import type { SpecialSetting } from "@/types/special-settings";

/** 思考强度展示属性。 */
interface ThinkingEffortDisplayProps {
  /** 使用记录中的请求参数与供应商覆写审计。 */
  specialSettings: SpecialSetting[] | null | undefined;
}

/**
 * 在使用记录中展示任意模型的思考强度（Codex / OpenAI chat/completions 的
 * reasoning.effort，或 Anthropic 的 output_config.effort）。
 *
 * 展示口径（2026-09-13 起，用户要求）：这一列要能回答「**协议转换之后、即将发给供应商的是哪个值**」。
 * 故当审计里有转换探针（`thinking_effort_forwarded`）时：
 *   - 右侧徐章取探针的 `forwardedEffort`（转换器产物里的事实，可与上游实收 body 对照）；
 *   - 与请求值不同时同时显示两侧，便于判断转换是否改写了它；
 *   - 转换把字段丢了（`dropped`）时显示丢弃标记，**不静默留空**。
 * 供应商覆写（provider_parameter_override）与协议转换改写的视觉一致：「请求值 → 实际值」。
 */
export function ThinkingEffortDisplay({ specialSettings }: ThinkingEffortDisplayProps) {
  const t = useTranslations("dashboard.logs.details");
  const effortInfo = extractThinkingEffortInfo(specialSettings);

  if (!effortInfo) {
    return <span className="text-muted-foreground">-</span>;
  }

  const messageNamespace =
    effortInfo.source === "anthropic"
      ? "effort"
      : effortInfo.source === "openai"
        ? "reasoningEffortOpenai"
        : "reasoningEffort";
  const conversion = effortInfo.conversion;
  // 转换把该字段丢了：必须在列里看得见，不能静默成 `-`（用户口径：这一列要当转换正确性探针用）。
  const dropped = conversion?.dropped === true;
  // 右侧徐章（= 实际发给上游的值）出现的两种情形：与请求值不同（覆写/转换改写），
  // 或者根本没有请求值（只有探针记录下来的转发事实）。
  const showEffectiveBadge =
    effortInfo.effectiveEffort != null &&
    (effortInfo.isOverridden || effortInfo.requestedEffort == null);
  const showArrow = effortInfo.requestedEffort != null && (showEffectiveBadge || dropped);
  const showProtocolPair =
    conversion?.converted === true &&
    conversion.clientProtocol != null &&
    conversion.targetProtocol != null;

  return (
    <TooltipProvider>
      <Tooltip delayDuration={250}>
        <TooltipTrigger asChild>
          <span
            className="relative z-20 inline-flex items-center gap-1 whitespace-nowrap"
            data-slot="thinking-effort"
          >
            {effortInfo.requestedEffort && (
              <ThinkingEffortBadge
                effort={effortInfo.requestedEffort}
                label={effortInfo.requestedEffort}
              />
            )}
            {showArrow && (
              <ArrowRight className="h-3 w-3 shrink-0 text-muted-foreground" aria-hidden="true" />
            )}
            {showEffectiveBadge && (
              <ThinkingEffortBadge
                effort={effortInfo.effectiveEffort as string}
                label={effortInfo.effectiveEffort as string}
              />
            )}
            {dropped && (
              <Badge
                variant="outline"
                className="w-fit border-amber-300 bg-amber-50 px-1 text-[10px] leading-tight text-amber-700 dark:border-amber-700 dark:bg-amber-950/40 dark:text-amber-300"
                data-slot="thinking-effort-dropped"
              >
                {t("effortConversion.dropped")}
              </Badge>
            )}
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-xs space-y-1">
          <p className="text-xs">{t(`${messageNamespace}.tooltip`)}</p>
          {showProtocolPair && (
            <p className="inline-flex items-center gap-1 font-mono text-xs text-muted-foreground">
              <span>{conversion?.clientProtocol}</span>
              <ArrowRight className="h-3 w-3 shrink-0" aria-hidden="true" />
              <span>{conversion?.targetProtocol}</span>
            </p>
          )}
          {effortInfo.isProviderOverridden && (
            <p className="text-xs text-muted-foreground">{t(`${messageNamespace}.overridden`)}</p>
          )}
          {dropped && (
            <p className="text-xs text-amber-600 dark:text-amber-400">
              {t("effortConversion.droppedTooltip")}
            </p>
          )}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
