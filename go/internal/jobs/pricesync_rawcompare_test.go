package jobs

import (
	"encoding/json"
	"fmt"
	"runtime"
	"testing"
)

// TestPriceDataEqualRawMatchesParsedComparison 钉住「按需解析」与「先解析再比」同判：
// 库里的原始 jsonb 文本解析后交给 priceDataEqual，结果必须与直接把解析结果传进去一致。
//
// 三种输入就是 jsonb 会制造的全部麻烦：键序由库侧排序、数值文本形态不同（1 vs 1.0）、
// 以及真的变了。若哪天真退化成字节比较，前两条会红。
func TestPriceDataEqualRawMatchesParsedComparison(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		incoming any
	}{
		{"键序不同", `{"b":{"x":2.0},"a":1.0}`, map[string]any{"a": 1.0, "b": map[string]any{"x": 2.0}}},
		{"数值文本不同", `{"a":1,"b":{"x":2}}`, map[string]any{"a": 1.0, "b": map[string]any{"x": 2.0}}},
		{"真不同", `{"a":1.5,"b":{"x":2.0}}`, map[string]any{"a": 1.0, "b": map[string]any{"x": 2.0}}},
	}
	for _, testCase := range cases {
		var decoded any
		if err := json.Unmarshal([]byte(testCase.existing), &decoded); err != nil {
			t.Fatalf("%s: 解码失败: %v", testCase.name, err)
		}
		want := priceDataEqual(decoded, testCase.incoming)
		got, err := priceDataEqualRaw([]byte(testCase.existing), testCase.incoming)
		if err != nil {
			t.Fatalf("%s: priceDataEqualRaw 失败: %v", testCase.name, err)
		}
		if got != want {
			t.Fatalf("%s: priceDataEqualRaw=%v 与 priceDataEqual=%v 不一致", testCase.name, got, want)
		}
	}
}

// TestPriceDataEqualRawVerdicts 固定三种输入的期望判定，避免上一条用例「两边一起错」仍绿。
func TestPriceDataEqualRawVerdicts(t *testing.T) {
	equal := func(existing string, incoming any) bool {
		t.Helper()
		got, err := priceDataEqualRaw([]byte(existing), incoming)
		if err != nil {
			t.Fatalf("priceDataEqualRaw 失败: %v", err)
		}
		return got
	}
	if !equal(`{"b":{"x":2.0},"a":1.0}`, map[string]any{"a": 1.0, "b": map[string]any{"x": 2.0}}) {
		t.Fatal("键序不同应判为相等")
	}
	if !equal(`{"a":1,"b":{"x":2}}`, map[string]any{"a": 1.0, "b": map[string]any{"x": 2.0}}) {
		t.Fatal("数值文本不同但数值相等应判为相等")
	}
	if equal(`{"a":1.5}`, map[string]any{"a": 1.0}) {
		t.Fatal("数值不同必须判为不等（否则该行永不重写）")
	}
	// 解析失败按「不相等」处理：宁可重写一次，也不能因为坏行跳过更新。
	if got, err := priceDataEqualRaw([]byte(`{"a":`), map[string]any{"a": 1.0}); err == nil || got {
		t.Fatalf("解析失败应返回错误且判为不相等，实际 equal=%v err=%v", got, err)
	}
}

// BenchmarkPriceSyncExistingRowHeap 量「整表读入内存」的常驻堆：同样 11838 行，
// 解析成 map[string]any 与保留 json.RawMessage 的 HeapAlloc 差值。
//
// 跑法（每次迭代就是一轮整表规模，故固定 1 次）：
//
//	go test ./internal/jobs/ -run '^$' -bench PriceSyncExistingRowHeap -benchtime=1x
func BenchmarkPriceSyncExistingRowHeap(b *testing.B) {
	const (
		rowCount   = 11838
		fieldCount = 280
	)
	payloads := make([][]byte, rowCount)
	for index := range payloads {
		fields := make(map[string]any, fieldCount)
		fields["mode"] = "chat"
		fields["litellm_provider"] = "openai"
		for len(fields) < fieldCount {
			fields[fmt.Sprintf("cost_field_%04d", len(fields))] = float64(len(fields)) * 1e-6
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			b.Fatalf("构造夹具失败: %v", err)
		}
		payloads[index] = encoded
	}
	b.Logf("rows=%d avgPayload=%dB totalRaw=%.1fMB", rowCount,
		len(payloads[0]), float64(rowCount*len(payloads[0]))/(1<<20))

	measure := func(b *testing.B, load func() map[string]any) {
		b.ReportAllocs()
		var retained float64
		for iteration := 0; iteration < b.N; iteration++ {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			held := load()
			runtime.ReadMemStats(&after)
			retained = float64(after.HeapAlloc) - float64(before.HeapAlloc)
			runtime.KeepAlive(held)
		}
		b.ReportMetric(retained/(1<<20), "retainedMB")
	}

	b.Run("parsed-any", func(b *testing.B) {
		measure(b, func() map[string]any {
			existing := make(map[string]any, rowCount)
			for index, payload := range payloads {
				// 每行拷一份：生产里这些字节来自驱动缓冲，不共享；解析后字节可回收，只有结构驻留。
				var decoded any
				if err := json.Unmarshal(append([]byte(nil), payload...), &decoded); err != nil {
					b.Fatalf("解码失败: %v", err)
				}
				existing[fmt.Sprintf("model-%05d", index)] = decoded
			}
			return existing
		})
	})
	b.Run("raw-bytes", func(b *testing.B) {
		measure(b, func() map[string]any {
			existing := make(map[string]any, rowCount)
			for index, payload := range payloads {
				// 同上：原始字节本身也是驻留成本的一部分（不解析，但要留着）。
				existing[fmt.Sprintf("model-%05d", index)] = json.RawMessage(append([]byte(nil), payload...))
			}
			return existing
		})
	})
}
