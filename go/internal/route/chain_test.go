package route

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// goldenRow 读取 Node 数据面一次真实请求产生的 message_request 行。
func goldenRow(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", "message_request_row.json"))
	if err != nil {
		t.Fatalf("读取黄金样本失败: %v", err)
	}
	var envelope struct {
		Row map[string]any `json:"row"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("解析黄金样本失败: %v", err)
	}
	return envelope.Row
}

func keysOf(record map[string]any) []string {
	keys := make([]string, 0, len(record))
	for key := range record {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// TestChainItemMatchesGoldenKeySet 用真实落库的 provider_chain[0] 钉住本包产出的字段集：
// 键集合必须一致，多一个键或少一个键都算与 Node 的落库形态不符。
func TestChainItemMatchesGoldenKeySet(t *testing.T) {
	row := goldenRow(t)
	chain, ok := row["provider_chain"].([]any)
	if !ok || len(chain) == 0 {
		t.Fatalf("黄金样本缺少 provider_chain")
	}
	goldenItem, ok := chain[0].(map[string]any)
	if !ok {
		t.Fatalf("provider_chain[0] 不是对象")
	}

	// 用与黄金样本同形的输入构造一次选路：单供应商、priority 0、weight 1、costMultiplier 1、
	// 已关联厂 1、类型 codex、分组 default 命中。
	provider := baseProvider(1, convert.ProviderCodex)
	provider.Name = "mock-upstream"
	provider.CostMultiplier = "1"
	provider.ProviderVendorID = i64Ptr(1)
	provider.GroupTag = nil

	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{provider}, byID: map[int64]Provider{1: provider}},
		Rand:   (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{
		Model: "gpt-5.6", Format: convert.FormatResponse, Group: GroupDefault,
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("应选中唯一候选")
	}

	encoded, err := json.Marshal(result.ChainItem())
	if err != nil {
		t.Fatalf("链项序列化失败: %v", err)
	}
	var produced map[string]any
	if err := json.Unmarshal(encoded, &produced); err != nil {
		t.Fatalf("链项反序列化失败: %v", err)
	}

	goldenKeys := keysOf(goldenItem)
	producedKeys := keysOf(produced)
	if len(goldenKeys) != len(producedKeys) {
		t.Fatalf("链项键数不一致：黄金 %v，本包 %v", goldenKeys, producedKeys)
	}
	for index, key := range goldenKeys {
		if producedKeys[index] != key {
			t.Fatalf("链项键集合不一致：黄金 %v，本包 %v", goldenKeys, producedKeys)
		}
	}

	// 决策上下文同样逐键对齐（黄金样本里 filteredProviders 为空数组，不是被省略）。
	goldenContext, ok := goldenItem["decisionContext"].(map[string]any)
	if !ok {
		t.Fatalf("黄金样本缺少 decisionContext")
	}
	producedContext, ok := produced["decisionContext"].(map[string]any)
	if !ok {
		t.Fatalf("本包未产出 decisionContext")
	}
	if got, want := keysOf(producedContext), keysOf(goldenContext); len(got) != len(want) {
		t.Fatalf("decisionContext 键数不一致：黄金 %v，本包 %v", want, got)
	} else {
		for index, key := range want {
			if got[index] != key {
				t.Fatalf("decisionContext 键集合不一致：黄金 %v，本包 %v", want, got)
			}
		}
	}

	// 数值形态：golden 的 costMultiplier 是不带引号的数字，本包也必须如此。
	if _, isNumber := produced["costMultiplier"].(float64); !isNumber {
		t.Errorf("costMultiplier 应为 JSON 数字，实际 %T", produced["costMultiplier"])
	}
	if produced["groupTag"] != nil {
		t.Errorf("未分组供应商的 groupTag 应显式为 null，实际 %v", produced["groupTag"])
	}
	if groups, ok := producedContext["priorityLevels"].([]any); !ok || len(groups) != 1 {
		t.Errorf("priorityLevels 应为数组，实际 %v", producedContext["priorityLevels"])
	}
}
