import type { CurrencyCode } from "@/lib/utils";
import type { CacheTtlPreference } from "./cache";

/**
 * 密钥数据库实体类型
 */
export interface Key {
  id: number;
  userId: number;
  name: string;
  key: string;
  isEnabled: boolean;
  expiresAt?: Date;

  // Web UI 登录权限控制
  canLoginWebUi: boolean;

  // 金额限流配置
  limit5hUsd: number | null;
  limit5hResetMode: "fixed" | "rolling";
  limitDailyUsd: number | null;
  dailyResetMode: "fixed" | "rolling";
  dailyResetTime: string; // HH:mm 格式
  limitWeeklyUsd: number | null;
  limitMonthlyUsd: number | null;
  limitTotalUsd?: number | null;
  costResetAt?: Date | null;
  limitConcurrentSessions: number;

  // Provider group override (null = inherit from user)
  providerGroup: string | null;

  // Cache TTL override (inherit -> follow provider/client)
  cacheTtlPreference: CacheTtlPreference | null;

  createdAt: Date;
  updatedAt: Date;
  deletedAt?: Date;
}

/**
 * 密钥创建数据
 */
export interface CreateKeyData {
  user_id: number;
  name: string;
  key: string;
  is_enabled?: boolean;
  expires_at?: Date | null; // null = 永不过期
  // Web UI 登录权限控制
  can_login_web_ui?: boolean;
  // 金额限流配置
  limit_5h_usd?: number | null;
  limit_5h_reset_mode?: "fixed" | "rolling";
  limit_daily_usd?: number | null;
  daily_reset_mode?: "fixed" | "rolling";
  daily_reset_time?: string;
  limit_weekly_usd?: number | null;
  limit_monthly_usd?: number | null;
  limit_total_usd?: number | null;
  cost_reset_at?: Date | null;
  limit_concurrent_sessions?: number;
  // Provider group override (null = inherit from user)
  provider_group?: string | null;

  // Cache TTL override
  cache_ttl_preference?: CacheTtlPreference;
}

/**
 * 密钥更新数据
 */
export interface UpdateKeyData {
  name?: string;
  is_enabled?: boolean;
  expires_at?: Date | null; // null = 清除日期（永不过期）
  // Web UI 登录权限控制
  can_login_web_ui?: boolean;
  // 金额限流配置
  limit_5h_usd?: number | null;
  limit_5h_reset_mode?: "fixed" | "rolling";
  limit_daily_usd?: number | null;
  daily_reset_mode?: "fixed" | "rolling";
  daily_reset_time?: string;
  limit_weekly_usd?: number | null;
  limit_monthly_usd?: number | null;
  limit_total_usd?: number | null;
  cost_reset_at?: Date | null;
  limit_concurrent_sessions?: number;
  // Provider group override (null = inherit from user)
  provider_group?: string | null;

  // Cache TTL override
  cache_ttl_preference?: CacheTtlPreference;
}

// ---- 2026-09 node 退役迁移（原 actions/key-quota、actions/keys） ----
export interface KeyQuotaItem {
  type: "limit5h" | "limitDaily" | "limitWeekly" | "limitMonthly" | "limitTotal" | "limitSessions";
  current: number;
  limit: number | null;
  mode?: "fixed" | "rolling";
  time?: string;
  resetAt?: Date;
}

export interface KeyQuotaUsageResult {
  keyName: string;
  items: KeyQuotaItem[];
  currencyCode: CurrencyCode;
}

export interface BatchUpdateKeysParams {
  keyIds: number[];
  updates: {
    providerGroup?: string | null;
    limit5hUsd?: number | null;
    limit5hResetMode?: "fixed" | "rolling";
    limitDailyUsd?: number | null;
    limitWeeklyUsd?: number | null;
    limitMonthlyUsd?: number | null;
    canLoginWebUi?: boolean;
    isEnabled?: boolean;
  };
}

/**
 * 仅更新密钥的某个限额字段，避免触发完整 KeyFormSchema 默认值覆盖（保护 providerGroup
 * / canLoginWebUi / dailyResetMode 等未传字段）。
 *
 * 用于 Key 限额使用情况弹窗 / 限额管理页的快捷编辑。
 */
export type PatchKeyLimitField =
  | "limit5hUsd"
  | "limitDailyUsd"
  | "limitWeeklyUsd"
  | "limitMonthlyUsd"
  | "limitTotalUsd"
  | "limitConcurrentSessions";
