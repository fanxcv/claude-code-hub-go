"use client";

import { useTranslations } from "next-intl";
import { Button } from "@/components/ui/button";
import { LanguageSwitcher } from "@/components/ui/language-switcher";
import { ThemeSwitcher } from "@/components/ui/theme-switcher";
import type { UiSessionSnapshot } from "@/components/ui-session-gate";
import { Link } from "@/i18n/routing";
import { DashboardNav } from "./dashboard-nav";
import { buildDashboardNavItems } from "./dashboard-nav-items";
import { MobileNav } from "./mobile-nav";
import { UserMenu } from "./user-menu";

interface DashboardHeaderProps {
  session: UiSessionSnapshot | null;
}

/**
 * 站点头部（导航菜单 + 语言/主题切换 + 用户菜单）。
 *
 * 为什么是客户端组件：它是 `[locale]/layout` 之外**唯一**渲染全站菜单的地方，而静态导出
 * 没有服务端会话——原实现用 `getTranslations`/`AuthSession` 走服务端，导出时整个
 * `dashboard/layout.tsx` 会被 `scripts/build-ui-export.mjs` 判为服务端绑定而移出，
 * 于是产物里**一个菜单都没有**（实测 `out/zh-CN/dashboard/index.html` 的 `<nav` 命中数为 0）。
 * 改为客户端后，文案取自 `I18nProvider` 的词表（全站一份共享 chunk），会话取自
 * `__CCH_BOOTSTRAP__`（Go 壳注入），菜单在首帧即可渲染。
 */
export function DashboardHeader({ session }: DashboardHeaderProps) {
  const t = useTranslations("dashboard.nav");
  const items = buildDashboardNavItems(session, t);

  return (
    <header className="sticky top-0 z-40 border-b border-border/80 bg-card/80 backdrop-blur supports-[backdrop-filter]:bg-card/60">
      <div className="mx-auto flex h-16 w-full max-w-[100rem] items-center justify-between px-6">
        <div className="flex items-center gap-4">
          <MobileNav items={items} />
          <DashboardNav items={items} />
        </div>
        <div className="flex items-center gap-3">
          <ThemeSwitcher />
          <LanguageSwitcher size="sm" />
          {session ? (
            <UserMenu user={{ id: session.user.id, name: session.user.name ?? "" }} />
          ) : (
            <Button asChild size="sm" variant="outline">
              <Link href="/login">{t("login")}</Link>
            </Button>
          )}
        </div>
      </div>
    </header>
  );
}
