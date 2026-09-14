package config

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// envMap 把 map 适配成 Load 需要的 getenv。
func envMap(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("默认值必须可装载，收到错误: %v", err)
	}
	if cfg.NodeEnv != NodeEnvDevelopment {
		t.Errorf("NODE_ENV 默认应为 development，收到 %q", cfg.NodeEnv)
	}
	if cfg.PublicPort != 23000 {
		t.Errorf("PORT 默认应为 23000（与 env.schema.ts 一致），收到 %d", cfg.PublicPort)
	}
	if cfg.EgressPages != EgressPagesOff {
		t.Errorf("CCH_EGRESS_PAGES 默认应为 off（Node 退役后不再有页面反代档），收到 %q", cfg.EgressPages)
	}
	if cfg.MaxStreams != 64 {
		t.Errorf("CCH_GO_MAX_STREAMS 默认应为 64，收到 %d", cfg.MaxStreams)
	}
	if cfg.SameProtocolWeightK != 2 {
		t.Errorf("CCH_SAME_PROTOCOL_WEIGHT_K 默认应为 2，收到 %d", cfg.SameProtocolWeightK)
	}
	// 剖析面默认关闭（安全默认），但地址仍要解析出来：启动日志得能区分
	// 「开关没开」与「地址写错了」。
	if cfg.Pprof.Enabled {
		t.Error("CCH_PPROF_ENABLED 默认应为 false（剖析面暴露堆内容，不该默认开）")
	}
	if cfg.Pprof.Addr != DefaultPprofAddr {
		t.Errorf("CCH_PPROF_ADDR 默认应为 %s，收到 %q", DefaultPprofAddr, cfg.Pprof.Addr)
	}
	if cfg.Pprof.BlockProfileRate != 1_000_000 {
		t.Errorf("CCH_PPROF_BLOCK_RATE 默认应为 1ms（只采长阻塞），收到 %d", cfg.Pprof.BlockProfileRate)
	}
	if cfg.Pprof.MutexProfileFraction != 0 {
		t.Errorf("CCH_PPROF_MUTEX_FRACTION 默认应为 0（锁剖析随争用变贵），收到 %d", cfg.Pprof.MutexProfileFraction)
	}
	if cfg.PoolTotal != DefaultDevelopmentPoolTotal {
		t.Errorf("开发档 DB_POOL_MAX 默认应为 %d，收到 %d", DefaultDevelopmentPoolTotal, cfg.PoolTotal)
	}
	if cfg.DB.StatementExpiry != 90*time.Second {
		t.Errorf("DB_STATEMENT_TIMEOUT_MS 默认应为 90s，收到 %s", cfg.DB.StatementExpiry)
	}
	if cfg.DB.LockExpiry != 5*time.Second {
		t.Errorf("DB_LOCK_TIMEOUT_MS 默认应为 5s，收到 %s", cfg.DB.LockExpiry)
	}
}

func TestLoadProductionPoolDefault(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{"NODE_ENV": "production"}))
	if err != nil {
		t.Fatalf("生产档默认值必须可装载: %v", err)
	}
	if cfg.PoolTotal != DefaultProductionPoolTotal {
		t.Fatalf("生产档 DB_POOL_MAX 默认应为 %d，收到 %d", DefaultProductionPoolTotal, cfg.PoolTotal)
	}
	// 计划 §8：40 -> data=31 -> maxOutstanding=248。
	if cfg.Pool.Data != 31 {
		t.Errorf("DB_POOL_MAX=40 时 data 应为 31，收到 %d", cfg.Pool.Data)
	}
	if got := cfg.Pool.MaxOutstanding(LaneData); got != 248 {
		t.Errorf("DB_POOL_MAX=40 时 maxOutstanding 应为 248，收到 %d", got)
	}
}

