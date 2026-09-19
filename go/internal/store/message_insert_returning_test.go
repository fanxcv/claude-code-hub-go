package store

import (
	"reflect"
	"strings"
	"testing"
)

// TestBuildMessageRequestInsertNarrowReturning 钉住开行的两条路径只在 RETURNING 上不同：
// INSERT 部分（列集、占位符、参数个数）逐字节相同，且「只取行标识」那条不再请求 jsonb 大列。
//
// 为什么要钉：生产路径（守卫链开行、回放审计行）只用 id，而 special_settings / routing_trace /
// affinity_fingerprint_chain 是 jsonb——RETURNING 会把它们的正文序列化回客户端。这类回归
// 不改任何返回值，真库用例也看不出来，只有对着语句本身才能钉住。
func TestBuildMessageRequestInsertNarrowReturning(t *testing.T) {
	cost := "0.000000000000000"
	data := CreateMessageRequestData{
		UserID:                   7,
		Key:                      "sk-test-narrow-returning",
		CostUSD:                  &cost,
		SpecialSettings:          []byte(`{"keep":"me"}`),
		RoutingTrace:             []byte(`[{"providerId":1}]`),
		AffinityFingerprintChain: []byte(`["fp"]`),
	}

	wideQuery, wideArgs, err := buildMessageRequestInsert(data, messageRequestReturning)
	if err != nil {
		t.Fatalf("宽路径组装失败: %v", err)
	}
	narrowQuery, narrowArgs, err := buildMessageRequestInsert(data, messageRequestIDReturning)
	if err != nil {
		t.Fatalf("窄路径组装失败: %v", err)
	}

	const marker = " RETURNING "
	wideIndex := strings.Index(wideQuery, marker)
	narrowIndex := strings.Index(narrowQuery, marker)
	if wideIndex < 0 || narrowIndex < 0 {
		t.Fatalf("语句缺少 RETURNING 段：\n宽=%s\n窄=%s", wideQuery, narrowQuery)
	}
	if wideQuery[:wideIndex] != narrowQuery[:narrowIndex] {
		t.Fatalf("两条路径的 INSERT 段必须逐字节相同：\n宽=%s\n窄=%s", wideQuery[:wideIndex], narrowQuery[:narrowIndex])
	}

	// 窄路径：只请求 id，且确实不再带 jsonb 大列。
	narrowReturning := narrowQuery[narrowIndex+len(marker):]
	if narrowReturning != "id" {
		t.Fatalf("窄路径 RETURNING 应为 id，实际 %q", narrowReturning)
	}
	// 列名仍会在 INSERT 段出现（本来就要写），所以只看 RETURNING 段。
	for _, column := range []string{"special_settings", "routing_trace", "affinity_fingerprint_chain"} {
		if strings.Contains(narrowReturning, column) {
			t.Fatalf("窄路径不应回传大列 %s：%s", column, narrowReturning)
		}
	}
	// 宽路径仍带它们——真库集成用例靠这些断言写入的列值。
	wideReturning := wideQuery[wideIndex+len(marker):]
	for _, column := range []string{"special_settings", "routing_trace", "affinity_fingerprint_chain"} {
		if !strings.Contains(wideReturning, column) {
			t.Fatalf("宽路径应保留大列 %s：%s", column, wideReturning)
		}
	}

	// 参数与占位符与 RETURNING 无关：两条路径必须同参同序。
	if len(wideArgs) != len(narrowArgs) || len(wideArgs) != len(messageRequestColumns) {
		t.Fatalf("参数个数不一致：宽=%d 窄=%d 列=%d", len(wideArgs), len(narrowArgs), len(messageRequestColumns))
	}
	for index := range wideArgs {
		if !reflect.DeepEqual(wideArgs[index], narrowArgs[index]) {
			t.Fatalf("第 %d 个参数不一致：宽=%v 窄=%v", index, wideArgs[index], narrowArgs[index])
		}
	}
}
