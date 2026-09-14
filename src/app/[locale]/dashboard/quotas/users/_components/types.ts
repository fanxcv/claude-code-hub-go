import type { CurrencyCode } from "@/lib/utils/currency";

export interface UserQuotaSnapshot {
  rpm: { current: number; limit: number | null; window: "per_minute" };
  dailyCost: { current: number; limit: number | null; resetAt?: Date };
}

export interface UserKeyWithUsage {
  id: number;
  name: string;
  status: "enabled" | "disabled";
  todayUsage: number;
  /** 累计成本（全时段、按自身与所属用户的重置时刻起算）；`null` 表示**读数不可用**（取数失败），
   * 界面必须显式说明而不是显示 0——0 是确定的错数（详见 `./total-usage.ts`）。 */
  totalUsage: number | null;
  limit5hUsd: number | null;
  limitDailyUsd: number | null;
  limitWeeklyUsd: number | null;
  limitMonthlyUsd: number | null;
  limitTotalUsd: number | null;
  limitConcurrentSessions: number;
  dailyResetMode: "fixed" | "rolling";
  dailyResetTime: string;
}

export interface UserQuotaWithUsage {
  id: number;
  name: string;
  note?: string;
  role: "admin" | "user";
  isEnabled: boolean;
  expiresAt: Date | null;
  providerGroup?: string | null;
  tags?: string[];
  quota: UserQuotaSnapshot | null;
  limit5hUsd: number | null;
  limitWeeklyUsd: number | null;
  limitMonthlyUsd: number | null;
  limitTotalUsd: number | null;
  limitConcurrentSessions: number | null;
  /** 同 `UserKeyWithUsage.totalUsage`：`null` 为读数不可用。 */
  totalUsage: number | null;
  keys: UserKeyWithUsage[];
}

export interface UsersQuotaClientProps {
  users: UserQuotaWithUsage[];
  currencyCode?: CurrencyCode;
  searchQuery?: string;
  sortBy?: "name" | "usage";
  filter?: "all" | "warning" | "exceeded";
}
