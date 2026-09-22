package store

import "testing"

// 本文件钉住**低速参数改单位后的「payload 名 ↔ 列名」绑定**（用户 2026-09-22 裁决：
// 列名换新、REST 字段名沿旧）。
//
// 为何必须钉这一条：本批改动里**只有这三行**是「两侧名字故意不同」——
// payload（外部契约）仍叫 slow_rate_window_seconds，而落库的列叫 slow_rate_window_minutes。
// 写错任一侧都是静默的：
//   - 列名写错 ⇒ 至少还会报「列不存在」；
//   - **payload 名写错** ⇒ 前端提交的旧名被白名单拒掉（422），或反过来白名单只认新名而界面
//     还在发旧名 ⇒ 用户看到「保存成功」但字段根本没写。这正是 0133 一系要消掉的那种无声旋钮。
//
// 旧三列（slow_rate_window_seconds / slow_rate_baseline_window_seconds /
// slow_rate_ratio_per_mille）在 0134 迁移里被**保留**（回滚依据），但已无人读。若绑定表还指着
// 它们，写入的值就落进没人读的列——也是无声旋钮。故两条都钉。

func TestSlowRateWriteBindingsUseNewColumnsWithLegacyPayloadNames(t *testing.T) {
	// payload 名（REST 契约，不变量） → 列名（已换新单位）。
	want := map[string]string{
		"slow_rate_window_seconds":          "slow_rate_window_minutes",
		"slow_rate_baseline_window_seconds": "slow_rate_baseline_window_days",
		"slow_rate_ratio_per_mille":         "slow_rate_ratio",
		// 下面几项列名与 payload 名相同（本来就同名，或是本批新增的字段）。
		"slow_rate_monitor_enabled":                "slow_rate_monitor_enabled",
		"slow_rate_min_samples":                    "slow_rate_min_samples",
		"slow_rate_trigger_count":                  "slow_rate_trigger_count",
		"slow_rate_penalty_step":                   "slow_rate_penalty_step",
		"slow_rate_penalty_max":                    "slow_rate_penalty_max",
		"slow_rate_recovery_requests":              "slow_rate_recovery_requests",
		"slow_rate_probe_after_first_byte_seconds": "slow_rate_probe_after_first_byte_seconds",
	}

	bindings := map[string]string{}
	for _, field := range adminProviderWriteFields {
		if len(field.Payload) > len("slow_rate_") && field.Payload[:len("slow_rate_")] == "slow_rate_" {
			bindings[field.Payload] = field.Column
		}
	}

	for payload, column := range want {
		got, ok := bindings[payload]
		if !ok {
			t.Errorf("payload 名 %q 不在 adminProviderWriteFields 里：前端提交它会 422（契约要求沿旧名）", payload)
			continue
		}
		if got != column {
			t.Errorf("payload %q 绑到了列 %q，期望 %q", payload, got, column)
		}
	}

	// 反面一：旧列名不得再出现在绑定表里（0134 保留旧列只为回滚，无人读）。
	abandoned := map[string]bool{
		"slow_rate_window_seconds":          true,
		"slow_rate_baseline_window_seconds": true,
		"slow_rate_ratio_per_mille":         true,
	}
	for payload, column := range bindings {
		if abandoned[column] {
			t.Errorf("payload %q 仍绑到已废弃的旧列 %q：该列不再被读，值写了也没人用（见 0134 迁移）",
				payload, column)
		}
	}

	// 反面二：这些列必须都是可空整数/数值型（null = 取代码默认值）。
	// 若被写成非空 kind，界面「清空输入框」会变成 invalid_type——本仓踩过这个坑（max_retry_attempts）。
	for _, field := range adminProviderWriteFields {
		if len(field.Payload) < len("slow_rate_") || field.Payload[:len("slow_rate_")] != "slow_rate_" {
			continue
		}
		if field.Payload == "slow_rate_monitor_enabled" {
			if field.Kind != providerBoolKind {
				t.Errorf("slow_rate_monitor_enabled 应是布尔 kind，得到 %v", field.Kind)
			}
			continue
		}
		// 提交前速率闸的**闸本身**是可空布尔（NULL = 未覆盖 ⇒ false），不是 nullable_int：
		// 它的默认值不是「数」而是「关」，且必须能分辨「未配」与「配成关」。
		if field.Payload == "slow_rate_precommit_enabled" {
			if field.Kind != providerNullableBoolKind {
				t.Errorf("slow_rate_precommit_enabled 应是 nullable_bool kind（NULL = 未覆盖），得到 %v", field.Kind)
			}
			continue
		}
		if field.Payload == "slow_rate_ratio_per_mille" {
			if field.Kind != providerNumericKind {
				t.Errorf("slow_rate_ratio_per_mille 的系数是 numeric(5,4) 小数，kind 应为 numeric，得到 %v", field.Kind)
			}
			continue
		}
		if field.Kind != providerNullableIntKind {
			t.Errorf("%s 应是 nullable_int kind（null = 取默认值），得到 %v", field.Payload, field.Kind)
		}
	}
}
