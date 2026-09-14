"use client";

import { AlertCircle } from "lucide-react";
import { useTranslations } from "next-intl";
import { Suspense } from "react";
import { LoadingState } from "@/components/loading/page-skeletons";
import { Section } from "@/components/section";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useUiSession } from "@/components/ui-session-gate";
import { AvailabilityDashboard } from "./_components/availability-dashboard";
import { AvailabilityDashboardSkeleton } from "./_components/availability-skeleton";

/**
 * 可用性监控页。改造前由 SSR 的 `getSession()` + 内联权限卡片判定；静态导出后
 * 无服务端，故改用壳注入的会话（`useUiSession`）。判定口径不变：
 * **仅 admin 可见监控面板**，其余（含未登录）显示受限提示卡片。
 */
export default function AvailabilityPage() {
  const t = useTranslations("dashboard");
  const state = useUiSession();

  if (state.status === "pending") return <LoadingState className="p-6" />;

  const isAdmin = state.status === "authenticated" && state.session.user.role === "admin";

  if (!isAdmin) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">{t("availability.title")}</h1>
          <p className="mt-2 text-muted-foreground">{t("availability.description")}</p>
        </div>
        <Section>
          <Card>
            <CardHeader>
              <CardTitle className="flex items-center gap-2">
                <AlertCircle className="h-5 w-5 text-muted-foreground" />
                {t("leaderboard.permission.title")}
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-4">
              <Alert>
                <AlertCircle className="h-4 w-4" />
                <AlertTitle>{t("leaderboard.permission.restricted")}</AlertTitle>
                <AlertDescription>{t("leaderboard.permission.userAction")}</AlertDescription>
              </Alert>
            </CardContent>
          </Card>
        </Section>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-3xl font-bold tracking-tight">{t("availability.title")}</h1>
        <p className="mt-2 text-muted-foreground">{t("availability.description")}</p>
      </div>
      <Section>
        <Suspense fallback={<AvailabilityDashboardSkeleton />}>
          <AvailabilityDashboard />
        </Suspense>
      </Section>
    </div>
  );
}
