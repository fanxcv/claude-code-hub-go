package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConnectionRuntimeParamsDisableJIT 钉住「连接一律关 JIT」。
//
// 为什么值得一条钉子：这条设置是**纯性能取向**的，删掉它不会有任何测试变红、
// 也不改变任何返回值，但它对 `/dashboard/overview` 是约 12 倍的差距
// （实测 97~119 ms → 7.6~9.7 ms，其中 JIT 编译自身占 78 ms）。
// 依据与「何时该重新评估」写在 pool.go 的 jitOff 注释里。
func TestConnectionRuntimeParamsDisableJIT(t *testing.T) {
	// 不连库：只需一个能解析的 DSN 来拿到 ConnConfig。
	cfg, err := pgxpool.ParseConfig("postgres://nail@127.0.0.1:1/nail")
	if err != nil {
		t.Fatalf("解析测试 DSN 失败: %v", err)
	}

	applyConnectionRuntimeParams(cfg, "cchd-test")

	params := cfg.ConnConfig.RuntimeParams
	if got := params["jit"]; got != "off" {
		t.Fatalf("连接运行时参数 jit 应为 off（关 JIT 是实测约 12 倍的收益），实际为 %q", got)
	}
	if got := params["application_name"]; got != "cchd-test" {
		t.Fatalf("application_name 应透传，实际为 %q", got)
	}
}

// TestApplyConnectionRuntimeParamsKeepsExisting 钉住「不覆盖 DSN 里已有的其它运行时参数」。
//
// DSN 可以带 options 或其它 GUC；关 JIT 不能以清空整张表为代价。
func TestApplyConnectionRuntimeParamsKeepsExisting(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://nail@127.0.0.1:1/nail?application_name=from-dsn&search_path=public")
	if err != nil {
		t.Fatalf("解析测试 DSN 失败: %v", err)
	}

	applyConnectionRuntimeParams(cfg, "cchd-test")

	params := cfg.ConnConfig.RuntimeParams
	if got := params["search_path"]; got != "public" {
		t.Fatalf("DSN 里已有的 search_path 不应被清掉，实际为 %q（键：%v）", got, params)
	}
	if got := params["application_name"]; got != "cchd-test" {
		t.Fatalf("application_name 应被显式值覆盖，实际为 %q", got)
	}
	if got := params["jit"]; got != "off" {
		t.Fatalf("连接运行时参数 jit 应为 off，实际为 %q", got)
	}
}

// TestIntegrationConnectionDisablesJIT 在**真库**上证明关 JIT 确实生效。
//
// 单测只能证明「配置里写了 jit=off」；它证明不了 PG 真的接受了这个启动参数。
// 若服务端因任何原因忽略它，页面上那 12 倍就白拿了——而那是只有连上去才能发现的差距。
func TestIntegrationConnectionDisablesJIT(t *testing.T) {
	ctx := context.Background()
	pools := openTestPools(t)

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制道连接池失败: %v", err)
	}

	var jit string
	if err := pool.Raw().QueryRow(ctx, "SHOW jit").Scan(&jit); err != nil {
		t.Fatalf("读取 jit 设置失败: %v", err)
	}
	if jit != "off" {
		t.Fatalf("分道池连接上的 jit 应为 off（实测 /dashboard/overview 因此从 97~119 ms 降到 7.6~9.7 ms），实际为 %q", jit)
	}

	// 专用连接走的是另一条构造路径（openDedicatedConn），同样要关 JIT——
	// 它是「占着连接等锁」的会话，跑聚合的机会不多，但漏一条路就是一处漂移。
	conn, dedicatedPool, err := pools.OpenDedicatedConn(ctx)
	if err != nil {
		t.Fatalf("开专用连接失败: %v", err)
	}
	defer func() {
		conn.Release()
		dedicatedPool.Close()
	}()

	var dedicatedJIT string
	if err := conn.QueryRow(ctx, "SHOW jit").Scan(&dedicatedJIT); err != nil {
		t.Fatalf("读取专用连接 jit 设置失败: %v", err)
	}
	if dedicatedJIT != "off" {
		t.Fatalf("专用连接上的 jit 应为 off，实际为 %q", dedicatedJIT)
	}
}
