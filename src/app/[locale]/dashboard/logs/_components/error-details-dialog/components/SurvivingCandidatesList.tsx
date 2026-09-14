"use client";

import { useTranslations } from "next-intl";
import { Badge } from "@/components/ui/badge";
import type { ProviderChainItem } from "@/types/message";

type SurvivingCandidate = NonNullable<
  NonNullable<ProviderChainItem["decisionContext"]>["survivingCandidates"]
>[number];

interface SurvivingCandidatesListProps {
  /** 通过全部硬校验的候选（Go 侧 decisionContext.survivingCandidates）。 */
  candidates: SurvivingCandidate[];
  /** 亲和命中的那一家（链项自身的 name），用来讲清其余候选为何没参与竞争。 */
  matchedProviderName: string;
}

/**
 * 决策链里「通过全部硬校验、却因前缀亲和短路而未参与竞争」的那批候选。
 *
 * **已被 `ConsideredCandidatesList` 取代，不再挂载**（2026-09-14）：Go 侧现在两条选路路径都写
 * `consideredCandidates`，且亲和行里它与 `survivingCandidates` **同源同序**（同一次计算两个投影），
 * 故参与池统一由 `ConsideredCandidatesList` 读 `consideredCandidates`、只从本组件的字段取
 * `affinitySkipped` 这一位。**不要把本组件再挂回链项**——两个键各渲染一遍，同一批供应商会被列两遍。
 *
 * 保留的原因：`survivingCandidates` 键此刻仍在写（老行也还有），它的单测是那一份形状的真源；
 * Go 侧确认旧行退尽、并把 `affinitySkipped` 并进 `ConsideredCandidate` 后，本文件连同其用例可一并删。
 *
 * 存在的理由（生产实证，2026-09-14）：亲和命中会短路整场选路，链上原先只看得到被选中的那一家，
 * 用户据此报「优先级 1/2/3 的渠道都没参与决策」，并把它们误读成「不支持该模型」——这与
 * filteredProviders（真被滤、带 reason）是两类完全不同的事实。把两者摆在一起看，用户才分得清
 * 「这家不行」与「这家行，只是没轮到它」。
 */
export function SurvivingCandidatesList({
  candidates,
  matchedProviderName,
}: SurvivingCandidatesListProps) {
  const t = useTranslations("dashboard.logs.details");
  if (candidates.length === 0) return null;

  // 同优先级内按 id 稳定排序，避免同一事实在不同渲染间抖动。
  const ordered = [...candidates].sort((a, b) => a.priority - b.priority || a.id - b.id);
  const skipped = ordered.filter((candidate) => candidate.affinitySkipped).length;

  return (
    <div className="pt-2 mt-2 border-t border-muted/50" data-testid="surviving-candidates">
      <div className="mb-2 text-[11px] text-muted-foreground">
        {t("logicTrace.survivingHint", { provider: matchedProviderName, count: skipped })}
      </div>
      <div className="space-y-1">
        {ordered.map((candidate) => (
          <div
            key={candidate.id}
            className="flex items-center justify-between gap-2 text-xs"
            data-candidate-id={candidate.id}
          >
            <span className="font-medium truncate">{candidate.name}</span>
            <div className="flex items-center gap-2 shrink-0">
              <Badge variant="outline" className="text-[10px]">
                P{candidate.priority}
              </Badge>
              <span className="text-muted-foreground">W:{candidate.weight}</span>
              <span className="text-muted-foreground">x{candidate.costMultiplier}</span>
              <Badge
                variant={candidate.selected ? "default" : "secondary"}
                className="text-[10px]"
                data-candidate-state={candidate.selected ? "selected" : "skipped"}
              >
                {candidate.selected
                  ? t("logicTrace.survivingSelected")
                  : t("logicTrace.survivingSkipped")}
              </Badge>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
