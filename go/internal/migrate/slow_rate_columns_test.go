package migrate

import (
	"strings"
	"testing"
)

// 本文件钉住 0127_slow_rate_provider_columns 的三条契约。它们是「默认关闭」这个
// 产品承诺在迁移层的全部体现——写错任一条，未开启低速降级的渠道行为就会与现状不同：
//
//  1. 开关列必须 NOT NULL DEFAULT false（漏 DEFAULT 会让既有行落 NULL，Go 侧 bool 读到 false
//     看似无碍，但列语义从「明确关闭」退化成「未设置」，后续无法区分）；
//  2. 五个参数列必须可空且**无默认值**（NULL = 取代码默认值；若给 DEFAULT 0，
//     会把「未配置」静默变成 0 系数、0 窗口这种非法值）；
//  3. 语句块数必须是 6（一列一句）——多一句或少一句都说明列数与设计不符。

const slowRateMigrationTag = "0127_slow_rate_provider_columns"

// slowRateSwitchColumn 是逐渠道开关列：NOT NULL DEFAULT false。
const slowRateSwitchColumn = `ALTER TABLE "providers" ADD COLUMN "slow_rate_monitor_enabled" boolean DEFAULT false NOT NULL;`

// slowRateParamColumns 是五个可空参数列，顺序即迁移内的书写顺序。
var slowRateParamColumns = []string{
	`ALTER TABLE "providers" ADD COLUMN "slow_rate_window_seconds" integer;`,
	`ALTER TABLE "providers" ADD COLUMN "slow_rate_min_samples" integer;`,
	`ALTER TABLE "providers" ADD COLUMN "slow_rate_ratio_per_mille" integer;`,
	`ALTER TABLE "providers" ADD COLUMN "slow_rate_penalty_step" integer;`,
	`ALTER TABLE "providers" ADD COLUMN "slow_rate_penalty_max" integer;`,
}

func findSlowRateMigration(t *testing.T) Migration {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for _, m := range migrations {
		if m.Tag == slowRateMigrationTag {
			return m
		}
	}
	t.Fatalf("迁移 %s 不在 journal 里（列数对不上时先看 journal 与 sql 副本是否同步）", slowRateMigrationTag)
	return Migration{}
}

func TestSlowRateMigrationAddsSwitchAndFiveNullableParams(t *testing.T) {
	m := findSlowRateMigration(t)

	if got := len(m.Statements); got != 6 {
		t.Fatalf("语句块数: 得到 %d，期望 6（1 开关 + 5 参数）", got)
	}
	// 切分按 literal breakpoint 做，故第二句起带前导换行（仓内既有迁移同形）；
	// 断言前 TrimSpace，既钉住列定义又不把换行当契约。
	if got := strings.TrimSpace(m.Statements[0]); got != slowRateSwitchColumn {
		t.Errorf("第一条开关列不符:\n得到 %q\n期望 %q", got, slowRateSwitchColumn)
	}
	for i, want := range slowRateParamColumns {
		if got := strings.TrimSpace(m.Statements[i+1]); got != want {
			t.Errorf("第 %d 个参数列不符:\n得到 %q\n期望 %q", i+1, got, want)
		}
	}
}

// TestSlowRateParamsStayNullableWithoutDefault 是「NULL = 取代码默认值」这条口径的钉子。
//
// 五个参数列一旦带上 DEFAULT，NULL 语义就没了：既有的未配置行会被填成一个具体值，
// 而那个值未必等于代码默认值（如 0.2 系数存的是 200 千分比，若误给 DEFAULT 0，
// 低速线会变成 0，任何正速率都不算低速——功能静默失效）。
func TestSlowRateParamsStayNullableWithoutDefault(t *testing.T) {
	m := findSlowRateMigration(t)

	for _, raw := range m.Statements[1:] {
		statement := strings.ToUpper(raw)
		if strings.Contains(statement, " DEFAULT ") {
			t.Errorf("参数列不得有 DEFAULT（NULL 表示取代码默认值）: %s", raw)
		}
		if strings.Contains(statement, "NOT NULL") {
			t.Errorf("参数列必须可空: %s", raw)
		}
		if !strings.Contains(statement, " INTEGER") {
			t.Errorf("参数列必须是 integer（系数用千分比存整数，不引入 numeric）: %s", raw)
		}
	}
}

// TestSlowRateSwitchDefaultsToOff 钉住「默认全关」：开关列必须 NOT NULL 且 DEFAULT false。
//
// 若 NOT NULL 丢了，既有行得 NULL，Go 侧 bool 读成 false——行为碰巧一致，但「明确关闭」
// 与「未设置」混为一谈，将来想做「全局默认开启、逐渠道覆写」时无法区分两者。
func TestSlowRateSwitchDefaultsToOff(t *testing.T) {
	m := findSlowRateMigration(t)

	statement := strings.ToUpper(m.Statements[0])
	if !strings.Contains(statement, "NOT NULL") {
		t.Errorf("开关列必须 NOT NULL: %s", m.Statements[0])
	}
	if !strings.Contains(statement, "DEFAULT FALSE") {
		t.Errorf("开关列必须 DEFAULT false（默认全关）: %s", m.Statements[0])
	}
	if !strings.Contains(statement, " BOOLEAN") {
		t.Errorf("开关列必须是 boolean: %s", m.Statements[0])
	}
}
