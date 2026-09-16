import type {
  ConversionFailurePhase,
  ConversionLossAction,
  ConversionLossSeverity,
  SpecialSetting,
} from "@/types/special-settings";

/** 使用记录中「该请求确实经过了协议转换」的信息。 */
export interface ProtocolConversionInfo {
  /** 客户端入站协议线，如 openai-responses。 */
  clientProtocol: string;
  /** 本次实际上游协议线，如 openai-chat。 */
  targetProtocol: string;
}

/** 使用记录中「本想转换但失败了」的信息。 */
export interface ProtocolConversionFailureInfo {
  /** 客户端入站协议线；无法判定时为 null。 */
  clientProtocol: string | null;
  /** 原本要转到的上游协议线；无法判定时为 null。 */
  targetProtocol: string | null;
  /** 失败阶段；缺值时为 null（展示时退用通用文案，不编造阶段）。 */
  phase: ConversionFailurePhase | null;
  /** 失败原因（后端已脱敏并定长）；缺值时为 null。 */
  reason: string | null;
  /** 本次是否已回退为原生直通。 */
  fallback: boolean;
}

/** 某一组损失：同一能力 + 同一动作的聚合计数。 */
export interface ProtocolConversionLossGroupInfo {
  /** 能力标识（Go 的 `convert.Loss*` 常量，如 cache_control）。 */
  capability: string;
  /** 已知三态之一；库里出现未知动作时原样保留（机器标识符，与协议名同口径展示）。 */
  action: string;
  /** 该组聚合到的损失条目数。 */
  count: number;
  /** 档位：列表徽章只数 rewrite，故每组必须给出确定档位（后端未给时按能力名推导）。 */
  severity: ConversionLossSeverity;
}

/** 使用记录中「本次转换丢/降/改了哪些能力」的信息。 */
export interface ProtocolConversionLossInfo {
  clientProtocol: string | null;
  targetProtocol: string | null;
  /** **未聚合**的损失条目数（全部档位）。 */
  total: number;
  /** 三档各自合计；列表徽章只展示 `rewriteTotal`。 */
  rewriteTotal: number;
  degradeTotal: number;
  infoTotal: number;
  /** 按 (capability, action) 聚合的分组；空数组表示只知总数、没有可展示的明细。 */
  groups: ProtocolConversionLossGroupInfo[];
}

/** 已知损失动作（与 Go 的 `convert.LossAction` 同取值域）；未知值不被剔除，见 `getProtocolConversionLoss`。 */
export const LOSS_ACTIONS: readonly ConversionLossAction[] = ["dropped", "downgraded", "rewritten"];

/** 已知损失档位（与 Go 的 `convert.LossSeverity` 同取值域）；顺序即 tooltip 里的展示顺序。 */
export const LOSS_SEVERITIES: readonly ConversionLossSeverity[] = ["rewrite", "degrade", "info"];

/** 过滤非字符串及空白值，避免把无效协议名或能力名画到表里。 */
function normalizeNonEmptyString(value: unknown): string | null {
  if (typeof value !== "string") {
    return null;
  }

  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : null;
}

/** 只接受正有限数：计数为 0 或非法时视作「没有这一项」，不编造 0。 */
function normalizePositiveCount(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) && value > 0 ? value : null;
}

/** 合计允许为 0（「本档无损失」是合法事实），但不接受负数与非法值。 */
function normalizeNonNegativeCount(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) && value >= 0 ? value : null;
}

/**
 * 历史条目的档位推导表（**精确名 → 档位**），须与 Go 的 `convert.LossSeverityOf` 逐条同步
 * （`go/internal/convert/hub.go`：`store`/`prompt_cache_key` → info，`thinking.signature`/
 * `thinking.derived`/`reasoning.replay`/`cache_control` → degrade，其余 → rewrite）。
 *
 * 为何需要：`severity` 是后加的字段，库里已落库的历史条目没有它。若无此表，读取侧只能
 * 要么整条不显示、要么把几十条降级当作改写报出来——两者都比多写几行表差。
 *
 * 为何必须是**精确名**而非族前缀：Go 只认精确常量，未列出者一律 rewrite。若此处按族放宽
 * （`thinking` / `store` / `cache_control` / `reasoning.replay` / `prompt_cache_key`），库里
 * 出现未列出的新名（`thinking.new`、`store.new`、`cache_control.v2`）时两侧就会分歧：Go 判
 * rewrite、此处判 degrade/info ⇒ 改写档合计算成 0 ⇒ 徽章不画，真损失被降噪吞掉。
 *
 * 改写档不在此列表：它与 Go 的 `default` 分支同义（`unknown_field.<reason>`、`image`、`top_k`
 * 等细分名一并落地），故「未列出 ⇒ rewrite」即是完整规则——多列一份只会再分叉。
 */
