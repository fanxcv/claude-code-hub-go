"use client";

import { AlertTriangle, ArrowRight, TrendingDown } from "lucide-react";
import { useTranslations } from "next-intl";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import {
  getProtocolConversion,
  getProtocolConversionFailure,
  getProtocolConversionLoss,
  LOSS_ACTIONS,
  type ProtocolConversionFailureInfo,
  type ProtocolConversionLossInfo,
} from "@/lib/utils/protocol-conversion";
import type { SpecialSetting } from "@/types/special-settings";

/** 协议转换展示属性。 */
interface ProtocolConversionDisplayProps {
  /** 使用记录中的请求审计。 */
  specialSettings: SpecialSetting[] | null | undefined;
}

/**
 * 在使用记录中展示「该请求是否经过协议转换」。
 *
 * 四种状态（前两种**互斥**，由后端同一次写入的二选一保证；损失徽章与「已转换」**并存**）：
 *  - 已转换：`protocol_conversion` 存在，tooltip 给出实际协议对；
 *  - **转换损失**：`protocol_conversion_loss` 存在 —— 转换确实发生了，但丢/降/改了若干能力。
 *    这一态若是留空，界面上就和「转换干净」完全一样，用户再也查不到跨线转换在丢东西
 *    （这正是本条审计存在的理由）；
 *  - **转换失败**：`protocol_conversion_failed` 存在 —— 画一个**显式失败徽章**并在 tooltip 里
 *    给出阶段与原因。这一态若是留空，界面上就和「没转换」完全一样；
 *  - 未转换（原生同协议）：三者都不存在 → 留空。
 */
export function ProtocolConversionDisplay({ specialSettings }: ProtocolConversionDisplayProps) {
  const failure = getProtocolConversionFailure(specialSettings);
  if (failure) {
    return <ProtocolConversionFailureBadge failure={failure} />;
  }

  const loss = getProtocolConversionLoss(specialSettings);
  if (!loss) {
    return <ProtocolConversionSuccessBadge specialSettings={specialSettings} />;
  }

  // 有损失时纵向排两枚徽章：该列宽度仅 84px，横排会溢出到隔壁列。
  return (
    <span className="inline-flex flex-col items-start gap-0.5">
      <ProtocolConversionSuccessBadge specialSettings={specialSettings} />
      <ProtocolConversionLossBadge loss={loss} />
    </span>
  );
}

