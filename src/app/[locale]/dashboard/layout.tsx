"use client";

import { type ReactNode, useEffect } from "react";
import { resolveSessionGateTarget, useUiSession } from "@/components/ui-session-gate";
import { useRouter } from "@/i18n/routing";
import { DashboardHeader } from "./_components/dashboard-header";
import { DashboardMain } from "./_components/dashboard-main";
import { WebhookMigrationDialog } from "./_components/webhook-migration-dialog";

/**
 * dashboard 布局：静态导出后没有服务端会话，改用客户端判定（与 `my-usage/layout.tsx` 同一手法）。
 *
 * 逐条对齐被替换掉的服务端实现（原 `getSession()` + `redirect()`）：
 * - 未登录 → `/login?from=/dashboard`（Node 是固定值，非当前路径；此处照抄以保持回跳落点一致）；
 * - 已登录但既非管理员、密钥也不允许登录 Web UI → `/my-usage`。
 *
 * 为什么必须去掉服务端绑定：`scripts/build-ui-export.mjs` 会把含 `getSession` 的 page/layout
 * 移出导出，而本布局是**全站菜单的宿主**——一被移出，产物里所有页面都没有导航。
 *
 * 为何「会话未知」时不渲染骨架而是渲染匿名外壳：预渲染期没有 `window.__CCH_BOOTSTRAP__`，
 * `useUiSession()` 必然停在 `pending`——若此时返回骨架，导出产物里既没有菜单也没有页面本体
 * （实测 `out/zh-CN/dashboard/index.html` 只有一段 `animate-pulse`）。静态壳的意义就在于
 * 「首帧就有可点的菜单」；会话判定交给客户端：Go 壳在 HTML 里注入 bootstrap 后，首次渲染
 * 即为真实会话，菜单随之补全。
 *
 * 角色未知（壳注入缺失时的回退探针态）时不跳转：此刻无从判断，误跳会把真正的管理员踢出去；
 * 受保护数据仍由服务端端点把关（403）。
 */
export default function DashboardLayout({ children }: { children: ReactNode }) {
  const state = useUiSession();
  const router = useRouter();

  const session = state.status === "authenticated" ? state.session : null;
  const target = resolveSessionGateTarget(state, {
    anonymousHref: "/login?from=/dashboard",
    forbiddenHref: "/my-usage",
    forbiddenRule: "admin-or-webui",
  });

  useEffect(() => {
    if (target) router.replace(target);
  }, [router, target]);

  // 判定为真时首帧就不渲染受保护内容，避免「先画再跳」的闪烁。
  if (target) return null;

  return (
    <div className="min-h-[var(--cch-viewport-height,100vh)] bg-background">
      <DashboardHeader session={session} />
      <DashboardMain>{children}</DashboardMain>
      <WebhookMigrationDialog />
    </div>
  );
}
