/**
 * 用户管理页「列表占满剩余高度」与「操作后不整页重载」的源码结构性钉子。
 *
 * 为什么用源码断言而不是截图/浏览器量高：本仓前端测试跑在 happy-dom，它不做布局计算
 * （没有任何元素的 offsetHeight 有意义），量高断言会退化成「类名写错也绿」。而这两条
 * 判据的真身就是**类名串**——本仓既有的同类钉子即此手法（见
 * tests/unit/dashboard-logs-virtualized-special-settings-ui.test.tsx 读源码比对）。
 *
 * 三条判据：
 *  1. 用户列表的虚拟滚动容器必须挂在 flex 链上（`h-[600px] grow shrink-0`），
 *     不能只留固定高度、也不能改成 `min-h-[600px]`（height:auto 的滚动容器内禀高度等于
 *     虚拟列表总高，会把整个页面撑成上万 px）。
 *  2. 从页根到滚动容器的链上每一级都必须是 `grow shrink-0`（照使用记录页的既有范式），否则
 *     余量传不到滚动容器。
 *  3. 两条重置路径不得整页重载（`window.location.reload` 会清空模块级展开态与筛选态）。
 */

import fs from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";

const TABLE_SOURCE = path.join(
  process.cwd(),
  "src/app/[locale]/dashboard/_components/user/user-management-table.tsx"
);
const PAGE_SOURCE = path.join(
  process.cwd(),
  "src/app/[locale]/dashboard/users/users-page-client.tsx"
);
const DIALOG_SOURCE = path.join(
  process.cwd(),
  "src/app/[locale]/dashboard/_components/user/edit-user-dialog.tsx"
);

function readSource(file: string): string {
  return fs.readFileSync(file, "utf8");
}

describe("用户管理页列表高度链", () => {
  test("虚拟滚动容器挂在 flex 链上，且不是 min-h 形态", () => {
    const source = readSource(TABLE_SOURCE);
    expect(source).toContain('className={cn("h-[600px] grow shrink-0 overflow-y-auto")}');
    // 反面：`min-h-[600px]` 会让 height:auto 的滚动容器内禀高度等于虚拟列表总高。
    expect(source).not.toContain("min-h-[600px]");
  });

  test("从页根到滚动容器的每一级都带 grow shrink-0", () => {
    const page = readSource(PAGE_SOURCE);
    const table = readSource(TABLE_SOURCE);

    // 页根与表格块（两处）在 page 里。
    const pageChain = page.match(/flex grow shrink-0 flex-col/g) ?? [];
    expect(pageChain.length).toBeGreaterThanOrEqual(2);

    // 表格根、边框盒、横向滚动盒、最小宽盒：四处都在 table 里。
    const tableChain = table.match(/flex grow shrink-0 flex-col/g) ?? [];
    expect(tableChain.length).toBeGreaterThanOrEqual(3);
    expect(table).toContain('"flex grow shrink-0 flex-col",\n          "overflow-hidden"');
  });
});

describe("用户管理页操作后不整页重载", () => {
  test("重置限额的两条路径都不调用 window.location.reload", () => {
    const source = readSource(DIALOG_SOURCE);
    expect(source).not.toContain("window.location.reload");
    // 两条路径都应改为失效用户列表那条 query。
    const invalidations = source.match(/invalidateQueries\(\{ queryKey: \["users"\] \}\)/g) ?? [];
    expect(invalidations.length).toBeGreaterThanOrEqual(3);
  });
});
