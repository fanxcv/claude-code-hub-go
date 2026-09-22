package adminapi

import "testing"

// 本文件钉住低速系数在**服务端**的合法域是 0-1 小数（用户 2026-09-22 裁决改单位：
// 千分比整数 → 0-1 小数）。
//
// 为何必须服务端也验：前端输入框的 min/max 只是提示，绕开界面直接打 API 就能送 300（旧千分比
// 习惯）或 1.5。放过去的话低速线会变成基线的 30000% / 150%，判定**恒假**（永不标慢）且不报错，
// 只有用户觉得「降权再也没触发过」。
//
// 另一条同样要紧：**null 必须被接受**（列可空，null = 取代码默认值）。本仓在
// max_retry_attempts 上踩过反向的坑——用了非空 spec，界面清空输入框就报
// invalid_type「Expected number, received null」。

func TestProviderSlowRateRatioSpecAcceptsOnlyUnitInterval(t *testing.T) {
	decodeRatio := func(raw string) (any, bool, int) {
		object := providerSampleObject(t, map[string]string{"slow_rate_ratio": raw})
		value, present := providerSlowRateRatioFieldSpec().Decode(object, "slow_rate_ratio")
		return value, present, len(object.issues)
	}

	// 非空合法值：解出 *float64 的值（与 store 侧 providerNumericKind 同型）。
	value, present, _ := decodeRatio(`0.3`)
	if !present {
		t.Fatal("0.3 应被接受")
	}
	number, ok := value.(*float64)
	if !ok || number == nil || *number != 0.3 {
		t.Fatalf("0.3 应解出 *float64(0.3)，得到 %#v", value)
	}

	// 上界 1 合法（区间上端是闭的）。
	if _, present, _ := decodeRatio(`1`); !present {
		t.Error("上界 1 应被接受")
	}

	// 越界（含旧千分比习惯值 300）必须被**驳回**：解码器的约定是「present 表示字段被给出」，
	// 驳回以 object.fail 记问题 + 返回 nil 值表达（见 providerSlowRateRatioFieldSpec 的 return nil, true）。
	for _, outOfRange := range []string{`1.5`, `300`, `2`, `-0.1`} {
		value, _, issues := decodeRatio(outOfRange)
		if issues == 0 {
			t.Errorf("系数 %s 超出 0-1 却未被拒（低速线会错得离谱且静默）", outOfRange)
		}
		if value != nil {
			t.Errorf("系数 %s 被拒时不应给回值，得到 %#v", outOfRange, value)
		}
	}

	// 合法值不得留下任何校验问题。
	if _, _, issues := decodeRatio(`0.3`); issues != 0 {
		t.Errorf("合法系数 0.3 不该留下校验问题，得到 %d 条", issues)
	}

	// null 必须被接受，且解出 nil *float64（可空列）。
	value, present, _ = decodeRatio(`null`)
	if !present {
		t.Fatal("null 应被接受（列可空，null = 取代码默认值）")
	}
	if pointer, ok := value.(*float64); !ok || pointer != nil {
		t.Fatalf("null 应解出 nil *float64，得到 %#v", value)
	}
}
