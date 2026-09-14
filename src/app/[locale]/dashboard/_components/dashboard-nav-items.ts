import type { UiSessionSnapshot } from "@/components/ui-session-gate";
import type { DashboardNavItem } from "./dashboard-nav";

/** 带管理员可见性标记的导航项：`adminOnly` 项仅对 `role === "admin"` 的会话可见。 */
export interface DashboardNavEntry extends DashboardNavItem {
  adminOnly?: boolean;
}

/** 只用到 `dashboard.nav` 命名空间下的键，故只要求「能给键取到文案」。 */
type Translate = (key: string) => string;

/**
 * 头部导航项与可见性判定。
 *
 * 抽成纯函数的原因：原实现是服务端组件（`getTranslations` + `await`），迁移到客户端后
 * 若把判定留在组件里，就只能靠渲染整棵树来测可见性；而可见性是**权限语义**，值得单独钉住。
 *
 * 逐条对齐被替换掉的服务端实现（`src/app/[locale]/dashboard/_layout 的 DashboardHeader`）：
 * - `adminOnly` 项（可用性、供应商、系统设置）仅管理员可见；
 * - 配额入口按角色二选一：管理员 `/dashboard/quotas`，其他人 `/dashboard/my-quota`。
 *
 * 只读会话（已登录但 `key.canLoginWebUi !== true` 且非管理员）**没有任何可见项**：它原本只有
 * 「文档」一项，而该入口已按要求移除，故此处有意返回空数组（不再回退到任何页）。
 */
export function buildDashboardNavItems(
  session: UiSessionSnapshot | null,
  t: Translate
): DashboardNavEntry[] {
  const isAdmin = session?.user.role === "admin";
  const canUseDashboard = !!session && (isAdmin || session.key?.canLoginWebUi === true);

  const navItems: DashboardNavEntry[] = [
    { href: "/dashboard", label: t("dashboard") },
    { href: "/dashboard/logs", label: t("usageLogs") },
    { href: "/dashboard/leaderboard", label: t("leaderboard") },
    { href: "/dashboard/availability", label: t("availability"), adminOnly: true },
    { href: "/dashboard/providers", label: t("providers"), adminOnly: true },
    isAdmin
      ? { href: "/dashboard/quotas", label: t("quotasManagement") }
      : { href: "/dashboard/my-quota", label: t("myQuota") },
    { href: "/dashboard/users", label: t("userManagement") },
    { href: "/settings", label: t("systemSettings"), adminOnly: true },
  ];

  if (session && !canUseDashboard) return [];
  return navItems.filter((item) => !item.adminOnly || isAdmin);
}
