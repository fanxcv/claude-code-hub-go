import type {
  SpecialSetting,
  ThinkingEffortForwardedSpecialSetting,
} from "@/types/special-settings";

/**
 * 协议转换探针给出的实际转发事实（Go 侧新增，见 `ThinkingEffortForwardedSpecialSetting`）。
 *
 * 它回答的是「转换之后、即将发给供应商的是哪个值」——用户口径要求思考强度这一列能当
 * **协议转换正确性探针**用，所以 forwardedEffort 必须来自转换器产物，而不是由展示层二次推导。
 */
export interface ThinkingEffortConversion {
  /** 转换后发给供应商的值；转换丢弃该字段时为 null。 */
  forwardedEffort: string | null;
  /** 转换把该字段丢了（显式记账）——界面必须给出可见提示，不能静默成 `-`。 */
  dropped: boolean;
  /** 本次是否真的发生了协议转换（原生直通为 false）。 */
  converted: boolean;
  clientProtocol: string | null;
  targetProtocol: string | null;
}

/**
 * 从审计里提取协议转换探针。
 *
 * 多条时取**最后一条**：终态追加的条目排在建行写入的客户端侧审计之后；若将来出现「同一请求
 * 多条探针」（例如多次上游尝试各记一条），末条即本次最终发出的那一次。
 */
export function extractThinkingEffortConversion(
  specialSettings: SpecialSetting[] | null | undefined
): ThinkingEffortConversion | null {
  if (!Array.isArray(specialSettings)) {
    return null;
  }
  let found: ThinkingEffortForwardedSpecialSetting | null = null;
  for (const setting of specialSettings) {
    if (setting.type === "thinking_effort_forwarded") {
      found = setting;
    }
  }
  if (!found) {
    return null;
  }
  return {
    forwardedEffort: normalizeEffort(found.forwardedEffort),
    dropped: found.dropped === true,
    converted: found.converted === true,
    clientProtocol: normalizeEffort(found.clientProtocol),
    targetProtocol: normalizeEffort(found.targetProtocol),
  };
}

/** 按协议线反推展示来源（用于「只有探针、没有客户端侧审计」时选文案）；认不出返回 null。 */
export function sourceForProtocol(
  protocol: string | null
): "codex" | "openai" | "anthropic" | null {
  switch (protocol) {
    case "anthropic-messages":
      return "anthropic";
    case "openai-responses":
      return "codex";
    case "openai-chat":
      return "openai";
    default:
      return null;
  }
}

/** 过滤非字符串与空白值：审计里的 null/空串都表示「没有这个值」。 */
function normalizeEffort(value: unknown): string | null {
  if (typeof value !== "string") {
    return null;
  }
  const trimmed = value.trim();
  return trimmed.length > 0 ? trimmed : null;
}