// 契约默认值与矩阵一致：PORT 默认 23000（不是历史实现的 3000）。
func TestLoadPortDefaultMatchesSchema(t *testing.T) {
	cfg, err := Load(envMap(nil))
	if err != nil {
		t.Fatalf("默认值必须可装载: %v", err)
	}
	if cfg.Env.Port != 23000 {
		t.Errorf("Env.Port 默认应为 23000，收到 %v", cfg.Env.Port)
	}
	if cfg.PublicPort != 23000 {
		t.Errorf("PublicPort 默认应为 23000，收到 %d", cfg.PublicPort)
	}
}

// 区分「未设置」与「设为空串」：布尔变量在 Node 侧对空串得到 true。
func TestLoadLookupDistinguishesUnsetFromEmpty(t *testing.T) {
	cfg, err := LoadLookup(lookupMap(map[string]string{"AUTO_MIGRATE": ""}))
	if err != nil {
		t.Fatalf("空串应可装载: %v", err)
	}
	if !cfg.Env.AutoMigrate {
		t.Fatal("AUTO_MIGRATE 设为空串时应为 true（booleanTransform 语义）")
	}

	// 兼容入口 Load 把空串当未设置，因此走默认值 true；两者的差异在「默认值为 false」的变量上才显现。
	cfg, err = Load(envMap(map[string]string{"DEBUG_MODE": ""}))
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if cfg.Env.DebugMode {
		t.Fatal("兼容入口 Load 把空串视为未设置，DEBUG_MODE 应回落到默认 false")
	}

	cfg, err = LoadLookup(lookupMap(map[string]string{"DEBUG_MODE": ""}))
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if !cfg.Env.DebugMode {
		t.Fatal("LoadLookup 下 DEBUG_MODE 设为空串应为 true")
	}
}

// PORT 在契约里无边界，但监听端口必须落在 1..65535，否则静默绑随机端口。
func TestLoadRejectsUnlistenablePort(t *testing.T) {
	for _, value := range []string{"0", "65536", "1.5"} {
		_, err := LoadLookup(lookupMap(map[string]string{"PORT": value}))
		if err == nil {
			t.Fatalf("PORT=%s 应被拒绝", value)
		}
		if !strings.Contains(err.Error(), "PORT") {
			t.Fatalf("错误信息应含变量名，收到 %v", err)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"PORT 非整数", map[string]string{"PORT": "abc"}, "PORT"},
		{"PORT 越界", map[string]string{"PORT": "70000"}, "PORT"},
		{"页面归属非法", map[string]string{"CCH_EGRESS_PAGES": "self"}, "CCH_EGRESS_PAGES"},
		// 已退役的 node 档必须**响亮地拒绝**，而不是静默当成 off：旧双跑 compose 写的就是它。
		{"页面归属为已退役的 node 档", map[string]string{"CCH_EGRESS_PAGES": "node"}, "CCH_EGRESS_PAGES"},
		{"NODE_ENV 非法", map[string]string{"NODE_ENV": "staging"}, "NODE_ENV"},
		{"在途字节上限过小", map[string]string{"CCH_GO_MAX_INFLIGHT_BYTES": "1024"}, "CCH_GO_MAX_INFLIGHT_BYTES"},
		{"在途字节上限非整数", map[string]string{"CCH_GO_MAX_INFLIGHT_BYTES": "1.5MiB"}, "CCH_GO_MAX_INFLIGHT_BYTES"},
		{"流数上限越界", map[string]string{"CCH_GO_MAX_STREAMS": "9000"}, "CCH_GO_MAX_STREAMS"},
		{"同协议倍率为 0", map[string]string{"CCH_SAME_PROTOCOL_WEIGHT_K": "0"}, "CCH_SAME_PROTOCOL_WEIGHT_K"},
		{"同协议倍率过大", map[string]string{"CCH_SAME_PROTOCOL_WEIGHT_K": "101"}, "CCH_SAME_PROTOCOL_WEIGHT_K"},
		{"同协议倍率非整数", map[string]string{"CCH_SAME_PROTOCOL_WEIGHT_K": "2.5"}, "CCH_SAME_PROTOCOL_WEIGHT_K"},
		{"DB_POOL_MAX 过小", map[string]string{"DB_POOL_MAX": "0"}, "DB_POOL_MAX"},
		{"DB_POOL_MAX 过大", map[string]string{"DB_POOL_MAX": "201"}, "DB_POOL_MAX"},
		{"DSN scheme 非法", map[string]string{"DSN": "mysql://user:pw@host:3306/db"}, "DSN"},
		{"DSN 非法 URL", map[string]string{"DSN": "postgres://bad host/db"}, "DSN"},
		{"REDIS_URL scheme 非法", map[string]string{"REDIS_URL": "http://127.0.0.1:6379"}, "REDIS_URL"},
		{"空闲超时越界", map[string]string{"DB_POOL_IDLE_TIMEOUT": "3601"}, "DB_POOL_IDLE_TIMEOUT"},
		{"建连超时越界", map[string]string{"DB_POOL_CONNECT_TIMEOUT": "0"}, "DB_POOL_CONNECT_TIMEOUT"},
		{"语句超时越界", map[string]string{"DB_STATEMENT_TIMEOUT_MS": "999"}, "DB_STATEMENT_TIMEOUT_MS"},
		{"锁超时越界", map[string]string{"DB_LOCK_TIMEOUT_MS": "60001"}, "DB_LOCK_TIMEOUT_MS"},
		{"剖析面地址缺端口", map[string]string{"CCH_PPROF_ADDR": "127.0.0.1"}, "CCH_PPROF_ADDR"},
		{"剖析面地址端口非数", map[string]string{"CCH_PPROF_ADDR": "127.0.0.1:http"}, "CCH_PPROF_ADDR"},
		{"剖析面地址端口越界", map[string]string{"CCH_PPROF_ADDR": "127.0.0.1:70000"}, "CCH_PPROF_ADDR"},
		{"剖析面阻塞采样率为负", map[string]string{"CCH_PPROF_BLOCK_RATE": "-1"}, "CCH_PPROF_BLOCK_RATE"},
		{"剖析面互斥采样率越界", map[string]string{"CCH_PPROF_MUTEX_FRACTION": "1001"}, "CCH_PPROF_MUTEX_FRACTION"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := Load(envMap(testCase.env))
			if err == nil {
				t.Fatalf("期望校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), testCase.wantVar) {
				t.Fatalf("错误信息必须包含变量名 %s，收到: %v", testCase.wantVar, err)
			}
		})
	}
}

