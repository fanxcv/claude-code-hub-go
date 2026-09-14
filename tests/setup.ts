/**
 * Vitest 测试前置脚本
 *
 * 在所有测试运行前执行的全局配置
 */

import { config } from "dotenv";
import { afterAll, beforeAll, vi } from "vitest";

// ==================== 加载环境变量 ====================

// 优先加载 .env.test（如果存在）
config({ path: ".env.test", quiet: true });

// 降级加载 .env
config({ path: ".env", quiet: true });

// ==================== 全局前置钩子 ====================

beforeAll(async () => {
  console.log("\nVitest 测试环境初始化...\n");

  // 安全检查：确保使用测试数据库
  const dsn = process.env.DSN || "";
  const dbName = dsn.split("/").pop() || "";

  if (process.env.NODE_ENV === "production") {
    throw new Error("禁止在生产环境运行测试");
  }

  // 强制要求：测试必须使用包含 'test' 的数据库（CI 和本地都检查）
  if (dbName && !dbName.includes("test")) {
    // 允许通过环境变量显式跳过检查（仅用于特殊情况）
    if (process.env.ALLOW_NON_TEST_DB !== "true") {
      throw new Error(
        `安全检查失败: 数据库名称必须包含 'test' 字样\n` +
          `   当前数据库: ${dbName}\n` +
          `   建议使用测试专用数据库（如 claude_code_hub_test）\n` +
          `   如需跳过检查，请设置环境变量: ALLOW_NON_TEST_DB=true`
      );
    }

    // 即使跳过检查也要发出警告
    console.warn("警告: 当前数据库不包含 'test' 字样");
    console.warn(`   数据库: ${dbName}`);
    console.warn("   建议使用独立的测试数据库避免数据污染\n");
  }

  // 显示测试配置
  console.log("测试配置:");
  console.log(`   - 数据库: ${dbName || "未配置"}`);
  console.log(`   - Redis: ${process.env.REDIS_URL?.split("//")[1]?.split("@")[1] || "未配置"}`);
  console.log(`   - API Base: ${process.env.API_BASE_URL || "http://localhost:13500"}`);
  console.log("");

  // 2026-09 node 退役：这里原先调 `@/repository/error-rules` 的 syncDefaultErrorRules 播种默认
  // 错误规则。该实现随 `src/repository/**` 删除，播种职责归 Go 侧（迁移/启动期），测试前置不再做。

  // ==================== 并行 Worker 清理协调 ====================
  // setupFiles 会在每个 worker 中执行；如果每个 worker 都在 afterAll 清理数据库，会出现“互相清理”的竞态。
  // 这里用 Redis 计数器实现：只有最后一个结束的 worker 才执行 cleanup。
  try {
    const shouldCleanup = Boolean(dsn) && process.env.AUTO_CLEANUP_TEST_DATA !== "false";
    if (!shouldCleanup) return;

    const dbNameForKey = dbName || "unknown";
    const counterKey = `cch:vitest:cleanup_workers:${dbNameForKey}`;
    // 直接指到 redis/client：`@/lib/redis` 汇合点的 re-export 面已随 Node 数据面退役收窄，
    // 而本 harness 只需要一个 Redis 客户端做 worker 计数。
    const { getRedisClient } = await import("@/lib/redis/client");
    const redis = getRedisClient();
    if (!redis) return;

    // 等待连接就绪（enableOfflineQueue=false，未 ready 时发命令会直接报错）
    if (redis.status !== "ready") {
      await new Promise<void>((resolve) => {
        const timeout = setTimeout(resolve, 2000);
        redis.once("ready", () => {
          clearTimeout(timeout);
          resolve();
        });
      });
    }

    if (redis.status !== "ready") {
      console.warn("Redis 未就绪，跳过并行清理协调（不影响测试结果）");
      return;
    }

    const current = await redis.incr(counterKey);
    if (current === 1) {
      // 防止异常退出导致计数器常驻
      await redis.expire(counterKey, 60 * 15);
    }
    process.env.__VITEST_CLEANUP_COUNTER_KEY__ = counterKey;
  } catch (error) {
    console.warn("并行清理协调初始化失败（不影响测试结果）:", error);
  }
});

