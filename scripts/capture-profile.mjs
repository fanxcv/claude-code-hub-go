#!/usr/bin/env node
/**
 * 取性能剖析（pprof）并给出最热函数的摘要。
 *
 * 用途：生产上出现「CPU 波动 / 内存摆动 / 延迟高但 CPU 低」时，一条命令把该拿的剖析
 * 一次拿齐，落到 tmp/profiles/<时间戳>/，并只打印 pprof -top 的头部若干行
 * （不把原始 pprof 输出倒进终端：它有几千行，看的人只需要最热的十个）。
 *
 * 前置：目标进程的剖析面已开（CCH_PPROF_ENABLED=true），或至少端点可达。
 * 取法（不重启进程优先）：
 *   1) 容器内直取：容器里有 wget 时可用本脚本的 --base 指向容器内地址，
 *      或直接 `docker exec` 里跑本脚本（需 node，通常没有）；更实际的是端口映射/nsenter（见文档）。
 *   2) 端口映射：把剖析面端口映射到宿主，再用本脚本取。
 *
 * 用法：
 *   node scripts/capture-profile.mjs --base http://127.0.0.1:3100
 *   node scripts/capture-profile.mjs --base http://127.0.0.1:3100 --cpu-seconds 30
 *   node scripts/capture-profile.mjs --base http://127.0.0.1:3100 --with-trace --with-block
 *   node scripts/capture-profile.mjs --base http://127.0.0.1:3100 --out tmp/profiles
 *
 * 退出码：0 取齐；1 目标不可达或某份必需剖析失败（**不静默产出空文件**）。
 */
import { mkdirSync, writeFileSync, statSync } from "node:fs";
import { join, resolve } from "node:path";
import { execFileSync } from "node:child_process";

function argValue(name, fallback) {
  const index = process.argv.indexOf(`--${name}`);
  if (index === -1) return fallback;
  const value = process.argv[index + 1];
  if (value === undefined || value.startsWith("--")) return fallback;
  return value;
}

function hasFlag(name) {
  return process.argv.includes(`--${name}`);
}

const base = (argValue("base", process.env.CCH_PPROF_BASE ?? "http://127.0.0.1:3100")).replace(/\/$/, "");
const cpuSeconds = Number(argValue("cpu-seconds", "30"));
const outRoot = resolve(argValue("out", join("tmp", "profiles")));
const topLines = Number(argValue("top", "15"));
const withTrace = hasFlag("with-trace");
const withBlock = hasFlag("with-block");
const withMutex = hasFlag("with-mutex");

if (!Number.isFinite(cpuSeconds) || cpuSeconds <= 0) {
  console.error(`cpu-seconds 必须是正数，收到 ${argValue("cpu-seconds", "30")}`);
  process.exit(1);
}

const stamp = new Date().toISOString().replace(/[:.]/g, "-");
const outDir = join(outRoot, stamp);

/** 先探可用性：不可达就明确报错，而不是产出 0 字节文件。 */
async function assertReachable() {
  let response;
  try {
    response = await fetch(`${base}/debug/metrics`, { signal: AbortSignal.timeout(8000) });
  } catch (error) {
    console.error(`剖析面不可达：${base}/debug/metrics`);
    console.error(`  原因：${error.message}`);
    console.error("  排查：开关未开（CCH_PPROF_ENABLED=true）、地址不是回环、或容器内 3100 未映射到宿主");
    process.exit(1);
  }
  if (response.status === 404) {
    console.error(`剖析面未启用（404）：${base}/debug/metrics`);
    console.error("  处置：把 CCH_PPROF_ENABLED 设为 true 后重启进程；默认关闭是有意的（避免无人看管时暴露堆内容）");
    process.exit(1);
  }
  if (!response.ok) {
    console.error(`剖析面应答异常：${response.status} ${response.statusText}`);
    process.exit(1);
  }
  return response.json();
}

