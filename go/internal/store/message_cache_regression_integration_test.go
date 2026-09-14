package store

import (
	"context"
	"testing"
)

// 本文件是「缓存回退」观测两列的真库用例（`CCH_TEST_DSN` 未设置时整组跳过）。
//
// 只有真库能证明的部分：两列由终态写语句自己算出来（前一行取自同 session_id 的行），
// 以及口径里那几种「无从比较」的边界确实留 NULL，而不是被写成 false。
func TestIntegrationCacheRegressionDerivedOnSettlement(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })
	ctx := context.Background()

	session := "go-store-cache-regression-" + key
	control, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}

	newRow := func(withSession bool) int64 {
		t.Helper()
		model := "go-store-it-model"
		endpoint := "/v1/messages"
		data := CreateMessageRequestData{
			ProviderID: 1,
			UserID:     1,
			Key:        key,
			Model:      &model,
			Endpoint:   &endpoint,
		}
		if withSession {
			data.SessionID = &session
		}
		request, err := pools.CreateMessageRequest(ctx, data)
		if err != nil {
			t.Fatalf("建行失败: %v", err)
		}
		return request.ID
	}

	// settle 走真实终态写路径——推导就挂在这条路径上。
	settle := func(id int64, cacheRead int64) {
		t.Helper()
		statusCode := 200
		durationMS := 100
		patch := DetailsPatch{
			StatusCode:           &statusCode,
			DurationMS:           &durationMS,
			CacheReadInputTokens: &cacheRead,
		}
		won, err := pools.UpdateDetailsIfUnfinalized(ctx, id, patch)
		if err != nil {
			t.Fatalf("终态写失败: %v", err)
		}
		if !won {
			t.Fatalf("行 %d 的首次终态写应当赢得唯一终态", id)
		}
	}

	assertRegression := func(id int64, wantRead int64, wantPrevious *int64, wantRegressed *bool) {
		t.Helper()
		var cacheRead, previous *int64
		var regressed *bool
		if err := control.QueryRow(ctx, `
			SELECT cache_read_input_tokens, prev_cache_read_tokens, cache_regressed
			FROM message_request WHERE id = $1`, id).Scan(&cacheRead, &previous, &regressed); err != nil {
			t.Fatalf("读回观测列失败: %v", err)
		}
		if cacheRead == nil || *cacheRead != wantRead {
			t.Fatalf("行 %d 的 cache_read = %v，want %d", id, cacheRead, wantRead)
		}
		switch {
		case wantPrevious == nil && previous != nil:
			t.Fatalf("行 %d 的 prev_cache_read_tokens 应为 NULL（无从比较），实际 %d", id, *previous)
		case wantPrevious != nil && previous == nil:
			t.Fatalf("行 %d 的 prev_cache_read_tokens 应为 %d，实际 NULL", id, *wantPrevious)
		case wantPrevious != nil && *previous != *wantPrevious:
			t.Fatalf("行 %d 的 prev_cache_read_tokens = %d，want %d", id, *previous, *wantPrevious)
		}
		switch {
		case wantRegressed == nil && regressed != nil:
			t.Fatalf("行 %d 的 cache_regressed 应为 NULL，实际 %v", id, *regressed)
		case wantRegressed != nil && regressed == nil:
			t.Fatalf("行 %d 的 cache_regressed 应为 %v，实际 NULL", id, *wantRegressed)
		case wantRegressed != nil && *regressed != *wantRegressed:
			t.Fatalf("行 %d 的 cache_regressed = %v，want %v", id, *regressed, *wantRegressed)
		}
	}

	no := false
	yes := true

	// 会话首行：同会话没有更早的行 ⇒ 两列留 NULL（「无从比较」不是「没有回退」）。
	first := newRow(true)
	settle(first, 100)
	assertRegression(first, 100, nil, nil)

	// 读数变大：不是回退，但既成事实（上一行读数）要留痕。
	second := newRow(true)
	settle(second, 200)
	assertRegression(second, 200, int64Ptr(100), &no)

	// 本章的主角：读数从 200 掉到 50。
	third := newRow(true)
	settle(third, 50)
	assertRegression(third, 50, int64Ptr(200), &yes)

	// 相等不算回退——这一条把 `<` 与 `<=` 分开。
	fourth := newRow(true)
	settle(fourth, 50)
	assertRegression(fourth, 50, int64Ptr(50), &no)

	// 软删除的上一行不算「前一行」：口径与 idx_message_request_session_id 的
	// `WHERE deleted_at IS NULL` 一致。999 这个读数专门用来看它是否真被跳过
	// （若没跳过，prev 会是 999 而不是 50）。
	removed := newRow(true)
	settle(removed, 999)
	if _, err := control.Exec(ctx,
		`UPDATE message_request SET deleted_at = now() WHERE id = $1`, removed); err != nil {
		t.Fatalf("软删除夹具行失败: %v", err)
	}
	afterRemoved := newRow(true)
	settle(afterRemoved, 300)
	assertRegression(afterRemoved, 300, int64Ptr(50), &no)

	// 本行没有 session_id ⇒ 无从比较，两列留 NULL，且不得因此报错。
	noSession := newRow(false)
	settle(noSession, 5)
	assertRegression(noSession, 5, nil, nil)

	// 反证：绕开推导、直接用同一条 SQL 形态写入时两列不会出现——证明它们确实出自推导，
	// 而不是触发器或列默认值之类的外部力量。
	raw := newRow(true)
	statusCode := 200
	rawRead := int64(1)
	query, args := BuildDetailsPatchQuery(raw, DetailsPatch{
		StatusCode:           &statusCode,
		CacheReadInputTokens: &rawRead,
	})
	if _, err := control.Exec(ctx, query, args...); err != nil {
		t.Fatalf("直写 SQL 失败: %v", err)
	}
	assertRegression(raw, 1, nil, nil)
}

func int64Ptr(value int64) *int64 { return &value }
