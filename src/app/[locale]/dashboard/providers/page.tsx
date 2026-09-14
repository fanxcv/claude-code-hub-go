"use client";

import { useQuery } from "@tanstack/react-query";
import { BarChart3 } from "lucide-react";
import { useTranslations } from "next-intl";
import { AutoSortPriorityDialog } from "@/app/[locale]/settings/providers/_components/auto-sort-priority-dialog";
import { DispatchSimulatorDialog } from "@/app/[locale]/settings/providers/_components/dispatch-simulator-dialog";
import { ProviderManagerLoader } from "@/app/[locale]/settings/providers/_components/provider-manager-loader";
import {
  deriveProviderViewer,
  type ProviderViewer,
} from "@/app/[locale]/settings/providers/_components/provider-viewer";
import { ReclusterVendorsDialog } from "@/app/[locale]/settings/providers/_components/recluster-vendors-dialog";
import { SchedulingRulesDialog } from "@/app/[locale]/settings/providers/_components/scheduling-rules-dialog";
import { Section } from "@/components/section";
import { Button } from "@/components/ui/button";
import { UiSessionGate, useUiSession } from "@/components/ui-session-gate";
import { Link } from "@/i18n/routing";
import { getProviders } from "@/lib/api-client/v1/actions/providers";
import { getCurrentUser } from "@/lib/api-client/v1/actions/users";
import type { ProviderDisplay } from "@/types/provider";

/**
 * 供应商管理页（dashboard 入口）。改造前是 SSR：`getSession()` 判 admin + `getProviders()` 取列表。
 * 静态导出后无服务端，故改为：壳注入会话做门禁（`UiSessionGate`），数据走 REST。
 *
 * 门禁口径与 SSR 版一致：非 admin 已登录 → /dashboard，未登录 → /login。
 */
export default function DashboardProvidersPage() {
  return (
    <UiSessionGate requireRole="admin" forbiddenHref="/dashboard">
      <ProvidersContent />
    </UiSessionGate>
  );
}

function ProvidersContent() {
  const t = useTranslations("settings");
  // 与 ProviderManagerLoader 同一个 queryKey：两者共用一份缓存，不重复请求。
  const providers = useQuery<ProviderDisplay[]>({
    queryKey: ["providers"],
    queryFn: getProviders,
    staleTime: 30_000,
  });
  const viewer = useQuery({
    queryKey: ["v1", "users", "self"],
    queryFn: getCurrentUser,
    staleTime: 60_000,
    // 管理令牌（ADMIN_TOKEN）身份下 `/users:self` 必然 404（合成身份 id:-1，库里无此行，
    // Node 同路径亦然）——预期行为，不重试。
    retry: false,
  });

  // 组件树只读 role/providerGroup（见 provider-viewer.ts）。**role 取壳注入会话**：
  // Node 版 SSR 直接拿 `getSession()` 的合成管理员，静态版若从 REST 推 role，用管理令牌
  // 登录时 `/users:self` 404 → currentUser 为空 → 操作按钮静默消失。REST 只补 providerGroup。
  const session = useUiSession();
  const sessionRole = session.status === "authenticated" ? session.session.user.role : undefined;
  // 不变量与踩坑记录见 deriveProviderViewer 的注释：role 只认会话。
  const currentUser: ProviderViewer | undefined = deriveProviderViewer(sessionRole, viewer.data);

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-3xl font-bold tracking-tight">{t("providers.title")}</h1>
        <p className="mt-2 text-muted-foreground">{t("providers.description")}</p>
      </div>

      <Section
        title={t("providers.section.title")}
        description={t("providers.section.description")}
        actions={
          <>
            <Button asChild variant="outline">
              <Link href="/dashboard/leaderboard?scope=provider">
                <BarChart3 className="h-4 w-4" />
                {t("providers.section.leaderboard")}
              </Link>
            </Button>
            <AutoSortPriorityDialog />
            <ReclusterVendorsDialog />
            <SchedulingRulesDialog />
            {/* 该对话框需要供应商列表做选项：加载完成才渲染，避免把空列表当既成事实画出来。 */}
            {providers.data ? <DispatchSimulatorDialog providers={providers.data} /> : null}
          </>
        }
      >
        <ProviderManagerLoader currentUser={currentUser} />
      </Section>
    </div>
  );
}
