"use client";

import { useTranslations } from "next-intl";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Link } from "@/i18n/routing";

/**
 * 配额页的标签容器。改造前用 `getTranslations`（服务端），静态导出下不可用，
 * 故改为客户端 `useTranslations`；文案与键位不变（`quota.layout.*`）。
 */
export default function QuotasLayout({ children }: { children: React.ReactNode }) {
  const t = useTranslations("quota.layout");

  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-3xl font-bold tracking-tight">{t("title")}</h2>
        <p className="text-muted-foreground">{t("description")}</p>
      </div>

      <Tabs defaultValue="users" className="space-y-4">
        <TabsList>
          <Link href="/dashboard/quotas/users">
            <TabsTrigger value="users">{t("tabs.users")}</TabsTrigger>
          </Link>
          <Link href="/dashboard/quotas/providers">
            <TabsTrigger value="providers">{t("tabs.providers")}</TabsTrigger>
          </Link>
        </TabsList>

        {children}
      </Tabs>
    </div>
  );
}
