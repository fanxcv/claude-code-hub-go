// Package config 装载并校验 cchd 的环境变量配置。
//
// 与 TS 侧 src/lib/config/env.schema.ts 一一对应：变量名、默认值、取值范围与
// 「非法即 fail fast」的语义必须一致，否则同一份 .env 会在两个数据面上分叉。
//
// 凭据纪律：DSN、REDIS_URL、ADMIN_TOKEN 属于凭据，任何日志、错误信息与格式化输出
// 都不得包含其原文。相关错误只报变量名与「已隐去」。
package config

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// NodeEnv 与 TS 侧 NODE_ENV 的三档取值一致。
type NodeEnv string

const (
	NodeEnvDevelopment NodeEnv = "development"
	NodeEnvProduction  NodeEnv = "production"
	NodeEnvTest        NodeEnv = "test"
)

// EgressPages 决定非 API 路径（SSR 页面、静态资源）的归属。
//
// Node 退役后不再有「反代回页面」这一档：原 `node` 档与 `CCH_INTERNAL_PORT` 一并删除
// 页面面要么由本进程的嵌入产物服务，
// 要么不由本进程管。
type EgressPages string

const (
	// EgressPagesOff 不服务非 API 路径：这类路径由本进程自答。**默认值**——
	// 缺省不再把这类路径交给一个已退役的后端，而是由运维显式开启嵌入产物。
	// 现役部署显式设 embed，故默认值变动对存量部署无影响。
	EgressPagesOff EgressPages = "off"
	// EgressPagesEmbed 从 embed 进二进制的 UI 静态产物作答（终态）：真正未实现的
	// 路径交给前端路由渲染，产物缺失时启动即失败（见 internal/uiapp 的 fail fast）。
	EgressPagesEmbed EgressPages = "embed"
)

// PatrolConfig 是「未落终态行」自愈巡检的配置（CCH_PATROL_*）。
//
// 这是 Go 专有面：Node 侧没有对应实现（见 的
// 「无自愈机制」），因此这些变量不进 envSpecs（那张表是 env.schema.ts 的逐变量契约），
// 与 CCH_EGRESS_* / CCH_GO_* 同类。
type PatrolConfig struct {
	// Enabled 为 false 时不装配巡检（回退档）。
	Enabled bool
	// UnsettledAfter 是「多久没终态即认定丢失」的阈值。它必须大于本部署可能的最长
	// 活跃流时长：没有任何全局上限约束流式响应总时长（streaming_idle_timeout_ms
	// 默认 0 = 不限制），实测最长 347.5 s、p99.9 300 s，故默认取 30 分钟（>5×p99.9）。
	UnsettledAfter time.Duration
	// Interval 是巡检间隔。
	Interval time.Duration
	// BatchSize 是单次查询的行数上限。
	BatchSize int
	// MaxRowsPerRound 是单轮跨页累计的处理行数上限。
	MaxRowsPerRound int
}

// DefaultPprofAddr 是性能剖析面的默认监听地址：**只绑回环**。
//
// 历史：它曾从 `CCH_INTERNAL_PORT`（Node 回退端口）派生，以避开与回退端口相撞。回退端口已随
// Node 退役删除，故改用常数 3101——取的是过去在默认回退端口（3100）下派生出的同一个值，
// 历史部署行为不变。
const DefaultPprofAddr = "127.0.0.1:3101"

// PprofConfig 是性能剖析面（pprof + 运行时指标）的配置（CCH_PPROF_*）。
//
// Go 专有面：Node 侧没有这个面，故这些变量不进 envSpecs，与 CCH_EGRESS_* / CCH_PATROL_*
// 同类（不进 go/env-parity.txt，免得被对账脚本判为「Go 多配」）。
type PprofConfig struct {
	// Enabled 为 false（默认）时不注册任何路由：所有路径一律 404。
	Enabled bool
	// Addr 是监听地址，**只应是回环地址**；非回环会绑得上但在启动日志里告警。
	Addr string
	// BlockProfileRate 是阻塞剖析的采样阈值（纳秒）：采「阻塞超过该阈值」的事件，
	// 0 表示关闭。默认 1ms：这个量级下开销可忽略（只记长阻塞），
	// 而它恰好能回答「CPU 不高但延迟高，谁在等谁」。
	BlockProfileRate int
	// MutexProfileFraction 是互斥锁剖析采样率（1/N）：0 表示关闭（默认）。
	// 它随争用上升而变贵，故不默认开；要查锁争用时显式设成 5/1 之类。
	MutexProfileFraction int
}

