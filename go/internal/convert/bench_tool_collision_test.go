package convert

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// 本文件量「展平后同名工具冲突检测」这一步的成本：O(n²) 写法（逐工具扫一遍已收列表）曾是本次
// 全部改动里唯一的显著 CPU 增量（200 工具 +40.6 µs / +37.7%，且全落在首帧之前），故改成在同一次
// 遍历内维护 map。两份实现跑在同一个工具体上，差值只来自检测策略。
//
// legacy 对照由 TestLegacyToolCollisionScanMatchesProduction 逐项对齐：基准里的基线必须是
// 「同一逻辑、只是检测策略退化成 O(n²)」，否则数字不说明任何事。

// toolCollisionBody 造一个带 count 个互不重名 function 工具的 responses 工具表（`tools` 值本身）。
func toolCollisionBody(tb testing.TB, count int) *Value {
	tb.Helper()
	var builder strings.Builder
	builder.WriteString(`[`)
	for index := 0; index < count; index++ {
		if index > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"type":"function","name":"tool_`)
		builder.WriteString(strconv.Itoa(index))
		builder.WriteString(`","description":"d","parameters":{"type":"object"},"strict":false}`)
	}
	builder.WriteString(`]`)
	value, err := ParseJSON([]byte(builder.String()))
	if err != nil {
		tb.Fatalf("解析基准工具表失败: %v", err)
	}
	return value
}

// legacyAppendResponsesToolEntries 是冲突检测走 O(n²) 的对照实现：逻辑与 appendResponsesToolEntries
// 逐行一致，只有「查重」退化为逐工具扫一遍已收列表（改动前的写法）。
func legacyAppendResponsesToolEntries(entries []*Value, tools *[]Tool, preserved *[]*Value, loss *LossCollector) {
	for _, entry := range entries {
		if !isRecord(entry) {
			loss.Dropped(LossUnknownField, "request", "tools[]")
			continue
		}
		entryType, _ := stringField(entry, "type")
		if entryType == "namespace" {
			inner := fieldOrNil(entry, "tools")
			if inner == nil || !inner.IsArray() {
				*preserved = append(*preserved, entry)
				continue
			}
			namespace, _ := stringField(entry, "name")
			loss.Dropped(LossUnknownFieldTool, "request", "tool.namespace."+namespace)
			legacyAppendResponsesToolEntries(inner.Items(), tools, preserved, loss)
			continue
		}
		name, hasName := stringField(entry, "name")
		if entryType != "function" || !hasName || name == "" {
			*preserved = append(*preserved, entry)
			continue
		}
		collided := false
		for _, existing := range *tools {
			if existing.Name == name {
				collided = true
				break
			}
		}
		if collided {
			loss.Dropped(LossUnknownFieldTool, "request", "tool.name_collision."+name)
			continue
		}
		tool := Tool{Name: name, Parameters: NewObject()}
		if description, ok := stringField(entry, "description"); ok {
			tool.Description = description
			tool.HasDescript = true
		}
		if parameters := fieldOrNil(entry, "parameters"); parameters != nil && parameters.IsObject() {
			tool.Parameters = parameters
		}
		for _, member := range entry.Members() {
			if responsesFunctionFields.contains(member.Key) || member.Value.IsNull() {
				continue
			}
			if member.Key == "strict" {
				if strict, ok := member.Value.Bool(); ok {
					tool.Strict, tool.HasStrict = strict, true
				} else {
					loss.Dropped(LossUnknownField, "request", "tool.strict")
				}
				continue
			}
			loss.Dropped(LossUnknownField, "request", "tool."+member.Key)
		}
		*tools = append(*tools, tool)
	}
}