/** 取一份剖析到文件；短读（0 字节）视为失败。 */
async function capture(name, path, { timeoutMs = 120_000, minBytes = 1 } = {}) {
  let response;
  try {
    response = await fetch(`${base}${path}`, { signal: AbortSignal.timeout(timeoutMs) });
  } catch (error) {
    console.error(`取 ${name} 失败：${error.message}`);
    process.exit(1);
  }
  if (!response.ok) {
    console.error(`取 ${name} 失败：HTTP ${response.status}`);
    process.exit(1);
  }
  const bytes = Buffer.from(await response.arrayBuffer());
  if (bytes.length < minBytes) {
    console.error(`取 ${name} 失败：只拿到 ${bytes.length} 字节（疑似空响应）`);
    process.exit(1);
  }
  const target = join(outDir, name);
  writeFileSync(target, bytes);
  return { target, bytes: bytes.length };
}

/** 打印 pprof -top 的头部若干行；解析失败不影响已落盘的剖析。 */
function summarize(label, file) {
  try {
    const output = execFileSync("go", ["tool", "pprof", "-top", `-nodecount=${topLines}`, file], {
      encoding: "utf8",
      stdio: ["ignore", "pipe", "pipe"],
    });
    const lines = output.trimEnd().split("\n").slice(0, topLines + 5);
    console.log(`--- ${label}（pprof -top 前 ${topLines} 行）---`);
    console.log(lines.join("\n"));
  } catch (error) {
    console.log(`--- ${label}：pprof 解析失败（文件已落盘，可自行分析）---`);
    console.log(`  ${error.message.split("\n")[0]}`);
  }
}

const metrics = await assertReachable();
mkdirSync(outDir, { recursive: true });
console.log(`剖析面 ${base}`);
console.log(`  Go ${metrics.process?.goVersion ?? "?"}  goroutines ${metrics.process?.goroutines ?? "?"}  GOMAXPROCS ${metrics.process?.gomaxprocs ?? "?"}`);
console.log(`  GOGC ${metrics.gc?.goGCPercent ?? "?"}  GOMEMLIMIT ${metrics.gc?.gomemlimitBytes ?? metrics.gc?.goMemLimitBytes ?? "?"}`);
console.log(`  堆在用 ${metrics.memory?.heapInuseBytes ?? "?"} 字节  下次 GC 阈值 ${metrics.memory?.nextGCBytes ?? "?"} 字节`);
console.log(`  输出目录 ${outDir}`);

writeFileSync(join(outDir, "metrics.json"), `${JSON.stringify(metrics, null, 2)}\n`);

const heap = await capture("heap.pb.gz", "/debug/pprof/heap", { minBytes: 128 });
const goroutine = await capture("goroutine.txt", "/debug/pprof/goroutine?debug=2", { minBytes: 64 });
console.log(`CPU 剖析采集中（${cpuSeconds}s）…`);
const cpu = await capture("cpu.pb.gz", `/debug/pprof/profile?seconds=${cpuSeconds}`, {
  timeoutMs: (cpuSeconds + 60) * 1000,
  minBytes: 128,
});

let block;
if (withBlock) {
  block = await capture("block.pb.gz", "/debug/pprof/block", { minBytes: 128 });
}
let mutex;
if (withMutex) {
  mutex = await capture("mutex.pb.gz", "/debug/pprof/mutex", { minBytes: 128 });
}
let trace;
if (withTrace) {
  console.log("执行 trace 采集中（3s）…");
  trace = await capture("trace.out", "/debug/pprof/trace?seconds=3", { timeoutMs: 60_000, minBytes: 128 });
}

console.log("");
summarize("CPU", cpu.target);
if (block) summarize("阻塞", block.target);
summarize("堆（inuse_space）", heap.target);

const sizes = [["cpu", cpu], ["heap", heap], ["goroutine", goroutine], ["block", block], ["mutex", mutex], ["trace", trace]]
  .filter(([, item]) => item)
  .map(([name, item]) => `${name} ${(statSync(item.target).size / 1024).toFixed(1)} KiB`)
  .join("  ");
console.log("");
console.log(`已落盘：${sizes}`);
console.log("提示：内存摆动看 heap 的 inuse_space 与 metrics.json 的 nextGCBytes；CPU 尖峰看 cpu.pb.gz 里后台作业栈。");