// ==================== 全局清理钩子 ====================

afterAll(async () => {
  console.log("\nVitest 测试环境清理...\n");

  // 清理测试期间创建的用户（仅清理最近 10 分钟内的）
  const dsn = process.env.DSN || "";
  if (dsn && process.env.AUTO_CLEANUP_TEST_DATA !== "false") {
    try {
      // 仅最后一个 worker 执行清理，避免并发互相删除
      const counterKey = process.env.__VITEST_CLEANUP_COUNTER_KEY__;
      const { getRedisClient } = await import("@/lib/redis/client");
      const redis = counterKey ? getRedisClient() : null;

      if (counterKey && redis) {
        if (redis.status !== "ready") {
          await new Promise<void>((resolve) => {
            const timeout = setTimeout(resolve, 2000);
            redis.once("ready", () => {
              clearTimeout(timeout);
              resolve();
            });
          });
        }

        if (redis.status === "ready") {
          const remaining = await redis.decr(counterKey);
          if (remaining <= 0) {
            const { cleanupRecentTestData } = await import("./cleanup-utils");
            const result = await cleanupRecentTestData();
            if (result.deletedUsers > 0) {
              console.log(`自动清理：删除 ${result.deletedUsers} 个测试用户\n`);
            }
            await redis.del(counterKey);
          } else {
            // 非最后一个 worker：跳过清理
          }
        } else {
          console.warn("Redis 未就绪，跳过自动清理（不影响测试结果）");
        }
      } else {
        // 无 Redis 协调：为了避免竞态，默认跳过清理
        console.warn("未启用清理协调，跳过自动清理（不影响测试结果）");
      }
    } catch (error) {
      console.warn(
        "自动清理失败（不影响测试结果）:",
        error instanceof Error ? error.message : error
      );
    }
  }

  console.log("Vitest 测试环境清理完成\n");
});

// ==================== 全局 Mock 配置（可选）====================

// 如果需要 mock 某些全局对象，可以在这里配置
// 例如：mock console.error 以避免测试输出过多错误日志

// 为什么需要这一节：UI 组件直接调 `@/lib/api-client/v1/actions/*`，单测里不能让它们真发请求，
// 所以逐个给假实现——这里列出全部动作模块。
//
// 2026-09 node 退役前，这一节是**桥接**：mock 指向旧的 Node Server Action（`@/actions/*`），
// 测试 mock 旧动作、api-client 跟着生效。`src/actions/**` 删除后桥的源头没了，改为**直接给
// api-client 假实现**：测试里 api-client 依旧是假的、不发请求，只是少了「旧动作」那一层。
const apiClientActionModules = [
  "active-sessions",
  "admin-user-insights",
  "audit-logs",
  "client-versions",
  "concurrent-sessions",
  "dashboard-realtime",
  "dispatch-simulator",
  "error-rules",
  "key-quota",
  "keys",
  "model-prices",
  "my-usage",
  "notification-bindings",
  "notifications",
  "overview",
  "provider-endpoints",
  "provider-groups",
  "provider-slots",
  "providers",
  "proxy-status",
  "public-status",
  "rate-limit-stats",
  "request-filters",
  "sensitive-words",
  "session-origin-chain",
  "session-response",
  "statistics",
  "system-config",
  "usage-logs",
  "users",
  "webhook-targets",
];

/** 假实现的默认返回：空列表。动作里列表读接口占多数，空列表让页面渲染空态而不是抛错。 */
const stubEmptyList = () => vi.fn(async () => []);

/** 少数动作返回的是 `{ ok, data }` 信封而不是列表，按模块显式给值（取值沿用桥接时代）。 */
const stubActionResult = () => vi.fn(async () => ({ ok: true, data: [] }));

