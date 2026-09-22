package migrate

import (
	"strings"
	"testing"
)

// 本文件钉住 0135_provider_live_stats_switch 的契约。
//
// 为什么加这一列：供应商页要展示「每个渠道的实时并发数」，而该统计必须**可选**——打开才统计显示、
// 关上完全不占用资源（用户裁决）。开关落 system_settings 全局列（而非 providers 逐渠道列）：
// 复用现成的管理面读写与设置页表单，且不碰 providers 写面。
//
// 三条契约：
//  1. 只 ADD 这一个列，不得顺手改别的列（同 0130/0133 的口径）；
//  2. 默认必须是 **false** —— 关是默认，否则存量部署一升级就会开始写统计键，
//     等于把「可选、关则零开销」变成默认开销；
//  3. 语句块数必须是 1。
//
// 为何断言用 Contains 而不是整句相等：迁移切分把前导注释并进第一个语句块（0133 的 SQL 只有一行
// DDL 才能整句比）。
const providerLiveStatsSwitchTag = "0135_provider_live_stats_switch"

func findProviderLiveStatsSwitch(t *testing.T) Migration {
	t.Helper()
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for _, m := range migrations {
		if m.Tag == providerLiveStatsSwitchTag {
			return m
		}
	}
	t.Fatalf("迁移 %s 不在 journal 里（先看 journal 与 sql 副本是否同步）", providerLiveStatsSwitchTag)
	return Migration{}
}

func TestProviderLiveStatsSwitchShape(t *testing.T) {
	m := findProviderLiveStatsSwitch(t)

	if got := len(m.Statements); got != 1 {
		t.Fatalf("语句块数: 得到 %d，期望 1", got)
	}
	statement := strings.TrimSpace(m.Statements[0])
	want := `ALTER TABLE "system_settings" ADD COLUMN "provider_live_stats_enabled" boolean DEFAULT false NOT NULL;`
	if !strings.Contains(statement, want) {
		t.Errorf("列定义不符:\n得到 %q\n期望含 %q", statement, want)
	}
}

// TestProviderLiveStatsSwitchDefaultsOff 钉住默认值这一条：默认 true 会让「可选」名不副实。
func TestProviderLiveStatsSwitchDefaultsOff(t *testing.T) {
	m := findProviderLiveStatsSwitch(t)

	joined := strings.Join(m.Statements, "\n")
	if !strings.Contains(joined, `DEFAULT false`) {
		t.Errorf("列默认值必须是 false（关上即零开销是默认态）：%s", joined)
	}
	if strings.Contains(joined, `DEFAULT true`) {
		t.Errorf("列默认值不得为 true：%s", joined)
	}
}

// TestProviderLiveStatsSwitchTouchesNothingElse 钉住「只加这一列」。
func TestProviderLiveStatsSwitchTouchesNothingElse(t *testing.T) {
	m := findProviderLiveStatsSwitch(t)

	for _, raw := range m.Statements {
		for _, kept := range []string{
			"affinity_enabled",
			"affinity_ignore_client_session_id",
			"legacy_hedge_max_in_flight",
			"limit_concurrent_sessions",
			"slow_rate_monitor_enabled",
		} {
			if strings.Contains(raw, `"`+kept+`"`) {
				t.Errorf("0135 不得动列 %s（只加实时统计开关那一列）: %s", kept, raw)
			}
		}
	}
}