func TestLoadAcceptsValidValues(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{
		"NODE_ENV":                   "production",
		"PORT":                       "3000",
		"CCH_EGRESS_PAGES":           "off",
		"CCH_GO_MAX_INFLIGHT_BYTES":  "268435456",
		"CCH_GO_MAX_STREAMS":         "128",
		"CCH_SAME_PROTOCOL_WEIGHT_K": "3",
		"DSN":                        "postgresql://user:pw@host:5432/db",
		"REDIS_URL":                  "redis://:pw@host:6379/0",
		"DB_POOL_MAX":                "40",
		"GOMEMLIMIT":                 "512MiB",
		"CCH_PPROF_ENABLED":          "true",
		"CCH_PPROF_ADDR":             "127.0.0.1:13337",
		"CCH_PPROF_BLOCK_RATE":       "0",
		"CCH_PPROF_MUTEX_FRACTION":   "5",
	}))
	if err != nil {
		t.Fatalf("合法配置必须通过: %v", err)
	}
	if cfg.EgressPages != EgressPagesOff {
		t.Errorf("CCH_EGRESS_PAGES 应为 off，收到 %q", cfg.EgressPages)
	}
	if cfg.SameProtocolWeightK != 3 {
		t.Errorf("CCH_SAME_PROTOCOL_WEIGHT_K 应取显式值 3，收到 %d", cfg.SameProtocolWeightK)
	}
	if cfg.MaxInflightBytes != 268435456 {
		t.Errorf("CCH_GO_MAX_INFLIGHT_BYTES 应为 268435456，收到 %d", cfg.MaxInflightBytes)
	}
	if !cfg.MemoryLimitSet {
		t.Errorf("GOMEMLIMIT 已设置时 MemoryLimitSet 应为 true")
	}
	if !cfg.Pprof.Enabled {
		t.Error("CCH_PPROF_ENABLED=true 应解析为已启用")
	}
	if cfg.Pprof.Addr != "127.0.0.1:13337" {
		t.Errorf("CCH_PPROF_ADDR 应取显式值，收到 %q", cfg.Pprof.Addr)
	}
	if cfg.Pprof.BlockProfileRate != 0 {
		t.Errorf("CCH_PPROF_BLOCK_RATE=0 应解析为关闭，收到 %d", cfg.Pprof.BlockProfileRate)
	}
	if cfg.Pprof.MutexProfileFraction != 5 {
		t.Errorf("CCH_PPROF_MUTEX_FRACTION 应取显式值 5，收到 %d", cfg.Pprof.MutexProfileFraction)
	}
}

