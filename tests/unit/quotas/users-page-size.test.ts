import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

/**
 * 前端索取的分页大小不得超过 REST 端点的 `limit` 上限。
 *
 * 真事故（2026-09-13，生产）：`dashboard/quotas/users` 静态化改造后按 SSR 时代的 200 取用户，
 * 而 `GET /users` 的 `limit` 上限是 **100**（Node `UserListQuerySchema` 与 Go
 * `parseUsersListQuery` 两侧一致：min 1 / max 100 / default 50）。
 * → 每个请求都被两后端同样拒为 `too_big`，整页取不到用户、近乎空白，
 * 而控制台只有一行 400，肉眼像「数据为空」而不是「参数超限」。
 *
 * 本钉子读**源码文本**（常量不导出）并断言，故是结构性发现：改回 200 会立刻红。
 */
const USERS_LIST_LIMIT_MAX = 100;

const PAGE = join(process.cwd(), "src/app/[locale]/dashboard/quotas/users/page.tsx");

function readUsersPageSize(): number {
  const source = readFileSync(PAGE, "utf8");
  const match = /const USERS_PAGE_SIZE\s*=\s*(\d+)\s*;/.exec(source);
  if (!match) throw new Error(`未在 ${PAGE} 找到 USERS_PAGE_SIZE：常量改名时请同步本钉子`);
  return Number(match[1]);
}

describe("配额页分页大小与端点上限一致", () => {
  it("USERS_PAGE_SIZE 不得超过 GET /users 的 limit 上限", () => {
    const size = readUsersPageSize();
    expect(Number.isFinite(size)).toBe(true);
    expect(size).toBeGreaterThan(0);
    // 超上限时这两个后端都会回 400 too_big，且失败形态是「整页无数据」——极难从界面反推。
    expect(size).toBeLessThanOrEqual(USERS_LIST_LIMIT_MAX);
  });

  it("钉子自身有效（能读到一个真实的正整数，不是解析失败的 0）", () => {
    const size = readUsersPageSize();
    expect(size).toBeGreaterThan(1);
  });
});
