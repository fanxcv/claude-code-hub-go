/**
 * 缓存命中率口径与配色。
 *
 * 口径：cacheRead / (input + cacheCreation + cacheRead) * 100，分母为 0 记 0。
 * 与「我的用量」模型明细同口径；排行榜接口另有直接给出的比值（0~1），不经此处换算。
 */
export function cacheHitRatePercent(tokens: {
  inputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
}): number {
  const denominator = tokens.inputTokens + tokens.cacheCreationTokens + tokens.cacheReadTokens;
  if (denominator <= 0) {
    return 0;
  }
  return (tokens.cacheReadTokens / denominator) * 100;
}

export function cacheHitRateColorClass(ratePercent: number): string {
  if (ratePercent >= 85) {
    return "text-green-600 dark:text-green-400";
  }
  if (ratePercent >= 60) {
    return "text-yellow-600 dark:text-yellow-400";
  }
  return "text-orange-600 dark:text-orange-400";
}
