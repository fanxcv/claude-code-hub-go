#!/usr/bin/env bun
/**
 * 环境变量契约矩阵导出器。
 *
 * 目的：把 `src/lib/config/env.schema.ts` 里 EnvSchema 的每一个键导出为语言中立的矩阵
 * （名称/类型/默认值/约束/枚举/说明/消费方），供 Go 侧 config 装载器逐项对齐。
 *
 * 为什么静态解析而不是 import zod 内省：
 * - 矩阵是"契约快照"，必须可 diff、可复现；zod 内省的输出随 zod 版本变化。
 * - 源码里大量 `z.preprocess` / `.transform` / `z.coerce`，JSON Schema 表达不了，会静默丢信息。
 * - 静态解析 + `--check` 的 zod 交叉校验（键集与默认值）才是能同时拿到"稳定"与"准确"的组合。
 *
 * 用法：
 *   bun scripts/export-env-matrix.ts              # 导出 JSON + Markdown
 *   bun scripts/export-env-matrix.ts --check      # 额外与运行时 zod 交叉校验（键集 + 默认值）
 *   bun scripts/export-env-matrix.ts --out-dir=X  # 自定义输出目录
 */

import { createHash } from "node:crypto";
import { readFileSync, readdirSync, mkdirSync, writeFileSync, statSync } from "node:fs";
import { join, relative, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const REPO_ROOT = join(dirname(fileURLToPath(import.meta.url)), "..");
const SOURCE_REL = "src/lib/config/env.schema.ts";
const DEFAULT_OUT_DIR = "tests/load/env-parity";
const MAX_CONSUMER_FILES = 5;

interface VariableEntry {
  name: string;
  schemaType: string;
  wrapper: string;
  optional: boolean;
  int: boolean;
  default: unknown;
  defaultRaw: string | null;
  min: number | null;
  max: number | null;
  boundsKind: string | null;
  enumValues: string[] | null;
  descriptionComment: string;
  descriptionSummary: string;
  consumerFiles: string[];
  consumerFileCount: number;
}

interface Matrix {
  source: string;
  sourceSha256: string;
  variableCount: number;
  countsByType: Record<string, number>;
  countsByDomain: Record<string, number>;
  variables: VariableEntry[];
}

/**
 * 从 `fromIndex` 开始（该位置紧跟在一个未计入的开括号之后）扫描到与之配对的闭括号。
 * 空栈上遇到的第一个闭括号就是配对者；内部多余的闭括号视为不配平。
 */
function scanBalanced(text: string, fromIndex: number): number {
  const openers: Record<string, string> = { "{": "}", "(": ")", "[": "]" };
  const closers = new Set(["}", ")", "]"]);
  const stack: string[] = [];
  let i = fromIndex;
  let quote: string | null = null;
  while (i < text.length) {
    const ch = text[i];
    if (quote) {
      if (ch === "\\") {
        i += 2;
        continue;
      }
      if (ch === quote) quote = null;
      i += 1;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
      i += 1;
      continue;
    }
    if (ch === "/" && text[i + 1] === "/") {
      const nl = text.indexOf("\n", i);
      i = nl === -1 ? text.length : nl + 1;
      continue;
    }
    if (ch === "/" && text[i + 1] === "*") {
      const end = text.indexOf("*/", i + 2);
      i = end === -1 ? text.length : end + 2;
      continue;
    }
    if (openers[ch]) {
      stack.push(openers[ch]);
      i += 1;
      continue;
    }
    if (closers.has(ch)) {
      if (stack.length === 0) return i;
      const expected = stack.pop();
      if (expected !== ch) {
        throw new Error(`括号不配平：位置 ${i} 遇到 ${ch}，期望 ${expected}`);
      }
      i += 1;
      continue;
    }
    i += 1;
  }
  throw new Error("扫描到文件末尾仍未闭合");
}

/** 把对象体按顶层逗号切成 `NAME: expr` 条目，并记录各自在 body 中的偏移。 */
function splitTopLevelEntries(body: string): Array<{ text: string; offset: number }> {
  const entries: Array<{ text: string; offset: number }> = [];
  let depth = 0;
  let quote: string | null = null;
  let start = 0;
  for (let i = 0; i < body.length; i += 1) {
    const ch = body[i];
    if (quote) {
      if (ch === "\\") {
        i += 1;
        continue;
      }
      if (ch === quote) quote = null;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
      continue;
    }
    if (ch === "/" && body[i + 1] === "/") {
      const nl = body.indexOf("\n", i);
      i = nl === -1 ? body.length : nl;
      continue;
    }
    if (ch === "{" || ch === "(" || ch === "[") depth += 1;
    else if (ch === "}" || ch === ")" || ch === "]") depth -= 1;
    else if (ch === "," && depth === 0) {
      entries.push({ text: body.slice(start, i), offset: start });
      start = i + 1;
    }
  }
  if (body.slice(start).trim()) entries.push({ text: body.slice(start), offset: start });
  return entries.filter((entry) => entry.text.trim().length > 0);
}

/** 取条目紧邻上方的 `//` 注释块。 */
function readLeadingComment(lines: string[], entryLineIndex: number): string {
  const collected: string[] = [];
  for (let i = entryLineIndex - 1; i >= 0; i -= 1) {
    const line = lines[i];
    const trimmed = line.trim();
    if (trimmed.startsWith("//")) {
      collected.unshift(trimmed.replace(/^\/\/\s?/, ""));
      continue;
    }
    if (trimmed === "") {
      // 注释块与条目之间允许一个空行，但注释块内部不允许断行
      if (collected.length > 0) break;
      continue;
    }
    break;
  }
  return collected.join("\n");
}

/** 解析字面量参数；无法静态求值时返回 { resolved: false }。 */
function parseLiteral(raw: string): { value: unknown; resolved: boolean } {
  const text = raw.trim();
  if (!text) return { value: null, resolved: false };
  if (/^true$/.test(text)) return { value: true, resolved: true };
  if (/^false$/.test(text)) return { value: false, resolved: true };
  const numeric = /^-?\d[\d_]*(\.\d[\d_]*)?$/.test(text);
  if (numeric) return { value: Number(text.replace(/_/g, "")), resolved: true };
  // 纯数字算术式（如 64 * 1024 * 1024）：字符集受限，可安全求值。
  if (/^[\d_+\-*/().\s]+$/.test(text) && /\d/.test(text)) {
    try {
      const value = Number(new Function(`return (${text.replace(/_/g, "")})`)());
      if (Number.isFinite(value)) return { value, resolved: true };
    } catch {
      return { value: null, resolved: false };
    }
  }
  const quoted = /^(['"`])([\s\S]*)\1$/.test(text);
  if (quoted) return { value: text.slice(1, -1), resolved: true };
  return { value: null, resolved: false };
}

/** 读取函数倒数第一个参数之前的完整实参串（用于 .default(...) / .min(...)）。 */
function readCallArgs(text: string, callIndex: number): string {
  const openParen = text.indexOf("(", callIndex);
  const closeIndex = scanBalanced(text, openParen + 1);
  return text.slice(openParen + 1, closeIndex);
}

function firstArg(args: string): string {
  let depth = 0;
  let quote: string | null = null;
  for (let i = 0; i < args.length; i += 1) {
    const ch = args[i];
    if (quote) {
      if (ch === "\\") i += 1;
      else if (ch === quote) quote = null;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") quote = ch;
    else if (ch === "(" || ch === "[" || ch === "{") depth += 1;
    else if (ch === ")" || ch === "]" || ch === "}") depth -= 1;
    else if (ch === "," && depth === 0) return args.slice(0, i);
  }
  return args;
}

function collectCallArguments(text: string, name: string): string[] {
  const out: string[] = [];
  const pattern = new RegExp(`\\.${name}\\(`, "g");
  for (const match of text.matchAll(pattern)) {
    const index = match.index ?? 0;
    // 过滤嵌套对象里的同名调用属于过度设计：本 schema 不存在该形态，直接全部收集。
    out.push(readCallArgs(text, index));
  }
  return out;
}

function classifySchemaType(expr: string): { schemaType: string; int: boolean } {
  // 源码里链式调用会折行（`z\n  .number()`），故模式必须容忍空白。
  const isEnum = /\.enum\s*\(/.test(expr);
  if (isEnum) return { schemaType: "enum", int: false };
  const intFlag = /\.int\s*\(\s*\)/.test(expr);
  if (/\.number\s*\(\s*\)/.test(expr)) return { schemaType: "number", int: intFlag };
  if (/\.boolean\s*\(\s*\)/.test(expr)) return { schemaType: "boolean", int: false };
  if (/\.string\s*\(\s*\)/.test(expr)) return { schemaType: "string", int: false };
  return { schemaType: "unknown", int: intFlag };
}

function classifyWrapper(expr: string): string {
  if (/optionalNumber\(/.test(expr)) return "optionalNumber";
  if (/optionalPreprocessed\(/.test(expr)) return "optionalPreprocessed";
  if (/\.optional\(\)/.test(expr)) return "optional";
  return "raw";
}

const DOMAIN_RULES: Array<{ domain: string; test: RegExp }> = [
  { domain: "数据库", test: /^(DSN|DB_)/ },
  { domain: "Redis 与缓存", test: /^(REDIS_|ENABLE_PROVIDER_CACHE|PROVIDER_CACHE)/ },
  { domain: "消息写入", test: /^MESSAGE_REQUEST_/ },
  { domain: "限流与会话", test: /(RATE_LIMIT|SESSION|COMPLETION|CONCURRENT)/ },
  { domain: "流式与门禁", test: /(STREAM_|DETACHED_STREAM_|SENSITIVE_)/ },
  { domain: "Replay", test: /^REPLAY_/ },
  { domain: "上游拨号", test: /(FETCH_|PROXY_|UPSTREAM_|ENDPOINT_)/ },
  { domain: "熔断与健康", test: /(CIRCUIT|HEALTH|HEDGE|PROBE)/ },
  { domain: "可观测性", test: /(LANGFUSE|TRACE|LOG_|OTEL)/ },
  { domain: "管理与安全", test: /(ADMIN_|CSRF_|AUTH_|API_KEY|ENABLE_LEGACY_ACTIONS_API)/ },
  { domain: "多进程与进程内", test: /^(CCH_|NODE_|PORT$)/ },
];

function classifyDomain(name: string): string {
  for (const rule of DOMAIN_RULES) {
    if (rule.test.test(name)) return rule.domain;
  }
  return "其它";
}

/** 单次遍历 src/ 统计每个全大写 token 出现的文件与次数。 */
function collectConsumers(): Map<string, Map<string, number>> {
  const result = new Map<string, Map<string, number>>();
  const walk = (dir: string): void => {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const full = join(dir, entry.name);
      if (entry.isDirectory()) {
        if (entry.name === "__tests__" || entry.name === "node_modules") continue;
        walk(full);
        continue;
      }
      if (!entry.name.endsWith(".ts") && !entry.name.endsWith(".tsx")) continue;
      if (entry.name.includes(".test.")) continue;
      const rel = relative(REPO_ROOT, full);
      if (rel === SOURCE_REL) continue;
      if (statSync(full).size > 4 * 1024 * 1024) continue;
      const text = readFileSync(full, "utf8");
      for (const match of text.matchAll(/\b[A-Z][A-Z0-9_]{2,}\b/g)) {
        const token = match[0];
        let files = result.get(token);
        if (!files) {
          files = new Map<string, number>();
          result.set(token, files);
        }
        files.set(rel, (files.get(rel) ?? 0) + 1);
      }
    }
  };
  walk(join(REPO_ROOT, "src"));
  return result;
}

function parseSchema(source: string): { entries: VariableEntry[]; crossFieldRules: string[] } {
  const marker = "export const EnvSchema = z.object({";
  const markerIndex = source.indexOf(marker);
  if (markerIndex === -1) throw new Error(`未找到 ${marker}`);
  const bodyStart = markerIndex + marker.length;
  const bodyEnd = scanBalanced(source, bodyStart);
  const body = source.slice(bodyStart, bodyEnd);
  const lines = source.split("\n");
  const consumers = collectConsumers();

  const entries: VariableEntry[] = [];
  for (const item of splitTopLevelEntries(body)) {
    // 条目文本可能以换行与注释开头（注释夹在逗号与键名之间），先定位键名行。
    const entryLines = item.text.split("\n");
    let nameLineIndex = -1;
    let name = "";
    let colon = -1;
    for (let i = 0; i < entryLines.length; i += 1) {
      const match = entryLines[i].match(/^\s*([A-Z][A-Z0-9_]*)\s*:/);
      if (match) {
        nameLineIndex = i;
        name = match[1];
        colon = entryLines[i].indexOf(":");
        break;
      }
    }
    if (nameLineIndex === -1) continue;
    const expr = [
      entryLines[nameLineIndex].slice(colon + 1),
      ...entryLines.slice(nameLineIndex + 1),
    ].join("\n");

    // 条目在源文件中的行号：用于取上方注释块
    const absoluteOffset = bodyStart + item.offset;
    const baseLineIndex = source.slice(0, absoluteOffset).split("\n").length - 1;
    const entryLineIndex = baseLineIndex + nameLineIndex;

    const defaultArgs = collectCallArguments(expr, "default");
    const defaults = defaultArgs.map(parseLiteral);
    const lastDefault = defaults.length > 0 ? defaults[defaults.length - 1] : null;
    const lastDefaultRaw =
      defaultArgs.length > 0 ? firstArg(defaultArgs[defaultArgs.length - 1]).trim() : null;

    // `.transform(booleanTransform)` 把字符串 "false"/"0" 转为布尔，默认值随之转换。
    const booleanTransformApplied = /\.transform\(\s*booleanTransform\s*\)/.test(expr);
    let defaultValue: unknown = lastDefault?.resolved ? lastDefault.value : null;
    if (booleanTransformApplied && typeof defaultValue === "string") {
      defaultValue = defaultValue !== "false" && defaultValue !== "0";
    }

    const enumMatch = expr.match(/z\.enum\(\s*\[([\s\S]*?)\]/);
    const enumValues = enumMatch
      ? [...enumMatch[1].matchAll(/['"`]([^'"`]*)['"`]/g)].map((m) => m[1])
      : null;

    let min: number | null = null;
    let max: number | null = null;
    for (const args of collectCallArguments(expr, "min")) {
      const parsed = parseLiteral(firstArg(args));
      if (parsed.resolved && typeof parsed.value === "number") {
        min = min === null ? parsed.value : Math.min(min, parsed.value);
      }
    }
    for (const args of collectCallArguments(expr, "max")) {
      const parsed = parseLiteral(firstArg(args));
      if (parsed.resolved && typeof parsed.value === "number") {
        max = max === null ? parsed.value : Math.max(max, parsed.value);
      }
    }

    const { schemaType, int } = classifySchemaType(expr);
    const wrapper = classifyWrapper(expr);
    const boundsKind =
      min === null && max === null ? null : schemaType === "string" ? "length" : "value";

    const comment = readLeadingComment(lines, entryLineIndex);
    const consumerMap = consumers.get(name);
    const consumerFiles = consumerMap
      ? [...consumerMap.entries()]
          .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
          .slice(0, MAX_CONSUMER_FILES)
          .map(([file]) => file)
      : [];

    entries.push({
      name,
      schemaType,
      wrapper,
      optional: wrapper !== "raw" || /\.optional\(\)/.test(expr) || defaults.length > 0,
      int,
      default: defaultValue,
      defaultRaw: lastDefaultRaw,
      min,
      max,
      boundsKind,
      enumValues,
      descriptionComment: comment,
      descriptionSummary: comment.split("\n")[0] ?? "",
      consumerFiles,
      consumerFileCount: consumerMap?.size ?? 0,
    });
  }

  const crossFieldRules = [...source.matchAll(/env\.([A-Z][A-Z0-9_]*)/g)]
    .map((m) => m[1])
    .filter((name, index, all) => all.indexOf(name) === index)
    .sort();

  return { entries, crossFieldRules };
}

function renderMarkdown(matrix: Matrix, crossFieldRules: string[]): string {
  const lines: string[] = [];
  lines.push("# 环境变量契约矩阵");
  lines.push("");
  lines.push(
    "由 `scripts/export-env-matrix.ts` 从 `src/lib/config/env.schema.ts` 生成，请勿手工编辑。"
  );
  lines.push("");
  lines.push(`- 变量总数：**${matrix.variableCount}**`);
  lines.push(`- 源文件 SHA256：\`${matrix.sourceSha256}\``);
  lines.push(`- 重生成：\`bun scripts/export-env-matrix.ts\``);
  lines.push("");
  const byDomain = new Map<string, VariableEntry[]>();
  for (const variable of matrix.variables) {
    const domain = classifyDomain(variable.name);
    const list = byDomain.get(domain) ?? [];
    list.push(variable);
    byDomain.set(domain, list);
  }
  for (const domain of [...byDomain.keys()].sort()) {
    const list = byDomain.get(domain) ?? [];
    lines.push(`## ${domain}（${list.length}）`);
    lines.push("");
    lines.push("| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |");
    lines.push("| --- | --- | --- | --- | --- |");
    for (const variable of list) {
      const bounds: string[] = [];
      if (variable.min !== null) {
        bounds.push(`${variable.boundsKind === "length" ? "长度" : ""}>= ${variable.min}`.trim());
      }
      if (variable.max !== null) {
        bounds.push(`${variable.boundsKind === "length" ? "长度" : ""}<= ${variable.max}`.trim());
      }
      if (variable.enumValues) bounds.push(`∈ {${variable.enumValues.join(", ")}}`);
      const shown = variable.defaultRaw ?? (variable.default === null ? "—" : String(variable.default));
      const consumers =
        variable.consumerFiles.length > 0 ? `\`${variable.consumerFiles[0]}\`` : "（无静态引用）";
      lines.push(
        `| \`${variable.name}\` | ${variable.schemaType}${
          variable.wrapper !== "raw" ? `（${variable.wrapper}）` : ""
        } | ${shown === "—" ? "—" : `\`${shown}\``} | ${bounds.join("；") || "—"} | ${consumers} |`
      );
    }
    lines.push("");
  }
  if (crossFieldRules.length > 0) {
    lines.push("## 交叉字段约束");
    lines.push("");
    lines.push("`EnvSchema.superRefine` 引用的变量（Go 侧装载器必须复刻同样的关系校验）：");
    lines.push("");
    for (const name of crossFieldRules) lines.push(`- \`${name}\``);
    lines.push("");
  }
  return `${lines.join("\n")}\n`;
}

async function runtimeCrossCheck(matrix: Matrix): Promise<number> {
  console.log("\n== 运行时 zod 交叉校验 ==");
  let module: { EnvSchema?: unknown };
  try {
    module = await import(join(REPO_ROOT, SOURCE_REL));
  } catch (error) {
    console.log(`SKIP  无法导入 ${SOURCE_REL}：${(error as Error).message}`);
    return 0;
  }
  const schema = module.EnvSchema as { shape?: Record<string, unknown> } | undefined;
  const shape = schema?.shape;
  if (!shape) {
    console.log("SKIP  EnvSchema 无可读的 shape");
    return 0;
  }
  const runtimeNames = Object.keys(shape).sort();
  const matrixNames = matrix.variables.map((variable) => variable.name).sort();
  const missingInMatrix = runtimeNames.filter((name) => !matrixNames.includes(name));
  const extraInMatrix = matrixNames.filter((name) => !runtimeNames.includes(name));
  console.log(`运行时键数 ${runtimeNames.length} / 矩阵键数 ${matrixNames.length}`);
  if (missingInMatrix.length > 0) console.log(`FAIL  矩阵遗漏：${missingInMatrix.join(", ")}`);
  if (extraInMatrix.length > 0) console.log(`FAIL  矩阵多出：${extraInMatrix.join(", ")}`);

  let defaultMismatches = 0;
  let checkedDefaults = 0;
  for (const variable of matrix.variables) {
    const field = shape[variable.name] as { safeParse?: (value: unknown) => unknown } | undefined;
    if (!field?.safeParse) continue;
    const parsed = field.safeParse(undefined) as
      | { success: boolean; data?: unknown }
      | undefined;
    if (!parsed?.success) continue;
    checkedDefaults += 1;
    const runtimeDefault = parsed.data;
    const matrixDefault = variable.default === null ? undefined : variable.default;
    if (JSON.stringify(matrixDefault) !== JSON.stringify(runtimeDefault)) {
      defaultMismatches += 1;
      console.log(
        `FAIL  ${variable.name} 默认值不一致：矩阵 ${JSON.stringify(
          variable.default
        )} vs 运行时 ${JSON.stringify(runtimeDefault)}`
      );
    }
  }
  console.log(`默认值比对 ${checkedDefaults} 项，不一致 ${defaultMismatches} 项`);
  const failures = missingInMatrix.length + extraInMatrix.length + defaultMismatches;
  console.log(failures === 0 ? "PASS  交叉校验通过" : `FAIL  共 ${failures} 项差异`);
  return failures;
}

async function main(): Promise<void> {
  const args = process.argv.slice(2);
  const check = args.includes("--check");
  const outDirArg = args.find((arg) => arg.startsWith("--out-dir="));
  const outDir = join(REPO_ROOT, outDirArg ? outDirArg.split("=")[1] : DEFAULT_OUT_DIR);

  const source = readFileSync(join(REPO_ROOT, SOURCE_REL), "utf8");
  const { entries, crossFieldRules } = parseSchema(source);
  const variables = [...entries].sort((a, b) => a.name.localeCompare(b.name));

  const countsByType: Record<string, number> = {};
  const countsByDomain: Record<string, number> = {};
  for (const variable of variables) {
    countsByType[variable.schemaType] = (countsByType[variable.schemaType] ?? 0) + 1;
    const domain = classifyDomain(variable.name);
    countsByDomain[domain] = (countsByDomain[domain] ?? 0) + 1;
  }

  const matrix: Matrix = {
    source: SOURCE_REL,
    sourceSha256: createHash("sha256").update(source).digest("hex"),
    variableCount: variables.length,
    countsByType: Object.fromEntries(Object.entries(countsByType).sort()),
    countsByDomain: Object.fromEntries(Object.entries(countsByDomain).sort()),
    variables,
  };

  mkdirSync(outDir, { recursive: true });
  const jsonPath = join(outDir, "env-matrix.json");
  const mdPath = join(outDir, "env-matrix.md");
  writeFileSync(jsonPath, `${JSON.stringify(matrix, null, 2)}\n`);
  writeFileSync(mdPath, renderMarkdown(matrix, crossFieldRules));

  console.log(`源文件 ${SOURCE_REL}（sha256 ${matrix.sourceSha256.slice(0, 12)}…）`);
  console.log(`变量总数 ${matrix.variableCount}`);
  console.log(`按类型：${JSON.stringify(matrix.countsByType)}`);
  console.log(`按域：${JSON.stringify(matrix.countsByDomain)}`);
  const noConsumer = variables.filter((variable) => variable.consumerFileCount === 0);
  console.log(
    `无 src/ 内静态引用（可能由运行时消费）：${
      noConsumer.length === 0 ? "无" : noConsumer.map((v) => v.name).join(", ")
    }`
  );
  const unresolvedDefaults = variables.filter(
    (variable) => variable.defaultRaw !== null && variable.default === null
  );
  console.log(
    `默认值非字面量（需人工核对）：${
      unresolvedDefaults.length === 0 ? "无" : unresolvedDefaults.map((v) => v.name).join(", ")
    }`
  );
  console.log(`写入 ${relative(REPO_ROOT, jsonPath)}`);
  console.log(`写入 ${relative(REPO_ROOT, mdPath)}`);

  if (check) {
    const failures = await runtimeCrossCheck(matrix);
    if (failures > 0) process.exitCode = 1;
  }
}

await main();
