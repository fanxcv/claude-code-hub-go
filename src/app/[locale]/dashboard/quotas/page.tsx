"use client";

import { useEffect } from "react";
import { LoadingState } from "@/components/loading/page-skeletons";
import { useUiSession } from "@/components/ui-session-gate";
import { useRouter } from "@/i18n/routing";

/**
 * 配额总入口：按会话角色分流（静态导出后无服务端，故用客户端跳转）。
 *
 * 分流口径与改造前的 SSR 版逐条一致：
 *   未登录 -> /login；角色非 admin -> /dashboard/my-quota；admin -> /dashboard/quotas/users。
 * 角色未知时（无壳注入、回退探针不含角色）按「不拒绝」处理：走 admin 落点，
 * 与 `UiSessionGate` 对未知角色的策略一致，避免把真管理员踢去 my-quota。
 */
export default function QuotasPage() {
  const state = useUiSession();
  const router = useRouter();

  useEffect(() => {
    if (state.status === "pending") return;
    if (state.status === "anonymous") {
      router.replace("/login");
      return;
    }
    const role = state.session.user.role;
    if (role !== undefined && role !== "admin") {
      router.replace("/dashboard/my-quota");
      return;
    }
    router.replace("/dashboard/quotas/users");
  }, [router, state]);

  return <LoadingState className="p-6" />;
}
