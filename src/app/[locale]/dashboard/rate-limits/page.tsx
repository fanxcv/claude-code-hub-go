"use client";

import { useTranslations } from "next-intl";
import { Section } from "@/components/section";
import { UiSessionGate } from "@/components/ui-session-gate";
import { RateLimitDashboard } from "./_components/rate-limit-dashboard";

/**
 * 限流监控页。静态化改造后由客户端鉴权壳把关，取代原服务端的 `getSession()` + `redirect()`
 * （非管理员一律 `/dashboard`）。文案改用 `useTranslations`，与页面同处客户端。
 *
 * 原页用 `<Suspense>` 包 `RateLimitDashboard` 只为等其父级异步取数；该组件自身已有
 * 加载态（`Loader2` + `useEffect` 取数），故此处不再需要 Suspense 边界。
 */
export default function RateLimitsPage() {
  const t = useTranslations("dashboard.rateLimits");

  return (
    <UiSessionGate requireRole="admin">
      <div className="space-y-6">
        <Section title={t("title")} description={t("description")}>
          <RateLimitDashboard />
        </Section>
      </div>
    </UiSessionGate>
  );
}
