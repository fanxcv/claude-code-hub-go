"use client";

import { UiSessionGate } from "@/components/ui-session-gate";
import { ActiveSessionsClient } from "./_components/active-sessions-client";

/**
 * 活跃会话页。静态化改造后由客户端鉴权壳把关，取代原服务端的 `getSession()` + `redirect()`：
 * 未登录 → `/login?from=<当前路径>`（`UiSessionGate` 的落点语义），非管理员 → `/dashboard`，
 * 与原页的两分支一致。
 */
export default function ActiveSessionsPage() {
  return (
    <UiSessionGate requireRole="admin">
      <ActiveSessionsClient />
    </UiSessionGate>
  );
}
