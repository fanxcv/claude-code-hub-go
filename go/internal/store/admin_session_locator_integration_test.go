package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// 本文件是**请求定位器与来源链**的真库验收。
//
// 为什么非真库不可：这两条查询的语义全在 SQL 里——两套 lookup 条件的差别（裸物理列 vs
// COALESCE、是否 OR 物理 session_id）、key epoch 对齐、时间边界的 (created_at, id) 字典序、
// `@> '{"reason":"initial_selection"}'` 的 jsonb 包含判定。桩件只能证明「调了哪条查询」。
//
// 夹具复用 admin_sessions_integration_test.go 的同一套（直插 message_request，账本行由触发器生成）。

// initialSelectionChain 造一条带 initial_selection 的理由的 provider_chain（来源链的判据）。
func initialSelectionChain(providerID int64, name string) string {
	return fmt.Sprintf(`[{"reason":"initial_selection","id":%d,"name":%q}]`, providerID, name)
}

// setAdminSessionRequestSequence 覆写一行的 request_sequence。
//
// 夹具的插入语句不显式给这一列（默认 1），而选择器测试要按序号区分行，故单独覆写。
func setAdminSessionRequestSequence(t *testing.T, f adminSessionFixture, id, sequence int64) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(),
		`UPDATE message_request SET request_sequence = $1 WHERE id = $2`, sequence, id,
	); err != nil {
		t.Fatalf("覆写请求序号失败: %v", err)
	}
}

