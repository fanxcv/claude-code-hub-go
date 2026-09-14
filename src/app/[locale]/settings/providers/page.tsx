"use client";

import { useQuery } from "@tanstack/react-query";
import { BarChart3 } from "lucide-react";
import { useTranslations } from "next-intl";
import { Section } from "@/components/section";
import { Button } from "@/components/ui/button";
import { UiSessionGate, useUiSession } from "@/components/ui-session-gate";
import { Link } from "@/i18n/routing";
import { getProviders } from "@/lib/api-client/v1/actions/providers";
import { getCurrentUser } from "@/lib/api-client/v1/actions/users";
import type { ProviderDisplay } from "@/types/provider";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { AutoSortPriorityDialog } from "./_components/auto-sort-priority-dialog";
import { DispatchSimulatorDialog } from "./_components/dispatch-simulator-dialog";
import { ProviderManagerLoader } from "./_components/provider-manager-loader";
import { deriveProviderViewer, type ProviderViewer } from "./_components/provider-viewer";
import { ReclusterVendorsDialog } from "./_components/recluster-vendors-dialog";
import { SchedulingRulesDialog } from "./_components/scheduling-rules-dialog";

/**
 * 设置区的供应商管理页。改造前是 SSR（`getSession()` + `getProviders()`）。
 *
 * 注意：`settings/layout.tsx` 本身也要求 admin（非 admin 跳 /dashboard），但它仍是服务端绑定文件
 * （不在本 lane 的文件归属内），故这里**自带同一道门禁**：既与 SSR 语义一致，也让本页在
 * 布局被导出流程移出时不至于对任何登录用户敞开。
 */
export default function SettingsProvidersPage() {
  return (
    <UiSessionGate requireRole="admin" forbiddenHref="/dashboard">
      <ProvidersSettingsContent />
    </UiSessionGate>
  );
}

function ProvidersSettingsContent() {
  const t = useTranslations("settings");
  const providers = useQuery<ProviderDisplay[]>({
    queryKey: ["providers"],
    queryFn: getProviders,
    staleTime: 30_000,
  });
  const viewer = useQuery({
    queryKey: ["v1", "users", "self"],
    queryFn: getCurrentUser,
    staleTime: 60_000,
    // 管理令牌（ADMIN_TOKEN）身份下 `/users:self` 必然 404：它在 Node 侧同样是合成身份
    // （`id: -1`，`src/lib/auth.ts`），库里没有对应行。这是**预期**而非故障，故不重试。
    retry: false,
  });

  // role 取**壳注入会话**（Go 写入 `__CCH_BOOTSTRAP__.session`，含 role），不取 REST：
  // Node 版是 SSR，`getSession()` 直接给出合成管理员（id:-1, role:admin），故按钮可见；
  // 静态版若把 role 也押在 `/users:self` 上，用管理令牌登录时该端点 404 → currentUser 为空
  // → 供应商页所有操作按钮静默消失（实测踩到）。REST 只用来补 providerGroup（会话里没有）。
  const session = useUiSession();
  const sessionRole = session.status === "authenticated" ? session.session.user.role : undefined;
  // 不变量与踩坑记录见 deriveProviderViewer 的注释：role 只认会话。
  const currentUser: ProviderViewer | undefined = deriveProviderViewer(sessionRole, viewer.data);

  return (
    <>
      <SettingsPageHeader title={t("providers.title")} description={t("providers.description")} />

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
            {providers.data ? <DispatchSimulatorDialog providers={providers.data} /> : null}
          </>
        }
      >
        <ProviderManagerLoader currentUser={currentUser} />
      </Section>
    </>
  );
}
