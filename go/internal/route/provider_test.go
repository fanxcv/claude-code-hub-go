package route

import (
	"encoding/json"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// providerFromStore 是本包从只读视图取数的唯一通道：四列必须原样过桥，
// 且 priority 与 protocol_conversion_enabled 必须落成非 nil 指针（选路侧按 Node 的
// `?? 0` 与 `=== true` 口径判读，指针落在 nil 上会把启用读成关闭）。
func TestProviderFromStoreBridgesRoutingColumns(t *testing.T) {
	vendorID := int64(4)
	groupTag := "vip"
	row := store.Provider{
		ID:                        9,
		Name:                      "p9",
		ProviderType:              string(convert.ProviderCodex),
		URL:                       "https://up.example",
		IsEnabled:                 true,
		Weight:                    3,
		Priority:                  7,
		CostMultiplier:            "1.5",
		GroupTag:                  &groupTag,
		AllowedModels:             json.RawMessage(`["gpt-*"]`),
		ProviderVendorID:          &vendorID,
		GroupPriorities:           json.RawMessage(`{"vip": 2}`),
		ProtocolConversionEnabled: true,
		DisableSessionReuse:       true,
	}

	provider := providerFromStore(row)

	if provider.ID != 9 || provider.Name != "p9" || provider.URL != "https://up.example" {
		t.Fatalf("基础列未过桥: %+v", provider)
	}
	if provider.ProviderType != convert.ProviderCodex {
		t.Fatalf("provider_type = %q, want %q", provider.ProviderType, convert.ProviderCodex)
	}
	if !provider.IsEnabled || provider.Weight != 3 {
		t.Fatalf("启用态或权重未过桥: %+v", provider)
	}
	if provider.Priority == nil || *provider.Priority != 7 || provider.EffectivePriority() != 7 {
		t.Fatalf("priority 未过桥: %v", provider.Priority)
	}
	if provider.ProtocolConversionEnabled == nil || !provider.ConversionEnabled() {
		t.Fatal("protocol_conversion_enabled 未过桥")
	}
	if provider.ProviderVendorID == nil || *provider.ProviderVendorID != 4 {
		t.Fatalf("provider_vendor_id 未过桥: %v", provider.ProviderVendorID)
	}
	if provider.GroupPriorities["vip"] != 2 {
		t.Fatalf("group_priorities 未过桥: %v", provider.GroupPriorities)
	}
	if !provider.DisableSessionReuse {
		t.Fatal("disable_session_reuse 未过桥")
	}
	if provider.GroupTag == nil || *provider.GroupTag != "vip" {
		t.Fatalf("group_tag 未过桥: %v", provider.GroupTag)
	}
	if provider.CostMultiplier.String() != "1.5" {
		t.Fatalf("cost_multiplier 未过桥: %q", provider.CostMultiplier.String())
	}
	if string(provider.AllowedModels) != `["gpt-*"]` {
		t.Fatalf("allowed_models 未过桥: %s", provider.AllowedModels)
	}

	// 关闭态的协议转换必须读成关闭，而不是「未知」。
	disabled := providerFromStore(store.Provider{Priority: 0})
	if disabled.ConversionEnabled() {
		t.Fatal("未开启协议转换时应读成关闭")
	}
	if disabled.Priority == nil || disabled.EffectivePriority() != 0 {
		t.Fatalf("缺省 priority 应读成 0，实际 %v", disabled.Priority)
	}
}
