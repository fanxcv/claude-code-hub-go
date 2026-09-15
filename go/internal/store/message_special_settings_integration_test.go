package store

import (
	"context"
	"encoding/json"
	"testing"
)

// 本文件是**审计条目终态后追加**的真库用例（`CCH_TEST_DSN` 未设置时跳过）。
//
// 为什么必须有真库用例：追加写的是 jsonb 表达式（`COALESCE(col,'[]'::jsonb) || $2::jsonb`），
// 列名、类型转换与「已有条目必须保留」这三件事都只有真库能证。SQL 形状单测只能证明语句长什么样。

// TestIntegrationAppendSpecialSettingsKeepsExistingEntries 钉住追加语义：
// 终态写先进的那批条目（守卫链写的客户端侧审计）必须留住，响应侧只往后补。
func TestIntegrationAppendSpecialSettingsKeepsExistingEntries(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	created := createRequestFixture(t, pools, key, false)
	ctx := context.Background()

	// 第一段：模拟守卫链在建行时写的客户端侧审计。
	statusCode := 200
	clientEntries := []byte(`[{"type":"thinking_effort_confirmed","scope":"request","hit":true}]`)
	if _, err := pools.UpdateDetailsIfUnfinalized(ctx, created.ID, DetailsPatch{
		StatusCode:      &statusCode,
		SpecialSettings: clientEntries,
	}); err != nil {
		t.Fatalf("写入客户端侧审计失败: %v", err)
	}

	// 第二段：模拟响应修复器在终态之后的补写。
	responseEntries := []byte(`[{"type":"response_fixer","scope":"response","hit":true,` +
		`"fixersApplied":[{"fixer":"encoding","applied":true,"details":"removed_utf8_bom"}],` +
		`"totalBytesProcessed":10,"processingTimeMs":0}]`)
	if err := pools.AppendSpecialSettings(ctx, created.ID, responseEntries); err != nil {
		t.Fatalf("追加审计条目失败: %v", err)
	}

	raw := readSpecialSettings(t, pools, created.ID)
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("special_settings 应是 JSON 数组: %v（原文 %s）", err, raw)
	}
	if len(entries) != 2 {
		t.Fatalf("追加后应保留两段条目，实际 %d 条：%s", len(entries), raw)
	}
	if entries[0]["type"] != "thinking_effort_confirmed" {
		t.Fatalf("先写的条目必须留在原位：%v", entries[0])
	}
	if entries[1]["type"] != "response_fixer" {
		t.Fatalf("追加的条目应在数组末尾：%v", entries[1])
	}
}

// TestIntegrationAppendSpecialSettingsHandlesNullColumn 钉住 COALESCE 分支：
// 列仍为 NULL（建行没写审计）时，追加不该整行失败，而应落成单个元素的数组。
func TestIntegrationAppendSpecialSettingsHandlesNullColumn(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	created := createRequestFixture(t, pools, key, false)
	ctx := context.Background()

	if raw := readSpecialSettings(t, pools, created.ID); raw != nil {
		t.Fatalf("前置条件：新行的 special_settings 应为 NULL，实际 %s", raw)
	}

	entries := []byte(`[{"type":"response_fixer","scope":"response","hit":true}]`)
	if err := pools.AppendSpecialSettings(ctx, created.ID, entries); err != nil {
		t.Fatalf("NULL 列上追加失败: %v", err)
	}

	var decoded []map[string]any
	if err := json.Unmarshal(readSpecialSettings(t, pools, created.ID), &decoded); err != nil {
		t.Fatalf("追加后应是合法 JSON 数组: %v", err)
	}
	if len(decoded) != 1 || decoded[0]["type"] != "response_fixer" {
		t.Fatalf("NULL 列上追加应落成单元素数组：%v", decoded)
	}
}

// TestIntegrationAppendSpecialSettingsNoopOnEmptyInput 钉住空入参不写库（零额外写入纪律）。
func TestIntegrationAppendSpecialSettingsNoopOnEmptyInput(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	created := createRequestFixture(t, pools, key, false)
	ctx := context.Background()

	if err := pools.AppendSpecialSettings(ctx, created.ID, nil); err != nil {
		t.Fatalf("空条目不该报错: %v", err)
	}
	if raw := readSpecialSettings(t, pools, created.ID); raw != nil {
		t.Fatalf("空条目不该写库，实际 %s", raw)
	}
}

// readSpecialSettings 直接读回 special_settings 列原文（nil 表示列仍为 NULL）。
func readSpecialSettings(t *testing.T, pools *Pools, id int64) []byte {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT "special_settings" FROM message_request WHERE "id" = $1`, id).Scan(&raw); err != nil {
		t.Fatalf("读回 special_settings 失败: %v", err)
	}
	return raw
}
