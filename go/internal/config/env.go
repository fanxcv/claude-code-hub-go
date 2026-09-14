// Package config 中本文件承载与 TS 侧 src/lib/config/env.schema.ts 的逐变量契约。
//
// 契约真源是 tests/load/env-parity/env-matrix.json（由 scripts/export-env-matrix.ts 从
// env.schema.ts 导出）。本文件把它固化为一张规格表 envSpecs，并由同一张表同时驱动：
//   - 装载与校验（loadEnv）
//   - 结构体字段的反射赋值（EnvConfig）
//   - 对账清单（ParityVariableNames → go/env-parity.txt，由 cmd/envlist 生成）
//
// 三者同源，因此不可能出现「清单里有、装载器没实现」这类静默分叉。
//
// 语义逐条复刻 zod：
//   - 「未设置」与「设为空串」是两种输入，必须区分（故装载入口用 LookupEnvFunc）。
//   - 布尔：booleanTransform 为 s != "false" && s != "0"，大小写敏感，不做归一。
//     故 AUTO_MIGRATE="" 在 Node 侧得到 true（空串是「已设置」），本实现保持一致。
//   - 数值：z.coerce.number() 对空串得到 0（JS Number("")），非数字得到 NaN 并报错。
//   - optionalNumber：未设置或空串走 undefined；若内层 schema 带 default 则取该默认值。
//   - optionalPreprocessed（凭据类）：未设置、空串与占位符（change-me 等）都视为未配置。
//
// 与 TS 侧的刻意偏离见 README.md，全部是「更严」方向，不影响合法配置。
package config

