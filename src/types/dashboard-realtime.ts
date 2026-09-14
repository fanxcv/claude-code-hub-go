/**
 * dashboard-realtime 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/actions/dashboard-realtime.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */

import type { OverviewData } from "@/types/dashboard-overview";
import type {
  LeaderboardEntry,
  ModelLeaderboardEntry,
  ProviderLeaderboardEntry,
} from "@/types/leaderboard";
import type { ProviderSlotInfo } from "@/types/provider";
/**
 * 数据大屏完整数据
 */
export interface DashboardRealtimeData {
  /** 核心指标 */
  metrics: OverviewData;

  /** 实时活动流（最近20条） */
  activityStream: ActivityStreamEntry[];

  /** 用户排行榜（Top 5） */
  userRankings: LeaderboardEntry[];

  /** 供应商排行榜（Top 5） */
  providerRankings: ProviderLeaderboardEntry[];

  /** 供应商并发插槽状态 */
  providerSlots: ProviderSlotInfo[];

  /** 模型调用分布 */
  modelDistribution: ModelLeaderboardEntry[];

  /** 24小时趋势数据 */
  trendData: Array<{
    hour: number;
    value: number;
  }>;
}

/**
 * 实时活动流条目
 */
export interface ActivityStreamEntry {
  /** 消息 ID */
  id: string;
  /** 用户名 */
  user: string;
  /** 模型名称 */
  model: string;
  /** 供应商名称 */
  provider: string;
  /** 响应时间（毫秒） */
  latency: number;
  /** HTTP 状态码 */
  status: number;
  /** 成本（美元） */
  cost: number;
  /** 开始时间 */
  startTime: number;
}
