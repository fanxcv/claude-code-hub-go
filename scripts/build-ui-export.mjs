#!/usr/bin/env bun
/**
 * UI 静态导出管线（配合 next.config.ts 的 CCH_UI_EXPORT=1 开关）。
 *
 * 为什么需要临时移出文件：
 *  - src/app/{api,v1,v1beta} 是 Go 的运行时路由（Node 退役后 Go 侧接管），
 *    产物里一旦有它们、next build 会把 force-dynamic/route.ts 一起编译，导出直接失败。
 *  - src/proxy.ts 是 next-intl 中间件，静态导出不存在中间件，必须移除。
 *  - 尚未去 SSR 的段目录（服务端绑定：getSession/@/repository/@/actions/headers）
 *    无法静态导出，先移出；对应的深层链接由 Go 壳回退兜底（见 go-ui-embed-plan §1/#6、#3）。
 *  - 三条动态段路径构建期无法枚举，同样移出，由 Go 壳回退深链。
 *
 * 移出的文件全部暂存到 .sisyphus/.export-stash-<pid>/，构建结束在 finally 中还原，
 * 不破坏工作树；产物收进 go/internal/uiapp/assets/（.gitignore 忽略，仅留 .gitkeep），
 * 由 go-embed-shell 波次的 `//go:embed all:assets` 收编。
 *
 * 与 standalone 管线共用 .next/ 的隔离（两者必须能共存）：
 * next build 起始会清空 distDir（Next build/index.js 的 cleanDistDir 步骤，只保留 cache/dev/lock/trace 四项），
 * 所以导出构建会把 standalone 管线刚生成的 .next/standalone 一并删掉。故本脚本构建前把
 * .next/standalone 整体 rename 到 .sisyphus/.standalone-stash，finally 里再 rename 回来。
 * 为什么不用独立 distDir：output=="export" 时 Next 会把 config.distDir 强制改回 '.next'
 * （next/dist/export/utils.js 的 hasCustomExportOutput），内部产物照样写进 .next，隔离不掉。
 *
 * 用法（仓库根目录）：
 *   bun scripts/build-ui-export.mjs               # 导出 + 收编到 go/internal/uiapp/assets
 *   cd go && go build -o /tmp/cchd ./cmd/cchd     # 产物 embed 进二进制
 *   DSN=... REDIS_URL=... PORT=18300 CCH_EGRESS_PAGES=embed /tmp/cchd
 *   curl -s http://127.0.0.1:18300/readyz         # 看 pages 结论（模式/文件数/BUILD_ID/locale）
 *
 * 前提：**worktree 里必须在位一份真实 node_modules**。
 *   Turbopack 遇到 `node_modules` 符号链接（指向 worktree 外）会 panic
 *   （`Symlink [project]/node_modules is invalid` / `leaves the filesystem root`），
 *   这不是本脚本的问题；临时做法：`cp -al <主仓>/node_modules ./node_modules`（硬链接，秒级）。
 *
 * 导出时移走什么、为什么：
 *  - `src/app/{api,v1,v1beta}`：Go 的运行时路由，导出无服务端；
 *  - `src/instrumentation.ts`：启动钩子（迁移/Redis 订阅/崩溃处理器），会把服务端链拉进构建；
 *  - **服务端模块闭包**（computeServerOnlyModules）：以 `server-only`/`next/headers`/`next/cache`/
 *    `ioredis`/`postgres`/`drizzle-orm`/`bull` 引用为根，沿值导入边反向传播，直到不动点。
 *    只移路由入口不够——Next 会编译 app 树下的所有 TS 文件，`_lib/**` 会把服务端链留在图里。
 *  - 仍服务端绑定的 page/layout 与三条动态段页：对应深链由 Go 壳回退兜底。
 *  - 两个**导出壳临时替换**（构建后还原）：
 *      `src/app/[locale]/layout.tsx` → 自带 generateStaticParams（导出必需）、不读 headers()/DB；
 *      `src/i18n/request.ts`         → timeZone 不再读 DB（真时区由壳注入）；
 *      `src/app/page.tsx`            → 根页改客户端改写（导出既无 cookies 也无服务端 redirect）。
 *  - 导出构建**不做类型检查**：本仓以 `"use server"` 标注 repository/actions（导出必须移出，否则
 *    Next 报 `Server Actions are not supported with static export`），而客户端组件以 `import type`
 *    从它们取类型。类型门禁由独立的 `bun run typecheck` 把守（导出前请先跑它）。
 *
 * 移出的文件全部暂存到 .sisyphus/.export-stash-<pid>/，构建结束在 finally 中还原（幂等），
 * 不破坏工作树；产物收进 go/internal/uiapp/assets/（.gitignore 忽略，仅留 .gitkeep）。
 */
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
// 语种清单的**单一真源**：直接读 UI 配置，避免在本脚本再写一份（`bun` 与 `node>=22` 都能直接导入 .ts）。
import { defaultLocale, locales, retiredLocales } from "../src/i18n/config.ts";
import { retiredLocaleRedirectHtml } from "../src/i18n/retired-locale.ts";