// TestPprofConfigSurfacesInRedactedSummary 钉住启动日志要能看到剖析面的开关与地址：
// 否则一个“起没起、绑在哪”的问题得进容器里猜。
func TestPprofConfigSurfacesInRedactedSummary(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{
		"CCH_PPROF_ENABLED": "true",
		"CCH_PPROF_ADDR":    "127.0.0.1:13338",
	}))
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	summary := cfg.Redacted()
	if summary["pprofEnabled"] != true {
		t.Errorf("摘要应含 pprofEnabled=true，收到 %v", summary["pprofEnabled"])
	}
	if summary["pprofAddr"] != "127.0.0.1:13338" {
		t.Errorf("摘要应含 pprofAddr，收到 %v", summary["pprofAddr"])
	}
}

// TestPprofAddrWithoutHostBindsAllInterfaces 钉住空主机的语义：
// “:3100” 会被规整成 0.0.0.0:3100（绑全网卡），由启动日志报警——不能当成回环静默处理。
func TestPprofAddrWithoutHostBindsAllInterfaces(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{"CCH_PPROF_ADDR": ":13339"}))
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if cfg.Pprof.Addr != "0.0.0.0:13339" {
		t.Errorf("空主机应规整为 0.0.0.0:13339，收到 %q", cfg.Pprof.Addr)
	}
}

// TestPprofEnabledAloneYieldsUsableAddr 是**回归钉子**（真实踩到过）：
//
// 只设 `CCH_PPROF_ENABLED=true`、不设地址时，派生出的默认地址不得与 Node 回退端口相同。
// 旧默认值写死 127.0.0.1:3100，而 CCH_INTERNAL_PORT 的默认值就是 3100 ⇒ debugapi.Open
// fail-closed 拒绝（“剖析面地址与 Node 回退端口相同”），于是**开了开关却永远没有剖析面**。
func TestPprofEnabledAloneYieldsUsableAddr(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{"CCH_PPROF_ENABLED": "true"}))
	if err != nil {
		t.Fatalf("装载失败: %v", err)
	}
	if !cfg.Pprof.Enabled {
		t.Fatal("CCH_PPROF_ENABLED=true 应生效")
	}
	if cfg.Pprof.Addr == "" {
		t.Fatal("只开开关时地址不得为空：debugapi.Open 对空地址直接报错")
	}
	if cfg.Pprof.Addr != DefaultPprofAddr {
		t.Fatalf("未显式配置地址时应取默认 %s，收到 %q", DefaultPprofAddr, cfg.Pprof.Addr)
	}
	_, portText, err := net.SplitHostPort(cfg.Pprof.Addr)
	if err != nil {
		t.Fatalf("默认地址应为 host:port，收到 %q: %v", cfg.Pprof.Addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("默认端口应为整数，收到 %q", portText)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("默认端口越界: %d", port)
	}
}