const DEGRADE_CAPABILITIES = [
  "thinking.signature",
  "thinking.derived",
  "reasoning.replay",
  "cache_control",
] as const;

const INFO_CAPABILITIES = ["store", "prompt_cache_key"] as const;

/** 按能力名推导档位；未列出者归 rewrite（与 Go 的 `default` 分支同口径，宁可多画也不漏报）。 */
function lossSeverityForCapability(capability: string): ConversionLossSeverity {
  if ((INFO_CAPABILITIES as readonly string[]).includes(capability)) {
    return "info";
  }
  if ((DEGRADE_CAPABILITIES as readonly string[]).includes(capability)) {
    return "degrade";
  }
  return "rewrite";
}

/**
 * 同一能力**按动作**分档的例外表：`thinking.block` 的两种事实档位不同。
 *
 * 为何要连动作一起看：「块被丢」是客户端的思考内容消失（改写档），「块被降级」只是强度载体的
 * 换算（降级档）。表里只有这一项——Go 的 `convert.LossSeverityOf` 同样只对 `thinking.block`
 * 连 action 一起看，其余能力一律按名判档（已逐条比对）。
 *
 * 影响面：库里历史条目没有 `severity` 字段，只能在这里判。若把 `thinking.block` 整族判成降级，
 * 含「思考整块被丢」的历史行会算不出改写档 ⇒ 徽章不画 ⇒ 真损失被降噪吞掉。
 */
const ACTION_DEPENDENT_SEVERITY: Record<string, (action: string) => ConversionLossSeverity> = {
  // 与 Go 同口径：除 downgraded 外一律改写档（未知动作也不许被降噪藏起来）。
  "thinking.block": (action) => (action === "downgraded" ? "degrade" : "rewrite"),
};

/** 按 (能力, 动作) 推导档位：先查动作相关例外，再退回能力名精确表。 */
function lossSeverityForGroup(capability: string, action: string): ConversionLossSeverity {
  const byAction = ACTION_DEPENDENT_SEVERITY[capability];
  return byAction ? byAction(action) : lossSeverityForCapability(capability);
}

/** 后端给的档位优先（宽进严出：未知取值当作未给，退回按 (能力, 动作) 推导）。 */
function normalizeSeverity(
  value: unknown,
  capability: string,
  action: string
): ConversionLossSeverity {
  return (
    LOSS_SEVERITIES.find((candidate) => candidate === value) ??
    lossSeverityForGroup(capability, action)
  );
}

/**
 * 从使用记录审计中读取协议转换信息。
 *
 * 只有确实发生转换的请求才由 forwarder 写入该记录；未转换的请求返回 null
 * （不写 hit:false 反向标记，故此处也不会看到「未转换」的记录）。
 */
export function getProtocolConversion(
  specialSettings: SpecialSetting[] | null | undefined
): ProtocolConversionInfo | null {
  if (!Array.isArray(specialSettings)) {
    return null;
  }

  for (const setting of specialSettings) {
    if (setting.type !== "protocol_conversion") {
      continue;
    }

    const clientProtocol = normalizeNonEmptyString(setting.clientProtocol);
    const targetProtocol = normalizeNonEmptyString(setting.targetProtocol);
    if (clientProtocol && targetProtocol) {
      return { clientProtocol, targetProtocol };
    }
  }

  return null;
}

/** 合法阶段取值集（宽进严出：库里出现未知值时按 null 处理，不把它画到表上）。 */
const FAILURE_PHASES: readonly ConversionFailurePhase[] = ["path_resolution", "body_conversion"];