// 版本号的**注入点**：真源是发布时注入的 APP_VERSION（与镜像 ENV 同一个值，见 go/deploy/Dockerfile*）。
// 把它改名为 NEXT_PUBLIC_APP_VERSION 交给 next build——只有 NEXT_PUBLIC_ 前缀的变量在构建期被
// 内联，页脚那个服务端组件才能把它写进静态产物。
//
// 未注入时退到 package.json 的 `version`（本仓声明的基线值）：它与 Go 侧 `appversion.Fallback`
// 同值，且那份相等关系由 internal/appversion 的用例钉住，于是「本地构建」（不注入）也不会与
// 二进制自报的版本分叉。下面会打印用的是哪个来源，不静默。
const nextVersionEnv = (process.env.APP_VERSION ?? process.env.NEXT_PUBLIC_APP_VERSION ?? "").trim();
const ROOT = process.cwd();
if (!fs.existsSync(path.join(ROOT, "next.config.ts"))) {
  console.error(`[build-ui-export] 必须在仓库根目录运行（当前 ${ROOT}）`);
  process.exit(1);
}

const STASH_ROOT = path.join(ROOT, ".sisyphus");
const STASH = path.join(STASH_ROOT, `.export-stash-${process.pid}`);
// standalone 产物的暂存点（见文件头的隔离说明）：与逐文件暂存树分开，避免被 restoreAll 误当普通文件搬回。
const STANDALONE_DIR = path.join(ROOT, ".next", "standalone");
const STANDALONE_STASH = path.join(STASH_ROOT, ".standalone-stash");
const OUT_DIR = path.join(ROOT, "out");
const ASSETS_DIR = path.join(ROOT, "go", "internal", "uiapp", "assets");
const LOCALE_DIR = "src/app/[locale]";

// appVersion 是最终写进产物的那个值；versionSource 只用于打印（非静默：一眼能看出用的是注入值还是声明值）。
function declaredVersion() {
  try {
    return String(JSON.parse(fs.readFileSync(path.join(ROOT, "package.json"), "utf-8")).version ?? "");
  } catch {
    return "";
  }
}

const appVersion = nextVersionEnv || declaredVersion().trim();
const versionSource = nextVersionEnv ? "APP_VERSION" : appVersion ? "package.json" : "缺失";
// 静态导出（trailingSlash: true）下每个路由目录里的壳名；与 Go 壳的 rootShell 同值。
const rootShellName = "index.html";

