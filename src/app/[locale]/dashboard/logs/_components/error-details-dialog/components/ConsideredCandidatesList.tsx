"use client";

import { useTranslations } from "next-intl";
import { Badge } from "@/components/ui/badge";
import type { ConsideredCandidate } from "@/types/message";

interface ConsideredCandidatesListProps {
  /** 本次通过全部硬校验、进入候选池的全部供应商（Go 侧 decisionContext.consideredCandidates）。 */
  candidates: ConsideredCandidate[];
  /** 本次选中的档位（decisionContext.selectedPriority），用来判定每一家为何落选。 */
  selectedPriority: number;
  /**
   * 亲和短路行里「未参与竞争」的那批 id（取自 `survivingCandidates` 的 `affinitySkipped` 位）。
   *
   * 只作**标记**用，**不参与渲染成员集**：Go 侧这两个键由同一次计算投影而来，成员与顺序完全
   * 相同，各渲染一遍就会把同一批供应商列两遍。亲和行的参与池同样读 `consideredCandidates`——
   * 生产 3 小时窗口里 1549 行 `affinity_hit` 的 considered 计数为 0、surviving 合计 5108，
   * 只读后者的话「统一读参与池」的界面在这几行上什么也看不到，而用户报的恰好就是这几行。
   */
  affinitySkippedIds?: readonly number[];
  /** 亲和命中的那一家（链项自身的 name）；缺省时取池中被选中那家的名称。 */
  matchedProviderName?: string;
}

/** 一行候选的结局。四者互斥，判定顺序即优先级（见 stateOf）。 */
type CandidateState = "selected" | "affinity-skipped" | "lower-tier" | "same-tier";

/**
 * 决策链里「谁参与了、各在哪一档、有没有被选中」的清单（两条选路路径共用一张）。
 *
 * 存在的理由（生产实证，2026-09-14）：这一档原先只渲染 `candidatesAtPriority`——那是**选中档位
 * 之内**的候选，于是档位更低而落选的家在界面上完全不可见。用户看着链里只有 CommandCode 一家，
 * 报「我看这个会话的决策链，还是看不到 opencode 那几个渠道参与呢？」，而那几家其实全部通过了
 * 硬校验、进了候选池，只是档位更低没被选中。两类完全不同的结局（「这家不行」与「这家行，只是
 * 这一档没轮到它」）在界面上被压成了一个。
 *
 * 落选原因由两个数字相减得出，不另设字段：`effectivePriority` 大于选中档位 ⇒ 档位更低；
 * 相等 ⇒ 同档但未被抽中（同档落选靠的是加权随机，不是档位，两者不可混为一谈）。亲和短路行例外，
 * 其落选原因是「没参与竞争」，由 `affinitySkippedIds` 标出——那批候选的档位往往**并不**更低，
 * 拿档位去解释会把「亲和短路」误说成「档位不够」。
 *
 * 配置值与分层值不同时另标一记：生产实证里 CommandCode 配置值 4、在 `fan` 组下分层值 0，
 * 只看配置值会把「选了某档」读成「选了低优先级」，从而把一次正确选择误判成缺陷。
 *
 * 按分层值升序展示（数值小 = 优先级高 = 本该先被选中），使「谁被跳过」一眼可读；记录层不排序
 * （Go 侧有意不重排，排序是展示层的事），故这里排。
 */