// Lane 是数据库连接池的逻辑分道，对应 TS 侧的 DbLane。
type Lane string

const (
	LaneData    Lane = "data"
	LaneControl Lane = "control"
	LaneWriter  Lane = "writer"
)

// 与 TS 侧 db.ts 的常量保持一致。
const (
	minOutstandingPerPool    = 32
	outstandingPerConnection = 8

	// DefaultProductionPoolTotal / DefaultDevelopmentPoolTotal 复刻 db.ts 的
	// defaultTotal：生产 40 → data=31 → maxOutstanding=248。
	DefaultProductionPoolTotal  = 40
	DefaultDevelopmentPoolTotal = 10
)

// 连接池预算的取值边界，复刻 env.schema.ts 对 DB_POOL_MAX 的约束。
const (
	MinPoolTotal = 1
	MaxPoolTotal = 200
)

// PoolBudget 是按 lane 切分后的连接预算。
type PoolBudget struct {
	Data    int
	Control int
	Writer  int
}

// SplitPoolBudget 复刻 db.ts 的 splitPoolBudget：
//
//	total == 1 -> {data:0, control:1, writer:0}
//	total == 2 -> {data:1, control:1, writer:0}
//	otherwise  -> writer=1, control=min(total-2, max(1, round(total*0.2))), data=total-control-writer
//
// 注意计划文档 §8 给出的公式是上面的通用分支；total 为 1 或 2 时源码走了特例，
// 这里以源码为准。
func SplitPoolBudget(total int) PoolBudget {
	switch total {
	case 1:
		return PoolBudget{Data: 0, Control: 1, Writer: 0}
	case 2:
		return PoolBudget{Data: 1, Control: 1, Writer: 0}
	default:
		writer := 1
		control := min(total-2, max(1, int(roundHalfUp(float64(total)*0.2))))
		return PoolBudget{Data: total - control - writer, Control: control, Writer: writer}
	}
}

// roundHalfUp 复刻 JS Math.round 对 .5 的向上取整语义。
func roundHalfUp(value float64) float64 {
	return float64(int64(value + 0.5))
}

// Size 返回该 lane 的预算连接数。
func (b PoolBudget) Size(lane Lane) int {
	switch lane {
	case LaneData:
		return b.Data
	case LaneControl:
		return b.Control
	case LaneWriter:
		return b.Writer
	default:
		return 0
	}
}

// PhysicalLane 复刻 db.ts 的 resolvePhysicalLane：预算为 0 的 lane 复用它道，
// 从而让「总预算」等于物理连接数上限，而不是各 lane 上限之和。
func (b PoolBudget) PhysicalLane(lane Lane) Lane {
	if b.Size(lane) > 0 {
		return lane
	}
	if lane == LaneWriter {
		if b.Control > 0 {
			return LaneControl
		}
		return LaneData
	}
	return LaneControl
}

// MaxOutstanding 复刻 admitted-client 的准入上限：max(32, max*8)。
// 该上限计的是瞬时 outstanding 数，与每请求查询数无关。
func (b PoolBudget) MaxOutstanding(lane Lane) int {
	size := b.Size(b.PhysicalLane(lane))
	if size <= 0 {
		return minOutstandingPerPool
	}
	return max(minOutstandingPerPool, size*outstandingPerConnection)
}

// LaneApplicationName 复刻 db.ts 的 APPLICATION_NAMES，写进 PostgreSQL 的
// application_name，便于在生产库里按分道定位连接。
func LaneApplicationName(lane Lane) string {
	return "claude-code-hub:" + string(lane)
}

// DBTimeouts 是连接与语句的超时口径。
type DBTimeouts struct {
	IdleTimeout     time.Duration
	ConnectTimeout  time.Duration
	StatementExpiry time.Duration
	LockExpiry      time.Duration
}

