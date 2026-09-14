"use client";

import { AlertCircle } from "lucide-react";
import { useTranslations } from "next-intl";
import { UiSessionGate } from "@/components/ui-session-gate";

/**
 * 内部数据生成工具页（非生产面）——**该工具已随 Node 后端退役下线**。
 *
 * 端到端事实：本页唯一的调用是 `POST /api/internal/data-gen`，它由 Node 侧实现；
 * 用户已裁决该端点在 Node 退役后不再提供（理由：它直接写入生产数据库，收益远低于风险，
 * 详见 的 G-2）。
 * 为什么保留路由而不是删掉页面：该页不在任何导航项里，只可能被抓取的直链或书签命中。
 * 保留一个**明确说明已下线**的页面，比让直链落到 404 更好排查（前者是「已知退役」，
 * 后者会被误读成「路由坏了」）。原 606 行的生成器客户端组件已随之删除，不留死代码。
 *
 * 鉴权壳保留：内部工具对非管理员不透露存在性，落点与旧实现一致（一律送登录页）。
 */
export default function Page() {
  const t = useTranslations("internal.dataGenerator.retired");

  return (
    <UiSessionGate requireRole="admin" forbiddenHref="/login">
      <div className="p-6">
        <div
          data-slot="retired-capability"
          className="flex items-start gap-3 rounded-lg border border-amber-300/60 bg-amber-50/60 p-4 text-sm dark:border-amber-900/60 dark:bg-amber-950/30"
        >
          <AlertCircle
            className="mt-0.5 h-4 w-4 shrink-0 text-amber-600 dark:text-amber-400"
            aria-hidden="true"
          />
          <div className="space-y-1">
            <p className="font-medium text-amber-900 dark:text-amber-200">{t("title")}</p>
            <p className="text-muted-foreground">{t("description")}</p>
          </div>
        </div>
      </div>
    </UiSessionGate>
  );
}
