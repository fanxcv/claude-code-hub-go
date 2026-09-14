#!/usr/bin/env node
/**
 * 环境变量契约对账：比较 Node 侧矩阵与 Go 侧清单。
 *
 * 用途：Go 侧 config 装载器落地后，用本脚本回答两个问题：
 *   1) 矩阵里有、Go 清单里没有的变量（Go 漏配 → 行为静默不同）
 *   2) Go 清单里有、矩阵里没有的变量（Go 多配 → 拼写错误或 Node 侧已删除）
 *
 * 用法：
 *   node scripts/check-env-parity.mjs                 # 默认读 tests/load/env-parity/env-matrix.json 与 go/env-parity.txt
 *   node scripts/check-env-parity.mjs --strict        # 有差异时以退出码 1 结束（默认 0）
 *   node scripts/check-env-parity.mjs --matrix=X --go-list=Y
 *
 * 约定：Go 侧清单每行一个变量名，`#` 开头为注释，空行忽略。
 * 若 Go 侧清单尚未提供，本脚本明确提示并以退出码 0 结束，不视为失败。
 */

import { readFileSync, existsSync } from "node:fs";
import { join, dirname, isAbsolute } from "node:path";
import { fileURLToPath } from "node:url";

const REPO_ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");

function argValue(name, fallback) {
  const hit = process.argv.slice(2).find((arg) => arg.startsWith(`--${name}=`));
  return hit ? hit.split("=").slice(1).join("=") : fallback;
}

/** 绝对路径原样使用，相对路径相对仓库根解析。 */
function resolvePath(value) {
  return isAbsolute(value) ? value : join(REPO_ROOT, value);
}

function parseGoList(text) {
  const names = [];
  const duplicates = [];
  for (const rawLine of text.split("\n")) {
    const line = rawLine.trim();
    if (!line || line.startsWith("#")) continue;
    const name = line.split(/\s+/)[0];
    if (names.includes(name)) duplicates.push(name);
    else names.push(name);
  }
  return { names, duplicates };
}

function main() {
  const strict = process.argv.includes("--strict");
  const matrixPath = resolvePath(argValue("matrix", "tests/load/env-parity/env-matrix.json"));
  const goListPath = resolvePath(argValue("go-list", "go/env-parity.txt"));

  if (!existsSync(matrixPath)) {
    console.error(`FAIL  找不到矩阵文件 ${matrixPath}；先运行 bun scripts/export-env-matrix.ts`);
    process.exit(2);
  }
  const matrix = JSON.parse(readFileSync(matrixPath, "utf8"));
  const matrixNames = matrix.variables.map((variable) => variable.name).sort();

  console.log(`矩阵：${matrixPath}`);
  console.log(`  变量总数 ${matrixNames.length}（源 ${matrix.source}，sha256 ${matrix.sourceSha256.slice(0, 12)}…）`);

  if (!existsSync(goListPath)) {
    console.log(`Go 侧清单尚未提供：${goListPath} 不存在。`);
    console.log(
      "  约定：每行一个变量名，`#` 开头为注释。生成后重跑本脚本即可得到差异清单。"
    );
    process.exit(0);
  }

  const { names: goNames, duplicates } = parseGoList(readFileSync(goListPath, "utf8"));
  console.log(`Go 清单：${goListPath}`);
  console.log(`  变量总数 ${goNames.length}`);

  const missingInGo = matrixNames.filter((name) => !goNames.includes(name));
  const extraInGo = [...goNames].sort().filter((name) => !matrixNames.includes(name));

  const printList = (title, list) => {
    console.log(`\n${title}（${list.length}）`);
    if (list.length === 0) {
      console.log("  无");
      return;
    }
    for (const name of list) console.log(`  - ${name}`);
  };

  printList("矩阵有而 Go 清单缺（Go 漏配）", missingInGo);
  printList("Go 清单有而矩阵没有（多配或拼写错误）", extraInGo);
  if (duplicates.length > 0) {
    printList("Go 清单内重复行", [...new Set(duplicates)]);
  }

  const differences = missingInGo.length + extraInGo.length + duplicates.length;
  console.log(
    `\n${differences === 0 ? "PASS  两侧一致" : `FAIL  共 ${differences} 项差异`}${
      differences > 0 && !strict ? "（未加 --strict，退出码仍为 0）" : ""
    }`
  );
  if (strict && differences > 0) process.exit(1);
}

main();