/**
 * 按模块指定的假实现。
 *
 * 上级（有意）：只保证**形状可用且不发请求**，不还原真实数据。需要具体数据的用例必须自己
 * `vi.mock("@/lib/api-client/v1/actions/<module>")` 覆盖本假实现。
 */
const actionStubOverrides: Record<string, Record<string, unknown>> = {
  keys: { getKeys: stubActionResult(), getKeysWithStatistics: stubActionResult() },
  "model-prices": {
    getAvailableModelCatalog: stubEmptyList(),
    getAvailableModelsByProviderType: stubEmptyList(),
  },
  providers: {
    getProviders: stubEmptyList(),
    getAvailableProviderGroups: stubEmptyList(),
    getAvailableModelCatalog: stubEmptyList(),
    getAvailableModelsByProviderType: stubEmptyList(),
  },
  "system-config": {
    getSystemSettings: vi.fn(async () => ({
      billingModelSource: "redirected",
      currencyDisplay: "USD",
    })),
  },
  users: { searchUsers: stubActionResult(), searchUsersForFilter: stubActionResult() },
};

/**
 * 把真实模块的**函数型导出**换成假实现，非函数导出原样保留。
 *
 * 先取真实模块再改，是为了让假实现的名导出集合与真实模块一一对应——漏一个都会让
 * `import { x } from "@/lib/api-client/v1/actions/<module>"` 在运行时炸掉。
 */
function stubActionModule(
  real: Record<string, unknown>,
  overrides: Record<string, unknown>
): Record<string, unknown> {
  const mocked: Record<string, unknown> = {};
  for (const [exportName, value] of Object.entries(real)) {
    mocked[exportName] = typeof value === "function" ? stubEmptyList() : value;
  }
  for (const [exportName, value] of Object.entries(overrides)) {
    mocked[exportName] = value;
  }
  return mocked;
}

for (const moduleName of apiClientActionModules) {
  vi.doMock(`@/lib/api-client/v1/actions/${moduleName}`, async () => {
    const real = await import(`@/lib/api-client/v1/actions/${moduleName}`);
    return stubActionModule(real, actionStubOverrides[moduleName] ?? {});
  });
}

// 保存原始 console.error
const originalConsoleError = console.error;

// 在测试中静默某些预期的错误（可选）
global.console.error = (...args: unknown[]) => {
  // 过滤掉某些已知的、预期的错误日志
  const message = args[0]?.toString() || "";

  // 跳过这些预期的错误日志
  const ignoredPatterns = [
    // 可以在这里添加需要忽略的错误模式
    // "某个预期的错误消息",
  ];

  const shouldIgnore = ignoredPatterns.some((pattern) => message.includes(pattern));

  if (!shouldIgnore) {
    originalConsoleError(...args);
  }
};

// ==================== 环境变量默认值 ====================

// 设置测试环境默认值（如果未配置）
process.env.NODE_ENV = process.env.NODE_ENV || "test";
process.env.API_BASE_URL = process.env.API_BASE_URL || "http://localhost:13500/api/actions";
// 便于 API 测试复用 ADMIN_TOKEN（validateKey 支持该 token 直通管理员会话）
process.env.ADMIN_TOKEN = process.env.ADMIN_TOKEN || "admin-token";
process.env.TEST_ADMIN_TOKEN = process.env.TEST_ADMIN_TOKEN || process.env.ADMIN_TOKEN;

// ==================== React act 环境标记 ====================
// React 18+ 在测试环境中会检查该标记，避免出现 “not configured to support act(...)” 的噪声警告。
(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

// ==================== 全局超时配置 ====================

// 设置全局默认超时（可以被单个测试覆盖）
const DEFAULT_TIMEOUT = 10000; // 10 秒

// 导出配置供测试使用
export const TEST_CONFIG = {
  timeout: DEFAULT_TIMEOUT,
  apiBaseUrl: process.env.API_BASE_URL,
  skipAuthTests: !process.env.TEST_AUTH_TOKEN,
};
