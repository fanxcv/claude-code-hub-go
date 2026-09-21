package migrate

import (
	"strings"
	"testing"
)

// 本文件钉住 0133_drop_slow_rate_probe_min_tokens 的契约。
//
// 为什么删列：门控在首次分类出内容帧时**即提交**，故该机制的探测窗口按定义不含任何内容帧，
// 窗口内已生成 token 恒为 0——token 闸与速率闸在其唯一适用窗口里恒不成立。留着它等于给
// 管理员一个能被设置却毫无效果的旋钮（比死代码更坏：配置界面会让它看起来生效）。
// 见 slowrate/probe.go 的文件头注释与 docs 侧的设计口径。
//
// 三条契约：
//  1. 只 DROP 这一个列，不得顺手改别的列（同 0130 的「纯增量」口径）；
//  2. 必须用 DROP COLUMN（不是改名，也不是留下孤儿列）；
//  3. 语句块数必须是 1。
const slowRateProbeMinTokensDropTag = "0133_drop_slow_rate_probe_min_tokens"

func findSlowRateProbeMinTokensDrop(t *testing.T) Migration {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for _, m := range migrations {
		if m.Tag == slowRateProbeMinTokensDropTag {
			return m
		}
	}
	t.Fatalf("迁移 %s 不在 journal 里（先看 journal 与 sql 副本是否同步）", slowRateProbeMinTokensDropTag)
	return Migration{}
}

func TestSlowRateProbeMinTokensDropShape(t *testing.T) {
	m := findSlowRateProbeMinTokensDrop(t)

	if got := len(m.Statements); got != 1 {
		t.Fatalf("语句块数: 得到 %d，期望 1", got)
	}
	statement := strings.TrimSpace(m.Statements[0])
	want := `ALTER TABLE "providers" DROP COLUMN "slow_rate_probe_min_tokens";`
	if statement != want {
		t.Errorf("列定义不符:\n得到 %q\n期望 %q", statement, want)
	}
}

// TestSlowRateProbeMinTokensDropTouchesNothingElse 钉住「只删这一列」。
//
// 探测阈值列（slow_rate_probe_after_first_byte_seconds）是纯时间判据的唯一旋钮，必须留着；
// 其余 slow_rate_* 参数列属终态判定，更不得受牵连。
func TestSlowRateProbeMinTokensDropTouchesNothingElse(t *testing.T) {
	m := findSlowRateProbeMinTokensDrop(t)

	for _, raw := range m.Statements {
		for _, kept := range []string{
			"slow_rate_probe_after_first_byte_seconds",
			"slow_rate_monitor_enabled",
			"slow_rate_window_seconds",
			"slow_rate_baseline_window_seconds",
			"slow_rate_min_samples",
			"slow_rate_trigger_count",
			"slow_rate_ratio_per_mille",
			"slow_rate_penalty_step",
			"slow_rate_penalty_max",
		} {
			if strings.Contains(raw, `"`+kept+`"`) {
				t.Errorf("0133 不得动列 %s（只删 token 下限那一列）: %s", kept, raw)
			}
		}
	}
}
