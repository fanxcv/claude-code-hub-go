import type { User } from "@/types/user";

/**
 * 供应商管理组件树对「当前用户」的唯一需求面。
 *
 * 实测全目录只有两处读取：`role`（决定管理操作可见性）与 `providerGroup`
 * （决定非管理员可见的供应商分组）。故这里按结构收窄，而不是要求完整 `User`。
 *
 * 为何收窄：静态导出后没有服务端 `getSession()`，只有 Go 壳注入的会话
 * （`window.__CCH_BOOTSTRAP__.session`，含 role、不含 providerGroup）。
 * 若坚持 `User`，调用方就得靠猜造出 description/createdAt 等字段——那是在伪造数据。
 * 真实取值走 `GET /api/v1/users:self`（Node 与 Go 同形）。
 */
export type ProviderViewer = Pick<User, "role" | "providerGroup">;

/**
 * 由「壳注入会话的 role」与「REST 补来的 providerGroup」合成组件树要的 viewer。
 *
 * 为什么单列一个函数：这里有一条**踩过坑的不变量**——`role` 必须来自会话，不能来自 REST。
 *
 * - Node 版是 SSR：`getSession()` 直接给出合成管理员（`id:-1, role:"admin"`，见 `src/lib/auth.ts`），
 *   所以用管理令牌（ADMIN_TOKEN）登录时，组件树拿到的是 `role:"admin"`，操作按钮可见。
 * - 静态导出后没有服务端会话，若把 `role` 也押在 `GET /api/v1/users:self` 上，则管理令牌身份下
 *   该端点**必然 404**（库里没有 `id=-1` 的行；Node 走同一路径也会 404），于是 viewer 为空、
 *   `isAdmin` 为假、**供应商页所有操作按钮静默消失**（2026-09-13 实测踩到）。
 *
 * 故：`role` 取会话（唯一可靠来源），REST 只用来补会话里没有的 `providerGroup`；
 * 会话没给出 role 时返回 `undefined`（宁可不给，也不臆造普通用户）。
 */
export function deriveProviderViewer(
  sessionRole: "admin" | "user" | undefined | null,
  restUser: { providerGroup?: string | null } | null | undefined
): ProviderViewer | undefined {
  if (!sessionRole) return undefined;
  return { role: sessionRole, providerGroup: restUser?.providerGroup ?? null };
}
