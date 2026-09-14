"use client";

import { useQuery } from "@tanstack/react-query";
import { UiSessionGate, useUiSession } from "@/components/ui-session-gate";
import { getServerTimeZone, getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import {
  UsageLogsActiveSessionsSection,
  UsageLogsDataSection,
} from "./_components/usage-logs-sections";
import { UsageLogsSkeleton } from "./_components/usage-logs-skeleton";

/**
 * 用量日志页。静态化改造后改为客户端取数，取代原来三处服务端绑定：
 * - `getSession()` + `redirect()` → `UiSessionGate`（未登录 → `/login?from=<当前路径>`；本页不限角色）；
 * - `getSystemSettings()`（`@/repository/system-config`）→ 客户端 action `/api/v1/system/settings`；
 * - `resolveSystemTimezone()`（服务端：DB 设置 → env `TZ` → UTC）→ 客户端 action
 * `/api/v1/system/timezone`。
 * 已知差异（与 `settings/config/page.tsx` 的同类记载一致）：`DASHBOARD_LOGS_POLL_INTERVAL_MS`
 * 来自 `getEnvConfig()`，浏览器里没有 `process.env`，故不再透传。使用记录页的刷新间隔改由
 * 用户在本机选择（`useLogsRefreshPreference`，存 localStorage，默认 3 秒、可关闭），
 * 与刷新模式（轮询 / 推送）一起在页面工具栏切换；`UsageLogsViewVirtualized` 的
 * `logsRefreshIntervalMs` 保留为「出厂默认覆盖」入口，将来接 `__CCH_BOOTSTRAP__` 或系统设置。
 *
 * 原页的 `<Suspense>` 边界是给「服务端子组件取数」用的，改造后两个区块都是同步客户端组件、
 * 各自有加载态，故边界移除（`ActiveSessionsSkeleton` 随之成为死代码并删除）。
 */
export default function UsageLogsPage() {
  return (
    <UiSessionGate>
      <UsageLogsContent />
    </UiSessionGate>
  );
}

function UsageLogsContent() {
  const session = useUiSession();
  const settingsQuery = useQuery({
    queryKey: ["system-settings"],
    queryFn: getSystemSettings,
    staleTime: 30_000,
  });
  const timeZoneQuery = useQuery({
    queryKey: ["system-timezone"],
    queryFn: getServerTimeZone,
    staleTime: 300_000,
  });

  // 壳已挡住未登录态；这里只是把联合类型收窄。
  if (session.status !== "authenticated") return null;

  // 设置未就绪时先显示页面骨架：`currencyDisplay`/`billingModelSource`/`enableHighConcurrencyMode`
  // 都来自它，先画再换会闪（原服务端版本首帧即持有）。
  if (settingsQuery.isPending) return <UsageLogsSkeleton />;

  const settings = settingsQuery.data;
  const timeZone = timeZoneQuery.data?.ok ? timeZoneQuery.data.data.timeZone : undefined;

  return (
    <div className="space-y-4">
      {!settings?.enableHighConcurrencyMode && (
        <UsageLogsActiveSessionsSection currencyCode={settings?.currencyDisplay ?? "USD"} />
      )}

      <UsageLogsDataSection
        isAdmin={session.session.user.role === "admin"}
        userId={session.session.user.id}
        currencyCode={settings?.currencyDisplay}
        billingModelSource={settings?.billingModelSource}
        serverTimeZone={timeZone}
      />
    </div>
  );
}