/**
 * 从使用记录审计中读取协议转换**失败**信息。
 *
 * 与 `getProtocolConversion` 互斥（同一次尝试只会有一条）：这里返回非空就说明本次**没有**
 * 成功转换，展示层必须据此画失败态——否则转换失败会被渲染成「无转换」，用户再也看不到它。
 */
export function getProtocolConversionFailure(
  specialSettings: SpecialSetting[] | null | undefined
): ProtocolConversionFailureInfo | null {
  if (!Array.isArray(specialSettings)) {
    return null;
  }

  for (const setting of specialSettings) {
    if (setting.type !== "protocol_conversion_failed") {
      continue;
    }

    const phase = FAILURE_PHASES.find((candidate) => candidate === setting.phase) ?? null;
    const reason =
      typeof setting.reason === "string" && setting.reason.trim().length > 0
        ? setting.reason.trim()
        : null;

    return {
      clientProtocol: normalizeNonEmptyString(setting.clientProtocol),
      // 失败时 targetProtocol 可能缺失（阶段在判定目标线之前），故不做「两个都要有」的过滤。
      targetProtocol: normalizeNonEmptyString(setting.targetProtocol),
      phase,
      reason,
      fallback: setting.fallback === true,
    };
  }

  return null;
}

/**
 * 从使用记录审计中读取协议转换**损失**信息。
 *
 * 与 `getProtocolConversionFailure` 不会同时命中：转换失败时根本没有损失集（转换没做，无从丢），
 * 而转换成功且零损失时后端不写本条。故读到它就意味着「转换发生了，但有损」。
 *
 * 宽进严出：只剔除「不可能成立」的项（能力名为空、计数非正数），未知动作**原样保留**——
 * 丢掉整组会让损失被少报，而动作名与协议名同属机器标识符，直接展示不损失可读性。
 *
 * 档位：优先读后端给的 `severity`，缺失时按 (能力, 动作) 推导（历史条目没有该字段，见前缀表与
 * 动作相关例外表的注释）。
 * 三档合计同样先读后端声明，缺失时按分组现算；**没有明细只有总数**时把总数归入改写档，
 * 宁可多画一个徽章，也不让整条损失从列表里消失。
 */
export function getProtocolConversionLoss(
  specialSettings: SpecialSetting[] | null | undefined
): ProtocolConversionLossInfo | null {
  if (!Array.isArray(specialSettings)) {
    return null;
  }

  for (const setting of specialSettings) {
    if (setting.type !== "protocol_conversion_loss") {
      continue;
    }

    let total = normalizePositiveCount(setting.total) ?? 0;
    const groups: ProtocolConversionLossGroupInfo[] = [];
    const computedTotals: Record<ConversionLossSeverity, number> = {
      rewrite: 0,
      degrade: 0,
      info: 0,
    };
    if (Array.isArray(setting.groups)) {
      for (const group of setting.groups) {
        const capability = normalizeNonEmptyString(group.capability);
        const action = normalizeNonEmptyString(group.action);
        const count = normalizePositiveCount(group.count);
        if (!capability || !action || count === null) {
          continue;
        }
        const severity = normalizeSeverity(group.severity, capability, action);
        computedTotals[severity] += count;
        groups.push({ capability, action, count, severity });
      }
    }

    // 总数与明细都没有的脏记录当作「没有这条审计」，不画一个「内容改写 0 项」的噪声徽章。
    if (total === 0 && groups.length === 0) {
      return null;
    }

    // 计数恒为正，故「有明细却报总数 0」可证伪：按明细求和，不把 0 当事实展示。
    if (total === 0) {
      total = computedTotals.rewrite + computedTotals.degrade + computedTotals.info;
    }

    return {
      clientProtocol: normalizeNonEmptyString(setting.clientProtocol),
      targetProtocol: normalizeNonEmptyString(setting.targetProtocol),
      total,
      rewriteTotal:
        normalizeNonNegativeCount(setting.rewriteTotal) ??
        (groups.length === 0 ? total : computedTotals.rewrite),
      degradeTotal: normalizeNonNegativeCount(setting.degradeTotal) ?? computedTotals.degrade,
      infoTotal: normalizeNonNegativeCount(setting.infoTotal) ?? computedTotals.info,
      groups,
    };
  }

  return null;
}
