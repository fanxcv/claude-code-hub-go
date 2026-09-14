"use client";

import { useQuery } from "@tanstack/react-query";
import { AlertCircle } from "lucide-react";
import { useTranslations } from "next-intl";
import { LoadingState } from "@/components/loading/page-skeletons";
import { Section } from "@/components/section";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { useUiSession } from "@/components/ui-session-gate";
import { Link } from "@/i18n/routing";
import { getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import { v1Keys } from "@/lib/api-client/v1/keys";
import { LeaderboardView } from "./_components/leaderboard-view";

/**
 * 成本排行榜页。改造前由 SSR 读 `getSession()` 与 `getSystemSettings()`；
 * 静态导出后无服务端，故改为壳注入会话 + REST 取系统设置。
 *
 * 权限口径不变：`admin || systemSettings.allowGlobalUsageView`。
 */
export default function LeaderboardPage() {
  const t = useTranslations("dashboard");
  const state = useUiSession();
  const settings = useQuery({
    queryKey: v1Keys.system.settings(),
    queryFn: () => getSystemSettings(),
  });

  if (state.status === "pending" || settings.isPending) {
    return <LoadingState className="p-6" />;
  }

  const isAdmin = state.status === "authenticated" && state.session.user.role === "admin";
  // 设置读取失败时按「无权限」处理：宁可显示受限提示，也不要在未确认授权时渲染全站成本数据。
  const hasPermission = isAdmin || settings.data?.allowGlobalUsageView === true;

  if (!hasPermission) {
    return (
      <div className="space-y-6">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">{t("title.costRanking")}</h1>
          <p className="mt-2 text-muted-foreground">{t("title.costRankingDescription")}</p>
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
                <AlertDescription>
                  {t("leaderboard.permission.description")}
                  {isAdmin && (
                    <span>
                      {" "}
                      <Link href="/settings/config" className="underline font-medium">
                        {t("leaderboard.permission.systemSettings")}
                      </Link>{" "}
                      {t("leaderboard.permission.adminAction")}
                    </span>
                  )}
                  {!isAdmin && <span> {t("leaderboard.permission.userAction")}</span>}
                </AlertDescription>
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
        <h1 className="text-3xl font-bold tracking-tight">{t("title.costRanking")}</h1>
        <p className="mt-2 text-muted-foreground">{t("title.costRankingDescription")}</p>
      </div>
      <Section>
        <LeaderboardView isAdmin={isAdmin} />
      </Section>
    </div>
  );
}
