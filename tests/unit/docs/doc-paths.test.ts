import { execSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";

/**
 * 仓库文档的**引用钉子**：文档里提到的仓库内路径、Markdown 相对链接与 npm 脚本都必须真实存在。
 *
 * 为什么需要它：`AGENTS.md` 在 Node 后端退役后仍长期写着 `src/repository`、`bun run db:generate`
 * 这类已不存在的东西，直到有人照它动手才发现；同类腐烂在 `go/README.md`（移植台账）、
 * `src/i18n/README.md`（未落地的 IMPL 计划）、`tests/README.md`（已删的 npm 脚本）里也各有一份。
 * 引用是文档里最容易腐烂、也最容易机器校验的部分，所以这里把它钉死——对不上即红，
 * 逼作者要么改代码要么改文档。
 *
 * 口径：
 * 1. 范围是**全部受跟踪的 `*.md`**（`git ls-files '*.md'`），新增文档自动纳入；
 * 2. 反引号片段：只认「落在已知顶层目录内」的相对路径，或顶层已知文件名；
 * 3. 含空格/通配/URL/绝对路径/等号/尖括号占位符的片段一律跳过（那是命令、环境变量或示例值）；
 * 4. 生成物（`out`、二进制、嵌入产物）允许缺席，但必须在文档里被描述成构建产物；
 * 5. Markdown 相对链接（无 scheme、不以 `/` 开头）按**文档所在目录**解析后必须存在；
 * 6. 文档里写的 `bun run <script>` 必须是 `package.json` 里真实存在的脚本；
 * 7. 已删除的历史路径按本仓惯例**不加反引号**（例如「原 src/app/v1/_lib/... 已删除」）。
 */

const ROOT = process.cwd();

/** 允许「文档里提到但干净检出时可能还没有」的构建产物（生成物，不入库）。 */
const BUILD_OUTPUT_ALLOWLIST = new Set(["out", "go/cchd", "go/internal/uiapp/assets"]);

/** 路径首段必须落在这些目录内，才被视为仓库内路径。 */
const TOP_LEVEL_DIRS = [
  "go",
  "src",
  "scripts",
  "tests",
  "drizzle",
  "lua",
  "messages",
  "data",
  "dev",
  "public",
  ".github",
];

/** 顶层文件也允许被提到。 */
const ROOT_FILES = [
  "AGENTS.md",
  "README.md",
  "README.en.md",
  "package.json",
  "Makefile",
  "docker-compose.yaml",
  "docker-compose.dev.yaml",
  ".env.example",
  "biome.json",
  "next.config.ts",
  "tsconfig.json",
];

/** 文档里用来表明「这是生成物」的词。 */
const BUILD_OUTPUT_WORDS = ["构建产物", "生成物", "编译产物"];

/** 解析量下限：正则或遍历失效时不能静默变绿（实测值的约一半）。 */
const MIN_DOCS = 20;
const MIN_PATH_CANDIDATES = 80;
const MIN_LINK_TARGETS = 40;
const MIN_BUN_RUN_MENTIONS = 20;

function trackedMarkdownFiles(): string[] {
  try {
    return execSync("git ls-files '*.md'", { cwd: ROOT, encoding: "utf8" })
      .trim()
      .split("\n")
      .filter(Boolean);
  } catch (error) {
    throw new Error(`无法列出受跟踪文档（需在仓库根、且可运行 git）：${String(error)}`);
  }
}

function extractBacktickedPaths(markdown: string): string[] {
  const found = new Set<string>();
  for (const match of markdown.matchAll(/`([^`\n]+)`/g)) {
    const raw = (match[1] ?? "").trim();
    if (!raw) continue;
    // 去掉紧贴在末尾的标点（中文顿号、逗号、句号等）。
    const candidate = raw.replace(/[，、。；：,.;:]+$/u, "").replace(/\/+$/u, "");
    if (!candidate) continue;
    if (/\s/u.test(candidate)) continue; // 命令、提交信息、多词短语
    if (/[*?]/u.test(candidate)) continue; // 通配
    if (candidate.includes("://")) continue; // URL
    if (candidate.startsWith("/") || candidate.startsWith("~")) continue; // 绝对路径
    if (candidate.includes("=")) continue; // 环境变量赋值
    if (/[<>]/u.test(candidate)) continue; // 占位符，如 messages/<locale>
    const first = candidate.split("/")[0];
    if (first === candidate) {
      // 顶层文件：只认白名单里的确切名字（否则会把 `usage_ledger`、`_journal.json` 当路径）。
      if (!ROOT_FILES.includes(candidate)) continue;
    } else if (!TOP_LEVEL_DIRS.includes(first)) {
      continue;
    }
    found.add(candidate);
  }
  return [...found];
}

function extractLinkTargets(markdown: string): string[] {
  const found = new Set<string>();
  for (const match of markdown.matchAll(/!?\[[^\]]*\]\(([^)]+)\)/g)) {
    const raw = (match[1] ?? "").trim();
    if (!raw) continue;
    const target = raw.split("#")[0]?.trim() ?? "";
    if (!target) continue; // 纯锚点
    if (target.startsWith("<")) continue; // 尖括号包起来的（含空格）不解析
    if (/^[a-zA-Z][a-zA-Z\d+.-]*:/u.test(target)) continue; // 有 scheme（http、mailto…）
    if (target.startsWith("/") || target.startsWith("~")) continue; // 站点绝对路径
    found.add(target);
  }
  return [...found];
}

function extractBunRunScripts(markdown: string): string[] {
  const found = new Set<string>();
  for (const match of markdown.matchAll(/\bbun run ([a-zA-Z0-9:_-]+)/g)) {
    const name = match[1];
    if (name) found.add(name);
  }
  return [...found];
}

const documents = trackedMarkdownFiles().map((file) => ({
  file,
  markdown: readFileSync(path.join(ROOT, file), "utf8"),
}));

const allPathCandidates: { file: string; candidate: string }[] = [];
const allLinkTargets: { file: string; target: string }[] = [];
const allBunRunScripts: { file: string; script: string }[] = [];
for (const { file, markdown } of documents) {
  for (const candidate of extractBacktickedPaths(markdown))
    allPathCandidates.push({ file, candidate });
  for (const target of extractLinkTargets(markdown)) allLinkTargets.push({ file, target });
  for (const script of extractBunRunScripts(markdown)) allBunRunScripts.push({ file, script });
}

const agentsMarkdown = readFileSync(path.join(ROOT, "AGENTS.md"), "utf8");
const mentionedPaths = extractBacktickedPaths(agentsMarkdown);

describe("全仓文档的引用完整性", () => {
  test("解析量达标（防空跑：遍历或正则失效时不能静默变绿）", () => {
    expect(documents.length).toBeGreaterThan(MIN_DOCS);
    expect(allPathCandidates.length).toBeGreaterThan(MIN_PATH_CANDIDATES);
    expect(allLinkTargets.length).toBeGreaterThan(MIN_LINK_TARGETS);
    expect(allBunRunScripts.length).toBeGreaterThan(MIN_BUN_RUN_MENTIONS);
  });

  test("每份受跟踪文档提到的仓库内路径都真实存在（生成物见白名单）", () => {
    const missing = allPathCandidates
      .filter(
        ({ candidate }) =>
          !BUILD_OUTPUT_ALLOWLIST.has(candidate) && !existsSync(path.join(ROOT, candidate))
      )
      .map(({ file, candidate }) => `${file}: ${candidate}`);
    expect(missing, `文档提到了不存在的路径：\n${missing.join("\n")}`).toEqual([]);
  });

  test("每份受跟踪文档里的相对链接都指向存在的目标", () => {
    const missing = allLinkTargets
      .filter(({ file, target }) => !existsSync(path.resolve(ROOT, path.dirname(file), target)))
      .map(({ file, target }) => `${file}: ${target}`);
    expect(missing, `相对链接指向不存在的目标：\n${missing.join("\n")}`).toEqual([]);
  });

  test("文档里写的 bun run 脚本都存在于 package.json", () => {
    const packageJson = JSON.parse(readFileSync(path.join(ROOT, "package.json"), "utf8")) as {
      scripts?: Record<string, string>;
    };
    const scripts = new Set(Object.keys(packageJson.scripts ?? {}));
    const missing = allBunRunScripts
      .filter(({ script }) => !scripts.has(script))
      .map(({ file, script }) => `${file}: bun run ${script}`);
    expect(missing, `文档引用了不存在的 npm 脚本：\n${missing.join("\n")}`).toEqual([]);
  });
});

describe("AGENTS.md 的路径钉子", () => {
  test("解析出足量路径（防空跑：正则失效时不能静默变绿）", () => {
    expect(mentionedPaths.length).toBeGreaterThan(30);
  });

  test("提到的每个仓库内路径都真实存在（生成物见白名单）", () => {
    const missing = mentionedPaths.filter(
      (candidate) =>
        !BUILD_OUTPUT_ALLOWLIST.has(candidate) && !existsSync(path.join(ROOT, candidate))
    );
    expect(missing, `文档提到了不存在的路径：${missing.join("、")}`).toEqual([]);
  });

  test("白名单里的生成物在文档里被描述成构建产物（防把源码混进白名单）", () => {
    for (const artifact of BUILD_OUTPUT_ALLOWLIST) {
      const line = agentsMarkdown
        .split("\n")
        .find((l) => l.includes(`\`${artifact}\``) || l.includes(artifact));
      expect(line, `文档未提到生成物 ${artifact}；若已不再需要，请从白名单删除`).toBeDefined();
      const context = agentsMarkdown
        .split("\n")
        .filter((l) => l.includes(artifact))
        .join("\n");
      expect(
        BUILD_OUTPUT_WORDS.some((w) => context.includes(w)),
        `${artifact} 在文档里未被描述为构建产物`
      ).toBe(true);
    }
  });

  test("代理指南只有 AGENTS.md 一份，不得再出现第二份真源", () => {
    // 原先是 `CLAUDE.md` 指针文件；现已删除——两份指南会在改一处漏一处时互相矛盾，
    // 而「哪份为准」本身不该成为需要判断的事。这条断言把这个决定钉住。
    expect(existsSync(path.join(ROOT, "CLAUDE.md")), "CLAUDE.md 已废弃，请勿重新添加").toBe(false);
  });
});
