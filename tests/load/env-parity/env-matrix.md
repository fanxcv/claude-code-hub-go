# 环境变量契约矩阵

由 `scripts/export-env-matrix.ts` 从 `src/lib/config/env.schema.ts` 生成，请勿手工编辑。

- 变量总数：**70**
- 源文件 SHA256：`f254742e56f0cf542b2ee14df6ec7cf09b9c42687374b1a0493c8f3ae99e1136`
- 重生成：`bun scripts/export-env-matrix.ts`

## Redis 与缓存（4）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `ENABLE_PROVIDER_CACHE` | string | `"true"` | — | （无静态引用） |
| `REDIS_COMMAND_TIMEOUT_MS` | number（optionalNumber） | `10_000` | >= 100；<= 120000 | `src/lib/redis/client.ts` |
| `REDIS_TLS_REJECT_UNAUTHORIZED` | string | `"true"` | — | `src/lib/redis/client.ts` |
| `REDIS_URL` | string（optional） | — | — | `src/lib/redis/client.ts` |

## Replay（4）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `REPLAY_LIVE_DEDUP_ENABLED` | string | `"true"` | — | （无静态引用） |
| `REPLAY_MAX_DETACHED_MS` | number | `300_000` | >= 10000；<= 1800000 | （无静态引用） |
| `REPLAY_MAX_PAYLOAD_BYTES` | number | `8 * 1024 * 1024` | >= 65536；<= 67108864 | （无静态引用） |
| `REPLAY_TTL_SECONDS` | number | `600` | >= 60；<= 7200 | （无静态引用） |

## 上游拨号（4）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `ENABLE_ENDPOINT_CIRCUIT_BREAKER` | string | `"false"` | — | （无静态引用） |
| `FETCH_BODY_TIMEOUT` | number | `600_000` | — | （无静态引用） |
| `FETCH_CONNECT_TIMEOUT` | number | `30000` | — | （无静态引用） |
| `FETCH_HEADERS_TIMEOUT` | number | `600_000` | — | （无静态引用） |

## 其它（17）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `AUTO_MIGRATE` | string | `"true"` | — | （无静态引用） |
| `DASHBOARD_LOGS_POLL_INTERVAL_MS` | number | `5000` | >= 250；<= 60000 | `src/app/[locale]/dashboard/logs/page.tsx` |
| `DEBUG_MODE` | string | `"false"` | — | `src/lib/logger.ts` |
| `ENABLE_CACHE_EFFECTIVENESS` | string | `"true"` | — | `src/lib/api-client/v1/openapi-types.gen.ts` |
| `ENABLE_PREFIX_AFFINITY` | string | `"false"` | — | （无静态引用） |
| `ENABLE_REQUEST_REPLAY` | string | `"true"` | — | `src/lib/api-client/v1/openapi-types.gen.ts` |
| `ENABLE_SECURE_COOKIES` | string | `"true"` | — | （无静态引用） |
| `IP_GEO_API_TOKEN` | string（optional） | — | — | （无静态引用） |
| `IP_GEO_API_URL` | string | `"https://ip-api.claude-code-hub.app"` | — | （无静态引用） |
| `IP_GEO_CACHE_TTL_SECONDS` | number | `3600` | >= 60；<= 86400 | （无静态引用） |
| `IP_GEO_TIMEOUT_MS` | number | `1500` | >= 100；<= 10000 | （无静态引用） |
| `LEGACY_ACTIONS_DOCS_MODE` | enum | `"deprecated"` | ∈ {deprecated, hidden} | （无静态引用） |
| `LEGACY_ACTIONS_SUNSET_DATE` | string | `"2026-12-31"` | — | （无静态引用） |
| `MAX_RETRY_ATTEMPTS_DEFAULT` | number | `2` | >= 1；<= 10 | （无静态引用） |
| `PREFIX_AFFINITY_TTL_SECONDS` | number | `3600` | >= 60；<= 86400 | （无静态引用） |
| `PREFIX_AFFINITY_WINDOW` | number | `8` | >= 1；<= 64 | （无静态引用） |
| `TZ` | string | `"Asia/Shanghai"` | — | （无静态引用） |

## 可观测性（6）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `LANGFUSE_BASE_URL` | string | `"https://cloud.langfuse.com"` | — | （无静态引用） |
| `LANGFUSE_DEBUG` | string | `"false"` | — | （无静态引用） |
| `LANGFUSE_PUBLIC_KEY` | string（optional） | — | — | （无静态引用） |
| `LANGFUSE_SAMPLE_RATE` | number | `1.0` | >= 0；<= 1 | （无静态引用） |
| `LANGFUSE_SECRET_KEY` | string（optional） | — | — | （无静态引用） |
| `LOG_LEVEL` | enum | `"info"` | ∈ {fatal, error, warn, info, debug, trace} | `src/lib/logger.ts` |

## 多进程与进程内（2）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `NODE_ENV` | enum | `"development"` | ∈ {development, production, test} | `src/lib/config/env-flags.ts` |
| `PORT` | number | `23000` | — | （无静态引用） |

