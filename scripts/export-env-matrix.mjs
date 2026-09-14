#!/usr/bin/env node
/**
 * export-env-matrix 的 Node 包装入口。
 *
 * 真正的实现在 export-env-matrix.ts（需要 bun 才能直接执行 TS 与导入 zod schema）。
 * 本包装只是为了 `node scripts/export-env-matrix.mjs` 也能用；参数原样透传。
 */

import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const result = spawnSync("bun", ["run", join(here, "export-env-matrix.ts"), ...process.argv.slice(2)], {
  stdio: "inherit",
});

if (result.error) {
  console.error(`FAIL  无法启动 bun：${result.error.message}`);
  process.exit(2);
}
process.exit(result.status ?? 1);