// 恒定排除：Go 运行时路由（只移路由文件，`_lib` 等共享模块必须在位——客户端会引用）。
const ROUTE_DIRS = ["src/app/api", "src/app/v1", "src/app/v1beta"];
// 静态导出没有 Node 进程，故一律移出：
//  - instrumentation.ts：启动钩子（迁移/Redis 订阅/崩溃处理器），并会把 drizzle/replay 等服务端链拉进构建；
//  - 下列三个只被「已移出的路由或根布局」引用的服务端模块（留着重则引用已移出的路由，tsc 报 TS2307）：
//    health/checker ← api/health*、api/actions；public-api-loader ← layout-metadata；layout-metadata ← 根布局（已被导出壳替换）。
const FIXED_EXCLUDES = [
  "src/proxy.ts",
  "src/instrumentation.ts",
  "src/lib/health/checker.ts",
  "src/lib/public-status/public-api-loader.ts",
  "src/lib/public-status/layout-metadata.ts",
];

// 动态段路径：客户端路由，构建期无法枚举（generateStaticParams 缺失），由 Go 壳深链兜底。
const DYNAMIC_SEGMENTS = [
  path.join(LOCALE_DIR, "status/[slug]"),
  path.join(LOCALE_DIR, "dashboard/sessions/[sessionId]"),
  path.join(LOCALE_DIR, "dashboard/leaderboard/user/[userId]"),
];

const ROOT_LAYOUT = path.join(LOCALE_DIR, "layout.tsx");

