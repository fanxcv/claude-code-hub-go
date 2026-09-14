package adminapi

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// TestBillingModelSourceDefaultMatchesNode 钉住缺省值本身。
//
// 回归背景（2026-09-13）：`leaderboard.go` 与 `admin_user_insights.go` 曾用**域外值** `"model"`
// 作回退（Node 的取值域只有 `original`/`redirected`），使设置行缺失时按「重定向后模型」聚合，
// 与 Node 相反。Node 四处证据（类型域 / DB 列默认 / 读侧 `?? "original"` / Node 自己的测试）
// 均指向 `"original"`，出处见 billing_model_source.go 的文件头。
func TestBillingModelSourceDefaultMatchesNode(t *testing.T) {
	if billingModelSourceDefault != "original" {
		t.Fatalf("缺省值应为 Node 的 %q，实际 %q", "original", billingModelSourceDefault)
	}
	// 值必须在 Node 的类型域内——`"model"` 不在其中，出现即说明又有人自造取值。
	nodeDomain := map[string]bool{"original": true, "redirected": true}
	if !nodeDomain[billingModelSourceDefault] {
		t.Fatalf("缺省值 %q 不在 Node 的 BillingModelSource 取值域内（original|redirected）", billingModelSourceDefault)
	}
}

// TestResolveBillingModelSourceMirrorsNodeNullishChain 钉住取值链与 Node 的 `??` 同义。
//
// Node：`dbSettings?.billingModelSource ?? "original"`
//   - 设置行缺失（nil）→ `"original"`；
//   - 行在则**原样**传（含空串——`??` 不覆盖空串，空串会走「优先 model 列」那支）；
//   - 不做空白归一：`" original "` 在 Node 眼里不是 original。
func TestResolveBillingModelSourceMirrorsNodeNullishChain(t *testing.T) {
	cases := []struct {
		name     string
		settings *store.SystemSettings
		want     string
	}{
		{"设置行缺失 → Node 缺省 original", nil, "original"},
		{"行在但值为空串 → 原样透传（Node 的 ?? 不覆盖空串）", &store.SystemSettings{}, ""},
		{"original 原样", &store.SystemSettings{BillingModelSource: "original"}, "original"},
		{"redirected 原样", &store.SystemSettings{BillingModelSource: "redirected"}, "redirected"},
		{"不做空白归一（Node 比的是原始串）", &store.SystemSettings{BillingModelSource: " original "}, " original "},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := resolveBillingModelSource(testCase.settings)
			if got != testCase.want {
				t.Fatalf("应为 %q，实际 %q", testCase.want, got)
			}
			// 回归守卫：任何输入都不得产出 Node 取值域之外的 `"model"`。
			if got == "model" {
				t.Fatalf("产出域外值 \"model\"（Node 的取值域只有 original|redirected）")
			}
		})
	}
}

// TestLeaderboardStoreQueryUsesNodeDefaultWhenSettingsMissing 走真实调用路径（纯函数）钉住
// 排行榜的缺省：**设置行缺失**时必须是 `"original"`（优先 original_model），而不是优先 model 列。
//
// 注意区分两种「没值」：
//   - 设置行**缺失** → Node 的 `?? "original"` 生效 → `"original"`；
//   - 设置行**在但值为空串** → Node 原样透传空串（`??` 不覆盖空串）→ 空串（等价于优先 model 列）。
func TestLeaderboardStoreQueryUsesNodeDefaultWhenSettingsMissing(t *testing.T) {
	missing := leaderboardStoreQuery(leaderboardRequestOptions{}, nil, "Asia/Shanghai")
	if missing.BillingModelSource != "original" {
		t.Fatalf("设置行缺失时 BillingModelSource 应为 original，实际 %q", missing.BillingModelSource)
	}
	empty := leaderboardStoreQuery(leaderboardRequestOptions{}, &store.SystemSettings{}, "Asia/Shanghai")
	if empty.BillingModelSource != "" {
		t.Fatalf("设置行在但为空串时应原样透传空串（Node 的 ?? 不覆盖空串），实际 %q", empty.BillingModelSource)
	}
	// 两者必须被区分开：否则「缺省错」与「空串透传」在查询层看不出差别（`store.leaderboardModelField`
	// 内部按 `== "original"` 分流，未导出，故此处只能钉到该值本身）。
	if missing.BillingModelSource == empty.BillingModelSource {
		t.Fatal("缺失与空串必须产出不同取值（original vs 空串），否则缺省错误在查询层不可见")
	}
	// 显式配置必须原样透传（不得被缺省覆盖）。
	query := leaderboardStoreQuery(
		leaderboardRequestOptions{},
		&store.SystemSettings{BillingModelSource: "redirected"},
		"Asia/Shanghai",
	)
	if query.BillingModelSource != "redirected" {
		t.Fatalf("显式 redirected 应原样透传，实际 %q", query.BillingModelSource)
	}
}

// TestMeUsageBillingModelSourceUsesNodeDefaultWhenSettingsMissing 钉住 my-usage 侧的同一缺省。
//
// 该处曾对 nil 设置返回空串，而空串在 `projectMeUsageEntry` 里等价于「非 original」（优先 model
// 列），与 Node 行缺失时的 `"original"` 相反。
func TestMeUsageBillingModelSourceUsesNodeDefaultWhenSettingsMissing(t *testing.T) {
	if got := meUsageBillingModelSource(nil); got != "original" {
		t.Fatalf("设置行缺失应为 original，实际 %q", got)
	}
	if got := meUsageBillingModelSource(&store.SystemSettings{BillingModelSource: "original"}); got != "original" {
		t.Fatalf("显式 original 应原样，实际 %q", got)
	}
}
