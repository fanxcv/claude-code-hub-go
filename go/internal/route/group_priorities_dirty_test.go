package route

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// TestProviderFromStoreToleratesDirtyGroupPriorities 钉住 store→route 的桥接口径：
// group_priorities 的脏值只代表「这家没有覆盖」，不得让这家供应商不可用；问题必须随
// Provider 带到选路器（告警点在那边，因为只有它有 logger）。
func TestProviderFromStoreToleratesDirtyGroupPriorities(t *testing.T) {
	groupTag := "fan"
	bridge := func(raw string) Provider {
		t.Helper()
		return providerFromStore(store.Provider{
			ID:              1,
			Name:            "p1",
			ProviderType:    string(convert.ProviderClaude),
			IsEnabled:       true,
			Weight:          1,
			Priority:        7,
			CostMultiplier:  "1",
			GroupTag:        &groupTag,
			GroupPriorities: json.RawMessage(raw),
		})
	}

	// 干净值：覆盖可用、无问题，且其余列不受影响。
	clean := bridge(`{"fan": 0, "vip": 2}`)
	if len(clean.groupPrioritiesIssues) != 0 {
		t.Fatalf("干净值不该报问题: %+v", clean.groupPrioritiesIssues)
	}
	if clean.GroupPriorities["fan"] != 0 || clean.GroupPriorities["vip"] != 2 {
		t.Fatalf("覆盖 = %v, want fan=0 vip=2", clean.GroupPriorities)
	}
	if clean.Priority == nil || *clean.Priority != 7 {
		t.Fatalf("priority 必须照旧过桥: %v", clean.Priority)
	}

	// 脏值：解不出覆盖，但**供应商本身仍可用**（ID/priority/分组都照旧），并带出问题。
	for _, raw := range []string{`"not-an-object"`, `[1,2]`, `{"fan":"0"}`, `{"fan":0.5}`} {
		dirty := bridge(raw)
		if len(dirty.GroupPriorities) != 0 {
			t.Fatalf("%s：不该解出任何覆盖，实际 %v", raw, dirty.GroupPriorities)
		}
		if len(dirty.groupPrioritiesIssues) == 0 {
			t.Fatalf("%s：必须带出问题（静默丢弃正是要消除的行为）", raw)
		}
		if dirty.ID != 1 || dirty.Name != "p1" || dirty.Priority == nil || *dirty.Priority != 7 {
			t.Fatalf("%s：脏值不得影响这家供应商的其它列: %+v", raw, dirty)
		}
		if dirty.GroupTag == nil || *dirty.GroupTag != groupTag {
			t.Fatalf("%s：group_tag 应照旧过桥", raw)
		}
	}

	// 无覆盖的两种形态（缺省 / null）都不算问题。
	for _, raw := range []string{"", "null"} {
		if provider := bridge(raw); len(provider.groupPrioritiesIssues) != 0 {
			t.Fatalf("%q：无覆盖不该报问题: %+v", raw, provider.groupPrioritiesIssues)
		}
	}
}

// TestSelectorStillSelectsFromBatchWithDirtyGroupPriorities 是选路侧的「脏值不扩散」钉子：
// 同批里有一家 group_priorities 是脏值（顶层标量），仍然必须选出供应商。
//
// 修前的后果不是「这家没有覆盖」，而是整批读取失败（只读路径逐行解码，readRowsAs 在第一个
// 解不开的行上返回错误）⇒ 选路拿到 0 家候选 ⇒ 503，且日志只说「反序列化只读行失败」。
// 另外钉住告警的频次：resolve 是**每请求**调用的，同一处值只该报一次。
func TestSelectorStillSelectsFromBatchWithDirtyGroupPriorities(t *testing.T) {
	// 告警走 logx.Warn，测试里显式把级别压到 warn 并在结束时还原。
	previousLevel := logx.CurrentLevel()
	logx.SetLevel("warn")
	t.Cleanup(func() { logx.SetLevel(previousLevel) })

	var logs bytes.Buffer
	groupTag := "fan"
	// 脏值那家配置优先级 5、干净那家 3 且在 fan 组下有覆盖 0 ⇒ 干净那家必然胜出
	// （既证明它仍可选，也证明它自己的覆盖仍然生效）。
	dirty := providerFromStore(store.Provider{
		ID: 1, Name: "dirty", ProviderType: string(convert.ProviderClaude), IsEnabled: true,
		Weight: 1, Priority: 5, CostMultiplier: "1", GroupTag: &groupTag,
		GroupPriorities: json.RawMessage(`"not-an-object"`),
	})
	clean := providerFromStore(store.Provider{
		ID: 2, Name: "clean", ProviderType: string(convert.ProviderClaude), IsEnabled: true,
		Weight: 1, Priority: 3, CostMultiplier: "1", GroupTag: &groupTag,
		GroupPriorities: json.RawMessage(`{"fan": 0}`),
	})
	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{dirty, clean}},
		Rand:   (&scriptedRand{values: []float64{0}}).next,
		Logger: logx.New(&logs),
	})
	request := Request{Model: "m", Format: convert.FormatClaude, Group: groupTag}

	result, err := selector.Select(context.Background(), request)
	if err != nil {
		t.Fatalf("脏值行不得让整批选路失败: %v", err)
	}
	if result.Provider == nil {
		t.Fatal("必须仍有候选被选出（修前这里会因整批读取失败而无候选）")
	}
	if result.Provider.ID != 2 {
		t.Fatalf("应选中覆盖后优先级最高的干净供应商 2，实际 %d", result.Provider.ID)
	}
	if result.Context.TotalProviders != 2 {
		t.Fatalf("脏值那家仍应在候选里（不是被丢弃），TotalProviders = %d",
			result.Context.TotalProviders)
	}

	// 第二次选路：同一处脏值不得重复告警（每请求都会走 resolve）。
	if _, err := selector.Select(context.Background(), request); err != nil {
		t.Fatalf("第二次选路失败: %v", err)
	}
	event := "route.group_priorities.invalid_value"
	if count := strings.Count(logs.String(), event); count != 1 {
		t.Fatalf("同一处脏值应只告警一次，实际 %d 次；日志：%s", count, logs.String())
	}
	if !strings.Contains(logs.String(), `"providerId":1`) || !strings.Contains(logs.String(), `"topLevel":true`) {
		t.Fatalf("告警必须带定位信息（供应商与顶层标记）：%s", logs.String())
	}
}
