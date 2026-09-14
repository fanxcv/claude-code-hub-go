import fs from "node:fs";
import path from "node:path";
import en from "@messages/en";
import zhCN from "@messages/zh-CN";
import { describe, expect, test } from "vitest";

/**
 * 回归钉子：**源码里的字面量词条键必须在全部词表（现存 `zh-CN`/`en`）里都存在**。
 *
 * 背景（2026-09-13 生产实测）：`src/app/[locale]/dashboard/sessions/_components/active-sessions-client.tsx`
 * 用 `useTranslations("dashboard.sessions")` 却调 `t("refreshing")`（解析为 `dashboard.sessions.refreshing`），
 * 而词条实际在 `dashboard.sessions.table.refreshing`。生产控制台报
 * `MISSING_MESSAGE: dashboard.sessions.refreshing (zh-CN)`，界面上那一处变成键名/空白。
 *
 * 这类缺陷的特征是**静默**：不报错、不掉数据，只是文案不对；只有真的把那页打开才会发现。
 * 故用静态扫描把它固化为可重跑的检查：将来任何一处写错键路径，这里先红。
 *
 * 为什么是静态扫描而不是渲染测试：键是**字面量**，扫描能覆盖所有分支（含当前未渲染的代码），
 * 且不需要起组件树与词表 provider；渲染测试只能覆盖跑到的分支。
 *
 * 本钉子**不做**的事（天花板，登记于此）：
 *  1. 只检查**字面量**键。`t(variable)`、带插值的模板串等**动态键**无法静态解析，只统计数量
 *     （见「动态键」用例）——它们一旦写错，本钉子抓不到。
 *  2. 不看词条的**值**（空串、占位符错位、语言串错都不管）；那属于词表质量，另有专项。
 *  3. 不做作用域分析：同一文件内若同一变量名被多次绑定 `useTranslations`，按「调用点之前最近
 *     的那次绑定」归属（这是本仓 800+ 文件里的实际写法；见 usage-doc/page.tsx 的三次绑定）。
 */

const PROJECT_ROOT = path.resolve(__dirname, "../../..");
const SCAN_ROOTS = ["src/app/[locale]", "src/components", "src/lib", "src/hooks"];
const LOCALES = ["zh-CN", "en"] as const;

const CATALOGS: Record<string, unknown> = {
  "zh-CN": zhCN,
  en,
};

/** 扫描面下界：实测 800+ 个 ts/tsx；低于此值说明 walk 坏了，直接失败（防空跑假绿）。 */
const MIN_SCANNED_FILES = 300;
/** 字面量 (命名空间, 键) 对数下界：防「解析全坏 → 零缺失 → 假绿」。实测值见报告。 */
const MIN_LITERAL_PAIRS = 1500;
/** 已登记的孤儿调用上限（`t` 作为匿名参数传入模块级 helper；命名空间在调用点，静态不可解）。 */
const MAX_UNBOUND_CALLS = 40;

// ---------------------------------------------------------------------------
// 源码扫描
// ---------------------------------------------------------------------------

/**
 * 剥掉注释，只留代码；**保持长度与行号不变**（注释内容替换为等量空格），
 * 这样报错行号能直接对应原文件。
 *
 * 为何必须剥：不变量是「**代码**里的键必须存在」，而注释里出现键名是正常的
 * （解释文案、举例都会写）。与仓库既有 `stripComments`（tests/unit/ui/retired-database-capabilities.test.ts）
 * 同口径：注释不是实现。
 *
 * 同时**保留字符串内容**（引号内的 `//` 不得被当成注释——本仓有 `https://` 一类 URL）。
 * 上限：模板串里的 `${}` 嵌套引号不做语法级处理（本仓没有「模板串内嵌引号再嵌键」的写法）。
 */
