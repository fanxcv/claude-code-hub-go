import type {
  ConversionFailurePhase,
  ConversionLossAction,
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
}

/** 使用记录中「本次转换丢/降/改了哪些能力」的信息。 */
export interface ProtocolConversionLossInfo {
  clientProtocol: string | null;
  targetProtocol: string | null;
  /** **未聚合**的损失条目数。 */
  total: number;
  /** 按 (capability, action) 聚合的分组；空数组表示只知总数、没有可展示的明细。 */
  groups: ProtocolConversionLossGroupInfo[];
}

/** 已知损失动作（与 Go 的 `convert.LossAction` 同取值域）；未知值不被剔除，见 `getProtocolConversionLoss`。 */
export const LOSS_ACTIONS: readonly ConversionLossAction[] = ["dropped", "downgraded", "rewritten"];

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

    const total = normalizePositiveCount(setting.total) ?? 0;
    const groups: ProtocolConversionLossGroupInfo[] = [];
    if (Array.isArray(setting.groups)) {
      for (const group of setting.groups) {
        const capability = normalizeNonEmptyString(group.capability);
        const action = normalizeNonEmptyString(group.action);
        const count = normalizePositiveCount(group.count);
        if (!capability || !action || count === null) {
          continue;
        }
        groups.push({ capability, action, count });
      }
    }

    // 总数与明细都没有的脏记录当作「没有这条审计」，不画一个「丢失 0 项」的噪声徽章。
    if (total === 0 && groups.length === 0) {
      return null;
    }

    return {
      clientProtocol: normalizeNonEmptyString(setting.clientProtocol),
      targetProtocol: normalizeNonEmptyString(setting.targetProtocol),
      total,
      groups,
    };
  }

  return null;
}
