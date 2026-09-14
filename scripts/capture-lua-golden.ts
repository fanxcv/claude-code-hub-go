/**
 * Lua 脚本黄金样本采集。
 *
 * 用途：为 `lua/*.lua` 的每一段脚本，用一组代表性 KEYS/ARGV 在真实 Redis 上执行一次，
 * 把「入参 → 返回值 → 调用后被触及键的状态」固化为 `tests/load/redis-parity/golden/*.json`。
 * 未来的 Go 数据面必须用同一段 Lua、同一组 KEYS/ARGV 得到同一结果；这些文件就是那条断言的基准。
 *
 * 用法：
 *   bun scripts/capture-lua-golden.ts
 *   REDIS_URL=redis://127.0.0.1:6379 bun scripts/capture-lua-golden.ts
 *
 * 隔离与清理：
 *   - 只使用 DB 15（可用 CCH_GOLDEN_REDIS_DB 覆盖），只操作 `cchgolden:` 前缀的键；
 *   - 每个场景执行前先删除本前缀的全部键，执行后再次删除，绝不触碰其它键；
 *   - 脚本本身带 10 秒超时，超时即非零退出。
 *
 * 采集不到的项（见 tests/load/redis-parity/README.md）：需要的入参约束无法在单机 Redis 上构造，
 * 或语义只在并发/集群下可见的场景，一律标注为未采集，不编造结果。
 */

import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import Redis from "ioredis";

const HERE = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(HERE, "..");
const LUA_DIR = join(REPO_ROOT, "lua");
const GOLDEN_DIR = join(REPO_ROOT, "tests/load/redis-parity/golden");

const REDIS_URL = process.env.REDIS_URL || "redis://127.0.0.1:6379";
const REDIS_DB = Number(process.env.CCH_GOLDEN_REDIS_DB ?? 15);
const NOW = 1_757_000_000_000; // 固定时间戳，保证 golden 可复现
const PREFIX = "cchgolden:";
const CALL_TIMEOUT_MS = 10_000;

const k = (suffix: string): string => `${PREFIX}${suffix}`;

/** 绑定三元组（规范 hash + 两个 legacy 镜像）的固定键名。 */
const BINDING = k("bundle:1");
const LEGACY_PROVIDER = k("session:1:provider");
const LEGACY_OWNER = k("session:1:key");
const COOLDOWN = k("cooldown:1");

interface Scenario {
  id: string;
  keys: string[];
  argv: string[];
  setup?: Array<[string, ...string[]]>;
}

interface ScriptGolden {
  constName: string;
  file: string;
  sha256: string;
  calls: Array<{
    id: string;
    keys: string[];
    argv: string[];
    setup: Array<[string, ...string[]]>;
    returned: unknown;
    after: Record<string, unknown>;
  }>;
  uncollected: Array<{ id: string; reason: string }>;
}

type RedisValue = string | number | null;

function loadManifest(): Array<{ constName: string; file: string; sha256: string }> {
  const raw = JSON.parse(readFileSync(join(LUA_DIR, "MANIFEST.json"), "utf8")) as Array<{
    constName: string;
    file: string;
    sha256: string;
  }>;
  return raw;
}

async function purge(redis: Redis): Promise<void> {
  let cursor = "0";
  do {
    const [next, keys] = await redis.scan(cursor, "MATCH", `${PREFIX}*`, "COUNT", 500);
    cursor = next;
    if (keys.length > 0) await redis.del(...keys);
  } while (cursor !== "0");
}

/** 读取被触及键的终态，按类型给出可比较的结构。 */
async function readKey(redis: Redis, key: string): Promise<unknown> {
  const type = await redis.type(key);
  switch (type) {
    case "none":
      return null;
    case "string":
      return { type, value: await redis.get(key), ttl: await redis.ttl(key) };
    case "hash":
      return { type, value: await redis.hgetall(key), ttl: await redis.ttl(key) };
    case "zset": {
      const flat = await redis.zrange(key, 0, -1, "WITHSCORES");
      const value: Record<string, string> = {};
      for (let i = 0; i < flat.length; i += 2) value[flat[i] as string] = flat[i + 1] as string;
      return { type, value, ttl: await redis.ttl(key) };
    }
    default:
      return { type, note: "存在但未做类型化读取" };
  }
}

