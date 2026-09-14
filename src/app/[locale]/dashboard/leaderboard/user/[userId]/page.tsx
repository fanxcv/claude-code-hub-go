"use client";

import { useQuery } from "@tanstack/react-query";
import { useParams } from "next/navigation";
import { useEffect } from "react";
import { LoadingState } from "@/components/loading/page-skeletons";
import { UiSessionGate } from "@/components/ui-session-gate";
import { useRouter } from "@/i18n/routing";
import { apiClient } from "@/lib/api-client/v1/client";
import type { UserDisplay } from "@/types/user";
import { UserInsightsView } from "./_components/user-insights-view";

/**
 * 单用户画像页（动态段）。改造前是 SSR：`getSession()` 判 admin + `findUserById()` 取名字。
 * 静态导出后无服务端，故用户数据走 REST（`GET /api/v1/users/{id}`），门禁走壳注入会话。
 *
 * 分流口径与 SSR 版一致：非 admin → /dashboard/leaderboard；userId 非法或用户不存在
 * → 同页（SSR 是 redirect，这里是客户端 replace）。
 *
 * **静态导出限制（重要）**：本路由是动态段，构建期无法枚举真实用户 id（`generateStaticParams`
 * 需要 DB，而导出阶段没有服务端），故被 `scripts/build-ui-export.mjs` 的 `DYNAMIC_SEGMENTS`
 * 移出、不产出 HTML，深层链接由 Go 壳兜底。本次改造让它**具备被导出的条件**（服务端绑定已清零），
 * 但要真正可用还需导出流程给出方案——见 `` §4。
 */
export default function UserInsightsPage() {
  return (
    <UiSessionGate requireRole="admin" forbiddenHref="/dashboard/leaderboard">
      <UserInsightsContent />
    </UiSessionGate>
  );
}

function UserInsightsContent() {
  const params = useParams<{ userId: string }>();
  const router = useRouter();
  const userId = Number(params?.userId);
  const isValidUserId = Number.isInteger(userId) && userId > 0;

  const user = useQuery({
    queryKey: ["v1", "users", "detail", userId],
    enabled: isValidUserId,
    queryFn: () => apiClient.get<UserDisplay>(`/api/v1/users/${userId}`),
    retry: false,
  });

  const missing = !isValidUserId || user.isError;

  useEffect(() => {
    if (missing) router.replace("/dashboard/leaderboard");
  }, [missing, router]);

  if (missing || user.isPending) return <LoadingState className="p-6" />;

  return <UserInsightsView userId={userId} userName={user.data?.name ?? ""} />;
}