## 数据库（6）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `DB_LOCK_TIMEOUT_MS` | number（optionalNumber） | `5_000` | >= 100；<= 60000 | （无静态引用） |
| `DB_POOL_CONNECT_TIMEOUT` | number（optionalNumber） | — | >= 1；<= 120 | （无静态引用） |
| `DB_POOL_IDLE_TIMEOUT` | number（optionalNumber） | — | >= 0；<= 3600 | （无静态引用） |
| `DB_POOL_MAX` | number（optionalNumber） | — | >= 1；<= 200 | （无静态引用） |
| `DB_STATEMENT_TIMEOUT_MS` | number（optionalNumber） | `90_000` | >= 1000；<= 119000 | （无静态引用） |
| `DSN` | string（optionalPreprocessed） | — | — | `src/lib/migrate.ts` |

## 流式与门禁（7）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `DETACHED_STREAM_BUDGET_BYTES` | number | `64 * 1024 * 1024` | >= 3211264；<= 1073741824 | （无静态引用） |
| `DETACHED_STREAM_MAX_CONCURRENCY` | number | `64` | >= 1；<= 4096 | （无静态引用） |
| `DETACHED_STREAM_METERING_RESERVE_BYTES` | number | `16 * 1024 * 1024` | >= 65536；<= 1073741824 | （无静态引用） |
| `STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP` | number | `256 * 1024 * 1024` | >= 2048；<= 2147483648 | （无静态引用） |
| `STREAM_GATE_MODE` | enum | `"enforce"` | ∈ {off, shadow, enforce} | （无静态引用） |
| `STREAM_GATE_PREBUFFER_BYTE_CAP` | number | `10 * 1024 * 1024` | >= 1024；<= 67108864 | （无静态引用） |
| `STREAM_GATE_PREBUFFER_EVENT_CAP` | number | `64` | >= 1；<= 4096 | （无静态引用） |

## 消息写入（4）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `MESSAGE_REQUEST_ASYNC_BATCH_SIZE` | number（optionalNumber） | — | >= 1；<= 2000 | （无静态引用） |
| `MESSAGE_REQUEST_ASYNC_FLUSH_INTERVAL_MS` | number（optionalNumber） | — | >= 10；<= 60000 | （无静态引用） |
| `MESSAGE_REQUEST_ASYNC_MAX_PENDING` | number（optionalNumber） | — | >= 100；<= 200000 | （无静态引用） |
| `MESSAGE_REQUEST_WRITE_MODE` | enum | `"async"` | ∈ {sync, async} | （无静态引用） |

## 熔断与健康（2）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `ENABLE_CIRCUIT_BREAKER_ON_NETWORK_ERRORS` | string | `"false"` | — | （无静态引用） |
| `HEDGE_LOSER_DRAIN_TIMEOUT_MS` | number | `120_000` | >= 1000 | （无静态引用） |

## 管理与安全（4）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `ADMIN_TOKEN` | string（optionalPreprocessed） | — | 长度>= 1 | `src/app/[locale]/dashboard/providers/page.tsx` |
| `CSRF_SECRET` | string（optionalPreprocessed） | — | 长度>= 16 | （无静态引用） |
| `ENABLE_API_KEY_ADMIN_ACCESS` | string | `"false"` | — | （无静态引用） |
| `ENABLE_LEGACY_ACTIONS_API` | string | `"true"` | — | （无静态引用） |

## 限流与会话（10）

| 变量 | 类型 | 默认值 | 约束 | 主要消费方 |
| --- | --- | --- | --- | --- |
| `AUTH_SESSION_TTL_SECONDS` | number | `604_800` | >= 60；<= 31536000 | （无静态引用） |
| `ENABLE_RATE_LIMIT` | string | `"true"` | — | `src/lib/redis/client.ts` |
| `REPLAY_MAX_CONCURRENT_SPOOLS` | number | `64` | >= 1；<= 1024 | （无静态引用） |
| `SESSION_REQUEST_ARTIFACT_MAX_BYTES` | number | `5 * 1024 * 1024` | >= 65536；<= 67108864 | （无静态引用） |
| `SESSION_RESPONSE_BODY_DEDUP_ENABLED` | string | `"false"` | — | （无静态引用） |
| `SESSION_RESPONSE_BODY_MAX_BYTES` | number | `5 * 1024 * 1024` | >= 65536；<= 67108864 | （无静态引用） |
| `SESSION_TOKEN_MODE` | enum | `"opaque"` | ∈ {legacy, dual, opaque} | （无静态引用） |
| `SESSION_TTL` | number | `300` | — | `src/app/[locale]/settings/config/_components/system-settings-form.tsx` |
| `STORE_SESSION_MESSAGES` | string | `"false"` | — | `src/app/[locale]/dashboard/sessions/[sessionId]/messages/_components/session-details-tabs.tsx` |
| `STORE_SESSION_RESPONSE_BODY` | string | `"true"` | — | （无静态引用） |

## 交叉字段约束

`EnvSchema.superRefine` 引用的变量（Go 侧装载器必须复刻同样的关系校验）：

- `DETACHED_STREAM_BUDGET_BYTES`
- `DETACHED_STREAM_METERING_RESERVE_BYTES`
- `STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP`
- `STREAM_GATE_PREBUFFER_BYTE_CAP`

