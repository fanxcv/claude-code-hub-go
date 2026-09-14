"use client";

import { useQuery } from "@tanstack/react-query";
import { useEffect } from "react";
import { UiSessionGate, useUiSession } from "@/components/ui-session-gate";
import { useRouter } from "@/i18n/routing";
import { hasPriceTable } from "@/lib/api-client/v1/actions/model-prices";
import { DashboardBentoSection } from "./_components/dashboard-bento-sections";
import { DashboardOverviewSkeleton } from "./_components/dashboard-skeletons";

export default function DashboardPage() {
  return (
    <UiSessionGate>
      <DashboardHome />
    </UiSessionGate>
  );
}

function DashboardHome() {
  const router = useRouter();
  const session = useUiSession();
  const isAdmin = session.status === "authenticated" && session.session.user.role === "admin";

  // 改造前这是服务端 `hasPriceTable()` + `redirect()`；改为客户端探测后语义不变：
  // 未建价格表即把用户送去建表页（`required=true` 触发该页的提示）。
  const { data: hasPrices, isLoading } = useQuery({
    queryKey: ["model-prices", "exists"],
    queryFn: hasPriceTable,
  });

  useEffect(() => {
    if (hasPrices === false) router.replace("/settings/prices?required=true");
  }, [hasPrices, router]);

  if (isLoading || hasPrices === false) return <DashboardOverviewSkeleton />;

  return <DashboardBentoSection isAdmin={isAdmin} />;
}