/** 判断某 page/layout 是否服务端绑定（静态导出下不可编译）。 */
function isServerBound(text) {
  const lines = text.split("\n").filter((l) => !/^\s*import\s+type\b/.test(l));
  const joined = lines.join("\n");
  if (/export const dynamic = "force-dynamic"/.test(text)) return true;
  // 运行时（非 type-only）引用服务端模块
  if (/from\s+["']@\/(?:lib\/auth|repository\/|actions\/)/.test(joined)) return true;
  if (/from\s+["']next\/(?:headers|cookies)["']/.test(joined)) return true;
  return false;
}

/**
 * 服务端模块判定：从「必然是服务端」的根出发，沿值导入边反向传播（不动点）。
 *
 * 为何要闭包：静态导出没有 Node（无中间件、无路由、无 server action），
 * 而 Next 会编译 app 树下的所有 TS 文件（不只 page/layout）。只移路由入口会把
 * `_lib/**`、`lib/**` 里的服务端链留在图里（上一波就卡在这）。
 */
const SERVER_IMPORT_SPECS = [
  "server-only",
  "next/headers",
  "next/cookies",
  "next/cache",
  "ioredis",
  "postgres",
  "drizzle-orm",
  "bull",
  "bullmq",
];
const SERVER_ROOT_DIRS = ["src/repository/", "src/drizzle/", "src/actions/", "src/app/api/", "src/app/v1/", "src/app/v1beta/"];
/** 构建期必需的服务端代码：`src/i18n/**` 是 next-intl 的构建期配置（导出壳调用 getMessages 时用到），不能移出。 */
const BUILD_TIME_KEEP_DIRS = ["src/i18n/"];

/** 计算服务端模块集合（含闭包）。 */
function computeServerOnlyModules() {
  const files = listSourceFiles(path.join(ROOT, "src")).map((p) => path.relative(ROOT, p));
  const set = new Set(files);

  const resolveSpec = (from, spec) => {
    let base;
    if (spec.startsWith("@/")) base = path.join("src", spec.slice(2));
    else if (spec.startsWith(".")) base = path.normalize(path.join(path.dirname(from), spec));
    else return null;
    for (const cand of [
      base,
      `${base}.ts`,
      `${base}.tsx`,
      path.join(base, "index.ts"),
      path.join(base, "index.tsx"),
    ]) {
      if (set.has(cand)) return cand;
    }
    return null;
  };

  const imports = new Map();
  for (const f of files) {
    const text = fs.readFileSync(path.join(ROOT, f), "utf8");
    const specs = [
      ...text.matchAll(/(?:^|\n)\s*import\s+(?!(?:type)\s)(?:[^"'\n]*?from\s*)?["']([^"']+)["']/g),
    ].map((m) => m[1]);
    for (const m of text.matchAll(/import\(\s*["']([^"']+)["']\s*\)/g)) specs.push(m[1]);
    imports.set(f, { specs, targets: specs.map((s) => resolveSpec(f, s)).filter(Boolean) });
  }

  const server = new Set();
  for (const f of files) {
    const abs = path.join(ROOT, f);
    const head = fs.readFileSync(abs, "utf8").split("\n").slice(0, 3).join("\n");
    if (/^\s*["']use server["'];?/m.test(head)) server.add(f);
    if (SERVER_ROOT_DIRS.some((d) => f.startsWith(d))) server.add(f);
    if (imports.get(f).specs.some((s) => SERVER_IMPORT_SPECS.includes(s))) server.add(f);
  }
  for (let round = 0; ; round++) {
    const before = server.size;
    for (const f of files) {
      if (server.has(f)) continue;
      if (imports.get(f).targets.some((t) => server.has(t))) server.add(f);
    }
    if (server.size === before) break;
  }
  return server;
}

/** 列出 src 下所有 TS/TSX 源文件（排除测试）。 */
function listSourceFiles(dir, acc = []) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) listSourceFiles(p, acc);
    else if (/\.(ts|tsx)$/.test(entry.name) && !/\.test\./.test(entry.name)) acc.push(p);
  }
  return acc;
}

/** 递归收集 [locale] 下的 page.tsx / layout.tsx。 */
function collectPageOrLayout(dir, acc = []) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) collectPageOrLayout(p, acc);
    else if (entry.name === "page.tsx" || entry.name === "layout.tsx") acc.push(p);
  }
  return acc;
}

/** 把路径移入暂存（目录逐文件搬运，避免父段与已暂存子段抢同名目录），返回是否搬动过。 */
function stash(rel) {
  const abs = path.join(ROOT, rel);
  if (!fs.existsSync(abs)) return false;
  const dst = path.join(STASH, rel);
  if (fs.statSync(abs).isDirectory()) {
    let any = false;
    for (const name of fs.readdirSync(abs)) {
      if (stash(path.join(rel, name))) any = true;
    }
    // 内容搬空后删掉空壳目录，留空壳会干扰后续向导
    if (fs.existsSync(abs) && fs.readdirSync(abs).length === 0) {
      fs.rmSync(abs, { recursive: true, force: true });
    }
    return any;
  }
  if (fs.existsSync(dst)) return false;
  fs.mkdirSync(path.dirname(dst), { recursive: true });
  fs.renameSync(abs, dst);
  return true;
}

/**
 * 把暂存树整体搬回原位（finally 中调用，幂等）。
 *
 * 实现要点：先把暂存里的文件 rename 到目标（同设备），再删空目录——
 * 直接用 cp 会丢权限与 mtime，而 `mv dir target` 在 target 已被占时会 ENOTEMPTY。
 */
function restoreAll() {
  if (!fs.existsSync(STASH)) return;
  const files = [];
  const dirs = [];
  const walkStash = (dir) => {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        walkStash(p);
        dirs.push(p);
      } else {
        files.push(p);
      }
    }
  };
  walkStash(STASH);
  let restored = 0;
  for (const src of files) {
    const rel = path.relative(STASH, src);
    const dst = path.join(ROOT, rel);
    fs.mkdirSync(path.dirname(dst), { recursive: true });
    fs.renameSync(src, dst);
    restored++;
  }
  // 目录自深而浅删空壳
  for (const d of dirs.sort((a, b) => b.length - a.length)) {
    try {
      fs.rmdirSync(d);
    } catch {
      // 非空目录说明有未纳入暂存的产物，留着不报
    }
  }
  fs.rmSync(STASH, { recursive: true, force: true });
  console.log(`[build-ui-export] 暂存已清空（还原 ${restored} 个文件）`);
}

/** 导出模式专用 i18n 配置（临时替换 src/i18n/request.ts，构建后还原）。
 *  差别只在 timeZone：原实现要读 DB（system_settings.timezone），静态导出无 DB；
 *  真实时区由 Go 壳在 `__CCH_BOOTSTRAP__.timeZone` 注入。 */
