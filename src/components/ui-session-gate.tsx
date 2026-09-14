"use client";

import { useQuery } from "@tanstack/react-query";
import { type ReactNode, useEffect } from "react";
import { LoadingState } from "@/components/loading/page-skeletons";
import { usePathname, useRouter } from "@/i18n/routing";
import { apiClient } from "@/lib/api-client/v1/client";
import { v1Keys } from "@/lib/api-client/v1/keys";

/**
 * 首屏引导数据的契约（Go 壳在 HTML 里注入 `window.__CCH_BOOTSTRAP__`）。
 *
 * 为什么要有它：静态导出后没有中间件也没有服务端 layout，若靠客户端 API 判断登录态，
 * 首帧必然先画受保护内容再被重定向（闪一下）。壳注入使判定在**首帧**即可完成。
 * 因此 `session` 字段的三态有明确语义：`undefined`（字段缺失）= 未注入，走回退探针；
 * `null` = 已注入且未登录。
 */
export type UiSessionSnapshot = {
  user: {
    id: number;
    name?: string;
    /** 回退探针（`/api/v1/me/metadata`）不返回角色，此时为 undefined：见 useUiSession 的说明。 */
    role?: "admin" | "user";
  };
  key?: {
    canLoginWebUi?: boolean;
  };
};

export type UiBootstrap = {
  session?: UiSessionSnapshot | null;
  siteTitle?: string;
  timeZone?: string;
  locale?: string;
};

declare global {
  interface Window {
    __CCH_BOOTSTRAP__?: UiBootstrap;
  }
}

export type UiSessionState =
  | { status: "pending" }
  | { status: "anonymous" }
  | { status: "authenticated"; session: UiSessionSnapshot };

type MeMetadataProbe = {
  userName?: string;
};

function readBootstrap(): UiBootstrap | null {
  if (typeof window === "undefined") return null;
  return window.__CCH_BOOTSTRAP__ ?? null;
}

/**
 * 会话状态。壳注入存在时以壳为唯一权威（同步可得，无请求）；未注入时回退到
 * `/api/v1/me/metadata` 探针——它只能证明「已登录」，拿不到角色，故回退态下 `role` 为
 * undefined，角色敏感的重定向由 `UiSessionGate` 明确跳过（见其注释）。
 *
 * 探针失败（含网络错误）一律按未登录处理：宁可跳登录页（那里会显示真实错误），
 * 也不要在未确认身份时渲染受保护内容。
 */
export function useUiSession(): UiSessionState {
  const bootstrap = readBootstrap();
  const bootstrapSession = bootstrap?.session ?? null;

  const probe = useQuery({
    queryKey: v1Keys.me.metadata(),
    queryFn: () => apiClient.get<MeMetadataProbe>("/api/v1/me/metadata"),
    enabled: bootstrap === null && typeof window !== "undefined",
    retry: false,
    staleTime: 60_000,
  });

  if (bootstrap !== null) {
    return bootstrapSession
      ? { status: "authenticated", session: bootstrapSession }
      : { status: "anonymous" };
  }

  if (probe.isSuccess) {
    return {
      status: "authenticated",
      session: { user: { id: 0, name: probe.data?.userName } },
    };
  }

  if (probe.isError) return { status: "anonymous" };

  return { status: "pending" };
}

export type SessionGateOptions =
  | {
      /** 未登录时的落点（含需要保留的回跳参数）。 */
      anonymousHref: string;
    }
  | {
      anonymousHref: string;
      /** 角色不符时的落点。 */
      forbiddenHref: string;
      /**
       * 何时算「角色不符」。两个控制台的门槛不同，不能合并成一个条件：
       * - `admin-or-webui`（dashboard）：非管理员**且**密钥不允许登录 Web UI 才退（只读密钥
       *   能进 my-usage 但进不了 dashboard）；
       * - `admin`（settings）：非管理员即退（系统设置只给管理员）。
       */
      forbiddenRule: "admin" | "admin-or-webui";
    };

/**
 * 由会话三态推出「应该跳到哪」。null 表示无需跳转。
 *
 * 抽成纯函数的理由：跳转语义是权限契约（哪些角色能进哪个控制台、未登录落点带什么参数），
 * 而它现在只能靠渲染组件来观察。各布局只需给出落点与门槛，判定本身共用一处。
 *
 * 三条规则（逐条对齐被替换掉的服务端实现）：
 * 1. `anonymous` → `anonymousHref`；
 * 2. `pending`（预渲染期无 bootstrap、或回退探针未回时）→ **不跳**：此刻无从判断，
 *    误跳会把真正的管理员踢出去；受保护数据仍由服务端端点把关（403）；
 * 3. `authenticated` 且角色已知且命中 `forbiddenRule` → `forbiddenHref`。
 *    角色未知（回退探针态不返回角色）时不跳，同上。
 */
export function resolveSessionGateTarget(
  state: UiSessionState,
  options: SessionGateOptions
): string | null {
  if (state.status === "anonymous") return options.anonymousHref;
  if (state.status !== "authenticated") return null;
  if (!("forbiddenHref" in options)) return null;

  const role = state.session.user.role;
  if (role === undefined) return null;

  const roleDenied =
    options.forbiddenRule === "admin"
      ? role !== "admin"
      : role !== "admin" && state.session.key?.canLoginWebUi !== true;

  return roleDenied ? options.forbiddenHref : null;
}

export type UiSessionGateProps = {
  children: ReactNode;
  /** 要求的最低角色。为 "admin" 且角色**已知**不符时跳 forbiddenHref。 */
  requireRole?: "admin";
  /** 角色不符时的落点，默认 /dashboard（与既有 SSR 页一致）。 */
  forbiddenHref?: string;
};

/**
 * 客户端鉴权壳：替代被删掉的 `getSession()` + `redirect()` 服务端判定。
 *
 * 落点语义（逐条对齐既有 SSR 页）：未登录 → `/login?from=<当前路径>`；角色不符 → forbiddenHref。
 *
 * 角色未知（回退探针态）时**不跳转**也不拦渲染：此刻无从判断，误跳会踢掉真正的管理员。
 * 受保护数据仍由服务端端点把关（403），页面自身的错误态即为此设计。壳注入上线后不存在这个态。
 */
export function UiSessionGate({
  children,
  requireRole,
  forbiddenHref = "/dashboard",
}: UiSessionGateProps) {
  const state = useUiSession();
  const pathname = usePathname();
  const router = useRouter();

  const roleUnknown =
    state.status === "authenticated" ? state.session.user.role === undefined : false;
  const roleDenied =
    state.status === "authenticated" &&
    requireRole === "admin" &&
    !roleUnknown &&
    state.session.user.role !== "admin";

  const target =
    state.status === "anonymous"
      ? `/login?from=${encodeURIComponent(pathname)}`
      : roleDenied
        ? forbiddenHref
        : null;

  useEffect(() => {
    if (target) router.replace(target);
  }, [router, target]);

  // 判定为真时首帧就不渲染任何受保护内容，避免"先画再跳"的闪烁。
  if (target) return null;
  if (state.status === "pending") return <LoadingState className="p-6" />;

  return <>{children}</>;
}