// Config 是本进程的完整配置快照。
//
// 分两层：Env 是与 TS 侧逐字段对齐的 70 项契约（见 env.go），其余字段是 Go 数据面自己的
// 运行视图（一部分由 Env 推导，另一部分为 Go 专有变量）。
type Config struct {
	// Env 是 env.schema.ts 的逐变量复刻，消费者应优先从这里取值。
	Env EnvConfig

	NodeEnv    NodeEnv
	PublicPort int
	// EgressPages 是 CCH_EGRESS_PAGES：非 API 路径（SSR 页面与静态资源）的归属。
	EgressPages EgressPages

	// MaxInflightBytes 是所有在途请求正文的字节上限，超限即拒绝而非排队。
	MaxInflightBytes int64
	// MaxStreams 是同时存在的流式响应数上限。
	MaxStreams int
	// SameProtocolWeightK 是同协议候选的权重倍率（选路偏好）：优先级一致时优先走
	// 不需要协议转换的供应商。1 表示不启用（与未设置同义）。
	//
	// Go 专有旋钮：不属 env.schema.ts 契约，故不进 envSpecs / go/env-parity.txt，
	// 避免被对账脚本判为「Go 多配」（同 CCH_GO_MAX_STREAMS 一类）。
	SameProtocolWeightK int

	// PlaceholderThinkingSignature 是 CCH_THINKING_SIGNATURE_PLACEHOLDER：允许给「思考来自
	// 非 Anthropic 上游、没有 Anthropic 签名」的思考块补一个占位签名，让 Anthropic 客户端
	// 愿意显示这段思考（语义见 convert/thinking_placeholder.go）。默认开。
	//
	// Go 专有旋钮：不属 env.schema.ts 契约，故不进 envSpecs / go/env-parity.txt，
	// 避免被对账脚本判为「Go 多配」。
	//
	// 为何需要开关：占位签名不是上游发的真签名，严格客户端或未来的上游校验都可能不认，
	// 运维要能不重新发版就关掉它（关掉即回退成「无签名思考块丢弃」）。
	PlaceholderThinkingSignature bool

	DSN      string
	RedisURL string

	PoolTotal int
	Pool      PoolBudget
	DB        DBTimeouts

	// Patrol 是未落终态行的自愈巡检（Go 专有）。
	Patrol PatrolConfig

	// Pprof 是性能剖析面（Go 专有，默认关闭）。
	Pprof PprofConfig

	// MemoryLimitSet 表示部署侧是否设了 GOMEMLIMIT。运行时会自行读取该变量，
	// 这里只为启动日志提供事实，空值时告警而不是失败。
	MemoryLimitSet bool
}

// Load 是兼容入口：把「取到空串」当成未设置。
//
// 生产入口应改用 LoadLookup(LookupFromOS())：只有它能区分「未设置」与「设为空串」，
// 而 zod 对这两者处理不同（例如 AUTO_MIGRATE="" 在 Node 侧是 true）。
// 本包不自行改动 cmd/cchd 的调用方式（那属其它 lane 的文件），差异已在 README 记明。
func Load(getenv func(string) string) (Config, error) {
	return LoadLookup(func(name string) (string, bool) {
		value := getenv(name)
		return value, value != ""
	})
}

