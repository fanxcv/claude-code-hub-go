"use client";

import { useEffect } from "react";
import { LoadingState } from "@/components/loading/page-skeletons";
import { useUiSession } from "@/components/ui-session-gate";
import { useRouter } from "@/i18n/routing";

// This page has been deprecated. Key-level quotas are now managed at user level.
// Users should visit /dashboard/quotas/users instead.
// Redirecting to user quotas page...

/**
 * 已废弃页：一律跳到用户级配额页（静态导出后无服务端，故用客户端跳转）。
 *
 * 非 admin 的分流与改造前的 SSR 版一致：已登录 -> /dashboard/my-quota，未登录 -> /login。
 * 角色未知时按「不拒绝」处理（同 `quotas/page.tsx` 的说明）。
 */
export default function KeysQuotaPage() {
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
