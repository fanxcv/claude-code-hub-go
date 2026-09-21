package migrate

import (
	"strings"
	"testing"
)

// 本文件钉住 0130_slow_rate_probe_columns 的契约。
//
// 两列是「首字后低速探测换家」机制的配置面：探测阈值 T（秒）与探测最低 token 数。
// 两条契约与 0127/0128 的参数列同源，理由也同源：
//
//  1. 必须可空且**无默认值**——NULL 有明确语义：T 为 NULL 表示**不探测**（机制默认关闭，
//     这是本机制的产品承诺）；min_tokens 为 NULL 表示取代码默认值（与 minOutputTokens 同值）。
//     若给 DEFAULT 0，则「未配置」会静默变成 T=0，而 T<=0 的语义是「恒不触发」——
//     列上看不出差别，排障时却分不清「没配」与「配成 0」。
//  2. 必须是 integer——与既有七个 slow_rate_* 参数列同形。
//  3. 语句块数必须是 2（一列一句）。
const slowRateProbeMigrationTag = "0130_slow_rate_probe_columns"

// slowRateProbeColumns 是两个探测列，顺序即迁移内的书写顺序。
var slowRateProbeColumns = []string{
	`ALTER TABLE "providers" ADD COLUMN "slow_rate_probe_after_first_byte_seconds" integer;`,
	`ALTER TABLE "providers" ADD COLUMN "slow_rate_probe_min_tokens" integer;`,
}

func findSlowRateProbeMigration(t *testing.T) Migration {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for _, m := range migrations {
		if m.Tag == slowRateProbeMigrationTag {
			return m
		}
	}
	t.Fatalf("迁移 %s 不在 journal 里（列数对不上时先看 journal 与 sql 副本是否同步）", slowRateProbeMigrationTag)
	return Migration{}
}

func TestSlowRateProbeMigrationAddsTwoNullableInts(t *testing.T) {
	m := findSlowRateProbeMigration(t)

	if got := len(m.Statements); got != 2 {
		t.Fatalf("语句块数: 得到 %d，期望 2", got)
	}
	for index, want := range slowRateProbeColumns {
		if got := strings.TrimSpace(m.Statements[index]); got != want {
			t.Errorf("第 %d 条列定义不符:\n得到 %q\n期望 %q", index, got, want)
		}
	}
}

// TestSlowRateProbeColumnsStayNullableWithoutDefault 是「NULL = 不探测 / 取代码默认值」这条口径的钉子。
func TestSlowRateProbeColumnsStayNullableWithoutDefault(t *testing.T) {
	m := findSlowRateProbeMigration(t)

	for _, raw := range m.Statements {
		statement := strings.ToUpper(strings.TrimSpace(raw))
		if strings.Contains(statement, " DEFAULT ") {
			t.Errorf("列不得有 DEFAULT（NULL 表示不探测 / 取代码默认值）: %s", raw)
		}
		if strings.Contains(statement, "NOT NULL") {
			t.Errorf("列必须可空: %s", raw)
		}
		if !strings.Contains(statement, " INTEGER") {
			t.Errorf("列必须是 integer（与既有七个参数列同形）: %s", raw)
		}
	}
}

// TestSlowRateProbeMigrationIsPurelyAdditive 钉住「新增探测列是增量，不改旧列」这条口径。
//
// 样本写入器按列名读慢-rate 参数列（window/trigger/ratio/…）；若有人在加探测列时顺手改了
// 它们的定义，那两处读取面会静默失配——而失配的表现是「配置不生效」，没有任何报错。
func TestSlowRateProbeMigrationIsPurelyAdditive(t *testing.T) {
	m := findSlowRateProbeMigration(t)

	for _, raw := range m.Statements {
		for _, existing := range []string{
			"slow_rate_monitor_enabled",
			"slow_rate_window_seconds",
			"slow_rate_baseline_window_seconds",
			"slow_rate_min_samples",
			"slow_rate_trigger_count",
			"slow_rate_ratio_per_mille",
			"slow_rate_penalty_step",
			"slow_rate_penalty_max",
		} {
			if strings.Contains(raw, `"`+existing+`"`) {
				t.Errorf("0130 不得改动既有列 %s（新增探测列只增不改）: %s", existing, raw)
			}
		}
	}
}
