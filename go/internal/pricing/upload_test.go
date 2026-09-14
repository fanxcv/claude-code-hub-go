package pricing

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
)

// TestParseUploadContentCptTable CPT v1 文件（上传路径最常见的一种）：
// schema 命中 → 走 cpt-schema 校验 + 转换，产出按模型名排序的 Entry。
func TestParseUploadContentCptTable(t *testing.T) {
	entries, err := ParseUploadContent(string(readTestdata(t, "cpt-fixture.json")))
	if err != nil {
		t.Fatalf("CPT 上传解析失败: %v", err)
	}
	names := entryNames(entries)
	want := []string{"claude-sonnet-test", "claude-sonnet-test-alias", "collision-model", "image-model-test"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("CPT 上传的模型集合/顺序不符：got=%v want=%v", names, want)
	}
	// 别名展开为独立键且与 canonical 同价（cpt-convert.ts:508-520）。
	canonical := entryData(t, entries, "claude-sonnet-test")
	alias := entryData(t, entries, "claude-sonnet-test-alias")
	if string(canonical) != string(alias) {
		t.Errorf("别名行应与 canonical 同价：canonical=%s alias=%s", canonical, alias)
	}
}

// TestParseUploadContentLegacyJson 内部旧格式（顶层 model_name → priceData 对象）：
// 不做 CPT 校验、不转换，原样作为条目；`sample_spec` 是元数据字段，跳过。
func TestParseUploadContentLegacyJson(t *testing.T) {
	content := `{
		"sample_spec": {"note": "metadata"},
		"zeta-model": {"mode": "chat", "input_cost_per_token": 1e-6},
		"alpha-model": {"mode": "chat", "input_cost_per_token": 2e-6}
	}`
	entries, err := ParseUploadContent(content)
	if err != nil {
		t.Fatalf("内部格式解析失败: %v", err)
	}
	// 保持文档顺序（Node 的 Object.entries 顺序），因此 zeta 在 alpha 之前。
	if got, want := strings.Join(entryNames(entries), ","), "zeta-model,alpha-model"; got != want {
		t.Fatalf("内部格式顺序/过滤不符：got=%q want=%q", got, want)
	}
	if string(entryData(t, entries, "alpha-model")) != `{"mode": "chat", "input_cost_per_token": 2e-6}` {
		t.Errorf("内部格式应原样保留 priceData 原文：%s", entryData(t, entries, "alpha-model"))
	}
}

// TestParseUploadContentJsonNonObject JSON 可解析但不是对象（数组/标量）：
// 与 Node 的 processPriceTableInternal 同判——「价格表必须是一个JSON对象」。
func TestParseUploadContentJsonNonObject(t *testing.T) {
	for _, content := range []string{`[1,2]`, `"text"`, `42`, `null`} {
		_, err := ParseUploadContent(content)
		if err == nil {
			t.Fatalf("输入 %s 应失败（Node 同判），实际成功", content)
		}
		if !errors.Is(err, errNotJSONObject) {
			t.Errorf("输入 %s 的错误应是「价格表必须是一个JSON对象」，实际: %v", content, err)
		}
	}
}

// TestParseUploadContentTomlDegraded 旧版 TOML：Go 侧降级（无 TOML 库），
// 报 UnsupportedTOML；HTTP 形状与 Node 的 TOML 解析失败同为 400 action_failed，
// 但**可观察行为不同**（Node 会接受合法 TOML），该降级登记在报告里。
func TestParseUploadContentTomlDegraded(t *testing.T) {
	toml := "[\"models\".\"gpt-test\"]\nmode = \"chat\"\ninput_cost_per_token = 1e-6\n"
	_, err := ParseUploadContent(toml)
	if !errors.Is(err, UnsupportedTOML) {
		t.Fatalf("TOML 输入应报 UnsupportedTOML，实际: %v", err)
	}
	if _, err := ParseUploadContent("   "); !errors.Is(err, UnsupportedTOML) {
		t.Fatalf("空内容应报 UnsupportedTOML（Node 侧 JSON/TOML 双解析均抛错），实际: %v", err)
	}
}

// TestParseUploadContentCptLikeButInvalid schema 命中但结构不合规时，
// 必须返回 cpt-schema 的具体错误（不是静默降级成内部格式）。
func TestParseUploadContentCptLikeButInvalid(t *testing.T) {
	_, err := ParseUploadContent(`{"schema":"cchp.pricing-table/v1","models":[],"providers":{}}`)
	if err == nil || !strings.Contains(err.Error(), "models 为空") {
		t.Fatalf("空 models 应报「价格表格式无效：models 为空」，实际: %v", err)
	}

	_, err = ParseUploadContent(`{"schema":"cchp.pricing-table/v1","models":[{"model_name":"x","slug":"x","vendor":"v","pricing":[{"provider":"p","charges":{"prompt":{"price":"1","unit":"per_M_tokens"}}}]}]}`)
	if err == nil || !strings.Contains(err.Error(), "缺少 providers 字典") {
		t.Fatalf("缺 providers 应报「价格表格式无效：缺少 providers 字典」，实际: %v", err)
	}
}

// TestConvertedTableEntriesSorted ConvertedTableEntries 的顺序口径：按模型名排序
// （Go 的 map 无插入序；该差异登记为白名单项），且每条的 Data 是可解析的 JSON 对象。
func TestConvertedTableEntriesSorted(t *testing.T) {
	table, err := jobs.ParseCptTable(readTestdata(t, "cpt-fixture.json"))
	if err != nil {
		t.Fatalf("ParseCptTable 失败: %v", err)
	}
	converted := jobs.ConvertCptTable(table)
	entries := ConvertedTableEntries(converted)

	previous := ""
	for _, entry := range entries {
		if previous != "" && entry.Name < previous {
			t.Fatalf("条目应按模型名升序：%q 出现在 %q 之后", entry.Name, previous)
		}
		previous = entry.Name
		var decoded map[string]any
		if err := json.Unmarshal(entry.Data, &decoded); err != nil {
			t.Fatalf("条目 %s 的 priceData 不是 JSON 对象: %v", entry.Name, err)
		}
		if _, ok := decoded["mode"]; !ok {
			t.Errorf("条目 %s 缺少 mode（写入侧会判为失败）", entry.Name)
		}
	}
	if len(entries) != len(converted.Models) {
		t.Fatalf("条目数应等于转换产出的模型数：entries=%d models=%d", len(entries), len(converted.Models))
	}
}

func entryNames(entries []Entry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}

func entryData(t *testing.T, entries []Entry, name string) json.RawMessage {
	t.Helper()
	for _, entry := range entries {
		if entry.Name == name {
			return entry.Data
		}
	}
	t.Fatalf("条目 %s 不存在（现有：%v）", name, entryNames(entries))
	return nil
}