// TestRetiredNodeVarsAreIgnored 钉住旧部署文件的兼容边界：
//
// 已删除的 `CCH_EGRESS_MODE` / `CCH_INTERNAL_PORT` / `CCH_EGRESS_ROUTES` 不再解析；而「配置里
// 多出未知变量」本就不报错（与 70 项契约表之外的变量同一处置），因此线上残留这三个变量不会
// 导致启动失败——它们只是不再有任何作用，须由运维自行清理（本仓的双跑模板已删）。
//
// 与 `CCH_EGRESS_PAGES=node` 的区别：那个是**已知变量的已退役取值**，会响亮报错
// TestLoadRejectsInvalidValues）。
func TestRetiredNodeVarsAreIgnored(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{
		"CCH_EGRESS_MODE":   "node",
		"CCH_INTERNAL_PORT": "3100",
		"CCH_EGRESS_ROUTES": "* /api/*,* /v1/*",
		"CCH_EGRESS_PAGES":  "embed",
	}))
	if err != nil {
		t.Fatalf("残留的已退役变量不应导致装载失败: %v", err)
	}
	if cfg.EgressPages != EgressPagesEmbed {
		t.Fatalf("CCH_EGRESS_PAGES 应为 embed，收到 %q", cfg.EgressPages)
	}
}

func TestCredentialsNeverAppearInErrorsOrSummary(t *testing.T) {
	const dsnSecret = "sup3r-s3cret-db"
	const redisSecret = "sup3r-s3cret-redis"

	_, err := Load(envMap(map[string]string{
		"DSN":       "mysql://user:" + dsnSecret + "@host:3306/db",
		"REDIS_URL": "redis://:" + redisSecret + "@host:6379",
	}))
	if err == nil {
		t.Fatal("期望 DSN scheme 校验失败")
	}
	if strings.Contains(err.Error(), dsnSecret) {
		t.Fatalf("错误信息泄漏了 DSN 凭据: %v", err)
	}
	if strings.Contains(err.Error(), "user:") {
		t.Fatalf("错误信息泄漏了 DSN 用户信息: %v", err)
	}

	cfg, loadErr := Load(envMap(map[string]string{
		"DSN":       "postgresql://user:" + dsnSecret + "@host:5432/db",
		"REDIS_URL": "redis://:" + redisSecret + "@host:6379",
	}))
	if loadErr != nil {
		t.Fatalf("合法凭据应通过校验: %v", loadErr)
	}
	summary := cfg.Redacted()
	if summary["dsnConfigured"] != true || summary["redisConfigured"] != true {
		t.Fatalf("脱敏摘要应只报「已配置」，收到 %v", summary)
	}
	for key, value := range summary {
		if text, ok := value.(string); ok && (strings.Contains(text, dsnSecret) || strings.Contains(text, redisSecret)) {
			t.Fatalf("脱敏摘要的 %s 泄漏了凭据: %q", key, text)
		}
	}
}

// 计划 §8 的连接预算公式，含 total 为 1 / 2 的源码特例。
func TestSplitPoolBudget(t *testing.T) {
	cases := []struct {
		total   int
		data    int
		control int
		writer  int
	}{
		{1, 0, 1, 0},
		{2, 1, 1, 0},
		{3, 1, 1, 1},
		{10, 7, 2, 1},
		{20, 15, 4, 1},
		{40, 31, 8, 1},
		{200, 159, 40, 1},
	}
	for _, testCase := range cases {
		got := SplitPoolBudget(testCase.total)
		if got.Data != testCase.data || got.Control != testCase.control || got.Writer != testCase.writer {
			t.Errorf("SplitPoolBudget(%d) = %+v，期望 data=%d control=%d writer=%d",
				testCase.total, got, testCase.data, testCase.control, testCase.writer)
		}
		if sum := got.Data + got.Control + got.Writer; sum != testCase.total {
			t.Errorf("SplitPoolBudget(%d) 各道之和应为 %d，收到 %d", testCase.total, testCase.total, sum)
		}
	}
}