const EXPORT_I18N_REQUEST = `/**
 * i18n Request Configuration（导出模式临时版，由 scripts/build-ui-export.mjs 写入，构建后还原）。
 *
 * 与常规版的唯一差别：timeZone 不再读 DB（静态导出无服务端），构建期用 env TZ 兜底 UTC。
 * 运行时的真实时区由 Go 壳在 window.__CCH_BOOTSTRAP__.timeZone 注入。
 */
import { getRequestConfig } from "next-intl/server";
import type { Locale } from "./config";
import { routing } from "./routing";

export default getRequestConfig(async ({ requestLocale }) => {
  let locale = await requestLocale;
  if (!locale || !routing.locales.includes(locale as Locale)) {
    locale = routing.defaultLocale;
  }
  const messages = await import(\`../../messages/\${locale}\`).then((module) => module.default);
  return {
    locale,
    messages,
    timeZone: process.env.TZ || "UTC",
    now: new Date(),
    getMessageFallback: ({ namespace, key }) => \`\${namespace}.\${key}\`,
  };
});
`;

/** 导出模式专用根页（临时替换 src/app/page.tsx，构建后还原）。
 *  原页用 cookies() 读 locale cookie 后 redirect()——静态导出既无 cookies 也无服务端 redirect，
 *  故改成客户端改写：先看 cookie，再按浏览器语言，最后落到默认 locale。 */
const EXPORT_ROOT_PAGE = `"use client";

import { useEffect } from "react";
import { defaultLocale, locales, localeCookieName } from "@/i18n/config";

/** 导出模式根页（scripts/build-ui-export.mjs 临时写入，构建后还原）。 */
export default function RootPage() {
  useEffect(() => {
    const cookieLocale = document.cookie
      .split("; ")
      .find((row) => row.startsWith(\`\${localeCookieName}=\`))
      ?.split("=")[1];
    const nav = navigator.languages?.find((l) =>
      locales.includes(l as (typeof locales)[number]),
    );
    const target =
      (cookieLocale && locales.includes(cookieLocale as (typeof locales)[number])
        ? cookieLocale
        : nav) || defaultLocale;
    window.location.replace(\`/\${target}/dashboard\`);
  }, []);

  return null;
}
`;

/** 导出模式专用根布局（临时替换 [locale]/layout.tsx，构建后还原）：静态导出不支持 headers()。 */
const EXPORT_ROOT_LAYOUT = `import "../globals.css";
import type { Metadata } from "next";
import { type ReactNode } from "react";
import { setRequestLocale } from "next-intl/server";
import { Footer } from "@/components/customs/footer";
import { I18nProvider } from "@/components/i18n-provider";
import { Toaster } from "@/components/ui/sonner";
import { locales } from "@/i18n/config";
import { DEFAULT_SITE_TITLE } from "@/lib/site-title";
import { AppProviders } from "../providers";

// 导出模式专用壳（scripts/build-ui-export.mjs 临时写入，构建后还原原 layout）。
// 静态导出不支持 headers()；站点标题等由 Go 壳在 __CCH_BOOTSTRAP__ 注入（go-ui-embed-plan §1/#5）。
// 词表不由服务端传给 provider——否则会被序列化进每个路由的 RSC payload（见 @/components/i18n-provider）。

export function generateStaticParams() {
  return locales.map((locale) => ({ locale }));
}

export async function generateMetadata({
  params,
}: {
  params: Promise<{ locale: string }>;
}): Promise<Metadata> {
  const { locale } = await params;
  return {
    title: DEFAULT_SITE_TITLE,
    description: DEFAULT_SITE_TITLE,
  };
}

export default async function RootLayout({
  children,
  params,
}: Readonly<{ children: ReactNode; params: Promise<{ locale: string }> }>) {
  const { locale } = await params;
  setRequestLocale(locale);
  // 真时区由 Go 壳在 window.__CCH_BOOTSTRAP__ 注入；构建期以 env TZ 兜底。
  const timeZone = process.env.TZ || "UTC";
  return (
    <html lang={locale} suppressHydrationWarning>
      <body className="antialiased">
        <I18nProvider locale={locale} timeZone={timeZone} now={new Date()}>          <AppProviders>
            <div className="flex min-h-[var(--cch-viewport-height,100vh)] flex-col bg-background text-foreground">
              <div className="flex-1">{children}</div>
              <Footer />
            </div>
            <Toaster />
          </AppProviders>
        </I18nProvider>
      </body>
    </html>
  );
}
`;

