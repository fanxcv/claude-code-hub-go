import type { OverviewData } from "@/types/dashboard-overview";
import { apiGet, toActionResult } from "./_compat";

export type { OverviewData } from "@/types/dashboard-overview";

export function getOverviewData() {
  return toActionResult(apiGet<OverviewData>("/api/v1/dashboard/overview"));
}
