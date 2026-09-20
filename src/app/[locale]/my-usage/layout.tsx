"use client";

import { type ReactNode, useEffect } from "react";
import { LoadingState } from "@/components/loading/page-skeletons";
import { useUiSession } from "@/components/ui-session-gate";
import { usePathname, useRouter } from "@/i18n/routing";

/**
 * my-usage（普通用户控制台）布局：静态导出后没有服务端会话，改用客户端判定。
 *
 * 逐条对齐被删掉的服务端实现（原 `getSession({ allowReadOnlyAccess: true })`）：
 * - 未登录 → `/login?from=<当前路径>`；
 * - 已登录且是管理员、或密钥允许登录 Web UI → `/dashboard`（这两个角色不该看用户控制台）。
 *
 * 与 `UiSessionGate` 的差别在于**反向**判定（把管理员挡出去），而壳只支持「要求 admin」，
 * 故此处直接用 `useUiSession()` 自判，复用它的三态与落点语义。
 *
 * 角色未知（壳注入缺失时的回退探针态，`/api/v1/me/metadata` 不返回角色）时**不跳转**：
 * 此刻无从判断是否管理员，误跳会把真正的用户挡在门外；受保护数据仍由服务端端点把关（403）。
 */
export default function MyUsageLayout({ children }: { children: ReactNode }) {
  const state = useUiSession();
  const pathname = usePathname();
  const router = useRouter();

  const role = state.status === "authenticated" ? state.session.user.role : undefined;
  const canLoginWebUi =
    state.status === "authenticated" ? state.session.key?.canLoginWebUi : undefined;

  const target =
    state.status === "anonymous"
      ? `/login?from=${encodeURIComponent(pathname)}`
      : role === "admin" || canLoginWebUi === true
        ? "/dashboard"
        : null;

  useEffect(() => {
    if (target) router.replace(target);
  }, [router, target]);

  // 判定为真时首帧就不渲染受保护内容，避免"先画再跳"的闪烁。
  if (target) return null;
  if (state.status === "pending") return <LoadingState className="p-6" />;

  return (
    <div className="min-h-[var(--cch-viewport-height,100vh)] bg-background">
      <main className="mx-auto w-full max-w-[100rem] px-4 py-6 sm:px-6">{children}</main>
    </div>
  );
}