function dirSizeMb(dir) {
  let bytes = 0;
  for (const rel of fs.readdirSync(dir, { recursive: true })) {
    const st = fs.statSync(path.join(dir, String(rel)));
    if (st.isFile()) bytes += st.size;
  }
  return (bytes / 1024 / 1024).toFixed(1);
}

function countFiles(dir) {
  let n = 0;
  for (const rel of fs.readdirSync(dir, { recursive: true })) {
    if (fs.statSync(path.join(dir, String(rel))).isFile()) n++;
  }
  return n;
}

const moved = [];
// standalone 是否已暂存（finally 要读，故不能在 try 里声明）。
let standaloneStashed = false;
const routeFiles = [];
try {
  // 0) 先护住 standalone 产物：导出构建的 cleanDistDir 会清空 .next（含 .next/standalone）。
  //    为什么不用独立 distDir：output=="export" 时 Next 会把 config.distDir 强制改回 '.next'。
  if (fs.existsSync(STANDALONE_DIR)) {
    fs.mkdirSync(STASH_ROOT, { recursive: true });
    fs.rmSync(STANDALONE_STASH, { recursive: true, force: true });
    fs.renameSync(STANDALONE_DIR, STANDALONE_STASH);
    standaloneStashed = true;
    console.log("[build-ui-export] 已暂存 .next/standalone（导出构建清空 .next 前先挪出）");
  }

  // 1) 恒定排除：运行时路由目录与中间件
  for (const dir of ROUTE_DIRS) {
    if (stash(dir)) routeFiles.push(dir);
  }
  for (const rel of FIXED_EXCLUDES) {
    if (stash(rel)) moved.push(rel);
  }
  // 动态段：只移该段的 page.tsx（同段 _components 可能被别处引用，保留）
  for (const rel of DYNAMIC_SEGMENTS) {
    for (const abs of collectPageOrLayout(path.join(ROOT, rel))) {
      if (stash(path.relative(ROOT, abs))) moved.push(path.relative(ROOT, abs));
    }
  }
  // 1.5) 服务端模块闭包：整个服务端链移出（静态导出无 Node）。
  //      仍留存的文件若引用它们，构建会以 `Module not found` 指名报出，不会静默。
  const serverOnlyBefore = new Set();
  const serverModules = computeServerOnlyModules();
  for (const rel of serverModules) {
    if (BUILD_TIME_KEEP_DIRS.some((d) => rel.startsWith(d))) continue;
    if (fs.existsSync(path.join(ROOT, rel)) && stash(rel)) {
      moved.push(rel);
      serverOnlyBefore.add(rel);
    }
  }
  // 构建期 i18n 配置改成无 DB 版（真时区由壳注入），构建后还原
  const I18N_REQUEST = "src/i18n/request.ts";
  if (stash(I18N_REQUEST)) {
    fs.writeFileSync(path.join(ROOT, I18N_REQUEST), EXPORT_I18N_REQUEST);
    moved.push(`${I18N_REQUEST}（导出壳临时替换，构建后还原）`);
  }
  // 根页同样要产一个静态入口（原页用 cookies()+redirect()，导出下不存在）。
  // 它在服务端闭包里已被移出，故先 stash（幂等）再无条件写导出版。
  const ROOT_PAGE = "src/app/page.tsx";
  stash(ROOT_PAGE);
  fs.writeFileSync(path.join(ROOT, ROOT_PAGE), EXPORT_ROOT_PAGE);
  moved.push(`${ROOT_PAGE}（导出壳临时替换，构建后还原）`);
  console.log(`[build-ui-export] 服务端模块闭包 ${serverOnlyBefore.size} 个文件`);
  console.log(`[build-ui-export] 已移出 ${routeFiles.length} 个运行时路由目录`);

  // 2) 扫描 [locale] 下服务端绑定的 page/layout → 移出该文件（深链由 Go 壳回退；
  //    同段 `_components` 常被其他页引用，故只移单个 page/layout 文件）。
  for (const file of collectPageOrLayout(LOCALE_DIR)) {
    if (file === ROOT_LAYOUT) continue; // 根布局单独处理（见下）
    if (!fs.existsSync(file)) continue;
    const text = fs.readFileSync(file, "utf8");
    if (isServerBound(text)) {
      if (stash(path.relative(ROOT, file))) moved.push(path.relative(ROOT, file));
    }
  }
  // 2.5) 根布局换成导出壳：自带 generateStaticParams（导出必需），且不依赖 headers()/DB。
  //      注意：原文件可能已在第 1.5 步被移出（服务端闭包），故先 stash 再无条件写壳。
  stash(ROOT_LAYOUT);
  fs.mkdirSync(path.dirname(ROOT_LAYOUT), { recursive: true });
  fs.writeFileSync(ROOT_LAYOUT, EXPORT_ROOT_LAYOUT);
  moved.push(`${ROOT_LAYOUT}（导出壳临时替换，构建后还原）`);

  console.log(`[build-ui-export] 已移出 ${moved.length} 项：`);
  for (const m of moved) console.log(`  - ${m}`);

  // 3) 导出构建
  if (appVersion === "") {
    console.warn("[build-ui-export] 版本缺失（APP_VERSION 与 package.json 都拿不到）：页脚将不显示版本号");
  } else {
    console.log(`[build-ui-export] 版本注入：v${appVersion.replace(/^v/i, "")}（来源 ${versionSource}）`);
  }
  console.log("[build-ui-export] next build (CCH_UI_EXPORT=1) ...");
  execFileSync(
    process.env.BUN_BIN || "bun",
    ["x", "next", "build"],
    {
      cwd: ROOT,
      env: {
        ...process.env,
        CCH_UI_EXPORT: "1",
        NEXT_TELEMETRY_DISABLED: "1",
        // 页脚版本随产物落盘（见文件头的 appVersion 说明）。
        NEXT_PUBLIC_APP_VERSION: appVersion,
      },
      stdio: "inherit",
    },
  );

  // 4) 统计并收编产物
  const htmlFiles = fs
    .readdirSync(OUT_DIR, { recursive: true })
    .filter((p) => String(p).endsWith(".html"))
    .map((p) => String(p));
  console.log("[build-ui-export] 导出页数（.html）：", htmlFiles.length);
  console.log("[build-ui-export] 产物大小：", dirSizeMb(OUT_DIR), "MiB，文件数", countFiles(OUT_DIR));

  // 4.5) 退役语种重定向壳
  //
  // 为何必须有这些文件：Go 壳的 resolve() 对「已注册 locale 前缀 + 未命中路径」会返回
  // `<locale>/index.html`（go/internal/uiapp/uiapp.go 的 resolve）。而注册集来自 Go 侧
  // `guard.SupportedLocales`，**不与产物求交**。两者不一致时（本仓现状：Go 仍列 5 个），
  // 若产物里没有该前缀的壳，这个 URL 会取到零值 asset ⇒ **空 200**（既不是重定向也不是 404）。
  // 写入一份壳后，旧链接（含深层路径、查询与片段）会被带到默认语言的等价路径。
  const redirectShells = [];
  for (const locale of retiredLocales) {
    // 防误覆盖：把已退役语种重新放回 locales 后，Next 会真导出该语种，本步会把它的首页重定向壳
    // 盖掉真页面。宁可失败，也不静默換成重定向。
    if (locales.includes(locale)) {
      throw new Error(
        `[build-ui-export] ${locale} 同时在 locales 与 retiredLocales 里：两份清单必须互斥`,
      );
    }
    const shellPath = path.join(OUT_DIR, locale, rootShellName);
    if (fs.existsSync(shellPath)) {
      throw new Error(
        `[build-ui-export] ${locale} 已有导出产物（${path.relative(ROOT, shellPath)}），拒绝用重定向壳覆盖`,
      );
    }
    fs.mkdirSync(path.dirname(shellPath), { recursive: true });
    fs.writeFileSync(shellPath, retiredLocaleRedirectHtml());
    redirectShells.push(`${locale}/${rootShellName}`);
  }
  console.log(
    `[build-ui-export] 退役语种重定向壳：${redirectShells.length} 个（${redirectShells.join(", ")}）`,
  );

  fs.rmSync(ASSETS_DIR, { recursive: true, force: true });
  fs.mkdirSync(ASSETS_DIR, { recursive: true });
  const buildId = fs.readFileSync(path.join(ROOT, ".next", "BUILD_ID"), "utf8").trim();
  // 压缩收编：默认走 brotli 压缩态（142 MiB -> ~33 MiB，见 scripts/build-ui-embed.mjs）。
  // CCH_UI_EMBED_RAW=1 时改为原样复制（回退到未压缩嵌入，行为与压缩前一致）。
  if (process.env.CCH_UI_EMBED_RAW === "1") {
    fs.cpSync(OUT_DIR, ASSETS_DIR, { recursive: true });
    console.log("[build-ui-export] CCH_UI_EMBED_RAW=1：产物原样收编（未压缩）");
  } else {
    execFileSync(
      process.env.BUN_BIN || "bun",
      [
        "scripts/build-ui-embed.mjs",
        "--src",
        OUT_DIR,
        "--dst",
        ASSETS_DIR,
        "--build-id",
        buildId,
      ],
      { cwd: ROOT, stdio: "inherit" },
    );
  }
  // 产物目录靠 .gitignore 忽略，但 .gitkeep 必须保留（它让目录入 git）
  fs.writeFileSync(path.join(ASSETS_DIR, ".gitkeep"), "");
  if (process.env.CCH_UI_EMBED_RAW === "1") {
    fs.writeFileSync(path.join(ASSETS_DIR, "BUILD_ID"), `${buildId}\n`);
  }
  console.log(`[build-ui-export] 产物已收进 ${path.relative(ROOT, ASSETS_DIR)}/ （BUILD_ID=${buildId}）`);

  // 5) BUILD_ID 摘要打印
  const sample = htmlFiles.slice(0, 8);
  console.log("[build-ui-export] 页样：");
  for (const s of sample) console.log(`  - ${s}`);
} finally {
  // 6) 无论如何还原（先撤掉临时根布局壳，再整体搬回）
  const origLayout = path.join(STASH, ROOT_LAYOUT);
  if (fs.existsSync(origLayout)) {
    fs.rmSync(ROOT_LAYOUT, { force: true });
  }
  const origI18n = path.join(STASH, "src/i18n/request.ts");
  if (fs.existsSync(origI18n)) {
    fs.rmSync(path.join(ROOT, "src/i18n/request.ts"), { force: true });
  }
  const origRootPage = path.join(STASH, "src/app/page.tsx");
  if (fs.existsSync(origRootPage)) {
    fs.rmSync(path.join(ROOT, "src/app/page.tsx"), { force: true });
  }
  restoreAll();
  // 7) 还原 standalone（导出构建清空 .next 前挪出的那棵子树）
  if (standaloneStashed && fs.existsSync(STANDALONE_STASH)) {
    fs.mkdirSync(path.dirname(STANDALONE_DIR), { recursive: true });
    fs.rmSync(STANDALONE_DIR, { recursive: true, force: true });
    fs.renameSync(STANDALONE_STASH, STANDALONE_DIR);
    console.log("[build-ui-export] 已还原 .next/standalone");
  }
  console.log(
    `[build-ui-export] 完成：${moved.length} 项已还原；检查 git status 确认工作树干净（out/ 未清理）。`,
  );
}
