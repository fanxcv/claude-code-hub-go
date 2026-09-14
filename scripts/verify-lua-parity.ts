/**
 * Lua 脚本语言中立导出与逐字节校验。
 *
 * 用途：`src/lib/redis/lua-scripts.ts` 是跨进程原子语义的唯一真源，未来的 Go 数据面必须
 * 复用同一段 Lua 原文，不得用 Go 多命令改写。本脚本把每段 Lua 原文导出到 `lua/*.lua`，
 * 并逐字节校验磁盘内容与 TS 常量是否一致。
 *
 * 用法：
 *   bun scripts/verify-lua-parity.ts           # 只校验，不一致即非零退出
 *   bun scripts/verify-lua-parity.ts --write   # 重新导出 lua/*.lua 与 lua/MANIFEST.json
 *
 * 校验同时覆盖两个方向：
 *   - 磁盘上的每个 .lua 必须与 TS 常量逐字节相同（含首尾空白）；
 *   - TS 常量集合与 MANIFEST 条目必须一一对应，任一侧多出条目即判失败（防新增脚本被漏导出）。
 */

import { createHash } from "node:crypto";
import { existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import * as LUA from "../src/lib/redis/lua-scripts";

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(HERE, "..");
const LUA_DIR = join(REPO_ROOT, "lua");
const MANIFEST_PATH = join(LUA_DIR, "MANIFEST.json");

const WRITE = process.argv.includes("--write");

/** 每段脚本的 KEYS/ARGV 契约（照 TS 注释，供 Go 侧接线时对照）。 */
const ARITY_NOTES: Record<string, string> = {
  DELETE_LEGACY_PROVIDER_IF_VALUE:
    "KEYS[1]=legacy provider 镜像键；ARGV[1]=期望值。单键，兼容 Redis Cluster。返回 1=已删除，0=值不符。",
  RESTORE_LEGACY_PROVIDER_IF_ABSENT:
    "KEYS[1]=legacy provider 镜像键；ARGV[1]=值，ARGV[2]=TTL 秒。返回 1=已写入，0=键已存在。",
  CHECK_AND_TRACK_SESSION:
    "KEYS[1]=provider:{id}:active_sessions，KEYS[2]=provider:{id}:active_session_refs；ARGV[1]=sessionId，ARGV[2]=limit，ARGV[3]=now(ms)，ARGV[4]=ttlMs(可选,默认 300000)。返回 {allowed,count,tracked,referenced}。",
  RELEASE_PROVIDER_SESSION:
    "KEYS[1]=provider:{id}:active_sessions，KEYS[2]=provider:{id}:active_session_refs；ARGV[1]=sessionId。返回 {removed,remainingRefs}。",
  FORCE_TERMINATE_PROVIDER_SESSION:
    "KEYS[1]=provider:{id}:active_sessions，KEYS[2]=provider:{id}:active_session_refs；ARGV[1]=sessionId。返回 {removedSession,removedRefs}。",
  FORCE_TERMINATE_KEY_USER_SESSION:
    "KEYS[1..3]=global/key/user 并发索引 ZSET；ARGV[1]=sessionId。返回 {removedGlobal,removedKey,removedUser}。",
  CHECK_AND_TRACK_KEY_USER_SESSION:
    "KEYS[1..3]={active_sessions}:global|key|user 并发索引 ZSET（须同 hash tag）；ARGV[1]=sessionId，ARGV[2]=keyLimit，ARGV[3]=userLimit，ARGV[4]=now(ms)，ARGV[5]=ttlMs(可选,默认 300000)。返回 {allowed,rejectedBy,keyCount,keyTracked,userCount,userTracked}。",
  BATCH_CHECK_SESSION_LIMITS:
    "KEYS=多个 provider:{id}:active_sessions；ARGV[1]=sessionId，ARGV[2..N-1]=各供应商 limit，ARGV[N]=now(ms)。返回每个 key 一项的 {{allowed,count}}。",
  TRACK_COST_ROLLING_WINDOW:
    "KEYS[1]={entity}:{id}:cost_{window}_rolling；ARGV[1]=cost，ARGV[2]=now(ms)，ARGV[3]=windowMs，ARGV[4]=requestId(可空)，ARGV[5]=ttlSeconds。任一枚举参数非法返回 error_reply('invalid rolling cost arguments')，正常返回 1。",
  GET_COST_5H_ROLLING_WINDOW:
    "KEYS[1]=成本滚动窗 ZSET；ARGV[1]=now(ms)，ARGV[2]=windowMs(默认 18000000)。返回窗口内总消费的字符串。",
  GET_COST_DAILY_ROLLING_WINDOW:
    "KEYS[1]=成本滚动窗 ZSET；ARGV[1]=now(ms)，ARGV[2]=windowMs(默认 86400000)。返回窗口内总消费的字符串。",
  READ_OR_RECONCILE_SESSION_BINDING:
    "KEYS[1]=规范绑定 hash，KEYS[2]=legacy provider 字符串，KEYS[3]=legacy key owner 字符串；ARGV[1]=currentKeyId，ARGV[2]=初始化/升级用 generation，ARGV[3]=绑定 TTL 秒。返回 {ok|conflict, source|reason, generation, providerIdOrEmpty}。",
  CAS_SESSION_BINDING:
    "KEYS[1..3]=绑定 hash 与两个 legacy 镜像；ARGV[1]=currentKeyId，ARGV[2]=expectedGeneration，ARGV[3]=nextGeneration，ARGV[4]=nextProviderId，ARGV[5]=TTL 秒。返回 {ok|conflict, ...}；缺失规范态一律 conflict，绝不在此初始化。",
  TOUCH_SESSION_BINDING:
    "KEYS[1..3]=绑定 hash 与两个 legacy 镜像；ARGV[1]=currentKeyId，ARGV[2]=expectedGeneration，ARGV[3]=expectedProviderId（空表示 null 绑定），ARGV[4]=TTL 秒。返回 {ok|conflict, ...}；只续 TTL，不旋转 generation。",
  CLEAR_SESSION_BINDING:
    "KEYS[1..3]=绑定 hash 与两个 legacy 镜像，KEYS[4]=租户级 cooldown 键（cooldown TTL 为 0 时不使用）；ARGV[1]=currentKeyId，ARGV[2]=expectedGeneration，ARGV[3]=nextGeneration，ARGV[4]=expectedProviderId（空表示 null 绑定），ARGV[5]=TTL 秒，ARGV[6]=cooldownProviderId（可空），ARGV[7]=cooldownTtl 秒（0 表示不写）。",
  RENEW_SESSION_DISCOVERY_LEASE:
    "KEYS[1]=租户级 Discovery 租约键；ARGV[1]=ownerToken，ARGV[2]=TTL 秒。返回 1=续期成功，0=非本 owner。",
  RELEASE_SESSION_DISCOVERY_LEASE:
    "KEYS[1]=租户级 Discovery 租约键；ARGV[1]=ownerToken。返回 1=已释放，0=非本 owner。",
  TERMINATE_SESSION_BINDING:
    "KEYS[1..3]=绑定 hash 与两个 legacy 镜像；ARGV[1]=currentKeyId，ARGV[2]=nextGeneration，ARGV[3]=TTL 秒，ARGV[4]=可选 expectedProviderId。返回 {ok|conflict, ...}；不比较旧 generation，但校验规范所有权与两个镜像。",
};

function kebab(name: string): string {
  return `${name.toLowerCase().replace(/_/g, "-")}.lua`;
}

function sha256(text: string): string {
  return createHash("sha256").update(text, "utf8").digest("hex");
}

/** 取 lua-scripts.ts 的全部字符串导出，按导出顺序稳定排序。 */
function collectScripts(): Array<{ constName: string; body: string }> {
  const entries = Object.entries(LUA as Record<string, unknown>).filter(
    (entry): entry is [string, string] => typeof entry[1] === "string"
  );
  return entries
    .map(([constName, body]) => ({ constName, body }))
    .sort((left, right) => left.constName.localeCompare(right.constName));
}

interface ManifestEntry {
  constName: string;
  file: string;
  sha256: string;
  bytes: number;
  keysArityNote: string;
}

function buildManifest(scripts: Array<{ constName: string; body: string }>): ManifestEntry[] {
  return scripts.map(({ constName, body }) => ({
    constName,
    file: kebab(constName),
    sha256: sha256(body),
    bytes: Buffer.byteLength(body, "utf8"),
    keysArityNote: ARITY_NOTES[constName] ?? "未登记 KEYS/ARGV 契约（须补 ARITY_NOTES）",
  }));
}

function writeAll(manifest: ManifestEntry[], scripts: Array<{ constName: string; body: string }>): void {
  if (existsSync(LUA_DIR)) {
    // 只清 .lua 与清单，避免误删目录内其它说明文件。
    for (const name of readdirSync(LUA_DIR)) {
      if (name.endsWith(".lua") || name === "MANIFEST.json") rmSync(join(LUA_DIR, name));
    }
  } else {
    mkdirSync(LUA_DIR, { recursive: true });
  }
  for (const { constName, body } of scripts) {
    writeFileSync(join(LUA_DIR, kebab(constName)), body, "utf8");
  }
  writeFileSync(MANIFEST_PATH, `${JSON.stringify(manifest, null, 2)}\n`, "utf8");
}

function verify(manifest: ManifestEntry[], scripts: Array<{ constName: string; body: string }>): string[] {
  const failures: string[] = [];
  const expectedByName = new Map(scripts.map((entry) => [entry.constName, entry.body]));

  for (const entry of manifest) {
    const path = join(LUA_DIR, entry.file);
    if (!existsSync(path)) {
      failures.push(`${entry.constName}: 缺少 ${entry.file}`);
      continue;
    }
    const onDisk = readFileSync(path, "utf8");
    const expected = expectedByName.get(entry.constName);
    if (expected === undefined) {
      failures.push(`${entry.constName}: MANIFEST 有条目但 TS 侧已无该常量（陈旧条目）`);
      continue;
    }
    if (onDisk !== expected) {
      failures.push(
        `${entry.constName}: ${entry.file} 与 TS 常量不一致（磁盘 ${sha256(onDisk).slice(0, 8)} vs TS ${sha256(expected).slice(0, 8)}）`
      );
      continue;
    }
    if (entry.sha256 !== sha256(expected)) {
      failures.push(`${entry.constName}: MANIFEST 记录的 sha256 已过期`);
      continue;
    }
    if (entry.keysArityNote.startsWith("未登记")) {
      failures.push(`${entry.constName}: ARITY_NOTES 未登记 KEYS/ARGV 契约`);
    }
  }

  const manifestNames = new Set(manifest.map((entry) => entry.constName));
  for (const { constName } of scripts) {
    if (!manifestNames.has(constName)) {
      failures.push(`${constName}: TS 侧存在但 MANIFEST 未收录`);
    }
  }

  const luaFiles = existsSync(LUA_DIR)
    ? readdirSync(LUA_DIR).filter((name) => name.endsWith(".lua"))
    : [];
  const expectedFiles = new Set(manifest.map((entry) => entry.file));
  for (const name of luaFiles) {
    if (!expectedFiles.has(name)) failures.push(`${name}: 目录中存在 MANIFEST 未收录的 .lua`);
  }

  return failures;
}

const scripts = collectScripts();
if (scripts.length === 0) {
  console.error("lua-scripts.ts 未导出任何字符串常量，拒绝继续");
  process.exit(2);
}
const manifest = buildManifest(scripts);

if (WRITE) {
  writeAll(manifest, scripts);
  console.log(`导出 ${scripts.length} 段 Lua 到 ${LUA_DIR}`);
}

const failures = verify(manifest, scripts);

console.log("constName                          bytes  sha256     file");
for (const entry of manifest) {
  console.log(
    `${entry.constName.padEnd(34)} ${String(entry.bytes).padStart(5)}  ${entry.sha256.slice(0, 8)}  ${entry.file}`
  );
}

if (failures.length > 0) {
  console.error(`\n校验失败（${failures.length} 项）：`);
  for (const failure of failures) console.error(`  - ${failure}`);
  process.exit(1);
}

console.log(`\n校验通过：${manifest.length} 段 Lua 与 TS 常量逐字节一致，MANIFEST 与目录一一对应`);