export function ConsideredCandidatesList({
  candidates,
  selectedPriority,
  affinitySkippedIds,
  matchedProviderName,
}: ConsideredCandidatesListProps) {
  const t = useTranslations("dashboard.logs.details");
  if (candidates.length === 0) return null;

  const affinitySkipped = new Set(affinitySkippedIds ?? []);
  const isAffinityFlow = affinitySkipped.size > 0;

  // 同档内按 id 稳定排序，避免同一事实在不同渲染间抖动。
  const ordered = [...candidates].sort(
    (a, b) => a.effectivePriority - b.effectivePriority || a.id - b.id
  );

  // 选中者即 Go 侧标记的那家（亲和行就是被提名者）；它不在池内时才退用链项名。
  const matchedName =
    ordered.find((candidate) => candidate.selected)?.name ?? matchedProviderName ?? "";

  const stateOf = (candidate: ConsideredCandidate): CandidateState => {
    // 选中优先于一切：亲和行里被提名者就是选中者，它不该被标成「未参与竞争」。
    if (candidate.selected) return "selected";
    // 其次才是「未参与竞争」：亲和短路时全体落选者都是这一位，其档位未必更低。
    if (affinitySkipped.has(candidate.id)) return "affinity-skipped";
    // 最后才按档位说事。选中档位是全场最小分层值，故这里只可能落在「同档」或「更低的档」；
    // 若真出现更小的值，说明后端分层与选中不一致，按「同档」呈现不掩盖这一点，数字就在旁边。
    return candidate.effectivePriority > selectedPriority ? "lower-tier" : "same-tier";
  };

  return (
    <div className="pt-2 mt-2 border-t border-muted/50" data-testid="considered-candidates">
      <div className="mb-2 text-[11px] text-muted-foreground">
        {isAffinityFlow
          ? t("logicTrace.survivingHint", {
              provider: matchedName,
              count: ordered.filter((candidate) => affinitySkipped.has(candidate.id)).length,
            })
          : t("logicTrace.consideredHint", { count: ordered.length, selected: selectedPriority })}
      </div>
      <div className="space-y-1">
        {ordered.map((candidate) => {
          const state = stateOf(candidate);
          const slowPenalty = candidate.slowPenalty ?? 0;
          return (
            <div
              key={candidate.id}
              className="flex items-center justify-between gap-2 text-xs"
              data-candidate-id={candidate.id}
            >
              <span className="font-medium truncate">{candidate.name}</span>
              <div className="flex items-center gap-2 shrink-0">
                <Badge variant="outline" className="text-[10px]">
                  {t("logicTrace.consideredTier", { tier: candidate.effectivePriority })}
                </Badge>
                {candidate.priority !== candidate.effectivePriority && (
                  // 两个值不同即「生效档位≠配置档位」——不标出来，读的人会拿供应商列表里的
                  // 配置值去核对，于是把一次正确选择看成矛盾。
                  //
                  // 但差值**未必**全由分组覆盖造成：`effectivePriority` 已在分组覆盖之后又叠了
                  // 低速降权，故差值可能来自两维。从两个数字确实拆不出各自贡献，所以这里如实
                  // 并列三者（配置值、生效值、降权量），并在有降权时换用不含「分组覆盖」归因的
                  // 文案——把降权造成的偏离说成「分组覆盖」是把一次正确选择误报成另一种成因。
                  <span
                    className="text-muted-foreground"
                    title={
                      slowPenalty > 0
                        ? t("logicTrace.consideredSlowPenaltyHint", { penalty: slowPenalty })
                        : t("logicTrace.consideredOverride")
                    }
                  >
                    {slowPenalty > 0
                      ? t("logicTrace.consideredConfiguredEffective", {
                          priority: candidate.priority,
                          effective: candidate.effectivePriority,
                        })
                      : t("logicTrace.consideredConfigured", { priority: candidate.priority })}
                  </span>
                )}
                {slowPenalty > 0 && (
                  // 独立、可辨认的降权标记：它是与 W/CostMultiplier 并列的**另一维**事实，
                  // 不塞进上面那句里，免得与「配置→生效」的差值混为一谈。
                  <Badge
                    variant="outline"
                    className="text-[10px] border-amber-500/50 text-amber-600 dark:text-amber-500"
                    data-testid="considered-slow-penalty"
                    title={t("logicTrace.consideredSlowPenaltyHint", { penalty: slowPenalty })}
                  >
                    {t("logicTrace.consideredSlowPenalty", { penalty: slowPenalty })}
                  </Badge>
                )}
                <span className="text-muted-foreground">W:{candidate.weight}</span>
                <span className="text-muted-foreground">x{candidate.costMultiplier}</span>
                <Badge
                  variant={state === "selected" ? "default" : "secondary"}
                  className="text-[10px]"
                  data-candidate-state={state}
                >
                  {state === "selected"
                    ? t("logicTrace.consideredSelected")
                    : state === "affinity-skipped"
                      ? t("logicTrace.survivingSkipped")
                      : state === "lower-tier"
                        ? t("logicTrace.consideredLowerTier")
                        : t("logicTrace.consideredSameTier")}
                </Badge>
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}
