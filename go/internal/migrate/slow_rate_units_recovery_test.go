package migrate

import (
	"strings"
	"testing"
)

// 本文件钉住 0134_slow_rate_units_and_recovery 的契约。
//
// 为什么是纯 ADD 而不改旧列语义：用户 2026-09-22 裁决「新列名 + REST 字段名沿旧」。
// 旧列名带 `_seconds` / `_per_mille` 后缀，改语义而不改名会让列名与语义永久不符——
// 正是 0133 删 slow_rate_probe_min_tokens 要消掉的那种「能被设置却毫无效果的旋钮」。
// 纯 ADD 还保可回滚：旧列与其存量值原样留着，代码回退一版即恢复旧语义（本仓无 down 迁移）。
//
// 四条契约：
//  1. 只 ADD 这四列，不得顺手改/删别的列（同 0130 的「纯增量」口径）；
//  2. 必须用 ADD COLUMN（不是改名，也不是 UPDATE 回填）；
//  3. 四列的类型与可空性逐字一致（ratio 是 numeric(5,4)，其余三个是 integer）；
//  4. 语句块数必须是 4。
const slowRateUnitsRecoveryTag = "0134_slow_rate_units_and_recovery"

// stripSQLLeadingComments 去掉语句块开头的 `--` 注释行。
//
// 为何必须有它：journal 的切分器按 `--> statement-breakpoint` 切块，而本仓惯例是把
// 「为什么这样做」写在迁移文件头部，于是**第一块**会连着那堆注释。断言列定义必须先剥注释，
// 否则钉的是注释排版而不是 DDL。
func stripSQLLeadingComments(statement string) string {
	lines := strings.Split(statement, "\n")
	index := 0
	for index < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[index]), "--") {
		index++
	}
	return strings.TrimSpace(strings.Join(lines[index:], "\n"))
}

func findSlowRateUnitsRecovery(t *testing.T) Migration {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for _, m := range migrations {
		if m.Tag == slowRateUnitsRecoveryTag {
			return m
		}
	}
	t.Fatalf("迁移 %s 不在 journal 里（先看 journal 与 sql 副本是否同步）", slowRateUnitsRecoveryTag)
	return Migration{}
}

func TestSlowRateUnitsRecoveryShape(t *testing.T) {
	m := findSlowRateUnitsRecovery(t)

	if got := len(m.Statements); got != 4 {
		t.Fatalf("语句块数: 得到 %d，期望 4", got)
	}
	want := []string{
		`ALTER TABLE "providers" ADD COLUMN "slow_rate_window_minutes" integer;`,
		`ALTER TABLE "providers" ADD COLUMN "slow_rate_baseline_window_days" integer;`,
		`ALTER TABLE "providers" ADD COLUMN "slow_rate_ratio" numeric(5,4);`,
		`ALTER TABLE "providers" ADD COLUMN "slow_rate_recovery_requests" integer;`,
	}
	for index, expected := range want {
		if got := stripSQLLeadingComments(m.Statements[index]); got != expected {
			t.Errorf("第 %d 块不符:\n得到 %q\n期望 %q", index, got, expected)
		}
	}
}

// TestSlowRateUnitsRecoveryKeepsDeprecatedColumns 钉住「旧列留着、不回填」。
//
// 三条旧列（window_seconds / baseline_window_seconds / ratio_per_mille）是本迁移的**回滚依据**：
// 代码回退一版后旧列必须仍是用户的原值，故不得 UPDATE、不得 DROP。新列也不得带 DEFAULT——
// 留 NULL 才等于「取代码默认」，带 DEFAULT 会把默认值固化进 DDL，改默认值就又要一次迁移。
func TestSlowRateUnitsRecoveryKeepsDeprecatedColumns(t *testing.T) {
	m := findSlowRateUnitsRecovery(t)

	for _, raw := range m.Statements {
		ddl := stripSQLLeadingComments(raw)
		upper := strings.ToUpper(ddl)
		if strings.Contains(upper, "DROP COLUMN") {
			t.Errorf("0134 不得删列（旧三列是回滚依据）: %s", ddl)
		}
		if strings.Contains(upper, "UPDATE ") {
			t.Errorf("0134 不得回填（存量留 NULL 即取代码默认）: %s", ddl)
		}
		if strings.Contains(upper, "DEFAULT") {
			t.Errorf("0134 新列不得带 DEFAULT（NULL 才是「取代码默认」的表达）: %s", ddl)
		}
		if strings.Contains(upper, "RENAME") {
			t.Errorf("0134 不得改名（改名会让旧列消失，破坏回滚）: %s", ddl)
		}
	}
}

// TestSlowRateUnitsRecoveryTouchesNothingElse 钉住「只 ADD 这四列，别的列一概不碰」。
//
// 尤其要保住三条已废弃的旧列与其余 slow_rate_* 参数列：它们要么是回滚依据，要么仍在生效。
func TestSlowRateUnitsRecoveryTouchesNothingElse(t *testing.T) {
	m := findSlowRateUnitsRecovery(t)

	kept := []string{
		"slow_rate_monitor_enabled",
		"slow_rate_window_seconds",
		"slow_rate_baseline_window_seconds",
		"slow_rate_min_samples",
		"slow_rate_trigger_count",
		"slow_rate_ratio_per_mille",
		"slow_rate_penalty_step",
		"slow_rate_penalty_max",
		"slow_rate_probe_after_first_byte_seconds",
	}
	for _, raw := range m.Statements {
		ddl := stripSQLLeadingComments(raw)
		for _, column := range kept {
			if strings.Contains(ddl, `"`+column+`"`) {
				t.Errorf("0134 不得动列 %s（只新增四列）: %s", column, ddl)
			}
		}
	}
}