import (
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

// envKind 是规格表里一条变量的解析类别，取值与 env.schema.ts 的写法一一对应。
type envKind uint8

const (
	// kindString：z.string().default(...)，未设置取默认值，空串是合法值。
	kindString envKind = iota
	// kindOptionalString：z.string().optional()，未设置即 nil。
	kindOptionalString
	// kindCredential：optionalPreprocessed(...)，空串与占位符视为未配置。
	kindCredential
	// kindBool：z.string().default(...).transform(booleanTransform)。
	kindBool
	// kindNumber：z.coerce.number()，可带 int/min/max/default。
	kindNumber
	// kindOptionalNumber：optionalNumber(...)，未设置或空串走 undefined。
	kindOptionalNumber
	// kindEnum：z.enum([...]).default(...)。
	kindEnum
)

// envSpec 是单个环境变量的契约。字段名与取值来自 env-matrix.json，逐条可对账。
type envSpec struct {
	name  string
	field string
	kind  envKind
	// def 是文本默认值；kindOptionalString / kindCredential 且无默认值时为空。
	def   string
	isInt bool
	// hasMin / hasMax 区分「无边界」与「边界恰为 0」。
	hasMin bool
	min    float64
	hasMax bool
	max    float64
	// minLen / maxLen 是字符串长度边界（对应 zod 的 .min()/.max() 用于字符串时）。
	minLen int
	maxLen int
	enum   []string
}

// envSpecs 按 env-matrix.json 的字母序排列，便于与矩阵逐行对照。
var envSpecs = []envSpec{
	{name: "ADMIN_TOKEN", field: "AdminToken", kind: kindCredential, minLen: 1},
	{name: "AUTH_SESSION_TTL_SECONDS", field: "AuthSessionTTLSeconds", kind: kindNumber, def: "604800", isInt: true, hasMin: true, min: 60, hasMax: true, max: 31536000},
	{name: "AUTO_MIGRATE", field: "AutoMigrate", kind: kindBool, def: "true"},
	{name: "CSRF_SECRET", field: "CSRFSecret", kind: kindCredential, minLen: 16},
	{name: "DASHBOARD_LOGS_POLL_INTERVAL_MS", field: "DashboardLogsPollIntervalMS", kind: kindNumber, def: "5000", isInt: true, hasMin: true, min: 250, hasMax: true, max: 60000},
	{name: "DB_LOCK_TIMEOUT_MS", field: "DBLockTimeoutMS", kind: kindOptionalNumber, def: "5000", isInt: true, hasMin: true, min: 100, hasMax: true, max: 60000},
	{name: "DB_POOL_CONNECT_TIMEOUT", field: "DBPoolConnectTimeout", kind: kindOptionalNumber, hasMin: true, min: 1, hasMax: true, max: 120},
	{name: "DB_POOL_IDLE_TIMEOUT", field: "DBPoolIdleTimeout", kind: kindOptionalNumber, hasMin: true, min: 0, hasMax: true, max: 3600},
	{name: "DB_POOL_MAX", field: "DBPoolMax", kind: kindOptionalNumber, isInt: true, hasMin: true, min: 1, hasMax: true, max: 200},
	{name: "DB_STATEMENT_TIMEOUT_MS", field: "DBStatementTimeoutMS", kind: kindOptionalNumber, def: "90000", isInt: true, hasMin: true, min: 1000, hasMax: true, max: 119000},
	{name: "DEBUG_MODE", field: "DebugMode", kind: kindBool, def: "false"},
	{name: "DETACHED_STREAM_BUDGET_BYTES", field: "DetachedStreamBudgetBytes", kind: kindNumber, def: "67108864", isInt: true, hasMin: true, min: 3211264, hasMax: true, max: 1073741824},
	{name: "DETACHED_STREAM_MAX_CONCURRENCY", field: "DetachedStreamMaxConcurrency", kind: kindNumber, def: "64", isInt: true, hasMin: true, min: 1, hasMax: true, max: 4096},
	{name: "DETACHED_STREAM_METERING_RESERVE_BYTES", field: "DetachedStreamMeteringReserveBytes", kind: kindNumber, def: "16777216", isInt: true, hasMin: true, min: 65536, hasMax: true, max: 1073741824},
	{name: "DSN", field: "DSN", kind: kindCredential},
	{name: "ENABLE_API_KEY_ADMIN_ACCESS", field: "EnableAPIKeyAdminAccess", kind: kindBool, def: "false"},
	{name: "ENABLE_CACHE_EFFECTIVENESS", field: "EnableCacheEffectiveness", kind: kindBool, def: "true"},
	{name: "ENABLE_CIRCUIT_BREAKER_ON_NETWORK_ERRORS", field: "EnableCircuitBreakerOnNetworkErrors", kind: kindBool, def: "false"},
	{name: "ENABLE_ENDPOINT_CIRCUIT_BREAKER", field: "EnableEndpointCircuitBreaker", kind: kindBool, def: "false"},
	{name: "ENABLE_LEGACY_ACTIONS_API", field: "EnableLegacyActionsAPI", kind: kindBool, def: "true"},
	{name: "ENABLE_PREFIX_AFFINITY", field: "EnablePrefixAffinity", kind: kindBool, def: "false"},
	{name: "ENABLE_PROVIDER_CACHE", field: "EnableProviderCache", kind: kindBool, def: "true"},
	{name: "ENABLE_RATE_LIMIT", field: "EnableRateLimit", kind: kindBool, def: "true"},
	{name: "ENABLE_REQUEST_REPLAY", field: "EnableRequestReplay", kind: kindBool, def: "true"},
	{name: "ENABLE_SECURE_COOKIES", field: "EnableSecureCookies", kind: kindBool, def: "true"},
	{name: "FETCH_BODY_TIMEOUT", field: "FetchBodyTimeout", kind: kindNumber, def: "600000"},
	{name: "FETCH_CONNECT_TIMEOUT", field: "FetchConnectTimeout", kind: kindNumber, def: "30000"},
	{name: "FETCH_HEADERS_TIMEOUT", field: "FetchHeadersTimeout", kind: kindNumber, def: "600000"},
	{name: "HEDGE_LOSER_DRAIN_TIMEOUT_MS", field: "HedgeLoserDrainTimeoutMS", kind: kindNumber, def: "120000", isInt: true, hasMin: true, min: 1000},
	{name: "IP_GEO_API_TOKEN", field: "IPGeoAPIToken", kind: kindOptionalString},
	{name: "IP_GEO_API_URL", field: "IPGeoAPIURL", kind: kindString, def: "https://ip-api.claude-code-hub.app"},
	{name: "IP_GEO_CACHE_TTL_SECONDS", field: "IPGeoCacheTTLSeconds", kind: kindNumber, def: "3600", isInt: true, hasMin: true, min: 60, hasMax: true, max: 86400},
	{name: "IP_GEO_TIMEOUT_MS", field: "IPGeoTimeoutMS", kind: kindNumber, def: "1500", isInt: true, hasMin: true, min: 100, hasMax: true, max: 10000},
	{name: "LANGFUSE_BASE_URL", field: "LangfuseBaseURL", kind: kindString, def: "https://cloud.langfuse.com"},
	{name: "LANGFUSE_DEBUG", field: "LangfuseDebug", kind: kindBool, def: "false"},
	{name: "LANGFUSE_PUBLIC_KEY", field: "LangfusePublicKey", kind: kindOptionalString},
	{name: "LANGFUSE_SAMPLE_RATE", field: "LangfuseSampleRate", kind: kindNumber, def: "1", hasMin: true, min: 0, hasMax: true, max: 1},
	{name: "LANGFUSE_SECRET_KEY", field: "LangfuseSecretKey", kind: kindOptionalString},
	{name: "LEGACY_ACTIONS_DOCS_MODE", field: "LegacyActionsDocsMode", kind: kindEnum, def: "deprecated", enum: []string{"deprecated", "hidden"}},
	{name: "LEGACY_ACTIONS_SUNSET_DATE", field: "LegacyActionsSunsetDate", kind: kindString, def: "2026-12-31"},
	{name: "LOG_LEVEL", field: "LogLevel", kind: kindEnum, def: "info", enum: []string{"fatal", "error", "warn", "info", "debug", "trace"}},
	{name: "MAX_RETRY_ATTEMPTS_DEFAULT", field: "MaxRetryAttemptsDefault", kind: kindNumber, def: "2", hasMin: true, min: 1, hasMax: true, max: 10},
	{name: "MESSAGE_REQUEST_ASYNC_BATCH_SIZE", field: "MessageRequestAsyncBatchSize", kind: kindOptionalNumber, isInt: true, hasMin: true, min: 1, hasMax: true, max: 2000},
	{name: "MESSAGE_REQUEST_ASYNC_FLUSH_INTERVAL_MS", field: "MessageRequestAsyncFlushIntervalMS", kind: kindOptionalNumber, isInt: true, hasMin: true, min: 10, hasMax: true, max: 60000},
	{name: "MESSAGE_REQUEST_ASYNC_MAX_PENDING", field: "MessageRequestAsyncMaxPending", kind: kindOptionalNumber, isInt: true, hasMin: true, min: 100, hasMax: true, max: 200000},
	{name: "MESSAGE_REQUEST_WRITE_MODE", field: "MessageRequestWriteMode", kind: kindEnum, def: "async", enum: []string{"sync", "async"}},
	{name: "NODE_ENV", field: "NodeEnv", kind: kindEnum, def: "development", enum: []string{"development", "production", "test"}},
	{name: "PORT", field: "Port", kind: kindNumber, def: "23000"},
	{name: "PREFIX_AFFINITY_TTL_SECONDS", field: "PrefixAffinityTTLSeconds", kind: kindNumber, def: "3600", isInt: true, hasMin: true, min: 60, hasMax: true, max: 86400},
	{name: "PREFIX_AFFINITY_WINDOW", field: "PrefixAffinityWindow", kind: kindNumber, def: "8", isInt: true, hasMin: true, min: 1, hasMax: true, max: 64},
	{name: "REDIS_COMMAND_TIMEOUT_MS", field: "RedisCommandTimeoutMS", kind: kindOptionalNumber, def: "10000", isInt: true, hasMin: true, min: 100, hasMax: true, max: 120000},
	{name: "REDIS_TLS_REJECT_UNAUTHORIZED", field: "RedisTLSRejectUnauthorized", kind: kindBool, def: "true"},
	{name: "REDIS_URL", field: "RedisURL", kind: kindOptionalString},
	{name: "REPLAY_LIVE_DEDUP_ENABLED", field: "ReplayLiveDedupEnabled", kind: kindBool, def: "true"},
	{name: "REPLAY_MAX_CONCURRENT_SPOOLS", field: "ReplayMaxConcurrentSpools", kind: kindNumber, def: "64", isInt: true, hasMin: true, min: 1, hasMax: true, max: 1024},
	{name: "REPLAY_MAX_DETACHED_MS", field: "ReplayMaxDetachedMS", kind: kindNumber, def: "300000", isInt: true, hasMin: true, min: 10000, hasMax: true, max: 1800000},
	{name: "REPLAY_MAX_PAYLOAD_BYTES", field: "ReplayMaxPayloadBytes", kind: kindNumber, def: "8388608", isInt: true, hasMin: true, min: 65536, hasMax: true, max: 67108864},
	{name: "REPLAY_TTL_SECONDS", field: "ReplayTTLSeconds", kind: kindNumber, def: "600", isInt: true, hasMin: true, min: 60, hasMax: true, max: 7200},
	{name: "SESSION_REQUEST_ARTIFACT_MAX_BYTES", field: "SessionRequestArtifactMaxBytes", kind: kindNumber, def: "5242880", isInt: true, hasMin: true, min: 65536, hasMax: true, max: 67108864},
	{name: "SESSION_RESPONSE_BODY_DEDUP_ENABLED", field: "SessionResponseBodyDedupEnabled", kind: kindBool, def: "false"},
	{name: "SESSION_RESPONSE_BODY_MAX_BYTES", field: "SessionResponseBodyMaxBytes", kind: kindNumber, def: "5242880", isInt: true, hasMin: true, min: 65536, hasMax: true, max: 67108864},
	{name: "SESSION_TOKEN_MODE", field: "SessionTokenMode", kind: kindEnum, def: "opaque", enum: []string{"legacy", "dual", "opaque"}},
	{name: "SESSION_TTL", field: "SessionTTL", kind: kindNumber, def: "300"},
	{name: "STORE_SESSION_MESSAGES", field: "StoreSessionMessages", kind: kindBool, def: "false"},
	{name: "STORE_SESSION_RESPONSE_BODY", field: "StoreSessionResponseBody", kind: kindBool, def: "true"},
	{name: "STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP", field: "StreamGateGlobalPrebufferByteCap", kind: kindNumber, def: "268435456", isInt: true, hasMin: true, min: 2048, hasMax: true, max: 2147483648},
	{name: "STREAM_GATE_MODE", field: "StreamGateMode", kind: kindEnum, def: "enforce", enum: []string{"off", "shadow", "enforce"}},
	{name: "STREAM_GATE_PREBUFFER_BYTE_CAP", field: "StreamGatePrebufferByteCap", kind: kindNumber, def: "10485760", isInt: true, hasMin: true, min: 1024, hasMax: true, max: 67108864},
	{name: "STREAM_GATE_PREBUFFER_EVENT_CAP", field: "StreamGatePrebufferEventCap", kind: kindNumber, def: "64", isInt: true, hasMin: true, min: 1, hasMax: true, max: 4096},
	{name: "TZ", field: "TZ", kind: kindString, def: "Asia/Shanghai"},
}

// ParityVariableNames 返回受契约覆盖的变量名，顺序与 envSpecs 一致。
// go/env-parity.txt 由 cmd/envlist 用本函数生成，不要手工维护该文件。
func ParityVariableNames() []string {
	names := make([]string, 0, len(envSpecs))
	for _, spec := range envSpecs {
		names = append(names, spec.name)
	}
	return names
}

// LookupEnvFunc 与 os.LookupEnv 同形：第二个返回值区分「未设置」与「设为空串」。
type LookupEnvFunc func(string) (string, bool)

// LookupFromOS 返回读进程环境的 LookupEnvFunc。
func LookupFromOS() LookupEnvFunc { return os.LookupEnv }

// EnvConfig 与 TS 侧 EnvConfig 逐字段对应。指针字段表示「未配置」。
type EnvConfig struct {
	// ADMIN_TOKEN
	AdminToken *string `env:"ADMIN_TOKEN"`
	// AUTH_SESSION_TTL_SECONDS
	AuthSessionTTLSeconds int `env:"AUTH_SESSION_TTL_SECONDS"`
	// AUTO_MIGRATE
	AutoMigrate bool `env:"AUTO_MIGRATE"`
	// CSRF_SECRET
	CSRFSecret *string `env:"CSRF_SECRET"`
	// DASHBOARD_LOGS_POLL_INTERVAL_MS
	DashboardLogsPollIntervalMS int `env:"DASHBOARD_LOGS_POLL_INTERVAL_MS"`
	// DB_LOCK_TIMEOUT_MS：等待数据库锁的最长时间（毫秒）
	DBLockTimeoutMS *int `env:"DB_LOCK_TIMEOUT_MS"`
	// DB_POOL_CONNECT_TIMEOUT：建连超时（秒）
	DBPoolConnectTimeout *float64 `env:"DB_POOL_CONNECT_TIMEOUT"`
	// DB_POOL_IDLE_TIMEOUT：空闲连接回收（秒）
	DBPoolIdleTimeout *float64 `env:"DB_POOL_IDLE_TIMEOUT"`
	// DB_POOL_MAX：PostgreSQL 连接池配置（postgres.js）
	DBPoolMax *int `env:"DB_POOL_MAX"`
	// DB_STATEMENT_TIMEOUT_MS：活动语句超时（毫秒），必须早于流式结算的 120 秒应用层 deadline
	DBStatementTimeoutMS *int `env:"DB_STATEMENT_TIMEOUT_MS"`
	// DEBUG_MODE
	DebugMode bool `env:"DEBUG_MODE"`
	// DETACHED_STREAM_BUDGET_BYTES
	DetachedStreamBudgetBytes int `env:"DETACHED_STREAM_BUDGET_BYTES"`
	// DETACHED_STREAM_MAX_CONCURRENCY
	DetachedStreamMaxConcurrency int `env:"DETACHED_STREAM_MAX_CONCURRENCY"`
	// DETACHED_STREAM_METERING_RESERVE_BYTES
	DetachedStreamMeteringReserveBytes int `env:"DETACHED_STREAM_METERING_RESERVE_BYTES"`
	// DSN
	DSN *string `env:"DSN"`
	// ENABLE_API_KEY_ADMIN_ACCESS
	EnableAPIKeyAdminAccess bool `env:"ENABLE_API_KEY_ADMIN_ACCESS"`
	// ENABLE_CACHE_EFFECTIVENESS：缓存效果计费模拟：理论 vs 实际缓存命中率聚合指标（仅展示，不影响路由，默认开启）
	EnableCacheEffectiveness bool `env:"ENABLE_CACHE_EFFECTIVENESS"`
	// ENABLE_CIRCUIT_BREAKER_ON_NETWORK_ERRORS
	EnableCircuitBreakerOnNetworkErrors bool `env:"ENABLE_CIRCUIT_BREAKER_ON_NETWORK_ERRORS"`
	// ENABLE_ENDPOINT_CIRCUIT_BREAKER：端点级别熔断器开关
	EnableEndpointCircuitBreaker bool `env:"ENABLE_ENDPOINT_CIRCUIT_BREAKER"`
	// ENABLE_LEGACY_ACTIONS_API
	EnableLegacyActionsAPI bool `env:"ENABLE_LEGACY_ACTIONS_API"`
	// ENABLE_PREFIX_AFFINITY：最长前缀亲和路由：链式指纹匹配的供应商粘性（软提名，仍走全套硬校验）
	EnablePrefixAffinity bool `env:"ENABLE_PREFIX_AFFINITY"`
	// ENABLE_PROVIDER_CACHE：供应商缓存开关
	EnableProviderCache bool `env:"ENABLE_PROVIDER_CACHE"`
	// ENABLE_RATE_LIMIT
	EnableRateLimit bool `env:"ENABLE_RATE_LIMIT"`
	// ENABLE_REQUEST_REPLAY：请求分离 + Replay：客户端断开后上游继续引流缓存，相同请求体重发续传
	EnableRequestReplay bool `env:"ENABLE_REQUEST_REPLAY"`
	// ENABLE_SECURE_COOKIES
	EnableSecureCookies bool `env:"ENABLE_SECURE_COOKIES"`
	// FETCH_BODY_TIMEOUT：Fetch 超时配置（毫秒）
	FetchBodyTimeout float64 `env:"FETCH_BODY_TIMEOUT"`
	// FETCH_CONNECT_TIMEOUT
	FetchConnectTimeout float64 `env:"FETCH_CONNECT_TIMEOUT"`
	// FETCH_HEADERS_TIMEOUT
	FetchHeadersTimeout float64 `env:"FETCH_HEADERS_TIMEOUT"`
	// HEDGE_LOSER_DRAIN_TIMEOUT_MS：竞速输家计费：后台 drain 竞速输家响应体以拿回 token 用量时的最大等待时长（毫秒）。
	HedgeLoserDrainTimeoutMS int `env:"HEDGE_LOSER_DRAIN_TIMEOUT_MS"`
	// IP_GEO_API_TOKEN
	IPGeoAPIToken *string `env:"IP_GEO_API_TOKEN"`
	// IP_GEO_API_URL：IP 归属地查询服务
	IPGeoAPIURL string `env:"IP_GEO_API_URL"`
	// IP_GEO_CACHE_TTL_SECONDS
	IPGeoCacheTTLSeconds int `env:"IP_GEO_CACHE_TTL_SECONDS"`
	// IP_GEO_TIMEOUT_MS
	IPGeoTimeoutMS int `env:"IP_GEO_TIMEOUT_MS"`
	// LANGFUSE_BASE_URL
	LangfuseBaseURL string `env:"LANGFUSE_BASE_URL"`
	// LANGFUSE_DEBUG
	LangfuseDebug bool `env:"LANGFUSE_DEBUG"`
	// LANGFUSE_PUBLIC_KEY
	LangfusePublicKey *string `env:"LANGFUSE_PUBLIC_KEY"`
	// LANGFUSE_SAMPLE_RATE
	LangfuseSampleRate float64 `env:"LANGFUSE_SAMPLE_RATE"`
	// LANGFUSE_SECRET_KEY
	LangfuseSecretKey *string `env:"LANGFUSE_SECRET_KEY"`
	// LEGACY_ACTIONS_DOCS_MODE
	LegacyActionsDocsMode string `env:"LEGACY_ACTIONS_DOCS_MODE"`
	// LEGACY_ACTIONS_SUNSET_DATE
	LegacyActionsSunsetDate string `env:"LEGACY_ACTIONS_SUNSET_DATE"`
	// LOG_LEVEL
	LogLevel string `env:"LOG_LEVEL"`
	// MAX_RETRY_ATTEMPTS_DEFAULT
	MaxRetryAttemptsDefault float64 `env:"MAX_RETRY_ATTEMPTS_DEFAULT"`
	// MESSAGE_REQUEST_ASYNC_BATCH_SIZE
	MessageRequestAsyncBatchSize *int `env:"MESSAGE_REQUEST_ASYNC_BATCH_SIZE"`
	// MESSAGE_REQUEST_ASYNC_FLUSH_INTERVAL_MS：异步批量写入参数
	MessageRequestAsyncFlushIntervalMS *int `env:"MESSAGE_REQUEST_ASYNC_FLUSH_INTERVAL_MS"`
	// MESSAGE_REQUEST_ASYNC_MAX_PENDING
	MessageRequestAsyncMaxPending *int `env:"MESSAGE_REQUEST_ASYNC_MAX_PENDING"`
	// MESSAGE_REQUEST_WRITE_MODE：message_request 写入模式
	MessageRequestWriteMode string `env:"MESSAGE_REQUEST_WRITE_MODE"`
	// NODE_ENV
	NodeEnv string `env:"NODE_ENV"`
	// PORT
	Port float64 `env:"PORT"`
	// PREFIX_AFFINITY_TTL_SECONDS
	PrefixAffinityTTLSeconds int `env:"PREFIX_AFFINITY_TTL_SECONDS"`
	// PREFIX_AFFINITY_WINDOW：指纹链回看窗口（尾部边界数）：覆盖编辑回退场景的拐点，超过 8 收益递减
	PrefixAffinityWindow int `env:"PREFIX_AFFINITY_WINDOW"`
	// REDIS_COMMAND_TIMEOUT_MS
	RedisCommandTimeoutMS *int `env:"REDIS_COMMAND_TIMEOUT_MS"`
	// REDIS_TLS_REJECT_UNAUTHORIZED
	RedisTLSRejectUnauthorized bool `env:"REDIS_TLS_REJECT_UNAUTHORIZED"`
	// REDIS_URL
	RedisURL *string `env:"REDIS_URL"`
	// REPLAY_LIVE_DEDUP_ENABLED
	ReplayLiveDedupEnabled bool `env:"REPLAY_LIVE_DEDUP_ENABLED"`
	// REPLAY_MAX_CONCURRENT_SPOOLS：单节点并发 spool 上限（超出的请求不做 replay，回退现状）
	ReplayMaxConcurrentSpools int `env:"REPLAY_MAX_CONCURRENT_SPOOLS"`
	// REPLAY_MAX_DETACHED_MS：客户端断开后上游继续引流的最长时长（毫秒；替代默认 60s drain 上限）
	ReplayMaxDetachedMS int `env:"REPLAY_MAX_DETACHED_MS"`
	// REPLAY_MAX_PAYLOAD_BYTES：单响应缓存上限（超限即放弃 spool，fail-open 回现状）
	ReplayMaxPayloadBytes int `env:"REPLAY_MAX_PAYLOAD_BYTES"`
	// REPLAY_TTL_SECONDS：Redis 热层 TTL（活跃/刚完成的响应块与元数据）
	ReplayTTLSeconds int `env:"REPLAY_TTL_SECONDS"`
	// SESSION_REQUEST_ARTIFACT_MAX_BYTES
	SessionRequestArtifactMaxBytes int `env:"SESSION_REQUEST_ARTIFACT_MAX_BYTES"`
	// SESSION_RESPONSE_BODY_DEDUP_ENABLED：两阶段发布开关。false 时保持旧 key 写入，所有实例升级后再启用单 key 去重布局。
	SessionResponseBodyDedupEnabled bool `env:"SESSION_RESPONSE_BODY_DEDUP_ENABLED"`
	// SESSION_RESPONSE_BODY_MAX_BYTES：会话响应正文写入 Redis 的字节上限。旧布局按单份限制，去重布局按唯一正文总字节限制。
	SessionResponseBodyMaxBytes int `env:"SESSION_RESPONSE_BODY_MAX_BYTES"`
	// SESSION_TOKEN_MODE
	SessionTokenMode string `env:"SESSION_TOKEN_MODE"`
	// SESSION_TTL
	SessionTTL float64 `env:"SESSION_TTL"`
	// STORE_SESSION_MESSAGES：会话消息存储控制
	StoreSessionMessages bool `env:"STORE_SESSION_MESSAGES"`
	// STORE_SESSION_RESPONSE_BODY：会话响应体存储开关
	StoreSessionResponseBody bool `env:"STORE_SESSION_RESPONSE_BODY"`
	// STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP：所有正在门禁 precommit 阶段及等待下游消费的前缀共享该进程级预算。
	StreamGateGlobalPrebufferByteCap int `env:"STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP"`
	// STREAM_GATE_MODE：===== CCHP 网关移植功能开关 =====
	StreamGateMode string `env:"STREAM_GATE_MODE"`
	// STREAM_GATE_PREBUFFER_BYTE_CAP
	StreamGatePrebufferByteCap int `env:"STREAM_GATE_PREBUFFER_BYTE_CAP"`
	// STREAM_GATE_PREBUFFER_EVENT_CAP：门控 precommit 缓冲上限：超限即视为该供应商流异常，failover 释放内存
	StreamGatePrebufferEventCap int `env:"STREAM_GATE_PREBUFFER_EVENT_CAP"`
	// TZ
	TZ string `env:"TZ"`
}

// LoadEnv 按 envSpecs 装载并校验全部 70 个契约变量。
func LoadEnv(lookup LookupEnvFunc) (EnvConfig, error) {
	return loadEnv(lookup, nil)
}

// loadEnv 是 LoadEnv 的内部形态；seen 非 nil 时记录被处理的变量名，
// 供测试断言「规格表里的每一项都被装载器真正读过」。
func loadEnv(lookup LookupEnvFunc, seen *[]string) (EnvConfig, error) {
	var env EnvConfig
	value := reflect.ValueOf(&env).Elem()

	for _, spec := range envSpecs {
		if seen != nil {
			*seen = append(*seen, spec.name)
		}
		raw, present := lookup(spec.name)
		parsed, err := parseEnvValue(spec, raw, present)
		if err != nil {
			return EnvConfig{}, err
		}
		field := value.FieldByName(spec.field)
		if !field.IsValid() {
			// 规格表与结构体不一致属程序缺陷，直接暴露而不是静默跳过。
			return EnvConfig{}, fmt.Errorf("内部错误：%s 对应的字段 %s 不存在于 EnvConfig", spec.name, spec.field)
		}
		if parsed == nil {
			field.Set(reflect.Zero(field.Type()))
			continue
		}
		field.Set(reflect.ValueOf(parsed))
	}

	if err := validateEnvCrossField(env); err != nil {
		return EnvConfig{}, err
	}
	return env, nil
}

// parseEnvValue 按 spec 的类别解析单个变量；返回 nil 表示未配置。
func parseEnvValue(spec envSpec, raw string, present bool) (any, error) {
	switch spec.kind {
	case kindString:
		if !present {
			return spec.def, nil
		}
		return raw, nil

	case kindOptionalString:
		if !present {
			return nil, nil
		}
		value := raw
		return &value, nil

	case kindCredential:
		// 空串与占位符都视为未配置；DSN 另有占位符模板判定。
		if !present || raw == "" {
			return nil, nil
		}
		if raw == "change-me" {
			return nil, nil
		}
		if spec.name == "DSN" && strings.Contains(raw, "user:password@host:port") {
			return nil, nil
		}
		if spec.minLen > 0 && utf8.RuneCountInString(raw) < spec.minLen {
			return nil, fmt.Errorf("%s 至少需要 %d 个字符", spec.name, spec.minLen)
		}
		if spec.maxLen > 0 && utf8.RuneCountInString(raw) > spec.maxLen {
			return nil, fmt.Errorf("%s 最多允许 %d 个字符", spec.name, spec.maxLen)
		}
		value := raw
		return &value, nil

	case kindBool:
		// 未设置走默认值，已设置（含空串）走 booleanTransform。
		text := spec.def
		if present {
			text = raw
		}
		return booleanTransform(text), nil

	case kindNumber:
		text := spec.def
		if present {
			text = raw
		}
		return parseNumericSpec(spec, text, spec.name)

	case kindOptionalNumber:
		var parsed any
		var err error
		if !present || raw == "" {
			if spec.def == "" {
				return nil, nil
			}
			parsed, err = parseNumericSpec(spec, spec.def, spec.name)
		} else {
			parsed, err = parseNumericSpec(spec, raw, spec.name)
		}
		if err != nil {
			return nil, err
		}
		// optional 类的字段是指针，未配置由 nil 表达。
		return pointerTo(parsed), nil

	case kindEnum:
		text := spec.def
		if present {
			text = raw
		}
		for _, candidate := range spec.enum {
			if text == candidate {
				return text, nil
			}
		}
		return nil, fmt.Errorf("%s 必须是 %s 之一，收到 %q", spec.name, strings.Join(spec.enum, "|"), text)
	}
	return nil, fmt.Errorf("内部错误：%s 的类别未知", spec.name)
}

// parseNumericSpec 复刻 z.coerce.number()：空串得 0，非数字即错，随后查 int 与边界。
func parseNumericSpec(spec envSpec, text, name string) (any, error) {
	trimmed := strings.TrimSpace(text)
	value := 0.0
	if trimmed != "" {
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, fmt.Errorf("%s 必须是数字，收到 %q", name, text)
		}
		value = parsed
	}
	if math.IsNaN(value) {
		return nil, fmt.Errorf("%s 必须是数字，收到 %q", name, text)
	}
	if spec.isInt && value != math.Trunc(value) {
		return nil, fmt.Errorf("%s 必须是整数，收到 %q", name, text)
	}
	if spec.hasMin && value < spec.min {
		return nil, fmt.Errorf("%s 不能小于 %s，收到 %s", name, formatBound(spec.min), formatNumber(value))
	}
	if spec.hasMax && value > spec.max {
		return nil, fmt.Errorf("%s 不能大于 %s，收到 %s", name, formatBound(spec.max), formatNumber(value))
	}
	if spec.isInt {
		return int(value), nil
	}
	return value, nil
}