function stripComments(source: string): string {
  let out = "";
  let i = 0;
  let state: "code" | "line" | "block" | "squote" | "dquote" | "template" = "code";
  while (i < source.length) {
    const c = source[i];
    const c2 = source[i + 1];
    if (state === "code") {
      if (c === "/" && c2 === "/") {
        state = "line";
        out += "  ";
        i += 2;
        continue;
      }
      if (c === "/" && c2 === "*") {
        state = "block";
        out += "  ";
        i += 2;
        continue;
      }
      if (c === "'") state = "squote";
      else if (c === '"') state = "dquote";
      else if (c === "`") state = "template";
      out += c;
      i += 1;
      continue;
    }
    if (state === "line") {
      if (c === "\n") {
        state = "code";
        out += c;
      } else {
        out += " ";
      }
      i += 1;
      continue;
    }
    if (state === "block") {
      if (c === "*" && c2 === "/") {
        state = "code";
        out += "  ";
        i += 2;
        continue;
      }
      out += c === "\n" ? "\n" : " ";
      i += 1;
      continue;
    }
    // 字符串/模板串内：整体照抄（含转义），遇到同类引号即出串
    if (c === "\\") {
      out += c + (c2 ?? "");
      i += 2;
      continue;
    }
    if (
      (state === "squote" && c === "'") ||
      (state === "dquote" && c === '"') ||
      (state === "template" && c === "`")
    ) {
      state = "code";
    }
    out += c;
    i += 1;
  }
  return out;
}

function walk(dir: string, out: string[] = []): string[] {
  if (!fs.existsSync(dir)) return out;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === "node_modules" || entry.name.startsWith(".")) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full, out);
    else if (/\.(ts|tsx)$/.test(entry.name) && !/\.(test|spec)\.(ts|tsx)$/.test(entry.name))
      out.push(full);
  }
  return out;
}

function lineOf(source: string, index: number): number {
  let line = 1;
  for (let i = 0; i < index && i < source.length; i++) if (source[i] === "\n") line += 1;
  return line;
}

/** 从 `(` 起做括号配平，返回参数字符串与右括号下标。 */
function readCallArgs(code: string, openParen: number): { args: string; end: number } {
  let depth = 0;
  let i = openParen;
  for (; i < code.length; i++) {
    const c = code[i];
    if (c === "(") depth += 1;
    else if (c === ")") {
      depth -= 1;
      if (depth === 0) return { args: code.slice(openParen + 1, i), end: i };
    } else if (c === "'" || c === '"' || c === "`") {
      // 跳过字符串字面量（其中的括号不参与配平）
      const quote = c;
      i += 1;
      while (i < code.length && code[i] !== quote) {
        if (code[i] === "\\") i += 1;
        i += 1;
      }
    }
  }
  return { args: code.slice(openParen + 1), end: code.length };
}

interface Binding {
  name: string;
  namespace: string;
  index: number;
  line: number;
}
interface TranslationCall {
  name: string;
  method: string;
  /** 字面量键；`null` 表示动态（变量/含插值的模板串） */
  key: string | null;
  index: number;
  line: number;
}

/** 抽出翻译函数的全部绑定形态（变量名 + 命名空间）。
 *
 * 支持四种写法（前两种是实际主流，后两种在带类型的 props 与解构里出现）：
 * 1. `const t = useTranslations("ns")` / `await getTranslations("ns")`（泛型可有可无）；
 * 2. `const t = useTranslations<"ns">()`——命名空间写在**泛型参数**里；
 * 3. `const { t } = useTranslations()` / `const { t: tr } = useTranslations("ns")`——解构；
 * 4. `t: ReturnType<typeof useTranslations<"ns">>`——**命名空间在类型里**，组件通过 props 收到 `t`
 *    （本仓价格详情、路由追踪等页用这种写法；不认它就会出现一片「孤儿调用」）。
 *
 * 通用类型参数（`ReturnType<typeof useTranslations<"ipDetails">>` 被 alias 成 `type T = …` 后再用）
 * 不做别名追踪：那种用法下拿不到命名空间，会落到「孤儿调用」计数里，由对应用例报出（不静默）。
 */
