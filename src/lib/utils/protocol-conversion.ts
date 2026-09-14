import type { ConversionFailurePhase, SpecialSetting } from "@/types/special-settings";

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

/** 过滤非字符串及空白值，避免把无效协议名画到表里。 */
function normalizeProtocol(value: unknown): string | null {
  if (typeof value !== "string") {
    return null;
  }

  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : null;
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

    const clientProtocol = normalizeProtocol(setting.clientProtocol);
    const targetProtocol = normalizeProtocol(setting.targetProtocol);
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
      clientProtocol: normalizeProtocol(setting.clientProtocol),
      // 失败时 targetProtocol 可能缺失（阶段在判定目标线之前），故不做「两个都要有」的过滤。
      targetProtocol: normalizeProtocol(setting.targetProtocol),
      phase,
      reason,
      fallback: setting.fallback === true,
    };
  }

  return null;
}
