"use client";

import { UiSessionGate, useUiSession } from "@/components/ui-session-gate";
import { UsersPageClient } from "./users-page-client";

/**
 * 用户管理页。静态化改造后由客户端鉴权壳把关，取代原服务端的 `getSession()` + `redirect()`：
 * 未登录 → `/login?from=<当前路径>`，与「所有登录用户可访问」的权限口径一致（不限角色）。
 *
 * `currentUser` 改由壳注入的会话快照提供（原为服务端完整 `User` 行）——
 * 字段收窄的依据见 `users-page-client.tsx` 的 `UsersPageCurrentUser`。
 */
export default function UsersPage() {
  return (
    <UiSessionGate>
      <UsersPageContent />
    </UiSessionGate>
  );
}

function UsersPageContent() {
  const session = useUiSession();
  // 壳已挡住未登录态；这里只是把联合类型收窄（pending/authenticated 之外的态不会到达）。
  if (session.status !== "authenticated") return null;

  return <UsersPageClient currentUser={session.session.user} />;
}
