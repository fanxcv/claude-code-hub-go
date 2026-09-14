/**
 * dashboard-overview 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/actions/overview.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */
/**
 * 概览数据（包含并发数和今日统计）
 */
export interface OverviewData {
  /** 当前并发数 */
  concurrentSessions: number;
  /** 今日总请求数 */
  todayRequests: number;
  /** 今日总消耗（美元） */
  todayCost: number;
  /** 平均响应时间（毫秒） */
  avgResponseTime: number;
  /** 今日错误率（百分比） */
  todayErrorRate: number;
  /** 昨日同时段请求数 */
  yesterdaySamePeriodRequests: number;
  /** 昨日同时段消耗 */
  yesterdaySamePeriodCost: number;
  /** 昨日同时段平均响应时间 */
  yesterdaySamePeriodAvgResponseTime: number;
  /** 最近1分钟请求数 (RPM) */
  recentMinuteRequests: number;
}
