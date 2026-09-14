"use client";

import { useTranslations } from "next-intl";
import { UiSessionGate } from "@/components/ui-session-gate";
import { AuditLogsView } from "./_components/audit-logs-view";

export default function AuditLogsPage() {
  const t = useTranslations("auditLogs");

  return (
    <UiSessionGate requireRole="admin">
      <div className="space-y-6">
        <div>
          <h1 className="text-3xl font-bold tracking-tight">{t("title")}</h1>
          <p className="mt-2 text-muted-foreground">{t("description")}</p>
        </div>
        <AuditLogsView />
      </div>
    </UiSessionGate>
  );
}
