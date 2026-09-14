"use client";

import { type ReactNode, useEffect } from "react";
import { resolveSessionGateTarget, useUiSession } from "@/components/ui-session-gate";
import { useRouter } from "@/i18n/routing";
import { DashboardHeader } from "../dashboard/_components/dashboard-header";
import { PageTransition } from "./_components/page-transition";
import { SettingsNav } from "./_components/settings-nav";
import { useTranslatedNavItems } from "./_lib/nav-items";

/**
 * settings 布局：静态导出后没有服务端会话，改用客户端判定（与 `dashboard/layout.tsx` 同一手法）。
 *
 * 逐条对齐被替换掉的服务端实现（原 `getSession()` + `redirect()`）：
 * - 未登录 → `/login`（Node 此处不带 `from`，照抄）；
 * - 已登录但非管理员 → `/dashboard`。
 *
 * 本布局也渲染站点头部，因此同样必须去掉服务端绑定：`getSession` / `getTranslatedNavItems`
 * 任一在场都会被导出脚本判为服务端绑定而把整个 layout（含头部菜单）移出产物。
 *
 * 会话未知（预渲染期无 bootstrap）时渲染匿名外壳而非骨架：理由同 `dashboard/layout.tsx`——
 * 若此时返回骨架，导出产物里既没有菜单也没有页面本体，静态壳就白做了。
 * 角色未知（壳注入缺失时的回退探针态）时不跳转。
 */
export default function SettingsLayout({ children }: { children: ReactNode }) {
  const state = useUiSession();
  const router = useRouter();
  const translatedNavItems = useTranslatedNavItems();

  const session = state.status === "authenticated" ? state.session : null;
  const target = resolveSessionGateTarget(state, {
    anonymousHref: "/login",
    forbiddenHref: "/dashboard",
    forbiddenRule: "admin",
  });

  useEffect(() => {
    if (target) router.replace(target);
  }, [router, target]);

  // 判定为真时首帧就不渲染受保护内容，避免「先画再跳」的闪烁。
  if (target) return null;

  return (
    <div className="min-h-[var(--cch-viewport-height,100vh)] bg-background">
      <DashboardHeader session={session} />
      <main className="mx-auto w-full max-w-[100rem] px-4 py-6 md:px-6 md:py-8 pb-24 md:pb-8">
        <div className="space-y-6">
          {/* Desktop: Grid layout with sidebar */}
          <div className="lg:grid lg:gap-6 lg:grid-cols-[220px_1fr]">
            {/* Desktop sidebar */}
            <aside className="hidden lg:block lg:sticky lg:top-24 lg:self-start">
              <SettingsNav items={translatedNavItems} />
            </aside>
            {/* Content area */}
            <div className="min-w-0 space-y-6">
              {/* Tablet: Horizontal nav shown above content */}
              <div className="lg:hidden">
                <SettingsNav items={translatedNavItems} />
              </div>
              <PageTransition>{children}</PageTransition>
            </div>
          </div>
        </div>
      </main>
    </div>
  );
}
