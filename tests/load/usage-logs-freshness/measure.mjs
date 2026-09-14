#!/usr/bin/env node
/**
 * 使用记录页刷新时延实测（浏览器/DOM 级）。
 *
 * 为什么必须用浏览器测：服务端耗时不是用户看到的东西。列表查询服务端只要 15–21ms，但每页
 * 载荷 284KB、公网 p50 861ms，且「统计只在手动
 * 刷新时更新」这类问题只体现在 DOM 上。故判据取「新行出现在列表顶部的墙钟时延」。
 *
 * 用法（需先有可访问的部署，且该部署包含本项目的前端改动与 Go 侧 sinceId/asc、SSE 端点）：
 *
 *   CCH_BASE=https://cch.example.com \
 *   CCH_MEASURE_COOKIE='<登录后的会话 Cookie 头>' \
 *   CCH_PRODUCE_CMD='curl -s -o /dev/null -H "Authorization: Bearer $CCH_KEY" ...' \
 *   node tests/load/usage-logs-freshness/measure.mjs
 *
 * 环境变量：
 *   CCH_BASE            站点根（默认 http://127.0.0.1:3000）
 *   CCH_MEASURE_COOKIE  会话 Cookie（形如 `cch_session=...`）；注入后可跳过登录
 *   CCH_MODE            pull | push | both（默认 both）
 *   CCH_INTERVAL_MS     期望的刷新间隔（默认 3000，用于判定「≤1 个周期」）
 *   CCH_PRODUCE_CMD     可选：每次测量前执行的外部命令，用来**造一条真实新记录**
 *                       （例如向上游打一发最小请求）。不提供时脚本会提示无法测量。
 *
 * 退出码：0 = 两种模式都达标；1 = 有模式未达标或无法测量。
 */
import { chromium } from "playwright";

const BASE = process.env.CCH_BASE ?? "http://127.0.0.1:3000";
const COOKIE = process.env.CCH_MEASURE_COOKIE ?? "";
const MODE = process.env.CCH_MODE ?? "both";
const INTERVAL_MS = Number(process.env.CCH_INTERVAL_MS ?? 3000);
const PRODUCE_CMD = process.env.CCH_PRODUCE_CMD ?? "";

/** 页面里设置刷新模式与间隔（写 localStorage，键与 `_utils/logs-refresh.ts` 一致）。 */
const STORAGE_KEY_PREFIX = "cch:logs-refresh";

function parseCookieHeader(header) {
  return header
    .split(";")
    .map((part) => part.trim())
    .filter(Boolean)
    .map((part) => {
      const index = part.indexOf("=");
      return { name: part.slice(0, index), value: part.slice(index + 1), url: BASE };
    });
}

async function produceRow() {
  if (!PRODUCE_CMD) return false;
  const { execSync } = await import("node:child_process");
  execSync(PRODUCE_CMD, { stdio: "ignore", timeout: 60_000 });
  return true;
}

/** 读列表顶行的行键（用行文本首列的时间/id 做标识）。 */
async function topRowKey(page) {
  return page.evaluate(() => {
    const first = document.querySelector("[data-index='0']");
    return first ? (first.textContent ?? "").slice(0, 120) : null;
  });
}

/**
 * 测一次：取顶行 → 造新记录 → 轮询 DOM 直到顶行变化 → 记录时延。
 * 判据是「顶行变化」而不是「包含某 id」：行文本里没有稳定 id 列，而新记录必然插在顶部。
 */
async function measureOnce(page, label) {
  const before = await topRowKey(page);
  const producedAt = Date.now();
  const produced = await produceRow();
  if (!produced) {
    return { label, skipped: true, reason: "未提供 CCH_PRODUCE_CMD，无法造新记录" };
  }

  const deadline = Date.now() + Math.max(INTERVAL_MS * 6, 20_000);
  while (Date.now() < deadline) {
    const now = await topRowKey(page);
    if (now && now !== before) {
      return {
        label,
        latencyMs: Date.now() - producedAt,
        withinOneInterval: Date.now() - producedAt <= INTERVAL_MS + 1500,
      };
    }
    await page.waitForTimeout(200);
  }
  return { label, latencyMs: null, timedOut: true };
}

async function runMode(page, mode) {
  // 打开页面并写入偏好（模式 + 间隔），随后重新加载使偏好生效。
  await page.goto(`${BASE}/dashboard/logs`, { waitUntil: "domcontentloaded" });
  await page.evaluate(
    ({ prefix, mode, intervalMs }) => {
      const raw = window.localStorage.getItem("cch:measure-user");
      // 用户 id 未知时用 0；页面读的是 `cch:logs-refresh:<userId>`，故同时写多个常见键。
      const ids = raw ? [Number(raw)] : [0, 1];
      for (const id of ids) {
        window.localStorage.setItem(`${prefix}:${id}`, JSON.stringify({ mode, intervalMs }));
      }
    },
    { prefix: STORAGE_KEY_PREFIX, mode, intervalMs: INTERVAL_MS }
  );
  await page.reload({ waitUntil: "networkidle" });
  await page.waitForSelector("[data-index='0']", { timeout: 30_000 });

  const samples = [];
  for (let i = 0; i < 3; i += 1) {
    samples.push(await measureOnce(page, `${mode}#${i + 1}`));
  }

  const ok = samples.filter((sample) => sample.latencyMs !== null);
  return {
    mode,
    samples,
    medianMs: ok.length
      ? [...ok.map((sample) => sample.latencyMs)].sort((a, b) => a - b)[Math.floor(ok.length / 2)]
      : null,
    allWithinOneInterval: ok.length > 0 && ok.every((sample) => sample.withinOneInterval),
  };
}

async function main() {
  const browser = await chromium.launch({ headless: true });
  const context = await browser.newContext();
  if (COOKIE) await context.addCookies(parseCookieHeader(COOKIE));
  const page = await context.newPage();

  const consoleErrors = [];
  page.on("console", (message) => {
    if (message.type() === "error") consoleErrors.push(message.text().slice(0, 200));
  });

  const modes = MODE === "both" ? ["pull", "push"] : [MODE];
  const results = [];
  for (const mode of modes) results.push(await runMode(page, mode));

  await browser.close();

  console.log(
    JSON.stringify({ base: BASE, intervalMs: INTERVAL_MS, results, consoleErrors }, null, 2)
  );

  const measurable = results.some((result) => result.medianMs !== null);
  if (!measurable) {
    console.error(
      "无法测量：未提供 CCH_PRODUCE_CMD（需要造一条真实新记录）。" +
        "该脚本不会造假数据——测不到时如实报错。"
    );
    process.exit(1);
  }
  const failed = results.filter(
    (result) => result.medianMs !== null && !result.allWithinOneInterval
  );
  if (failed.length > 0) {
    console.error(
      `未达标：${failed.map((result) => result.mode).join(", ")} 存在超出 1 个刷新周期的样本`
    );
    process.exit(1);
  }
  process.exit(0);
}

main().catch((error) => {
  console.error("测量脚本失败：", error);
  process.exit(1);
});