// TestIntegrationFindAdminSessionRequestLocator 逐条钉住定位器的选择器语义。
func TestIntegrationFindAdminSessionRequestLocator(t *testing.T) {
	fixture := seedAdminSessionFixture(t)
	other := "other-model"
	status := 200

	// 三行同会话同序号（默认 1）、递增时间：定位器应取时间最新的那一行。
	firstID := insertAdminSessionTestRequest(t, fixture.pool, adminSessionTestRequest{
		UserID: fixture.ownerID, Key: fixture.keyAName, ProviderID: fixture.namedID,
		SessionID: "marker-sid-loc", Model: &other, StatusCode: &status,
		CreatedAt: fixture.at(0), UserAgent: fixture.userAgent, APIType: fixture.apiType,
	})
	secondID := insertAdminSessionTestRequest(t, fixture.pool, adminSessionTestRequest{
		UserID: fixture.ownerID, Key: fixture.keyAName, ProviderID: fixture.namedID,
		SessionID: "marker-sid-loc", Model: &other, StatusCode: &status,
		CreatedAt: fixture.at(1), UserAgent: fixture.userAgent, APIType: fixture.apiType,
	})
	setAdminSessionRequestSequence(t, fixture, secondID, 2)
	thirdID := insertAdminSessionTestRequest(t, fixture.pool, adminSessionTestRequest{
		UserID: fixture.ownerID, Key: fixture.keyAName, ProviderID: fixture.namedID,
		SessionID: "marker-sid-loc", Model: &other, StatusCode: &status,
		CreatedAt: fixture.at(2), UserAgent: fixture.userAgent, APIType: fixture.apiType,
	})
	setAdminSessionRequestSequence(t, fixture, thirdID, 3)

	ctx := context.Background()

	// 1. 不带选择器：取最新一行（created_at DESC NULLS LAST, id DESC）。
	locator, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "marker-sid-loc",
		AdminSessionRequestLocatorSelector{}, fixture.ownerID)
	if err != nil {
		t.Fatalf("定位失败: %v", err)
	}
	if locator == nil {
		t.Fatal("不带选择器应定位到最新一行")
	}
	if locator.RequestID != thirdID || locator.RequestSequence != 3 {
		t.Fatalf("应定位到 id=%d seq=3，实得 id=%d seq=%d", thirdID, locator.RequestID, locator.RequestSequence)
	}
	if locator.CanonicalSessionID != "marker-sid-loc" || locator.SourceSessionID != "marker-sid-loc" {
		t.Fatalf("identity 与物理来源应同为 marker-sid-loc，实得 %q / %q",
			locator.CanonicalSessionID, locator.SourceSessionID)
	}
	if locator.KeyID != fixture.keyAID || locator.UserID != fixture.ownerID {
		t.Fatalf("keyId/userId 应为 %d/%d，实得 %d/%d",
			fixture.keyAID, fixture.ownerID, locator.KeyID, locator.UserID)
	}
	if locator.IdentityKind != "session_id" {
		t.Fatalf("identityKind 应为 session_id（NULL 归并），实得 %q", locator.IdentityKind)
	}

	// 2. 按序号指定：走物理 lookup（非保留前缀会 OR 物理 session_id）。
	bySequence, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "marker-sid-loc",
		AdminSessionRequestLocatorSelector{RequestSequence: 1}, fixture.ownerID)
	if err != nil {
		t.Fatalf("按序号定位失败: %v", err)
	}
	if bySequence == nil || bySequence.RequestID != firstID {
		t.Fatalf("按序号 1 应定位到 id=%d，实得 %v", firstID, bySequence)
	}

	// 3. 按 requestId 指定：精确命中（带 requestId 时用物理 lookup，且序号条件可省）。
	byRequest, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "marker-sid-loc",
		AdminSessionRequestLocatorSelector{RequestID: secondID}, fixture.ownerID)
	if err != nil {
		t.Fatalf("按 requestId 定位失败: %v", err)
	}
	if byRequest == nil || byRequest.RequestID != secondID || byRequest.RequestSequence != 2 {
		t.Fatalf("按 requestId 应命中 id=%d seq=2，实得 %v", secondID, byRequest)
	}

	// 4. 序号与物理来源组合：命中，且来源不匹配时落空。
	combo, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "marker-sid-loc",
		AdminSessionRequestLocatorSelector{RequestSequence: 2, SourceSessionID: "marker-sid-loc"},
		fixture.ownerID)
	if err != nil {
		t.Fatalf("组合定位失败: %v", err)
	}
	if combo == nil || combo.RequestID != secondID {
		t.Fatalf("组合选择器应命中 id=%d，实得 %v", secondID, combo)
	}
	mismatch, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "marker-sid-loc",
		AdminSessionRequestLocatorSelector{SourceSessionID: "marker-sid-does-not-exist"},
		fixture.ownerID)
	if err != nil {
		t.Fatalf("来源不匹配定位失败: %v", err)
	}
	if mismatch != nil {
		t.Fatalf("物理来源不匹配应落空，实得 %v", mismatch)
	}

	// 5. 所有者不匹配：落空（这正是「别人的会话答 404」的依据）。
	foreign, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "marker-sid-loc",
		AdminSessionRequestLocatorSelector{}, fixture.otherID)
	if err != nil {
		t.Fatalf("他人定位失败: %v", err)
	}
	if foreign != nil {
		t.Fatalf("他人不应定位到本会话，实得 %v", foreign)
	}

	// 6. 软删行不参与定位。
	deletedID := insertAdminSessionTestRequest(t, fixture.pool, adminSessionTestRequest{
		UserID: fixture.ownerID, Key: fixture.keyAName, ProviderID: fixture.namedID,
		SessionID: "marker-sid-loc", Model: &other, StatusCode: &status,
		CreatedAt: fixture.at(4), UserAgent: fixture.userAgent, APIType: fixture.apiType,
		Deleted: true,
	})
	_ = deletedID
	afterDelete, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "marker-sid-loc",
		AdminSessionRequestLocatorSelector{}, fixture.ownerID)
	if err != nil {
		t.Fatalf("软删后定位失败: %v", err)
	}
	if afterDelete == nil || afterDelete.RequestID != thirdID {
		t.Fatalf("软删行不应被定位，期望 id=%d，实得 %v", thirdID, afterDelete)
	}

	// 7. 前缀亲和：identityKind 取自列，scopeTag / fingerprint 一并带出。
	kind := "prefix_affinity"
	scope := "scope-loc"
	fingerprint := "fp-loc"
	// 前缀亲和的规范 identity 走另一套列：session_identity = pfx:...，物理 session_id 另取。
	affinityID := insertAdminSessionTestRequest(t, fixture.pool, adminSessionTestRequest{
		UserID: fixture.ownerID, Key: fixture.keyAName, ProviderID: fixture.namedID,
		SessionID: "marker-sid-loc-physical", SessionIdentity: ptr("pfx:marker-loc"),
		IdentityKind: &kind, ScopeTag: &scope, Fingerprint: &fingerprint,
		Model: &other, StatusCode: &status,
		CreatedAt: fixture.at(5), UserAgent: fixture.userAgent, APIType: fixture.apiType,
	})
	affinity, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "pfx:marker-loc",
		AdminSessionRequestLocatorSelector{}, fixture.ownerID)
	if err != nil {
		t.Fatalf("前缀亲和定位失败: %v", err)
	}
	if affinity == nil {
		t.Fatal("前缀亲和应定位到行")
	}
	if affinity.RequestID != affinityID || affinity.IdentityKind != "prefix_affinity" {
		t.Fatalf("前缀亲和应命中 id=%d 且 identityKind=prefix_affinity，实得 %v", affinityID, affinity)
	}
	if affinity.SourceSessionID != "marker-sid-loc-physical" {
		t.Fatalf("物理来源应为 marker-sid-loc-physical，实得 %q", affinity.SourceSessionID)
	}
	if affinity.ScopeTag == nil || *affinity.ScopeTag != scope ||
		affinity.Fingerprint == nil || *affinity.Fingerprint != fingerprint {
		t.Fatalf("scopeTag/fingerprint 应为 %q/%q，实得 %v/%v",
			scope, fingerprint, affinity.ScopeTag, affinity.Fingerprint)
	}

	// 8. 物理 id 归并：用物理 session_id 也能定位到前缀亲和行（messageSessionLookup 的 OR 支）。
	//    这条只在带 requestId 或非保留前缀时才走 OR，故用 requestId 触发。
	byPhysicalID, err := fixture.pools.FindAdminSessionRequestLocator(ctx, "marker-sid-loc-physical",
		AdminSessionRequestLocatorSelector{RequestID: affinityID}, fixture.ownerID)
	if err != nil {
		t.Fatalf("物理 id 归并定位失败: %v", err)
	}
	if byPhysicalID == nil || byPhysicalID.RequestID != affinityID {
		t.Fatalf("物理 id 应能归并到前缀亲和行，实得 %v", byPhysicalID)
	}
}

