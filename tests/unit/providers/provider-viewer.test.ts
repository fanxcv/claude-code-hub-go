import { describe, expect, test } from "vitest";
import { deriveProviderViewer } from "@/app/[locale]/settings/providers/_components/provider-viewer";

/**
 * 回归钉子：供应商页操作按钮的可见性取决于 `currentUser.role === "admin"`。
 *
 * 2026-09-13 线上实测的缺陷：静态导出版曾把 role 也押在 `GET /api/v1/users:self` 上，
 * 而管理令牌（ADMIN_TOKEN）身份下该端点**必然 404**（合成身份 `id:-1`，库里无对应行；
 * Node 走同一路径也会 404）。于是 viewer 为空 → `isAdmin` 为假 → **供应商列表里的
 * 操作按钮全部静默消失**。
 *
 * 正确口径（Node SSR 版的等价语义）：role 来自会话——Node 用 `getSession()` 拿合成管理员，
 * 静态版用 Go 壳注入的 `__CCH_BOOTSTRAP__.session`。REST 只用来补 providerGroup。
 */
describe("deriveProviderViewer：role 只认会话", () => {
  test("会话是 admin 且 REST 404（restUser 为空）时仍给出 admin —— 按钮必须可见", () => {
    expect(deriveProviderViewer("admin", null)).toEqual({ role: "admin", providerGroup: null });
    expect(deriveProviderViewer("admin", undefined)).toEqual({
      role: "admin",
      providerGroup: null,
    });
  });

  test("会话是普通用户时不被 REST 提升为 admin", () => {
    expect(deriveProviderViewer("user", { providerGroup: "g1" })).toEqual({
      role: "user",
      providerGroup: "g1",
    });
  });

  test("会话没给出 role 时不臆造身份（返回 undefined）", () => {
    expect(deriveProviderViewer(undefined, { providerGroup: "g1" })).toBeUndefined();
    expect(deriveProviderViewer(null, null)).toBeUndefined();
  });

  test("providerGroup 优先取 REST，缺失时为 null（与 Node 合成管理员的 providerGroup:null 同形）", () => {
    expect(deriveProviderViewer("admin", {})?.providerGroup).toBeNull();
    expect(deriveProviderViewer("admin", { providerGroup: undefined })?.providerGroup).toBeNull();
    expect(deriveProviderViewer("user", { providerGroup: "CC-Paid" })?.providerGroup).toBe(
      "CC-Paid"
    );
  });
});
