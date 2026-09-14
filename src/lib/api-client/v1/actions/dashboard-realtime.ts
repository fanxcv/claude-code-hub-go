import type { DashboardRealtimeData } from "@/types/dashboard-realtime";
import { apiGet, toActionResult } from "./_compat";

export type { DashboardRealtimeData } from "@/types/dashboard-realtime";

export function getDashboardRealtimeData() {
  return toActionResult(apiGet<DashboardRealtimeData>("/api/v1/dashboard/realtime"));
}
