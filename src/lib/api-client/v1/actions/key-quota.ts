import type { KeyQuotaUsageResult } from "@/types/key";
import { apiGet, toActionResult } from "./_compat";

export type { KeyQuotaItem, KeyQuotaUsageResult } from "@/types/key";

export function getKeyQuotaUsage(keyId: number) {
  return toActionResult(apiGet<KeyQuotaUsageResult>(`/api/v1/keys/${keyId}/quota`));
}