function buildScenarios(): Record<string, { collected: Scenario[]; uncollected: ScriptGolden["uncollected"] }> {
  const providerSessions = k("provider:1:active_sessions");
  const providerRefs = k("provider:1:active_session_refs");
  const costKey = k("key:1:cost_5h_rolling");
  const dailyCostKey = k("key:1:cost_daily_rolling");
  const globalKey = k("{active_sessions}:global:active_sessions");
  const keyKey = k("{active_sessions}:key:1:active_sessions");
  const userKey = k("{active_sessions}:user:1:active_sessions");
  const leaseKey = k("discovery:lease:session-1");
  const batchA = k("provider:11:active_sessions");
  const batchB = k("provider:12:active_sessions");

  const zset = (key: string, member: string, score: number): [string, ...string[]] => [
    "ZADD",
    key,
    String(score),
    member,
  ];

  return {
    DELETE_LEGACY_PROVIDER_IF_VALUE: {
      collected: [
        { id: "value-matches", keys: [LEGACY_PROVIDER], argv: ["7"], setup: [["SET", LEGACY_PROVIDER, "7"]] },
        { id: "value-differs", keys: [LEGACY_PROVIDER], argv: ["7"], setup: [["SET", LEGACY_PROVIDER, "9"]] },
      ],
      uncollected: [],
    },
    RESTORE_LEGACY_PROVIDER_IF_ABSENT: {
      collected: [
        { id: "key-absent", keys: [LEGACY_PROVIDER], argv: ["7", "60"] },
        { id: "key-present", keys: [LEGACY_PROVIDER], argv: ["7", "60"], setup: [["SET", LEGACY_PROVIDER, "9"]] },
      ],
      uncollected: [],
    },
    CHECK_AND_TRACK_SESSION: {
      collected: [
        {
          id: "fresh-session-tracked",
          keys: [providerSessions, providerRefs],
          argv: ["session-a", "10", String(NOW), "300000"],
        },
        {
          id: "already-tracked-with-refs",
          keys: [providerSessions, providerRefs],
          argv: ["session-a", "10", String(NOW), "300000"],
          setup: [zset(providerSessions, "session-a", NOW), ["HINCRBY", providerRefs, "session-a", "1"]],
        },
        {
          id: "limit-reached-rejects-new",
          keys: [providerSessions, providerRefs],
          argv: ["session-b", "1", String(NOW), "300000"],
          setup: [zset(providerSessions, "session-other", NOW)],
        },
        {
          id: "invalid-ttl-falls-back",
          keys: [providerSessions, providerRefs],
          argv: ["session-c", "10", String(NOW), "0"],
        },
      ],
      uncollected: [],
    },
    RELEASE_PROVIDER_SESSION: {
      collected: [
        {
          id: "last-ref-removes-member",
          keys: [providerSessions, providerRefs],
          argv: ["session-a"],
          setup: [zset(providerSessions, "session-a", NOW), ["HSET", providerRefs, "session-a", "1"]],
        },
        {
          id: "remaining-refs-keeps-member",
          keys: [providerSessions, providerRefs],
          argv: ["session-a"],
          setup: [zset(providerSessions, "session-a", NOW), ["HSET", providerRefs, "session-a", "2"]],
        },
        {
          id: "no-refs-is-noop",
          keys: [providerSessions, providerRefs],
          argv: ["session-a"],
        },
      ],
      uncollected: [],
    },
    FORCE_TERMINATE_PROVIDER_SESSION: {
      collected: [
        {
          id: "removes-refs-and-member",
          keys: [providerSessions, providerRefs],
          argv: ["session-a"],
          setup: [zset(providerSessions, "session-a", NOW), ["HSET", providerRefs, "session-a", "3"]],
        },
      ],
      uncollected: [],
    },
    FORCE_TERMINATE_KEY_USER_SESSION: {
      collected: [
        {
          id: "removes-from-three-indexes",
          keys: [globalKey, keyKey, userKey],
          argv: ["session-a"],
          setup: [
            zset(globalKey, "session-a", NOW),
            zset(keyKey, "session-a", NOW),
            zset(userKey, "session-a", NOW),
          ],
        },
      ],
      uncollected: [],
    },
    CHECK_AND_TRACK_KEY_USER_SESSION: {
      collected: [
        {
          id: "fresh-within-limits",
          keys: [globalKey, keyKey, userKey],
          argv: ["session-a", "10", "10", String(NOW), "300000"],
        },
        {
          id: "key-limit-rejects",
          keys: [globalKey, keyKey, userKey],
          argv: ["session-b", "1", "10", String(NOW), "300000"],
          setup: [zset(keyKey, "session-other", NOW)],
        },
        {
          id: "user-limit-rejects",
          keys: [globalKey, keyKey, userKey],
          argv: ["session-c", "10", "1", String(NOW), "300000"],
          setup: [zset(userKey, "session-other", NOW)],
        },
      ],
      uncollected: [],
    },
    BATCH_CHECK_SESSION_LIMITS: {
      collected: [
        {
          id: "both-under-limit",
          keys: [batchA, batchB],
          argv: ["session-a", "10", "10", String(NOW)],
        },
        {
          id: "first-over-limit",
          keys: [batchA, batchB],
          argv: ["session-a", "1", "10", String(NOW)],
          setup: [zset(batchA, "session-other", NOW)],
        },
      ],
      uncollected: [],
    },
    TRACK_COST_ROLLING_WINDOW: {
      collected: [
        {
          id: "appends-with-request-id",
          keys: [costKey],
          argv: ["1.5", String(NOW), "18000000", "req-1", "3600"],
        },
        {
          id: "appends-without-request-id",
          keys: [costKey],
          argv: ["2.5", String(NOW), "18000000", "", "3600"],
        },
        {
          id: "invalid-arguments-error-reply",
          keys: [costKey],
          argv: ["abc", "not-a-number", "18000000", "", "3600"],
        },
      ],
      uncollected: [],
    },
    GET_COST_5H_ROLLING_WINDOW: {
      collected: [
        {
          id: "sums-window",
          keys: [costKey],
          argv: [String(NOW), "18000000"],
          setup: [zset(costKey, `${NOW - 1000}:1.5`, NOW - 1000), zset(costKey, `${NOW - 2000}:2.5`, NOW - 2000)],
        },
      ],
      uncollected: [],
    },
    GET_COST_DAILY_ROLLING_WINDOW: {
      collected: [
        {
          id: "sums-window",
          keys: [dailyCostKey],
          argv: [String(NOW), "86400000"],
          setup: [
            zset(dailyCostKey, `${NOW - 1000}:1.5`, NOW - 1000),
            zset(dailyCostKey, `${NOW - 2000}:2.5`, NOW - 2000),
          ],
        },
      ],
      uncollected: [],
    },
    READ_OR_RECONCILE_SESSION_BINDING: {
      collected: [
        {
          id: "created-from-scratch",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "60"],
        },
        {
          id: "existing-canonical",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "60"],
          setup: [["HSET", BINDING, "key_id", "1", "generation", "100"], ["SET", LEGACY_OWNER, "1"]],
        },
        {
          id: "legacy-upgraded-with-provider",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "60"],
          setup: [["SET", LEGACY_OWNER, "1"], ["SET", LEGACY_PROVIDER, "7"]],
        },
        {
          id: "canonical-key-mismatch",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "60"],
          setup: [["HSET", BINDING, "key_id", "2", "generation", "100"], ["SET", LEGACY_OWNER, "1"]],
        },
      ],
      uncollected: [
        {
          id: "mirror-missing-after-canonical-write",
          reason: "需要规范态存在而 legacy owner 缺失，属滚动升级中的瞬时中间态；单线程脚本调用可构造，但该组合的语义由 session-binding.ts 的调用顺序决定，采集会固化一个并非真实序列的中间态。",
        },
      ],
    },
    CAS_SESSION_BINDING: {
      collected: [
        {
          id: "updated-to-next-generation",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "101", "7", "60"],
          setup: [["HSET", BINDING, "key_id", "1", "generation", "100"], ["SET", LEGACY_OWNER, "1"]],
        },
        {
          id: "canonical-missing",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "101", "7", "60"],
        },
        {
          id: "generation-mismatch",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "101", "7", "60"],
          setup: [["HSET", BINDING, "key_id", "1", "generation", "999"], ["SET", LEGACY_OWNER, "1"]],
        },
      ],
      uncollected: [],
    },
    TOUCH_SESSION_BINDING: {
      collected: [
        {
          id: "touched-null-binding",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "", "60"],
          setup: [["HSET", BINDING, "key_id", "1", "generation", "100"], ["SET", LEGACY_OWNER, "1"]],
        },
        {
          id: "provider-mismatch",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "100", "", "60"],
          setup: [
            ["HSET", BINDING, "key_id", "1", "generation", "100", "provider_id", "7"],
            ["SET", LEGACY_OWNER, "1"],
            ["SET", LEGACY_PROVIDER, "7"],
          ],
        },
      ],
      uncollected: [],
    },
    CLEAR_SESSION_BINDING: {
      collected: [
        {
          id: "cleared-with-cooldown",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER, COOLDOWN],
          argv: ["1", "100", "101", "7", "60", "7", "300"],
          setup: [
            ["HSET", BINDING, "key_id", "1", "generation", "100", "provider_id", "7"],
            ["SET", LEGACY_OWNER, "1"],
            ["SET", LEGACY_PROVIDER, "7"],
          ],
        },
        {
          id: "canonical-missing",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER, COOLDOWN],
          argv: ["1", "100", "101", "", "60", "", "0"],
        },
      ],
      uncollected: [],
    },
    RENEW_SESSION_DISCOVERY_LEASE: {
      collected: [
        { id: "renewed-by-owner", keys: [leaseKey], argv: ["owner-token", "60"], setup: [["SET", leaseKey, "owner-token"]] },
        { id: "not-owner", keys: [leaseKey], argv: ["owner-token", "60"], setup: [["SET", leaseKey, "other-token"]] },
      ],
      uncollected: [],
    },
    RELEASE_SESSION_DISCOVERY_LEASE: {
      collected: [
        { id: "released-by-owner", keys: [leaseKey], argv: ["owner-token"], setup: [["SET", leaseKey, "owner-token"]] },
        { id: "not-owner", keys: [leaseKey], argv: ["owner-token"], setup: [["SET", leaseKey, "other-token"]] },
      ],
      uncollected: [],
    },
    TERMINATE_SESSION_BINDING: {
      collected: [
        {
          id: "terminated-null-binding",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "101", "60", ""],
          setup: [["HSET", BINDING, "key_id", "1", "generation", "100"], ["SET", LEGACY_OWNER, "1"]],
        },
        {
          id: "conditional-provider-mismatch",
          keys: [BINDING, LEGACY_PROVIDER, LEGACY_OWNER],
          argv: ["1", "101", "60", "9"],
          setup: [
            ["HSET", BINDING, "key_id", "1", "generation", "100", "provider_id", "7"],
            ["SET", LEGACY_OWNER, "1"],
            ["SET", LEGACY_PROVIDER, "7"],
          ],
        },
      ],
      uncollected: [],
    },
  };
}

