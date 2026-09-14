package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
)

// TestConvertCptTableMatchesNodeGolden 把 Go 的 CPT 转换器与 **Node 侧同一实现的产物**逐模型比对。
//
// golden 由 `bun scripts/cpt-convert-golden.ts go/internal/pricing/testdata/cpt-fixture.json
// go/internal/pricing/testdata/cpt-golden.json` 产出（源：src/lib/price-sync/cpt-convert.ts 的
// convertCptTable）。这是本移植唯一的等价性判据——读代码断言不了「546 行 TS 与 766 行 Go 是否同义」。
//
// 比对口径：两侧都先 marshal 成 JSON 再解成 any，然后**按路径递归**比，差异逐条打印。
// 不依赖键序（Go 的 map 无序），但**依赖数值**：价格为浮点乘算结果，任何一位偏差都会被抓到
// （两侧都用同一套 roundPrecision/parseDecimal 语义，见 cpt-convert.ts 与 cpt_convert.go）。
func TestConvertCptTableMatchesNodeGolden(t *testing.T) {
	fixture := readTestdata(t, "cpt-fixture.json")
	goldenRaw := readTestdata(t, "cpt-golden.json")

	var golden struct {
		Version     string                    `json:"version"`
		Currency    string                    `json:"currency"`
		RefreshedAt string                    `json:"refreshedAt"`
		Vendors     []map[string]any          `json:"vendors"`
		Models      map[string]map[string]any `json:"models"`
	}
	if err := json.Unmarshal(goldenRaw, &golden); err != nil {
		t.Fatalf("golden 解析失败: %v", err)
	}

	table, err := jobs.ParseCptTable(fixture)
	if err != nil {
		t.Fatalf("Go 侧 ParseCptTable 失败: %v", err)
	}
	converted := jobs.ConvertCptTable(table)

	if converted.Version != golden.Version {
		t.Errorf("version: Go=%q Node=%q", converted.Version, golden.Version)
	}
	if converted.Currency != golden.Currency {
		t.Errorf("currency: Go=%q Node=%q", converted.Currency, golden.Currency)
	}
	if converted.RefreshedAt != golden.RefreshedAt {
		t.Errorf("refreshedAt: Go=%q Node=%q", converted.RefreshedAt, golden.RefreshedAt)
	}

	// 模型集合必须完全一致（多一个少一个都是移植偏差）。
	goNames := make([]string, 0, len(converted.Models))
	for name := range converted.Models {
		goNames = append(goNames, name)
	}
	nodeNames := make([]string, 0, len(golden.Models))
	for name := range golden.Models {
		nodeNames = append(nodeNames, name)
	}
	sort.Strings(goNames)
	sort.Strings(nodeNames)
	if got, want := len(goNames), len(nodeNames); got != want {
		t.Fatalf("模型数不一致：Go=%d Node=%d\nGo=%v\nNode=%v", got, want, goNames, nodeNames)
	}
	for index := range goNames {
		if goNames[index] != nodeNames[index] {
			t.Fatalf("模型集合不一致（第 %d 个）：Go=%q Node=%q\nGo=%v\nNode=%v",
				index, goNames[index], nodeNames[index], goNames, nodeNames)
		}
	}

	// 逐模型逐字段比：差异按路径列出，便于直接定位到 cpt_convert.go 的哪一段。
	for _, name := range nodeNames {
		goModel := normalizeJSON(t, converted.Models[name])
		nodeModel := normalizeJSON(t, golden.Models[name])
		diffs := diffJSON("", nodeModel, goModel, nil)
		if len(diffs) > 0 {
			t.Errorf("模型 %s 与 Node golden 不一致（%d 处）：\n  %s",
				name, len(diffs), joinLines(diffs))
		}
	}

	// vendor 汇总比（集合语义 + 每条的字段）。
	if got, want := len(converted.Vendors), len(golden.Vendors); got != want {
		t.Fatalf("vendor 数不一致：Go=%d Node=%d", got, want)
	}
	goVendors := map[string]map[string]any{}
	for _, vendor := range converted.Vendors {
		normalized, ok := normalizeJSON(t, vendor).(map[string]any)
		if !ok {
			t.Fatalf("vendor 归一化失败: %#v", vendor)
		}
		slug, _ := normalized["vendor"].(string)
		goVendors[slug] = normalized
	}
	for _, nodeVendor := range golden.Vendors {
		normalized := normalizeJSON(t, nodeVendor).(map[string]any)
		slug, _ := normalized["vendor"].(string)
		goVendor, ok := goVendors[slug]
		if !ok {
			t.Errorf("供应商汇总缺少 %s（Go 侧没有）", slug)
			continue
		}
		if diffs := diffJSON("vendors."+slug, normalized, goVendor, nil); len(diffs) > 0 {
			t.Errorf("供应商 %s 汇总不一致：\n  %s", slug, joinLines(diffs))
		}
	}
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读 testdata/%s 失败: %v", name, err)
	}
	return raw
}

// normalizeJSON 把任意值经 JSON 往返归一，使两侧可比（map[string]any 与结构体统一成 any）。
func normalizeJSON(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal 失败: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal 失败: %v", err)
	}
	return decoded
}

// diffJSON 递归比较，返回 "路径: Node=… Go=…" 形式的差异列表（path 为空表示根）。
func diffJSON(path string, want, got any, out []string) []string {
	switch wanted := want.(type) {
	case map[string]any:
		actual, ok := got.(map[string]any)
		if !ok {
			return append(out, path+" 类型不一致: Node=object Go="+typeName(got))
		}
		for key, value := range wanted {
			child := key
			if path != "" {
				child = path + "." + key
			}
			actualValue, exists := actual[key]
			if !exists {
				out = append(out, child+" 缺失: Node="+preview(value)+" Go=<无此键>")
				continue
			}
			out = diffJSON(child, value, actualValue, out)
		}
		for key, value := range actual {
			if _, exists := wanted[key]; !exists {
				child := key
				if path != "" {
					child = path + "." + key
				}
				out = append(out, child+" 多出: Go="+preview(value))
			}
		}
		return out
	case []any:
		actual, ok := got.([]any)
		if !ok {
			return append(out, path+" 类型不一致: Node=array Go="+typeName(got))
		}
		if len(actual) != len(wanted) {
			return append(out, path+" 长度不一致: Node="+preview(wanted)+" Go="+preview(actual))
		}
		for index := range wanted {
			out = diffJSON(path+"["+itoa(index)+"]", wanted[index], actual[index], out)
		}
		return out
	default:
		// 数值统一按 float64 比（JSON 解出的数字都是 float64）。
		if !scalarEqual(want, got) {
			return append(out, path+" 值不一致: Node="+preview(want)+" Go="+preview(got))
		}
		return out
	}
}

func scalarEqual(want, got any) bool {
	wantFloat, wantIsFloat := want.(float64)
	gotFloat, gotIsFloat := got.(float64)
	if wantIsFloat && gotIsFloat {
		return wantFloat == gotFloat
	}
	return want == got
}

func typeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	default:
		return "unknown"
	}
}

func preview(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<marshal 失败>"
	}
	text := string(encoded)
	if len(text) > 160 {
		return text[:160] + "…"
	}
	return text
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	sign := ""
	if value < 0 {
		sign, value = "-", -value
	}
	digits := make([]byte, 0, 8)
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return sign + string(digits)
}

func joinLines(lines []string) string {
	joined := ""
	for index, line := range lines {
		if index > 0 {
			joined += "\n  "
		}
		joined += line
	}
	return joined
}