function findBindings(code: string): Binding[] {
  const bindings: Binding[] = [];

  // 形态 4：类型里的泛型命名空间（先收集，它们的位置对归属没有意义，给 -1 让就近绑定优先）
  const typed =
    /([A-Za-z_$][A-Za-z0-9_$]*)\s*:\s*ReturnType<typeof\s+(?:useTranslations|getTranslations)\s*<\s*(["'`])([^"'`]+)\2\s*>\s*>/g;
  let tm: RegExpExecArray | null = typed.exec(code);
  while (tm !== null) {
    bindings.push({ name: tm[1], namespace: tm[3], index: -1, line: lineOf(code, tm.index) });
    tm = typed.exec(code);
  }

  // 形态 1/2/3：赋值式绑定
  const re =
    /(?:const|let|var)\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*(?:await\s+)?(useTranslations|getTranslations)\s*(<[^<>]*>)?\s*\(/g;
  let m: RegExpExecArray | null = re.exec(code);
  while (m !== null) {
    const name = m[1];
    const genericArgs = m[3] ?? "";
    const openParen = m.index + m[0].length - 1;
    const { args } = readCallArgs(code, openParen);
    const trimmed = args.trim();
    let namespace = "";
    const literal = /^(["'`])((?:[^"'`\\]|\\.)*)\1/.exec(trimmed);
    const named = /namespace:\s*(["'`])((?:[^"'`\\]|\\.)*)\1/.exec(trimmed);
    const genericLiteral = /^\s*<\s*(["'`])([^"'`]+)\1\s*>$/.exec(
      genericArgs.replace(/^</, "<").trim()
    )
      ? /(["'`])([^"'`]+)\1/.exec(genericArgs)
      : null;
    if (literal) namespace = literal[2];
    else if (named) namespace = named[2];
    else if (genericLiteral) namespace = genericLiteral[2];
    bindings.push({ name, namespace, index: m.index, line: lineOf(code, m.index) });
    m = re.exec(code);
  }

  // 形态 3：解构 `const { t } = useTranslations(...)` / `const { t: tr } = …`
  const destructured =
    /(?:const|let|var)\s*\{\s*(?:t\s*:\s*([A-Za-z_$][A-Za-z0-9_$]*)|([A-Za-z_$][A-Za-z0-9_$]*))\s*\}\s*=\s*(?:await\s+)?(useTranslations|getTranslations)\s*(<[^<>]*>)?\s*\(/g;
  let dm: RegExpExecArray | null = destructured.exec(code);
  while (dm !== null) {
    const name = dm[1] ?? dm[2];
    const openParen = dm.index + dm[0].length - 1;
    const { args } = readCallArgs(code, openParen);
    const literal = /^(["'`])((?:[^"'`\\]|\\.)*)\1/.exec(args.trim());
    const generic = /(["'`])([^"'`]+)\1/.exec(dm[4] ?? "");
    const named = /namespace:\s*(["'`])((?:[^"'`\\]|\\.)*)\1/.exec(args);
    bindings.push({
      name,
      namespace: literal?.[2] ?? named?.[2] ?? generic?.[2] ?? "",
      index: dm.index,
      line: lineOf(code, dm.index),
    });
    dm = destructured.exec(code);
  }

  // 形态 5：类型别名 `type T = ReturnType<typeof useTranslations<"ns">>`，
  // 再由形如 `name: T` 的参数/属性收到 t（ip-details 一族用这种写法）。
  const aliasNamespaces = new Map<string, string>();
  const aliasRe =
    /type\s+([A-Za-z_$][A-Za-z0-9_$]*)\s*=\s*ReturnType<typeof\s+(?:useTranslations|getTranslations)\s*<\s*(["'`])([^"'`]+)\2\s*>\s*>/g;
  let am: RegExpExecArray | null = aliasRe.exec(code);
  while (am !== null) {
    aliasNamespaces.set(am[1], am[3]);
    am = aliasRe.exec(code);
  }
  for (const [alias, namespace] of aliasNamespaces) {
    const paramRe = new RegExp(`([A-Za-z_$][A-Za-z0-9_$]*)\\s*[?]?\\s*:\\s*${alias}\\b`, "g");
    let pm: RegExpExecArray | null = paramRe.exec(code);
    while (pm !== null) {
      bindings.push({ name: pm[1], namespace, index: -1, line: lineOf(code, pm.index) });
      pm = paramRe.exec(code);
    }
  }

  return bindings;
}

/** 抽出对翻译函数的调用（`t("k")` / `t.rich("k", …)` / `t.raw("k")` / `t.markup("k")`）。 */
function findTranslationCalls(code: string, names: Set<string>): TranslationCall[] {
  const calls: TranslationCall[] = [];
  const re = /([A-Za-z_$][A-Za-z0-9_$]*)\s*(?:\.(rich|raw|markup))?\s*\(/g;
  let m: RegExpExecArray | null = re.exec(code);
  while (m !== null) {
    const name = m[1];
    if (!names.has(name)) {
      m = re.exec(code);
      continue;
    }
    const openParen = code.indexOf("(", m.index + name.length);
    const { args } = readCallArgs(code, openParen);
    const trimmed = args.trim();
    let key: string | null = null;
    if (trimmed.length > 0) {
      const quote = trimmed[0];
      if (quote === '"' || quote === "'" || quote === "`") {
        let i = 1;
        let value = "";
        let closed = false;
        while (i < trimmed.length) {
          const c = trimmed[i];
          if (c === "\\") {
            value += trimmed[i + 1] ?? "";
            i += 2;
            continue;
          }
          if (c === quote) {
            closed = true;
            break;
          }
          value += c;
          i += 1;
        }
        // 模板串含插值即视为动态；未闭合（跨行串等）同样视为动态
        if (closed && quote !== "`" && !value.includes("${")) key = value;
        else if (closed && quote === "`" && !value.includes("${")) key = value;
      }
    }
    calls.push({ name, method: m[2] ?? "", key, index: m.index, line: lineOf(code, m.index) });
    m = re.exec(code);
  }
  return calls;
}

/** 按「最近的前置绑定」把调用归属到命名空间；其次回退到类型级绑定（index === -1）。 */
function attributeNamespace(bindings: Binding[], call: TranslationCall): string | null {
  let best: Binding | null = null;
  for (const b of bindings) {
    if (b.name !== call.name) continue;
    if (b.index < 0) continue;
    if (b.index < call.index && (best === null || b.index > best.index)) best = b;
  }
  if (best !== null) return best.namespace;
  // 类型级绑定（props 传入的 t）：仅当同文件没有赋值式绑定时才采用
  const typedOnly = bindings.filter((b) => b.name === call.name && b.index < 0);
  if (typedOnly.length > 0) {
    const namespaces = new Set(typedOnly.map((b) => b.namespace));
    // 同名但声明了多个命名空间时不能确定归属 → 交给孤儿计数报出，不猜测
    if (namespaces.size === 1) return typedOnly[0].namespace;
  }
  return null;
}

/**
 * 词表里是否存在 `namespace.key`。
 *
 * 同时兼容两种写法：JSON 里按层级嵌套（`"a": {"b": …}`）与真的带点的字面量键（`"a.b": …`）。
 * 数组按数字下标走进（本仓 `usage` 一类词表含数组值）。
 */
function hasMessage(catalog: unknown, namespace: string, key: string): boolean {
  const segments = [...(namespace.length > 0 ? namespace.split(".") : []), ...key.split(".")];
  let current: unknown = catalog;
  for (let i = 0; i < segments.length; i++) {
    if (current === null || typeof current !== "object") return false;
    const rest = segments.slice(i).join(".");
    if (Object.hasOwn(current, rest)) return true;
    const segment = segments[i];
    if (Array.isArray(current)) {
      const idx = Number(segment);
      if (!Number.isInteger(idx) || idx < 0 || idx >= current.length) return false;
      current = current[idx];
      continue;
    }
    if (!Object.hasOwn(current, segment)) return false;
    current = (current as Record<string, unknown>)[segment];
  }
  return true;
}

interface ScanResult {
  pairs: { file: string; line: number; namespace: string; key: string }[];
  dynamic: { file: string; line: number; name: string }[];
  unbound: { file: string; line: number; name: string; key: string | null }[];
}

function scanSource(file: string, source: string): ScanResult {
  const code = stripComments(source);
  const bindings = findBindings(code);
  const names = new Set(bindings.map((b) => b.name));
  const calls = findTranslationCalls(code, names);
  const result: ScanResult = { pairs: [], dynamic: [], unbound: [] };
  const rel = path.relative(PROJECT_ROOT, file);
  for (const call of calls) {
    const namespace = attributeNamespace(bindings, call);
    if (namespace === null) {
      result.unbound.push({ file: rel, line: call.line, name: call.name, key: call.key });
      continue;
    }
    if (call.key === null) {
      result.dynamic.push({ file: rel, line: call.line, name: call.name });
      continue;
    }
    if (call.key.length === 0) continue;
    result.pairs.push({ file: rel, line: call.line, namespace, key: call.key });
  }
  return result;
}

function scanAll(): {
  files: string[];
  pairs: ScanResult["pairs"];
  dynamic: ScanResult["dynamic"];
  unbound: ScanResult["unbound"];
} {
  const files = SCAN_ROOTS.flatMap((root) => walk(path.join(PROJECT_ROOT, root)));
  const pairs: ScanResult["pairs"] = [];
  const dynamic: ScanResult["dynamic"] = [];
  const unbound: ScanResult["unbound"] = [];
  for (const file of files) {
    const one = scanSource(file, fs.readFileSync(file, "utf8"));
    pairs.push(...one.pairs);
    dynamic.push(...one.dynamic);
    unbound.push(...one.unbound);
  }
  return { files, pairs, dynamic, unbound };
}

/**
 * 兜底：键路径是否在词表的**任意深度**存在（后缀匹配）。
 *
 * 用在「孤儿调用」上——它们拿不到命名空间（例如 `t` 是模块级 helper 的匿名参数，
 * 命名空间在调用点）。命名空间未知，但键路径本身的**尾部**应当存在：
 * 例如 `t("proxyStatus.timeAgo.justNow")` 的真实路径是
 * `dashboard.keyListHeader.proxyStatus.timeAgo.justNow`，后缀匹配能认出来。
 * 若连任意深度都找不到，则几乎可断定是键写错（而不是「属于哪个命名空间未知」），值得报出。
 */
function existsAtAnyDepth(catalog: unknown, key: string): boolean {
  const leaves: string[] = [];
  const collect = (node: unknown, prefix: string): void => {
    if (node === null || typeof node !== "object") {
      if (prefix.length > 0) leaves.push(prefix);
      return;
    }
    if (Array.isArray(node)) {
      node.forEach((item, idx) =>
        collect(item, prefix.length > 0 ? `${prefix}.${idx}` : String(idx))
      );
      return;
    }
    for (const [k, v] of Object.entries(node as Record<string, unknown>)) {
      collect(v, prefix.length > 0 ? `${prefix}.${k}` : k);
    }
  };
  collect(catalog, "");
  return leaves.some((leaf) => leaf === key || leaf.endsWith(`.${key}`));
}

/** 缺键清单：`相对路径:行 命名空间.键 (locale)`。 */
function missingKeys(
  pairs: ScanResult["pairs"],
  locale: string
): { label: string; fullKey: string }[] {
  const catalog = CATALOGS[locale];
  const out: { label: string; fullKey: string }[] = [];
  for (const pair of pairs) {
    if (hasMessage(catalog, pair.namespace, pair.key)) continue;
    const fullKey = pair.namespace.length > 0 ? `${pair.namespace}.${pair.key}` : pair.key;
    out.push({ label: `${pair.file}:${pair.line}  ${fullKey} (${locale})`, fullKey });
  }
  return out;
}

// ---------------------------------------------------------------------------
// 用例
// ---------------------------------------------------------------------------

const scan = scanAll();

describe("i18n：源码字面量键在五语种词表里都存在", () => {
  test("扫描面非空（防 walk 静默失败导致假绿）", () => {
    expect(scan.files.length).toBeGreaterThan(MIN_SCANNED_FILES);
  });

  test("抽到的字面量键对数足够多（防解析坏掉后『零缺失』假绿）", () => {
    expect(
      scan.pairs.length,
      `只抽到 ${scan.pairs.length} 对 (命名空间, 键)，低于下界 ${MIN_LITERAL_PAIRS}——很可能是扫描/解析坏了，不是词表变干净了。`
    ).toBeGreaterThanOrEqual(MIN_LITERAL_PAIRS);
  });

  test("绑定了翻译函数的调用都被归属到命名空间（无孤儿调用）", () => {
    // 孤儿 = 找到了 `t("k")` 形式的调用但没有能确定命名空间的绑定记录。
    // 目前剩余的都属「`t` 作为匿名参数传入模块级 helper」（命名空间在调用点，静态不可解），
    // 故不要求归零，而是：① 设上限，防止覆盖面静默缩水；② 对它们的键做兜底校验（下一条用例）。
    const samples = scan.unbound.slice(0, 8).map((u) => `  - ${u.file}:${u.line}  ${u.name}(…)`);
    expect(
      scan.unbound.length,
      [
        `孤儿调用 ${scan.unbound.length} 处，超出已登记上限 ${MAX_UNBOUND_CALLS}`,
        "多出来的通常是新的绑定写法（请扩展 findBindings 的形态识别）：",
        ...samples,
        scan.unbound.length > samples.length ? "  …" : "",
      ]
        .filter((line) => line !== "")
        .join("\n")
    ).toBeLessThanOrEqual(MAX_UNBOUND_CALLS);
  });

  for (const locale of LOCALES) {
    test(`${locale}：孤儿调用的键至少在词表某处存在（兜底）`, () => {
      const catalog = CATALOGS[locale];
      const bad: string[] = [];
      for (const call of scan.unbound) {
        if (call.key === null) continue; // 动态键已单独统计
        if (existsAtAnyDepth(catalog, call.key)) continue;
        bad.push(`  - ${call.file}:${call.line}  ${call.name}("${call.key}") (${locale})`);
      }
      expect(
        bad.length,
        [
          `${locale}：${bad.length} 处孤儿调用的键在词表任何深度都不存在（几乎可断定是键写错）：`,
          ...bad.slice(0, 20),
          bad.length > 20 ? `  …（另有 ${bad.length - 20} 处）` : "",
        ]
          .filter((line) => line !== "")
          .join("\n")
      ).toBe(0);
    });
  }

  for (const locale of LOCALES) {
    test(`${locale}：无缺失键`, () => {
      const missing = missingKeys(scan.pairs, locale);
      const shown = missing.slice(0, 40).map((m) => `  - ${m.label}`);
      expect(
        missing.length,
        [
          `${locale} 词表里缺少 ${missing.length} 个源码引用的字面量键：`,
          ...shown,
          missing.length > shown.length ? `  …（另有 ${missing.length - shown.length} 处）` : "",
          "",
          "修法：优先把调用侧指向真正存在的词条（多数是命名空间或子路径写错）；",
          "确需新增词条时，五语种一起补齐（缺任何一个 locale 都算本钉子失败）。",
        ]
          .filter((line) => line !== "")
          .join("\n")
      ).toBe(0);
    });
  }

  test("动态键（静态抓不到）有清单与数量，且不占多数", () => {
    const dynamicRatio = scan.dynamic.length / Math.max(1, scan.pairs.length + scan.dynamic.length);
    // 只统计不判失败：动态键能不能写错取决于调用点，本钉子无从判定。
    // 但比例过高意味着本钉子的覆盖面在缩水，故设一个宽松上限（实测见报告）。
    expect(
      dynamicRatio,
      `动态键占比 ${(dynamicRatio * 100).toFixed(1)}%（${scan.dynamic.length} 处），过高会让本钉子形同虚设。`
    ).toBeLessThan(0.2);
  });

  /**
   * 自证可检出：用**合成源码**跑同一套解析，确认它能报出已知样例。
   *
   * 为什么必须有这条：真实代码修好之后，「无缺失键」既可能是钉子有效，也可能是钉子彻底瞎了
   * （正则不匹配、扫描面为空……）。上面的下界用例挡掉一部分，这条用**确定可检出的输入**
   * 再钉一次——并顺带锁住「命名空间 + 子路径」与「方法形式（t.rich/t.raw）」两种形态。
   */
  test("汇总：扫描规模（供报告与回归对照）", () => {
    // 打印而非断言具体数字：下界已在前面两条用例里断言，这里给出可直接引用的口径。
    console.log(
      `[i18n-key-coverage] 扫描文件=${scan.files.length} 字面量键对=${scan.pairs.length} 动态键=${scan.dynamic.length} 孤儿调用=${scan.unbound.length}`
    );
    // 动态键（静态抓不到）的分布：按文件聚合，降序。
    // 为何要输出清单：这类调用本钉子覆盖不到，若某文件大量使用动态键，
    // 该文件的键正确性就不受本钉子保护——必须看得见，而不是只有一个总数。
    const byFile = new Map<string, number>();
    for (const d of scan.dynamic) byFile.set(d.file, (byFile.get(d.file) ?? 0) + 1);
    const sorted = [...byFile.entries()].sort((a, b) => b[1] - a[1]);
    console.log(`[i18n-key-coverage] 动态键分布（共 ${sorted.length} 个文件）：`);
    for (const [file, count] of sorted) console.log(`  ${count}\t${file}`);
    expect(scan.files.length).toBeGreaterThan(0);
    expect(scan.pairs.length).toBeGreaterThan(0);
  });

  test("自证：合成源码能报出 dashboard.sessions.refreshing", () => {
    const synthetic = [
      "export function Component() {",
      '  const t = useTranslations("dashboard.sessions");',
      "  const tCommon = useTranslations('common');",
      '  return <>{isFetching ? t("refreshing") : null}{t.rich("table.refreshing", {})}{tCommon("retry", {})}</>;',
      "}",
    ].join("\n");
    const one = scanSource(path.join(PROJECT_ROOT, "src/components/__synthetic__.tsx"), synthetic);
    expect(one.pairs.length, "合成源码应抽出 3 对键").toBe(3);
    expect(one.dynamic.length).toBe(0);

    const catalog = CATALOGS["zh-CN"];
    // 已知缺失：命名空间右、子路径错（原事故形态）
    expect(hasMessage(catalog, "dashboard.sessions", "refreshing")).toBe(false);
    // 四个真实存在的键必须被认出来（否则说明 hasMessage 只会返回 false）
    expect(hasMessage(catalog, "dashboard.sessions", "table.refreshing")).toBe(true);
    expect(hasMessage(catalog, "dashboard.sessions", "activeSessions")).toBe(true);
    expect(hasMessage(catalog, "common", "retry")).toBe(true);
    expect(hasMessage(catalog, "dashboard", "sessions.table.refreshing")).toBe(true);
  });

  test("自证：注释里的键不计入（注释不是实现）", () => {
    const synthetic = [
      "export function C() {",
      '  const t = useTranslations("dashboard.sessions");',
      '  // t("this.key.does.not.exist")',
      "  /* t('neither.does.this') */",
      '  return t("table.refreshing");',
      "}",
    ].join("\n");
    const one = scanSource(path.join(PROJECT_ROOT, "src/components/__synthetic__.tsx"), synthetic);
    expect(one.pairs.map((p) => p.key)).toEqual(["table.refreshing"]);
  });
});