async function applySetup(redis: Redis, setup: Array<[string, ...string[]]> = []): Promise<void> {
  for (const [command, ...args] of setup) {
    await redis.call(command, ...args);
  }
}

async function main(): Promise<void> {
  const manifest = loadManifest();
  const scenarios = buildScenarios();

  const redis = new Redis(REDIS_URL, {
    db: REDIS_DB,
    lazyConnect: true,
    maxRetriesPerRequest: 1,
    connectTimeout: 5000,
  });
  await redis.connect();
  const pong = await redis.ping();
  if (pong !== "PONG") throw new Error(`Redis PING 返回 ${pong}`);
  process.stderr.write(`redis ${REDIS_URL} db=${REDIS_DB} ping=ok\n`);

  rmSync(GOLDEN_DIR, { recursive: true, force: true });
  mkdirSync(GOLDEN_DIR, { recursive: true });

  let collectedCalls = 0;
  const uncollectedAll: string[] = [];

  for (const entry of manifest) {
    const definition = scenarios[entry.constName];
    if (!definition) throw new Error(`缺少 ${entry.constName} 的场景定义`);
    const lua = readFileSync(join(LUA_DIR, entry.file), "utf8");
    const golden: ScriptGolden = {
      constName: entry.constName,
      file: entry.file,
      sha256: createHash("sha256").update(lua, "utf8").digest("hex"),
      calls: [],
      uncollected: definition.uncollected,
    };

    for (const scenario of definition.collected) {
      await purge(redis);
      await applySetup(redis, scenario.setup);
      let returned: unknown;
      try {
        returned = await Promise.race([
          redis.eval(lua, scenario.keys.length, ...scenario.keys, ...scenario.argv) as Promise<unknown>,
          new Promise((_, reject) => setTimeout(() => reject(new Error("eval 超时")), CALL_TIMEOUT_MS)),
        ]);
      } catch (error) {
        returned = { error: error instanceof Error ? error.message : String(error) };
      }
      const after: Record<string, unknown> = {};
      for (const key of scenario.keys) after[key] = await readKey(redis, key);
      golden.calls.push({
        id: scenario.id,
        keys: scenario.keys,
        argv: scenario.argv,
        setup: scenario.setup ?? [],
        returned,
        after,
      });
      collectedCalls += 1;
    }

    for (const item of definition.uncollected) uncollectedAll.push(`${entry.constName}: ${item.id} — ${item.reason}`);

    writeFileSync(join(GOLDEN_DIR, `${entry.file.replace(/\.lua$/, "")}.json`), `${JSON.stringify(golden, null, 2)}\n`, "utf8");
  }

  await purge(redis);
  await redis.quit();

  process.stdout.write(
    `采集完成：${manifest.length} 段脚本 / ${collectedCalls} 个场景 / 未采集 ${uncollectedAll.length} 项\n`
  );
  for (const item of uncollectedAll) process.stdout.write(`  未采集 ${item}\n`);
}

main().catch((error: unknown) => {
  console.error(error instanceof Error ? error.stack ?? error.message : String(error));
  process.exit(1);
});
