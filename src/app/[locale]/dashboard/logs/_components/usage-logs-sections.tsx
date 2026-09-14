"use client";

import { ActiveSessionsList } from "@/components/customs/active-sessions-list";
import type { CurrencyCode } from "@/lib/utils";
import type { BillingModelSource } from "@/types/system-config";
import { UsageLogsViewVirtualized } from "./usage-logs-view-virtualized";

/**
 * 用量日志页的两个区块。静态化改造后本模块**不再取数**：
 * 服务端版本在这里读 `getSystemSettings()` / `resolveSystemTimezone()` / `getEnvConfig()`，
 * 三者都拿不到客户端（`getEnvConfig` 在浏览器里没有 process.env）；现在由 `page.tsx`
 * 用客户端 action（`/api/v1/system/settings`、`/api/v1/system/timezone`）取好后传入。
 *
 * 原 `searchParams` 形参一并删除：服务端版本把它透传给 `UsageLogsViewVirtualized` 只为
 * 「SSR 首帧」，而该组件实际用 `useSearchParams()` 读筛选条件（其内部注释即如此写），
 * 静态导出下没有 SSR 首帧，这个形参是死参。
 */

export function UsageLogsActiveSessionsSection({ currencyCode }: { currencyCode: CurrencyCode }) {
  return (
    <ActiveSessionsList currencyCode={currencyCode} maxHeight="200px" showTokensCost={false} />
  );
}

interface UsageLogsDataSectionProps {
  isAdmin: boolean;
  userId: number;
  currencyCode?: CurrencyCode;
  billingModelSource?: BillingModelSource;
  serverTimeZone?: string;
}

export function UsageLogsDataSection({
  isAdmin,
  userId,
  currencyCode,
  billingModelSource,
  serverTimeZone,
}: UsageLogsDataSectionProps) {
  return (
    <UsageLogsViewVirtualized
      isAdmin={isAdmin}
      userId={userId}
      currencyCode={currencyCode}
      billingModelSource={billingModelSource}
      serverTimeZone={serverTimeZone}
    />
  );
}