// LoadLookup 从 lookup 装载配置。lookup 注入以便测试不依赖进程环境。
func LoadLookup(lookup LookupEnvFunc) (Config, error) {
	env, err := LoadEnv(lookup)
	if err != nil {
		return Config{}, err
	}

	// PORT 的边界：契约里 PORT 无边界，但监听端口为 0 会让网关静默绑随机端口，
	// 比启动失败更难排查，故保留既有硬边界（见 README 的偏离清单）。
	publicPort, err := portFromNumber("PORT", env.Port)
	if err != nil {
		return Config{}, err
	}
	egressPages, err := parseEnum(lookupValue(lookup, "CCH_EGRESS_PAGES"), "CCH_EGRESS_PAGES", EgressPagesOff,
		[]EgressPages{EgressPagesOff, EgressPagesEmbed})
	if err != nil {
		return Config{}, err
	}

	maxInflightBytes, err := parseInt64Range(
		lookupValue(lookup, "CCH_GO_MAX_INFLIGHT_BYTES"), "CCH_GO_MAX_INFLIGHT_BYTES", 64*1024*1024,
		1*1024*1024, 4*1024*1024*1024)
	if err != nil {
		return Config{}, err
	}

	maxStreams, err := parseIntRange(lookupValue(lookup, "CCH_GO_MAX_STREAMS"), "CCH_GO_MAX_STREAMS", 64, 1, 4096)
	if err != nil {
		return Config{}, err
	}

	// 同协议偏好的倍率：默认 2（温和偏好），1 即关闭。
	// 上界 100 是防呆：权重是候选之间的相对值，再大的倍率形式上等价于「几乎总选同协议」，
	// 而那本质上是过滤而非偏好，应当先想清楚要求而不是把数调大。
	sameProtocolWeightK, err := parseIntRange(
		lookupValue(lookup, "CCH_SAME_PROTOCOL_WEIGHT_K"), "CCH_SAME_PROTOCOL_WEIGHT_K", 2, 1, 100)
	if err != nil {
		return Config{}, err
	}

	// 占位思考签名：默认开（未设置即开，设 "false"/"0" 关）。
	placeholderThinkingSignature := boolFromNode(
		lookupValue(lookup, "CCH_THINKING_SIGNATURE_PLACEHOLDER"), true)

	// 巡检：Go 专有面，取值与边界都留在本包，装配侧只读结论。
	patrolUnsettledAfter, err := parseIntRange(
		lookupValue(lookup, "CCH_PATROL_UNSETTLED_AFTER_MS"), "CCH_PATROL_UNSETTLED_AFTER_MS",
		30*60*1000, 60*1000, 24*60*60*1000)
	if err != nil {
		return Config{}, err
	}
	patrolInterval, err := parseIntRange(
		lookupValue(lookup, "CCH_PATROL_INTERVAL_MS"), "CCH_PATROL_INTERVAL_MS", 60*1000, 1_000, 60*60*1000)
	if err != nil {
		return Config{}, err
	}
	patrolBatchSize, err := parseIntRange(
		lookupValue(lookup, "CCH_PATROL_BATCH_SIZE"), "CCH_PATROL_BATCH_SIZE", 200, 1, 1000)
	if err != nil {
		return Config{}, err
	}
	patrolMaxRows, err := parseIntRange(
		lookupValue(lookup, "CCH_PATROL_MAX_ROWS_PER_ROUND"), "CCH_PATROL_MAX_ROWS_PER_ROUND", 2000, 1, 10000)
	if err != nil {
		return Config{}, err
	}

	// 性能剖析面：即便关闭也把地址解析出来——启动日志要如实报出「关着，地址是哪个」，
	// 否则排查时无法区分「开关没开」与「地址写错了」。
	pprofAddr, err := parsePprofAddr(lookupValue(lookup, "CCH_PPROF_ADDR"))
	if err != nil {
		return Config{}, err
	}
	pprofBlockRate, err := parseIntRange(
		lookupValue(lookup, "CCH_PPROF_BLOCK_RATE"), "CCH_PPROF_BLOCK_RATE", 1_000_000, 0, 1_000_000_000)
	if err != nil {
		return Config{}, err
	}
	pprofMutexFraction, err := parseIntRange(
		lookupValue(lookup, "CCH_PPROF_MUTEX_FRACTION"), "CCH_PPROF_MUTEX_FRACTION", 0, 0, 1000)
	if err != nil {
		return Config{}, err
	}

	dsn := stringValue(env.DSN)
	if err := validateDSN(dsn); err != nil {
		return Config{}, err
	}
	redisURL := stringValue(env.RedisURL)
	if err := validateRedisURL(redisURL); err != nil {
		return Config{}, err
	}

	defaultPoolTotal := DefaultDevelopmentPoolTotal
	if env.NodeEnv == string(NodeEnvProduction) {
		defaultPoolTotal = DefaultProductionPoolTotal
	}
	poolTotal := defaultPoolTotal
	if env.DBPoolMax != nil {
		poolTotal = *env.DBPoolMax
	}

	// 未设置即取规格表里的默认值：默认值只在 envSpecs 写一遍。
	idleSeconds := defaultOf("DB_POOL_IDLE_TIMEOUT")
	if env.DBPoolIdleTimeout != nil {
		idleSeconds = *env.DBPoolIdleTimeout
	}
	connectSeconds := defaultOf("DB_POOL_CONNECT_TIMEOUT")
	if env.DBPoolConnectTimeout != nil {
		connectSeconds = *env.DBPoolConnectTimeout
	}
	statementMillis := 0
	if env.DBStatementTimeoutMS != nil {
		statementMillis = *env.DBStatementTimeoutMS
	}
	lockMillis := 0
	if env.DBLockTimeoutMS != nil {
		lockMillis = *env.DBLockTimeoutMS
	}

	return Config{
		Env:              env,
		NodeEnv:          NodeEnv(env.NodeEnv),
		PublicPort:       publicPort,
		EgressPages:      egressPages,
		MaxInflightBytes: maxInflightBytes,
		MaxStreams:       maxStreams,

		SameProtocolWeightK: sameProtocolWeightK,

		PlaceholderThinkingSignature: placeholderThinkingSignature,
		Patrol: PatrolConfig{
			// 默认开启：缺口的成因（滚动重启撞上断线）发生在每次部署，而巡检是幂等的。
			Enabled:         boolFromNode(lookupValue(lookup, "CCH_PATROL_ENABLED"), true),
			UnsettledAfter:  time.Duration(patrolUnsettledAfter) * time.Millisecond,
			Interval:        time.Duration(patrolInterval) * time.Millisecond,
			BatchSize:       patrolBatchSize,
			MaxRowsPerRound: patrolMaxRows,
		},
		Pprof: PprofConfig{
			// 默认关闭：剖析面会暴露调用栈与堆内容，不该在没人看着时开着。
			Enabled:              boolFromNode(lookupValue(lookup, "CCH_PPROF_ENABLED"), false),
			Addr:                 pprofAddr,
			BlockProfileRate:     pprofBlockRate,
			MutexProfileFraction: pprofMutexFraction,
		},
		DSN:       dsn,
		RedisURL:  redisURL,
		PoolTotal: poolTotal,
		Pool:      SplitPoolBudget(poolTotal),
		DB: DBTimeouts{
			IdleTimeout:     time.Duration(idleSeconds * float64(time.Second)),
			ConnectTimeout:  time.Duration(connectSeconds * float64(time.Second)),
			StatementExpiry: time.Duration(statementMillis) * time.Millisecond,
			LockExpiry:      time.Duration(lockMillis) * time.Millisecond,
		},
		MemoryLimitSet: lookupPresent(lookup, "GOMEMLIMIT"),
	}, nil
}

