package store

import (
	"math"
	"testing"
)

func TestFormatCostForStorage(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantValue string
		wantOK    bool
	}{
		{name: "整数补足到 COST_SCALE", input: "1", wantValue: "1.000000000000000", wantOK: true},
		{name: "保留 15 位小数", input: "0.000000000000001", wantValue: "0.000000000000001", wantOK: true},
		{name: "超出 15 位四舍五入", input: "0.0000000000000015", wantValue: "0.000000000000002", wantOK: true},
		{name: "负数保留符号", input: "-2.5", wantValue: "-2.500000000000000", wantOK: true},
		{name: "科学计数法可解析", input: "1e-3", wantValue: "0.001000000000000", wantOK: true},
		{name: "两侧空白被裁掉", input: "  3.25  ", wantValue: "3.250000000000000", wantOK: true},
		{name: "空串非法", input: "", wantOK: false},
		{name: "纯空白非法", input: "   ", wantOK: false},
		{name: "非数字非法", input: "abc", wantOK: false},
		{name: "NaN 非法", input: "NaN", wantOK: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := FormatCostForStorage(testCase.input)
			if ok != testCase.wantOK {
				t.Fatalf("ok = %v, want %v（输入 %q）", ok, testCase.wantOK, testCase.input)
			}
			if ok && got != testCase.wantValue {
				t.Fatalf("值 = %q, want %q", got, testCase.wantValue)
			}
		})
	}
}

func TestFormatCostFloat(t *testing.T) {
	got, ok := FormatCostFloat(0.1)
	if !ok {
		t.Fatal("0.1 应当被接受")
	}
	if got != "0.100000000000000" {
		t.Fatalf("0.1 -> %q", got)
	}

	if _, ok := FormatCostFloat(math.NaN()); ok {
		t.Fatal("NaN 必须被拒绝")
	}
	if _, ok := FormatCostFloat(math.Inf(1)); ok {
		t.Fatal("+Inf 必须被拒绝")
	}
}

func TestAddCostStrings(t *testing.T) {
	got, ok := AddCostStrings("0.1", "0.2")
	if !ok {
		t.Fatal("合法入参应当成功")
	}
	// 十进制精确相加：0.1 + 0.2 必须是 0.3，而不是浮点误差值。
	if got != "0.300000000000000" {
		t.Fatalf("0.1 + 0.2 = %q", got)
	}

	got, ok = AddCostStrings("", "1.5")
	if !ok {
		t.Fatal("空串应按 0 处理")
	}
	if got != "1.500000000000000" {
		t.Fatalf("空串 + 1.5 = %q", got)
	}

	if _, ok := AddCostStrings("x", "1"); ok {
		t.Fatal("非法入参必须失败")
	}
}