// pointerTo 把解析出的数值包成字段所需的指针类型。
func pointerTo(value any) any {
	switch typed := value.(type) {
	case int:
		return &typed
	case float64:
		return &typed
	default:
		return nil
	}
}

// booleanTransform 逐字复刻 env.schema.ts 的同名函数：大小写敏感，不做归一。
func booleanTransform(text string) bool {
	return text != "false" && text != "0"
}

func formatBound(value float64) string {
	if value == math.Trunc(value) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func formatNumber(value float64) string {
	if value == math.Trunc(value) {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// validateEnvCrossField 复刻 env.schema.ts 末尾的 superRefine 两条跨字段约束。
func validateEnvCrossField(env EnvConfig) error {
	if env.DetachedStreamMeteringReserveBytes > env.DetachedStreamBudgetBytes {
		return fmt.Errorf(
			"DETACHED_STREAM_METERING_RESERVE_BYTES cannot exceed DETACHED_STREAM_BUDGET_BYTES（%d > %d）",
			env.DetachedStreamMeteringReserveBytes, env.DetachedStreamBudgetBytes)
	}
	if env.StreamGateGlobalPrebufferByteCap < env.StreamGatePrebufferByteCap*4 {
		return fmt.Errorf(
			"STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP must be at least four times STREAM_GATE_PREBUFFER_BYTE_CAP（%d < 4 * %d）",
			env.StreamGateGlobalPrebufferByteCap, env.StreamGatePrebufferByteCap)
	}
	return nil
}

// EnvSummary 返回可安全写进日志的契约摘要：只报计数、档位与「凭据是否已配置」，
// 任何凭据原文（DSN、ADMIN_TOKEN、CSRF_SECRET、Langfuse 与 IP 归属地密钥）都不出现。
func (e EnvConfig) EnvSummary() map[string]any {
	return map[string]any{
		"coveredVariables":         len(envSpecs),
		"logLevel":                 e.LogLevel,
		"streamGateMode":           e.StreamGateMode,
		"messageRequestWriteMode":  e.MessageRequestWriteMode,
		"sessionTokenMode":         e.SessionTokenMode,
		"enableRateLimit":          e.EnableRateLimit,
		"enableRequestReplay":      e.EnableRequestReplay,
		"storeSessionResponseBody": e.StoreSessionResponseBody,
		"adminTokenConfigured":     e.AdminToken != nil,
		"csrfSecretConfigured":     e.CSRFSecret != nil,
		"dsnConfigured":            e.DSN != nil,
		"redisConfigured":          e.RedisURL != nil,
		"langfuseConfigured":       e.LangfusePublicKey != nil && e.LangfuseSecretKey != nil,
	}
}