// boolFromNode 复刻 env.schema.ts 的 booleanTransform：s != "false" && s != "0"，
// 大小写敏感，空串是「已设置」故取 true。仅用于 Go 专有变量：契约变量走 envSpecs。
func boolFromNode(raw string, fallback bool) bool {
	if raw == "" {
		return fallback
	}
	return raw != "false" && raw != "0"
}

// lookupValue 只取「已设置且非空」的值，供 Go 专有变量沿用既有语义。
func lookupValue(lookup LookupEnvFunc, name string) string {
	value, present := lookup(name)
	if !present {
		return ""
	}
	return value
}

func lookupPresent(lookup LookupEnvFunc, name string) bool {
	value, present := lookup(name)
	return present && strings.TrimSpace(value) != ""
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

// portFromNumber 把契约里的 PORT（无边界）落到监听的硬边界上。
func portFromNumber(name string, value float64) (int, error) {
	if value != math.Trunc(value) {
		return 0, fmt.Errorf("%s 必须是整数，收到 %s", name, formatNumber(value))
	}
	port := int(value)
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("%s 必须在 1 到 65535 之间，收到 %d", name, port)
	}
	return port, nil
}

// defaultOf 取规格表里的默认值，避免默认值在两处各写一遍。
func defaultOf(name string) float64 {
	for _, spec := range envSpecs {
		if spec.name != name {
			continue
		}
		value, err := strconv.ParseFloat(spec.def, 64)
		if err != nil {
			return 0
		}
		return value
	}
	return 0
}

