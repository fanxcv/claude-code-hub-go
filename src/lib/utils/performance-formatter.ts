export const NON_BILLING_ENDPOINTS = [
  "/v1/messages/count_tokens",
  "/v1/responses/compact",
] as const;

export const NON_BILLING_ENDPOINT = NON_BILLING_ENDPOINTS[0];

function normalizeEndpointForBilling(endpoint: string): string {
  const trimmed = endpoint.trim().toLowerCase();
  if (!trimmed) {
    return "";
  }

  return trimmed === "/" ? trimmed : trimmed.replace(/\/+$/, "");
}

export function isNonBillingEndpoint(endpoint: string | null | undefined): boolean {
  if (!endpoint) {
    return false;
  }

  const normalizedEndpoint = normalizeEndpointForBilling(endpoint);
  return NON_BILLING_ENDPOINTS.includes(
    normalizedEndpoint as (typeof NON_BILLING_ENDPOINTS)[number]
  );
}

/**
 * 格式化会话耗时（分档更细，与 formatDuration 的区别是这里到分钟级）。
 *
 * session-list-item 与 active-sessions-table 原先各抄了一份逐字相同的实现，收拢到此。
 * - < 1s：毫秒
 * - < 60s：秒（一位小数）
 * - 其余：`Nm Ns`
 */
export function formatSessionDuration(durationMs: number | undefined): string {
  if (!durationMs) return "-";

  if (durationMs < 1000) {
    return `${durationMs}ms`;
  }
  if (durationMs < 60000) {
    return `${(Number(durationMs) / 1000).toFixed(1)}s`;
  }
  const minutes = Math.floor(durationMs / 60000);
  const seconds = Math.floor((durationMs % 60000) / 1000);
  return `${minutes}m ${seconds}s`;
}

/**
 * 格式化请求耗时
 * - 1000ms 以上显示为秒（如 "1.23s"）
 * - 1000ms 以下显示为毫秒（如 "850ms"）
 */
export function formatDuration(durationMs: number | null): string {
  if (durationMs == null) return "-";

  // 1000ms 以上转换为秒
  if (durationMs >= 1000) {
    return `${(Number(durationMs) / 1000).toFixed(2)}s`;
  }

  // 1000ms 以下显示毫秒
  return `${durationMs}ms`;
}

/**
 * 计算输出速率（tokens/second）
 *
 * 生成窗口以真 TTFB 为起点。firstByteMs 缺失（流式门禁上线前的历史行）返回 null，
 * 不再退回总耗时——那会把上游排队和中性帧窗口算进生成时间，高估速率。
 */
export function calculateOutputRate(
  outputTokens: number | null,
  durationMs: number | null,
  firstByteMs: number | null
): number | null {
  if (outputTokens == null || outputTokens <= 0 || durationMs == null || durationMs <= 0) {
    return null;
  }
  if (firstByteMs == null) return null;
  const generationTimeMs = durationMs - firstByteMs;
  if (generationTimeMs <= 0) return null;
  return outputTokens / (generationTimeMs / 1000);
}

/**
 * Determine if output rate should be hidden due to blocked streaming request.
 * Rule: Hide when generationTimeMs / durationMs < 0.1 AND outputRate > 5000
 * This indicates TTFB is very close to total duration with abnormally high tok/s.
 */
export function shouldHideOutputRate(
  outputRate: number | null,
  durationMs: number | null,
  firstByteMs: number | null
): boolean {
  if (
    outputRate == null ||
    !Number.isFinite(outputRate) ||
    durationMs == null ||
    durationMs <= 0 ||
    firstByteMs == null
  ) {
    return false;
  }
  const generationTimeMs = durationMs - firstByteMs;
  if (generationTimeMs <= 0) return false;
  const ratio = generationTimeMs / durationMs;
  return ratio < 0.1 && outputRate > 5000;
}