// TestIntegrationFindAdminSessionOriginChain 钉住来源链的三条判据。
func TestIntegrationFindAdminSessionOriginChain(t *testing.T) {
	fixture := seedAdminSessionFixture(t)
	model := "m-origin"
	status := 200

	// R1：带初始选链的起点行（副密钥上）。
	startID := insertAdminSessionTestRequest(t, fixture.pool, adminSessionTestRequest{
		UserID: fixture.ownerID, Key: fixture.keyBName, ProviderID: fixture.namedID,
		SessionID:     "marker-sid-origin",
		ProviderChain: initialSelectionChain(fixture.namedID, "start"),
		Model:         &model, StatusCode: &status,
		CreatedAt: fixture.at(0), UserAgent: fixture.userAgent, APIType: fixture.apiType,
	})
	// R2：中途换供应商（链里没有 initial_selection）——不该被当作来源。
	insertAdminSessionTestRequest(t, fixture.pool, adminSessionTestRequest{
		UserID: fixture.ownerID, Key: fixture.keyBName, ProviderID: fixture.namedID,
		SessionID:     "marker-sid-origin",
		ProviderChain: `[{"reason":"hedge_winner","id":1}]`,
		Model:         &model, StatusCode: &status,
		CreatedAt: fixture.at(1), UserAgent: fixture.userAgent, APIType: fixture.apiType,
	})
	// R3：选中行（无链）。
	selectedID := insertAdminSessionTestRequest(t, fixture.pool, adminSessionTestRequest{
		UserID: fixture.ownerID, Key: fixture.keyBName, ProviderID: fixture.namedID,
		SessionID: "marker-sid-origin", Model: &model, StatusCode: &status,
		CreatedAt: fixture.at(2), UserAgent: fixture.userAgent, APIType: fixture.apiType,
	})

	ctx := context.Background()

	// 1. 命中起点行：选中行 R3 的边界内，最近一条带初始选链的行是 R1。
	chain, err := fixture.pools.FindAdminSessionOriginChain(ctx, selectedID, fixture.keyBID, fixture.ownerID)
	if err != nil {
		t.Fatalf("查询来源链失败: %v", err)
	}
	if chain == nil {
		t.Fatal("应查到来源链")
	}
	var decoded []map[string]any
	if err := json.Unmarshal(chain, &decoded); err != nil {
		t.Fatalf("来源链不是合法 JSON 数组: %v（原文 %s）", err, chain)
	}
	if len(decoded) != 1 || decoded[0]["reason"] != "initial_selection" {
		t.Fatalf("来源链应是起点行的链，实得 %s", chain)
	}

	// 2. 时间边界：拿起点行自己当选中行，返回它自己（lte id 含自身）。
	self, err := fixture.pools.FindAdminSessionOriginChain(ctx, startID, fixture.keyBID, fixture.ownerID)
	if err != nil {
		t.Fatalf("查询自链失败: %v", err)
	}
	if self == nil {
		t.Fatal("选中行本身带初始选链时应返回它自己")
	}

	// 3. key epoch：用主密钥的 keyId 查（起点行在副密钥上）→ 落空。
	wrongEpoch, err := fixture.pools.FindAdminSessionOriginChain(ctx, selectedID, fixture.keyAID, fixture.ownerID)
	if err != nil {
		t.Fatalf("查询错 epoch 失败: %v", err)
	}
	if wrongEpoch != nil {
		t.Fatalf("错 key epoch 应落空，实得 %s", wrongEpoch)
	}

	// 4. 所有者：他人查不到。
	foreign, err := fixture.pools.FindAdminSessionOriginChain(ctx, selectedID, fixture.keyBID, fixture.otherID)
	if err != nil {
		t.Fatalf("他人查询失败: %v", err)
	}
	if foreign != nil {
		t.Fatalf("他人应查不到来源链，实得 %s", foreign)
	}

	// 5. warmup 排除：把起点行标成 warmup 后落空（R1 是唯一的初始选链行）。
	if _, err := fixture.pool.Exec(ctx,
		`UPDATE message_request SET blocked_by = 'warmup' WHERE id = $1`, startID,
	); err != nil {
		t.Fatalf("标记 warmup 失败: %v", err)
	}
	warmup, err := fixture.pools.FindAdminSessionOriginChain(ctx, selectedID, fixture.keyBID, fixture.ownerID)
	if err != nil {
		t.Fatalf("warmup 查询失败: %v", err)
	}
	if warmup != nil {
		t.Fatalf("warmup 行不应作为来源链，实得 %s", warmup)
	}

	// 6. 软删排除：起点行软删后同样落空。
	if _, err := fixture.pool.Exec(ctx, `UPDATE message_request
		SET blocked_by = NULL, deleted_at = $1 WHERE id = $2`,
		fixture.at(10), startID,
	); err != nil {
		t.Fatalf("软删起点行失败: %v", err)
	}
	deleted, err := fixture.pools.FindAdminSessionOriginChain(ctx, selectedID, fixture.keyBID, fixture.ownerID)
	if err != nil {
		t.Fatalf("软删查询失败: %v", err)
	}
	if deleted != nil {
		t.Fatalf("软删行不应作为来源链，实得 %s", deleted)
	}
}

// ptr 取字符串指针（本文件的多处可空列夹具用）。
func ptr(value string) *string {
	return &value
}