// Redacted 返回可安全写进日志的配置摘要，不含任何凭据原文。
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"nodeEnv":             string(c.NodeEnv),
		"publicPort":          c.PublicPort,
		"egressPages":         string(c.EgressPages),
		"maxInflightBytes":    c.MaxInflightBytes,
		"maxStreams":          c.MaxStreams,
		"sameProtocolWeightK": c.SameProtocolWeightK,
		"dsnConfigured":       c.DSN != "",
		"redisConfigured":     c.RedisURL != "",
		"poolTotal":           c.PoolTotal,
		"poolData":            c.Pool.Data,
		"poolControl":         c.Pool.Control,
		"poolWriter":          c.Pool.Writer,
		"patrolEnabled":       c.Patrol.Enabled,
		"patrolAfterMs":       c.Patrol.UnsettledAfter.Milliseconds(),
		"patrolIntervalMs":    c.Patrol.Interval.Milliseconds(),
		"pprofEnabled":        c.Pprof.Enabled,
		"pprofAddr":           c.Pprof.Addr,
		"memoryLimitSet":      c.MemoryLimitSet,
	}
}

// RedactedEnv 返回 70 项契约的脱敏摘要（计数、档位与凭据是否已配置）。
func (c Config) RedactedEnv() map[string]any {
	return c.Env.EnvSummary()
}

// parsePprofAddr 解析并校验剖析面监听地址。
//
// 与其它 Go 专有变量一样，空串取默认值；非法则**启动失败**而不是静默退回默认地址：
// 地址写错却安静地绑到别处，是排查时最费时的一类事。
//
// internalPort 已随 Node 回退端口一并删除，故默认地址取常数 DefaultPprofAddr。
func parsePprofAddr(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return DefaultPprofAddr, nil
	}
	host, portText, err := net.SplitHostPort(text)
	if err != nil {
		return "", fmt.Errorf("CCH_PPROF_ADDR 需为 host:port，收到 %q", text)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return "", fmt.Errorf("CCH_PPROF_ADDR 的端口必须是整数，收到 %q", text)
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("CCH_PPROF_ADDR 的端口必须在 1 到 65535 之间，收到 %d", port)
	}
	// 空主机（":3100"）会绑全网卡：允许，但必须点明语义，免得当成回环写。
	if strings.TrimSpace(host) == "" {
		return net.JoinHostPort("0.0.0.0", portText), nil
	}
	return net.JoinHostPort(host, portText), nil
}

func parseIntRange(raw, name string, fallback, minimum, maximum int) (int, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数，收到 %q", name, text)
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s 必须在 %d 到 %d 之间，收到 %d", name, minimum, maximum, value)
	}
	return value, nil
}

func parseInt64Range(raw, name string, fallback, minimum, maximum int64) (int64, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s 必须是整数，收到 %q", name, text)
	}
	if value < minimum || value > maximum {
		return 0, fmt.Errorf("%s 必须在 %d 到 %d 之间，收到 %d", name, minimum, maximum, value)
	}
	return value, nil
}

func parseEnum[T ~string](raw, name string, fallback T, allowed []T) (T, error) {
	text := strings.TrimSpace(raw)
	if text == "" {
		return fallback, nil
	}
	for _, candidate := range allowed {
		if text == string(candidate) {
			return candidate, nil
		}
	}
	names := make([]string, 0, len(allowed))
	for _, candidate := range allowed {
		names = append(names, string(candidate))
	}
	return fallback, fmt.Errorf("%s 必须是 %s 之一，收到 %q", name, strings.Join(names, "|"), text)
}

// validateDSN 只做结构校验；错误信息绝不回显原值。
func validateDSN(dsn string) error {
	if dsn == "" {
		return nil
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("DSN 不是合法 URL（值已隐去）: %v", redactURLError(err))
	}
	switch parsed.Scheme {
	case "postgres", "postgresql":
		return nil
	default:
		return fmt.Errorf("DSN 的 scheme 必须是 postgres 或 postgresql，收到 %q（值已隐去）", parsed.Scheme)
	}
}

// validateRedisURL 只做结构校验；错误信息绝不回显原值。
func validateRedisURL(raw string) error {
	if raw == "" {
		return nil
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("REDIS_URL 不是合法 URL（值已隐去）: %v", redactURLError(err))
	}
	switch parsed.Scheme {
	case "redis", "rediss":
		return nil
	default:
		return fmt.Errorf("REDIS_URL 的 scheme 必须是 redis 或 rediss，收到 %q（值已隐去）", parsed.Scheme)
	}
}

// redactURLError 去掉 url 解析错误里可能回显的原始串。
func redactURLError(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}
