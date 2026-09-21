package migrate

import (
	"strings"
	"testing"
)

// 本文件钉住 0128_slow_rate_trigger_count 的契约。
//
// 该列是为修一处**列语义抢占**而加的：`slow_rate_min_samples` 原本被两处按不同语义读——
// 样本写入器当「触发阈值」（默认 3）、基线任务当「基线样本下限」（默认 100）。
// 拆开后 min_samples 保持基线语义（只增不改），新增 trigger_count 承担阈值语义。
//
// 三条契约与 0127 的参数列同源，理由也同源：
//  1. 必须可空且**无默认值**——NULL 表示取代码默认值 3；若给 DEFAULT 0，
//     `count < 0` 恒假、阈值门槛消失，且 penalty 档位分母为 0（除零）；
//  2. 必须是 integer——与既有五个参数列同形；
//  3. 语句块数必须是 1（一列一句）。
const slowRateTriggerMigrationTag = "0128_slow_rate_trigger_count"

const slowRateTriggerColumn = `ALTER TABLE "providers" ADD COLUMN "slow_rate_trigger_count" integer;`

func findSlowRateTriggerMigration(t *testing.T) Migration {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for _, m := range migrations {
		if m.Tag == slowRateTriggerMigrationTag {
			return m
		}
	}
	t.Fatalf("迁移 %s 不在 journal 里（列数对不上时先看 journal 与 sql 副本是否同步）", slowRateTriggerMigrationTag)
	return Migration{}
}

func TestSlowRateTriggerMigrationAddsOneNullableInt(t *testing.T) {
	m := findSlowRateTriggerMigration(t)

	if got := len(m.Statements); got != 1 {
		t.Fatalf("语句块数: 得到 %d，期望 1", got)
	}
	if got := strings.TrimSpace(m.Statements[0]); got != slowRateTriggerColumn {
		t.Errorf("列定义不符:\n得到 %q\n期望 %q", got, slowRateTriggerColumn)
	}
}

// TestSlowRateTriggerColumnStaysNullableWithoutDefault 是「NULL = 取代码默认值」这条口径的钉子。
func TestSlowRateTriggerColumnStaysNullableWithoutDefault(t *testing.T) {
	m := findSlowRateTriggerMigration(t)

	statement := strings.ToUpper(strings.TrimSpace(m.Statements[0]))
	if strings.Contains(statement, " DEFAULT ") {
		t.Errorf("列不得有 DEFAULT（NULL 表示取代码默认值 3）: %s", m.Statements[0])
	}
	if strings.Contains(statement, "NOT NULL") {
		t.Errorf("列必须可空: %s", m.Statements[0])
	}
	if !strings.Contains(statement, " INTEGER") {
		t.Errorf("列必须是 integer（与既有五个参数列同形）: %s", m.Statements[0])
	}
}

// TestSlowRateTriggerMigrationIsPurelyAdditive 钉住「拆分是增量，不改旧列」这条口径。
//
// 基线任务按列名读 slow_rate_min_samples；若有人在加新列时顺手改了它的定义
// （改名、加 DEFAULT、变类型），基线读取面会静默失配。
func TestSlowRateTriggerMigrationIsPurelyAdditive(t *testing.T) {
	m := findSlowRateTriggerMigration(t)

	for _, raw := range m.Statements {
		if strings.Contains(raw, "slow_rate_min_samples") {
			t.Errorf("0128 不得改动 slow_rate_min_samples（基线任务的列，拆分只增不改）: %s", raw)
		}
	}
}