func TestPoolBudgetPhysicalLaneAndOutstanding(t *testing.T) {
	// total=2 时 writer 无独立连接，须复用到 control 道，而不是再开一条。
	budget := SplitPoolBudget(2)
	if got := budget.PhysicalLane(LaneWriter); got != LaneControl {
		t.Errorf("total=2 时 writer 应复用 control 道，收到 %q", got)
	}
	if got := budget.MaxOutstanding(LaneWriter); got != 32 {
		t.Errorf("total=2 时 writer 的准入上限应回落到下限 32，收到 %d", got)
	}
	if got := budget.PhysicalLane(LaneData); got != LaneData {
		t.Errorf("total=2 时 data 应保留独立道，收到 %q", got)
	}
	// 计划 §8 的 maxOutstanding 公式。
	large := SplitPoolBudget(40)
	if got := large.MaxOutstanding(LaneControl); got != 64 {
		t.Errorf("total=40 时 control 的准入上限应为 8*8=64，收到 %d", got)
	}
	if got := large.MaxOutstanding(LaneData); got != 248 {
		t.Errorf("total=40 时 data 的准入上限应为 31*8=248，收到 %d", got)
	}
	if got := LaneApplicationName(LaneWriter); got != "claude-code-hub:writer" {
		t.Errorf("application_name 应与 TS 侧一致，收到 %q", got)
	}
}

// 预算退化与非法 lane 不得 panic，也不得算出非法的连接数。
func TestPoolBudgetDegenerateCases(t *testing.T) {
	empty := PoolBudget{}
	if got := empty.Size(Lane("bogus")); got != 0 {
		t.Errorf("未知 lane 的大小应为 0，收到 %d", got)
	}
	if got := empty.PhysicalLane(Lane("bogus")); got != LaneControl {
		t.Errorf("未知 lane 应回落到 control 道，收到 %q", got)
	}
	// control 也为 0 时，writer 只能退到 data 道。
	if got := empty.PhysicalLane(LaneWriter); got != LaneData {
		t.Errorf("control 为 0 时 writer 应退到 data 道，收到 %q", got)
	}
	if got := empty.MaxOutstanding(LaneData); got != 32 {
		t.Errorf("物理道为 0 时准入上限应回落到下限 32，收到 %d", got)
	}
}

func TestValidateRedisURLRejectsMalformed(t *testing.T) {
	if err := validateRedisURL("redis://%zz@host:6379"); err == nil {
		t.Fatal("非法 REDIS_URL 必须被拒绝")
	}
	if err := validateRedisURL(""); err != nil {
		t.Errorf("空 REDIS_URL 表示未配置，不应报错: %v", err)
	}
	if err := validateRedisURL("rediss://host:6379"); err != nil {
		t.Errorf("rediss scheme 应被接受: %v", err)
	}
}

// redactURLError 必须剥掉 url.Error 里的原始串，且对 nil 保持 nil。
func TestRedactURLErrorStripsRawValue(t *testing.T) {
	if redactURLError(nil) != nil {
		t.Fatal("nil 错误应保持 nil")
	}
	const secret = "postgres://user:top-secret@host:5432/db"
	inner := errors.New("invalid port")
	wrapped := &url.Error{Op: "parse", URL: secret, Err: inner}
	redacted := redactURLError(wrapped)
	if strings.Contains(redacted.Error(), "top-secret") {
		t.Fatalf("redactURLError 未抹掉原文: %v", redacted)
	}
	if redacted.Error() != "invalid port" {
		t.Fatalf("redactURLError 应只保留内层原因，收到 %v", redacted)
	}
	plain := errors.New("plain failure")
	if redactURLError(plain).Error() != "plain failure" {
		t.Fatalf("非 url.Error 应原样返回: %v", redactURLError(plain))
	}
}

// embed 是 CCH_EGRESS_PAGES 的第三态（Node 下线后由本进程服务 embed 产物），必须被接受。
func TestLoadAcceptsEmbedPages(t *testing.T) {
	cfg, err := Load(envMap(map[string]string{"CCH_EGRESS_PAGES": "embed"}))
	if err != nil {
		t.Fatalf("embed 必须合法: %v", err)
	}
	if cfg.EgressPages != EgressPagesEmbed {
		t.Fatalf("CCH_EGRESS_PAGES 应为 embed，收到 %q", cfg.EgressPages)
	}
}