// TestLegacyToolCollisionScanMatchesProduction 钉住基准基线确实等价：同一份带冲突的工具表，
// 两版收下的工具名与记损条目必须逐条相同。否则 O(n²) 对照的 ns/op 无从比较。
func TestLegacyToolCollisionScanMatchesProduction(t *testing.T) {
	const body = `{"model":"m","input":[{"type":"message","role":"user","content":"hi"}],"tools":[
	  {"type":"function","name":"dup","description":"first","parameters":{"type":"object"}},
	  {"type":"namespace","name":"agents","tools":[
	    {"type":"function","name":"dup","description":"second","parameters":{"type":"object"}},
	    {"type":"function","name":"solo","parameters":{"type":"object"}}]}]}`
	raw := mustParsePayload(t, body)

	productionLoss := &LossCollector{}
	productionTools, _ := decodeResponsesTools(fieldOrNil(raw, "tools"), productionLoss)

	legacyLoss := &LossCollector{}
	legacyTools := []Tool{}
	legacyPreserved := []*Value{}
	legacyAppendResponsesToolEntries(asUnknownArray(fieldOrNil(raw, "tools")), &legacyTools, &legacyPreserved, legacyLoss)

	if names, other := toolNameList(productionTools), toolNameList(legacyTools); strings.Join(names, ",") != strings.Join(other, ",") {
		t.Fatalf("两版收下的工具名不一致：production=%q legacy=%q", names, other)
	}
	if got, want := describeLossEntries(legacyLoss.Report()), describeLossEntries(productionLoss.Report()); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("两版记损不一致：production=%q legacy=%q", want, got)
	}
}

func toolNameList(tools []Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	return names
}

func describeLossEntries(report LossReport) []string {
	out := make([]string, 0, len(report.Entries))
	for _, entry := range report.Entries {
		out = append(out, entry.Capability+"/"+string(entry.Action)+"/"+entry.Detail)
	}
	return out
}

// BenchmarkResponsesToolCollision 对 5 / 50 / 200 个工具各量一趟工具表解码：map 版（已上线）
// 对比 O(n²) 对照版。工具表在计时外解析，两版跑的是同一份 *Value。
func BenchmarkResponsesToolCollision(b *testing.B) {
	for _, count := range []int{5, 50, 200} {
		entries := toolCollisionBody(b, count)
		b.Run(fmt.Sprintf("%d/map", count), func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				loss := &LossCollector{}
				if tools, _ := decodeResponsesTools(entries, loss); len(tools) != count {
					b.Fatalf("工具数应为 %d，实际 %d", count, len(tools))
				}
				if len(loss.entries) != 0 {
					b.Fatalf("互不重名的工具表不得记损：%+v", loss.entries)
				}
			}
		})
		b.Run(fmt.Sprintf("%d/legacy-o2", count), func(b *testing.B) {
			b.ReportAllocs()
			for index := 0; index < b.N; index++ {
				loss := &LossCollector{}
				tools := []Tool{}
				preserved := []*Value{}
				legacyAppendResponsesToolEntries(entries.Items(), &tools, &preserved, loss)
				if len(tools) != count {
					b.Fatalf("工具数应为 %d，实际 %d", count, len(tools))
				}
				if len(loss.entries) != 0 {
					b.Fatalf("互不重名的工具表不得记损：%+v", loss.entries)
				}
			}
		})
	}
}

// TestToolCollisionNoMapForSingleTool 钉住约束「工具数 ≤1 时不得建 map」：此时没有可冲突的对象，
// 建 map 只是白送一次堆分配（单工具是最常见形状——很多客户端只声明一个工具）。
//
// 判据是与 O(n²) 对照版同档相等：两版除查重策略外逐行一致，若 map 版多出分配就是 map 本身。
// 不拿 0 工具比——那时两版都提前不分配，比不出检测策略的差别。
func TestToolCollisionNoMapForSingleTool(t *testing.T) {
	allocsFor := func(run func(entries *Value)) float64 {
		entries := toolCollisionBody(t, 1)
		return testing.AllocsPerRun(50, func() { run(entries) })
	}
	production := allocsFor(func(entries *Value) {
		decodeResponsesTools(entries, &LossCollector{})
	})
	legacy := allocsFor(func(entries *Value) {
		tools := []Tool{}
		preserved := []*Value{}
		legacyAppendResponsesToolEntries(entries.Items(), &tools, &preserved, &LossCollector{})
	})
	if production != legacy {
		t.Fatalf("单工具不得因冲突检测多分配（map 版 %.0f 次、O(n²) 对照 %.0f 次）", production, legacy)
	}
}
