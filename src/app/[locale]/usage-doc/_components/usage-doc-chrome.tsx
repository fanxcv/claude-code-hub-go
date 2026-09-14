"use client";

import { Book, LogIn } from "lucide-react";
import { useTranslations } from "next-intl";
import { useUiSession } from "@/components/ui-session-gate";
import { Link } from "@/i18n/routing";
import { DashboardHeader } from "../../dashboard/_components/dashboard-header";
import { UsageDocAuthProvider } from "./usage-doc-auth-context";

/**
 * 文档页面的外壳（容器、样式、共用头部、登录态上下文）。
 *
 * 为什么拆成客户端组件：`layout.tsx` 需要保留 `generateMetadata`（文档页的 title/description
 * 是 Node 版就有、并会被静态导出写进各页 `<head>` 的行为），而 `generateMetadata` 只能由
 * 服务端组件导出。于是分工是「服务端布局管 metadata → 客户端外壳管首帧会话与头部」：
 * 布局不引用任何被导出脚本判为服务端绑定的模块，外壳则用 `__CCH_BOOTSTRAP__` 同步取会话。
 *
 * 逐条对齐被替换掉的服务端实现（原 `getSession({ allowReadOnlyAccess: true })`）：
 * 只读会话（`canLoginWebUi=false`）**也算已登录**——文档页对它们开放，只是「回到控制台」
 * 的快捷入口会死在登录页，故 `canUseDashboard` 用与 `DashboardHeader` 同一判据。
 */
export function UsageDocChrome({ children }: { children: React.ReactNode }) {
  const t = useTranslations("usage");
  const state = useUiSession();
  const session = state.status === "authenticated" ? state.session : null;

  // U06: read-only sessions (canLoginWebUi=false) can open the docs but cannot
  // use the dashboard, so the "Back to Dashboard" quick link would dead-end at
  // the login form. Gate it on the same predicate as DashboardHeader.
  const canUseDashboard =
    !!session && (session.user.role === "admin" || session.key?.canLoginWebUi === true);

  return (
    <div className="min-h-[var(--cch-viewport-height,100vh)] bg-background">
      {/* 条件渲染头部：已登录显示 DashboardHeader，未登录显示简化版头部 */}
      {session ? (
        <DashboardHeader session={session} />
      ) : (
        <header className="sticky top-0 z-50 w-full border-b bg-background/95 backdrop-blur supports-[backdrop-filter]:bg-background/60">
          <div className="container flex h-14 items-center justify-between px-6">
            <div className="flex items-center gap-2">
              <Book className="h-5 w-5 text-orange-500" />
              <span className="font-semibold">{t("layout.headerTitle")}</span>
            </div>
            <Link
              href="/login?from=/usage-doc"
              className="inline-flex items-center gap-2 rounded-md bg-orange-500 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-orange-600"
            >
              <LogIn className="h-4 w-4" />
              {t("layout.loginConsole")}
            </Link>
          </div>
        </header>
      )}

      <main className="mx-auto w-full max-w-[100rem] px-6 py-8">
        <UsageDocAuthProvider isLoggedIn={canUseDashboard}>{children}</UsageDocAuthProvider>
      </main>
    </div>
  );
}