/** 已转换徽章（与 Node 同形，保持既有外观）。 */
function ProtocolConversionSuccessBadge({
  specialSettings,
}: {
  specialSettings: SpecialSetting[] | null | undefined;
}) {
  const t = useTranslations("dashboard.logs.protocolConversion");
  const conversion = getProtocolConversion(specialSettings);

  if (!conversion) {
    return null;
  }

  return (
    <TooltipProvider>
      <Tooltip delayDuration={250}>
        <TooltipTrigger asChild>
          <span
            className="relative z-20 inline-flex items-center gap-1 whitespace-nowrap"
            data-slot="protocol-conversion"
          >
            <Badge
              variant="outline"
              className="w-fit border-sky-200 bg-sky-50 px-1 text-[10px] leading-tight text-sky-700 dark:border-sky-800 dark:bg-sky-950/30 dark:text-sky-300"
            >
              {t("badge")}
            </Badge>
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-xs space-y-1">
          <p className="text-xs">{t("tooltip")}</p>
          <p className="inline-flex items-center gap-1 font-mono text-xs text-muted-foreground">
            <span>{conversion.clientProtocol}</span>
            <ArrowRight className="h-3 w-3 shrink-0" aria-hidden="true" />
            <span>{conversion.targetProtocol}</span>
          </p>
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}

/**
 * 转换损失徽章。
 *
 * 与成功徽章**并存**（不是替代）：转换确实发生了，只是过程中丢了东西，两者都是事实。
 * 颜色用橙色（而非失败态的了琥珀色）：琥珀说的是「转换没生效、已回退」，本条说的是「转换生效但有损」，
 * 两件事的排查方向不同，用同一色会让运维把它们混为一谈。
 * 徽章只给未聚合总数，逐组明细放 tooltip（组数上界 63 组，不会把提示栏刷满）。
 */
function ProtocolConversionLossBadge({ loss }: { loss: ProtocolConversionLossInfo }) {
  const t = useTranslations("dashboard.logs.protocolConversion");

  // 未知动作原样展示（机器标识符）：剔除整组会让损失被少报，而少报比难看严重。
  const actionLabel = (action: string): string =>
    LOSS_ACTIONS.some((known) => known === action) ? t(`lossAction.${action}`) : action;

  return (
    <TooltipProvider>
      <Tooltip delayDuration={250}>
        <TooltipTrigger asChild>
          <span
            className="relative z-20 inline-flex items-center gap-1 whitespace-nowrap"
            data-slot="protocol-conversion-loss"
          >
            <Badge
              variant="outline"
              className="w-fit gap-1 border-orange-200 bg-orange-50 px-1 text-[10px] leading-tight text-orange-700 dark:border-orange-800 dark:bg-orange-950/30 dark:text-orange-300"
            >
              <TrendingDown className="h-2.5 w-2.5 shrink-0" aria-hidden="true" />
              {t("lossBadge", { count: loss.total })}
            </Badge>
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm space-y-1">
          <p className="text-xs">{t("lossTooltip")}</p>
          {loss.clientProtocol && loss.targetProtocol ? (
            <p className="inline-flex items-center gap-1 font-mono text-xs text-muted-foreground">
              <span>{loss.clientProtocol}</span>
              <ArrowRight className="h-3 w-3 shrink-0" aria-hidden="true" />
              <span>{loss.targetProtocol}</span>
            </p>
          ) : null}
          <p className="text-xs">
            <span className="text-muted-foreground">{t("lossTotalLabel")}：</span>
            {loss.total}
          </p>
          {loss.groups.length > 0 ? (
            <div className="space-y-0.5">
              {loss.groups.map((group) => (
                <p
                  key={`${group.capability}:${group.action}`}
                  className="font-mono text-xs text-muted-foreground"
                >
                  {group.capability} × {actionLabel(group.action)} × {group.count}
                </p>
              ))}
            </div>
          ) : null}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}

/**
 * 转换失败徽章。
 *
 * 颜色用琥珀（而非红色）：请求**并没有失败**——它按 Node 语义回退成了原生直通并照常完成，
 * 用红色会让运维误以为这次调用挂了。真正的事实是「转换没生效，值得查一眼」。
 */
function ProtocolConversionFailureBadge({ failure }: { failure: ProtocolConversionFailureInfo }) {
  const t = useTranslations("dashboard.logs.protocolConversion");

  return (
    <TooltipProvider>
      <Tooltip delayDuration={250}>
        <TooltipTrigger asChild>
          <span
            className="relative z-20 inline-flex items-center gap-1 whitespace-nowrap"
            data-slot="protocol-conversion-failed"
          >
            <Badge
              variant="outline"
              className="w-fit gap-1 border-amber-300 bg-amber-50 px-1 text-[10px] leading-tight text-amber-800 dark:border-amber-800 dark:bg-amber-950/30 dark:text-amber-300"
            >
              <AlertTriangle className="h-2.5 w-2.5 shrink-0" aria-hidden="true" />
              {t("failedBadge")}
            </Badge>
          </span>
        </TooltipTrigger>
        <TooltipContent className="max-w-sm space-y-1">
          <p className="text-xs">{t("failedTooltip")}</p>
          {failure.clientProtocol && failure.targetProtocol ? (
            <p className="inline-flex items-center gap-1 font-mono text-xs text-muted-foreground">
              <span>{failure.clientProtocol}</span>
              <ArrowRight className="h-3 w-3 shrink-0" aria-hidden="true" />
              <span>{failure.targetProtocol}</span>
            </p>
          ) : null}
          <p className="text-xs">
            <span className="text-muted-foreground">{t("phaseLabel")}：</span>
            {failure.phase ? t(`phase.${failure.phase}`) : t("phase.unknown")}
          </p>
          <p className="text-xs break-words">
            <span className="text-muted-foreground">{t("reasonLabel")}：</span>
            {failure.reason ?? t("reasonUnknown")}
          </p>
          {failure.fallback ? (
            <p className="text-xs text-muted-foreground">{t("fallbackNotice")}</p>
          ) : null}
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  );
}
