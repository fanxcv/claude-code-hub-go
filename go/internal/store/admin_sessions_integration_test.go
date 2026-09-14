package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 本文件是 sessions 聚合底座的**真库**验收：造多会话多请求（含 replay 行、软删行、
// 非计费端点行、被拦截行、不同 key、前缀亲和、坏形状 provider_chain），逐字段断言 Go 与
// Node 口径一致。
//
// 夹具直接 INSERT message_request，账本行由触发器 trg_upsert_usage_ledger 生成——与生产
// 同一条路径，故「哪些行进得了账本」这件事本身也被覆盖（replay 计 0 且被计费条件排除、
// count_tokens 端点被触发器删账本行、blocked_by 非空被排除）。

type adminSessionTestRequest struct {
	UserID           int64
	Key              string
	ProviderID       int64
	SessionID        string
	SessionIdentity  *string
	IdentityKind     *string
	ScopeTag         *string
	Fingerprint      *string
	FingerprintChain string
	ProviderChain    string
	Model            *string
	CostUSD          string
	InputTokens      int64
	OutputTokens     int64
	CacheCreation    int64
	CacheRead        int64
	DurationMS       int64
	CacheTTL         *string
	Endpoint         string
	StatusCode       *int
	IsReplay         bool
	BlockedBy        *string
	Deleted          bool
	CreatedAt        time.Time
	UserAgent        string
	APIType          string
}

func insertAdminSessionTestUser(t *testing.T, pool *Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO users (name, role) VALUES ($1, 'user') RETURNING id`, name,
	).Scan(&id); err != nil {
		t.Fatalf("建用户夹具失败: %v", err)
	}
	return id
}

func insertAdminSessionTestKey(t *testing.T, pool *Pool, userID int64, key, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO keys (user_id, key, name) VALUES ($1, $2, $3) RETURNING id`,
		userID, key, name,
	).Scan(&id); err != nil {
		t.Fatalf("建密钥夹具失败: %v", err)
	}
	return id
}

func insertAdminSessionTestProvider(t *testing.T, pool *Pool, name string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO providers (name, url, key) VALUES ($1, $2, $3) RETURNING id`,
		name, "https://"+name+".example.invalid", "sk-"+name,
	).Scan(&id); err != nil {
		t.Fatalf("建供应商夹具失败: %v", err)
	}
	return id
}

// insertAdminSessionTestRequest 插入一行 message_request，账本行交给触发器。
func insertAdminSessionTestRequest(t *testing.T, pool *Pool, spec adminSessionTestRequest) int64 {
	t.Helper()
	var providerChain any
	if spec.ProviderChain != "" {
		providerChain = spec.ProviderChain
	}
	var fingerprintChain any
	if spec.FingerprintChain != "" {
		fingerprintChain = spec.FingerprintChain
	}
	var deletedAt any
	if spec.Deleted {
		deletedAt = spec.CreatedAt.Add(time.Second)
	}
	endpoint := spec.Endpoint
	if endpoint == "" {
		endpoint = "/v1/messages"
	}
	// cost_usd 是 numeric 非空列：空串不是合法数值，缺省补 "0"。
	cost := spec.CostUSD
	if cost == "" {
		cost = "0"
	}
	var id int64
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO message_request (
			provider_id, user_id, key, session_id, session_identity, session_identity_kind,
			affinity_scope_tag, affinity_fingerprint, affinity_fingerprint_chain, provider_chain,
			model, cost_usd, input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			duration_ms, cache_ttl_applied, endpoint, status_code, is_replay, blocked_by,
			deleted_at, created_at, user_agent, api_type
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10::jsonb,
			$11, $12::numeric, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
			$23, $24, $25, $26
		) RETURNING id`,
		spec.ProviderID, spec.UserID, spec.Key, spec.SessionID, spec.SessionIdentity,
		spec.IdentityKind, spec.ScopeTag, spec.Fingerprint, fingerprintChain, providerChain,
		spec.Model, cost, spec.InputTokens, spec.OutputTokens,
		spec.CacheCreation, spec.CacheRead, spec.DurationMS, spec.CacheTTL, endpoint,
		spec.StatusCode, spec.IsReplay, spec.BlockedBy, deletedAt, spec.CreatedAt,
		spec.UserAgent, spec.APIType,
	).Scan(&id); err != nil {
		t.Fatalf("建请求夹具失败: %v", err)
	}
	return id
}

// adminSessionFixture 是一整套夹具与它的事后清理。
type adminSessionFixture struct {
	pools        *Pools
	pool         *Pool
	ownerID      int64
	otherID      int64
	keyAID       int64
	keyBID       int64
	keyAName     string
	keyBName     string
	namedID      int64
	namedName    string
	unnamedID    int64
	ownerName    string
	otherName    string
	userAgent    string
	apiType      string
	base         time.Time
	keysToDelete []string
}

// seedAdminSessionFixture 建两个用户、两个密钥、一个有名字的供应商，并登记清理。
func seedAdminSessionFixture(t *testing.T) adminSessionFixture {
	t.Helper()
	pools := openTestPools(t)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	marker := itKey(t)
	fixture := adminSessionFixture{
		pools:     pools,
		pool:      pool,
		ownerName: marker + "-owner",
		otherName: marker + "-other",
		keyAName:  marker + "-key-a",
		keyBName:  marker + "-key-b",
		namedName: marker + "-provider",
		userAgent: marker + "-ua",
		apiType:   "chat",
		// 时间戳固定到秒并对齐到过去一小时，避免与当前时刻的边界比较扯上关系。
		base:      time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
		unnamedID: 999_999,
	}
	fixture.ownerID = insertAdminSessionTestUser(t, pool, fixture.ownerName)
	fixture.otherID = insertAdminSessionTestUser(t, pool, fixture.otherName)
	fixture.keyAID = insertAdminSessionTestKey(t, pool, fixture.ownerID, fixture.keyAName, "主密钥")
	fixture.keyBID = insertAdminSessionTestKey(t, pool, fixture.ownerID, fixture.keyBName, "副密钥")
	fixture.namedID = insertAdminSessionTestProvider(t, pool, fixture.namedName)
	fixture.keysToDelete = []string{fixture.keyAName, fixture.keyBName}

	t.Cleanup(func() {
		ctx := context.Background()
		cleanupRequestRows(t, pools, fixture.keysToDelete)
		_, _ = pool.Exec(ctx, `DELETE FROM keys WHERE id = ANY($1)`,
			[]int64{fixture.keyAID, fixture.keyBID})
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = ANY($1)`,
			[]int64{fixture.ownerID, fixture.otherID})
		_, _ = pool.Exec(ctx, `DELETE FROM providers WHERE id = $1`, fixture.namedID)
	})
	return fixture
}

func (f adminSessionFixture) at(minutes int) time.Time {
	return f.base.Add(time.Duration(minutes) * time.Minute)
}

// seedClaudeSessionRows 造 marker-sid-a 会话：两个 key、replay 行、被拦截行、非计费端点行。
func seedAdminSessionMixedRows(t *testing.T, f adminSessionFixture) {
	t.Helper()
	model1 := "m1"
	model2 := "m2"
	model4 := "m4"
	ttl5m := "5m"
	ttl1h := "1h"
	status := 200

	// A1 在副密钥上：同一物理 session 的第二个来源。
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyBName, ProviderID: f.unnamedID,
		SessionID: "marker-sid-a", Model: &model4, CostUSD: "1.0",
		InputTokens: 1, OutputTokens: 1, DurationMS: 10, CacheTTL: &ttl5m,
		StatusCode: &status, CreatedAt: f.at(0), UserAgent: f.userAgent, APIType: f.apiType,
	})
	// A2 在主密钥上：chain 末元素给出最终供应商（有名字的那个）。
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		ProviderChain: fmt.Sprintf(`[{"id":"%d","name":"x"}]`, f.namedID),
		SessionID:     "marker-sid-a", Model: &model1, CostUSD: "1.5",
		InputTokens: 10, OutputTokens: 20, CacheCreation: 3, CacheRead: 4,
		DurationMS: 100, CacheTTL: &ttl5m, StatusCode: &status,
		CreatedAt: f.at(1), UserAgent: f.userAgent, APIType: f.apiType,
	})
	// A3：无 chain，最终供应商回退到 provider_id（库里没有这一行 → 名字走兜底文案）。
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.unnamedID,
		SessionID: "marker-sid-a", Model: &model2, CostUSD: "0.25",
		InputTokens: 5, OutputTokens: 7, DurationMS: 50, CacheTTL: &ttl1h,
		StatusCode: &status, CreatedAt: f.at(3), UserAgent: f.userAgent, APIType: f.apiType,
	})
	// A4 replay 行：既被计费条件排除，也被物理来源的回放过滤排除。
	replayModel := "m-replay"
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-a", Model: &replayModel, CostUSD: "3.0",
		InputTokens: 100, OutputTokens: 100, StatusCode: &status, IsReplay: true,
		CreatedAt: f.at(4), UserAgent: f.userAgent, APIType: f.apiType,
	})
	// A5 被拦截行（warmup）：blocked_by 非空 → 不计费。
	blockedModel := "m-blocked"
	blockedBy := "warmup"
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-a", Model: &blockedModel, CostUSD: "5.0",
		StatusCode: &status, BlockedBy: &blockedBy,
		CreatedAt: f.at(5), UserAgent: f.userAgent, APIType: f.apiType,
	})
	// A6 非计费端点：触发器会删掉它的账本行。
	countModel := "m-count"
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-a", Model: &countModel, CostUSD: "7.0",
		Endpoint: "/v1/messages/count_tokens", StatusCode: &status,
		CreatedAt: f.at(6), UserAgent: f.userAgent, APIType: f.apiType,
	})
}

// seedAdminSessionAffinityRows 造 pfx: 会话：两行，新的那行决定 scopeTag / fingerprint 取值。
func seedAdminSessionAffinityRows(t *testing.T, f adminSessionFixture) {
	t.Helper()
	identity := "pfx:marker-c"
	kind := "prefix_affinity"
	newScope := "scope-new"
	newFingerprint := "fp-new"
	oldScope := "scope-old"
	oldFingerprint := "fp-old"
	model := "m3"
	cost := "2.0"
	status := 200
	ttl := "5m"

	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-c", SessionIdentity: &identity, IdentityKind: &kind,
		ScopeTag: &oldScope, Fingerprint: &oldFingerprint,
		Model: &model, CostUSD: "0.5", CacheTTL: &ttl, StatusCode: &status,
		CreatedAt: f.at(0), UserAgent: f.userAgent, APIType: f.apiType,
	})
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-c", SessionIdentity: &identity, IdentityKind: &kind,
		ScopeTag: &newScope, Fingerprint: &newFingerprint,
		FingerprintChain: `["fp-chain"]`,
		Model:            &model, CostUSD: cost, CacheTTL: &ttl, StatusCode: &status,
		CreatedAt: f.at(2), UserAgent: f.userAgent, APIType: f.apiType,
	})
}

func findAdminSessionSummary(
	t *testing.T,
	summaries []AdminSessionSummary,
	sessionID string,
) AdminSessionSummary {
	t.Helper()
	for _, summary := range summaries {
		if summary.SessionID == sessionID {
			return summary
		}
	}
	t.Fatalf("结果里没有会话 %s（实得 %d 条）", sessionID, len(summaries))
	return AdminSessionSummary{}
}

func TestIntegrationAdminSessionAggregateStats(t *testing.T) {
	f := seedAdminSessionFixture(t)
	ctx := context.Background()
	seedAdminSessionMixedRows(t, f)
	seedAdminSessionAffinityRows(t, f)

	// S4：只有 replay 行的会话——身份归并找得到它（那份查询不过滤 replay），但账本一条都不算。
	replayOnlyModel := "m-replay-only"
	status := 200
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-r", Model: &replayOnlyModel, CostUSD: "9.0",
		StatusCode: &status, IsReplay: true, CreatedAt: f.at(7),
		UserAgent: f.userAgent, APIType: f.apiType,
	})
	// S5：只有软删行的会话——身份归并要 deleted_at IS NULL，故整个会话不可见（账本行虽在）。
	deletedModel := "m-deleted"
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-d", Model: &deletedModel, CostUSD: "4.0",
		StatusCode: &status, Deleted: true, CreatedAt: f.at(8),
		UserAgent: f.userAgent, APIType: f.apiType,
	})

	summaries, err := f.pools.AggregateAdminSessionStats(
		ctx, []string{"marker-sid-a", "marker-sid-c", "marker-sid-r"}, 0)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}

	// --- marker-sid-a：三个 key 混合，逐字段 ---
	mixed := findAdminSessionSummary(t, summaries, "marker-sid-a")
	if mixed.RequestedSessionIDs == nil || len(mixed.RequestedSessionIDs) != 1 ||
		mixed.RequestedSessionIDs[0] != "marker-sid-a" {
		t.Fatalf("requestedSessionIds 不对: %#v", mixed.RequestedSessionIDs)
	}
	// 只有 A1/A2/A3 进账本：replay、warmup、count_tokens 三条都被计费条件排除。
	if mixed.RequestCount != 3 {
		t.Fatalf("requestCount = %d, want 3（replay/warmup/count_tokens 不得计入）", mixed.RequestCount)
	}
	if mixed.TotalCostUSD != "2.750000000000000" {
		t.Fatalf("totalCostUsd = %s, want 2.750000000000000", mixed.TotalCostUSD)
	}
	if mixed.TotalInputTokens != 16 || mixed.TotalOutputTokens != 28 {
		t.Fatalf("token 合计 = %d/%d, want 16/28",
			mixed.TotalInputTokens, mixed.TotalOutputTokens)
	}
	if mixed.TotalCacheCreationTokens != 3 || mixed.TotalCacheReadTokens != 4 {
		t.Fatalf("缓存 token 合计 = %d/%d, want 3/4",
			mixed.TotalCacheCreationTokens, mixed.TotalCacheReadTokens)
	}
	if mixed.TotalDurationMS != 160 {
		t.Fatalf("totalDurationMs = %d, want 160", mixed.TotalDurationMS)
	}
	if mixed.FirstRequestAt == nil || !mixed.FirstRequestAt.Equal(f.at(0)) {
		t.Fatalf("firstRequestAt = %v, want %v", mixed.FirstRequestAt, f.at(0))
	}
	if mixed.LastRequestAt == nil || !mixed.LastRequestAt.Equal(f.at(3)) {
		t.Fatalf("lastRequestAt = %v, want %v", mixed.LastRequestAt, f.at(3))
	}
	// 供应商按「首次使用」定序：副密钥那行最早（999999 无名），随后是 chain 末元素那个有名字的。
	if len(mixed.Providers) != 2 {
		t.Fatalf("providers = %#v, want 2 项", mixed.Providers)
	}
	if mixed.Providers[0].ID != f.unnamedID || mixed.Providers[0].Name != "Provider #999999" {
		t.Fatalf("首个供应商应为无名兜底，实际 %#v", mixed.Providers[0])
	}
	if mixed.Providers[1].ID != f.namedID || mixed.Providers[1].Name != f.namedName {
		t.Fatalf("第二个供应商应是有名字那个，实际 %#v", mixed.Providers[1])
	}
	if len(mixed.Models) != 3 || mixed.Models[0] != "m4" || mixed.Models[1] != "m1" ||
		mixed.Models[2] != "m2" {
		t.Fatalf("models 应按首次使用定序为 [m4 m1 m2]，实际 %#v", mixed.Models)
	}
	if mixed.CacheTTLApplied == nil || *mixed.CacheTTLApplied != "mixed" {
		t.Fatalf("cacheTtlApplied 应为 mixed（5m 与 1h 混用），实际 %v", mixed.CacheTTLApplied)
	}
	if mixed.UserID != f.ownerID || mixed.UserName != f.ownerName {
		t.Fatalf("用户信息不对: %d %s", mixed.UserID, mixed.UserName)
	}
	// 身份归并挑的是最新那行（A6，主密钥），故 key 信息来自主密钥。
	if mixed.KeyID != f.keyAID || mixed.KeyName != "主密钥" {
		t.Fatalf("密钥信息不对: %d %s", mixed.KeyID, mixed.KeyName)
	}
	if mixed.APIType == nil || *mixed.APIType != f.apiType {
		t.Fatalf("apiType = %v, want %s", mixed.APIType, f.apiType)
	}
	if mixed.SessionIdentityKind != "session_id" {
		t.Fatalf("sessionIdentityKind = %q, want session_id", mixed.SessionIdentityKind)
	}
	if mixed.SessionFingerprint != nil {
		t.Fatalf("该会话没有亲和指纹，实际 %v", *mixed.SessionFingerprint)
	}

	// --- marker-sid-c：入参是物理 id，经 LATERAL 第二支归并到 pfx: 规范 identity ---
	affinity := findAdminSessionSummary(t, summaries, "pfx:marker-c")
	if affinity.SessionIdentityKind != "prefix_affinity" {
		t.Fatalf("sessionIdentityKind = %q, want prefix_affinity", affinity.SessionIdentityKind)
	}
	if len(affinity.RequestedSessionIDs) != 1 || affinity.RequestedSessionIDs[0] != "marker-sid-c" {
		t.Fatalf("requestedSessionIds 应为物理 id，实际 %#v", affinity.RequestedSessionIDs)
	}
	if affinity.RequestCount != 2 || affinity.TotalCostUSD != "2.500000000000000" {
		t.Fatalf("亲和会话聚合不对: %d / %s", affinity.RequestCount, affinity.TotalCostUSD)
	}
	if affinity.CacheTTLApplied == nil || *affinity.CacheTTLApplied != "5m" {
		t.Fatalf("单值 cacheTtlApplied 应为 5m，实际 %v", affinity.CacheTTLApplied)
	}

	// --- marker-sid-r：只有 replay 行 → 会话在、统计全零 ---
	replayOnly := findAdminSessionSummary(t, summaries, "marker-sid-r")
	if replayOnly.RequestCount != 0 || replayOnly.TotalCostUSD != "0" {
		t.Fatalf("replay-only 会话应全零，实际 %d / %s",
			replayOnly.RequestCount, replayOnly.TotalCostUSD)
	}
	if replayOnly.FirstRequestAt != nil || replayOnly.LastRequestAt != nil {
		t.Fatal("replay-only 会话不应有首末请求时刻")
	}
	if len(replayOnly.Providers) != 0 || len(replayOnly.Models) != 0 {
		t.Fatalf("replay-only 会话不应有供应商/模型: %#v %#v",
			replayOnly.Providers, replayOnly.Models)
	}
	if replayOnly.CacheTTLApplied != nil {
		t.Fatalf("replay-only 会话不应有 cacheTtlApplied，实际 %v", *replayOnly.CacheTTLApplied)
	}

	// --- 入参顺序与去重：pfx 与物理 id 都指同一会话 → 一条，requestedSessionIds 按入参序 ---
	merged, err := f.pools.AggregateAdminSessionStats(
		ctx, []string{"pfx:marker-c", "marker-sid-c", "pfx:marker-c"}, 0)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if len(merged) != 1 {
		t.Fatalf("两个入参指同一会话应给一条，实际 %d 条", len(merged))
	}
	if len(merged[0].RequestedSessionIDs) != 2 ||
		merged[0].RequestedSessionIDs[0] != "pfx:marker-c" ||
		merged[0].RequestedSessionIDs[1] != "marker-sid-c" {
		t.Fatalf("requestedSessionIds 应按入参序去重，实际 %#v", merged[0].RequestedSessionIDs)
	}

	// --- 软删会话不可见 ---
	softDeleted, err := f.pools.AggregateAdminSessionStats(ctx, []string{"marker-sid-d"}, 0)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if len(softDeleted) != 0 {
		t.Fatalf("只有软删行的会话不应可见，实际 %#v", softDeleted)
	}

	// --- 不存在的会话给空结果（不是报错） ---
	missing, err := f.pools.AggregateAdminSessionStats(ctx, []string{"marker-nope"}, 0)
	if err != nil {
		t.Fatalf("不存在的会话不应报错: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("不存在的会话应给空结果，实际 %d 条", len(missing))
	}

	// --- 空入参短路 ---
	empty, err := f.pools.AggregateAdminSessionStats(ctx, nil, 0)
	if err != nil {
		t.Fatalf("空入参不应报错: %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("空入参应给非 nil 空切片，实际 %#v", empty)
	}

	// --- 所有者作用域：换成另一个用户，什么都查不到 ---
	scoped, err := f.pools.AggregateAdminSessionStats(
		ctx, []string{"marker-sid-a", "marker-sid-c"}, f.otherID)
	if err != nil {
		t.Fatalf("按所有者聚合失败: %v", err)
	}
	if len(scoped) != 0 {
		t.Fatalf("非所有者不应看到任何会话，实际 %#v", scoped)
	}

	// --- 所有者作用域：带对自己的 id 时结果与管理员一致 ---
	owned, err := f.pools.AggregateAdminSessionStats(ctx, []string{"marker-sid-a"}, f.ownerID)
	if err != nil {
		t.Fatalf("按所有者聚合失败: %v", err)
	}
	if len(owned) != 1 || owned[0].RequestCount != 3 {
		t.Fatalf("带自身 id 的聚合应与管理员一致，实际 %#v", owned)
	}
}

func TestIntegrationAdminSessionResolveIdentity(t *testing.T) {
	f := seedAdminSessionFixture(t)
	ctx := context.Background()
	seedAdminSessionAffinityRows(t, f)

	// 保留前缀 identity：命中，且 scopeTag / fingerprint 取最新那行，fingerprints 按行序收集。
	resolved, found, err := f.pools.ResolveAdminSessionIdentity(ctx, "pfx:marker-c", 0)
	if err != nil {
		t.Fatalf("解析 identity 失败: %v", err)
	}
	if !found {
		t.Fatal("应命中 pfx:marker-c")
	}
	if resolved.Identity != "pfx:marker-c" {
		t.Fatalf("identity = %q", resolved.Identity)
	}
	if resolved.SourceSessionID == nil || *resolved.SourceSessionID != "marker-sid-c" {
		t.Fatalf("sourceSessionId = %v, want marker-sid-c", resolved.SourceSessionID)
	}
	if resolved.IdentityKind == nil || *resolved.IdentityKind != "prefix_affinity" {
		t.Fatalf("identityKind = %v, want prefix_affinity", resolved.IdentityKind)
	}
	if resolved.ScopeTag == nil || *resolved.ScopeTag != "scope-new" {
		t.Fatalf("scopeTag 应取最新行，实际 %v", resolved.ScopeTag)
	}
	if resolved.Fingerprint == nil || *resolved.Fingerprint != "fp-new" {
		t.Fatalf("fingerprint 应取最新行，实际 %v", resolved.Fingerprint)
	}
	// 行序为 created_at DESC：最新的 fp-new + 它的 chain，然后才是旧行的 fp-old。
	want := []string{"fp-new", "fp-chain", "fp-old"}
	if len(resolved.Fingerprints) != len(want) {
		t.Fatalf("fingerprints = %#v, want %#v", resolved.Fingerprints, want)
	}
	for index := range want {
		if resolved.Fingerprints[index] != want[index] {
			t.Fatalf("fingerprints = %#v, want %#v", resolved.Fingerprints, want)
		}
	}

	// 非保留 identity 只比规范列：物理 id 的规范 identity 是 pfx:…，故这里查不到。
	// 这与聚合（走 LATERAL 两支、物理 id 能归并）**是两件不同的事**。
	if _, found, err := f.pools.ResolveAdminSessionIdentity(ctx, "marker-sid-c", 0); err != nil {
		t.Fatalf("解析 identity 失败: %v", err)
	} else if found {
		t.Fatal("非保留 identity 不应命中规范列不等于它的会话")
	}

	// 所有者作用域：换人查不到。
	if _, found, err := f.pools.ResolveAdminSessionIdentity(ctx, "pfx:marker-c", f.otherID); err != nil {
		t.Fatalf("解析 identity 失败: %v", err)
	} else if found {
		t.Fatal("非所有者不应命中")
	}

	// 不存在与「只有软删行」都不命中。
	deletedModel := "m-deleted"
	status := 200
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-d", Model: &deletedModel, Deleted: true,
		StatusCode: &status, CreatedAt: f.at(8),
	})
	for _, identity := range []string{"marker-nope", "marker-sid-d"} {
		if _, found, err := f.pools.ResolveAdminSessionIdentity(ctx, identity, 0); err != nil {
			t.Fatalf("解析 identity 失败: %v", err)
		} else if found {
			t.Fatalf("%s 不应命中", identity)
		}
	}
}

func TestIntegrationListPhysicalSessionSources(t *testing.T) {
	f := seedAdminSessionFixture(t)
	ctx := context.Background()
	seedAdminSessionMixedRows(t, f)

	sources, err := f.pools.ListPhysicalSessionSourcesForIdentity(ctx, "marker-sid-a", 0)
	if err != nil {
		t.Fatalf("枚举物理来源失败: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("同一物理 session 落在两个 key 上应给两个来源，实际 %#v", sources)
	}
	// 定序为 (session_id, key_id)：副密钥 id 更大，故主密钥在前。
	first := sources[0]
	if first.SessionID != "marker-sid-a" || first.KeyID != f.keyAID || first.UserID != f.ownerID {
		t.Fatalf("首个来源不对: %#v", first)
	}
	// 主密钥上：A2 的 chain 末元素（有名供应商）+ A3 的 provider_id；replay 行不算。
	wantFirst := []int64{f.namedID, f.unnamedID}
	if len(first.ProviderIDs) != len(wantFirst) {
		t.Fatalf("首个来源 providerIds = %#v, want %#v", first.ProviderIDs, wantFirst)
	}
	for index := range wantFirst {
		if first.ProviderIDs[index] != wantFirst[index] {
			t.Fatalf("首个来源 providerIds = %#v, want %#v", first.ProviderIDs, wantFirst)
		}
	}
	second := sources[1]
	if second.KeyID != f.keyBID || len(second.ProviderIDs) != 1 || second.ProviderIDs[0] != f.unnamedID {
		t.Fatalf("副密钥来源不对: %#v", second)
	}

	// 所有者作用域：换人查不到。
	otherScoped, err := f.pools.ListPhysicalSessionSourcesForIdentity(ctx, "marker-sid-a", f.otherID)
	if err != nil {
		t.Fatalf("按所有者枚举失败: %v", err)
	}
	if len(otherScoped) != 0 {
		t.Fatalf("非所有者不应看到来源，实际 %#v", otherScoped)
	}

	// 只有 replay 行的会话没有物理来源。
	replayModel := "m-replay-only"
	status := 200
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-r", Model: &replayModel, StatusCode: &status, IsReplay: true,
		CreatedAt: f.at(7),
	})
	replayOnly, err := f.pools.ListPhysicalSessionSourcesForIdentity(ctx, "marker-sid-r", 0)
	if err != nil {
		t.Fatalf("枚举物理来源失败: %v", err)
	}
	if len(replayOnly) != 0 {
		t.Fatalf("replay 行不算物理来源，实际 %#v", replayOnly)
	}

	// 换绑：旧绑定的行不再被枚举——最新行的规范 identity 变了，旧行就对不上了。
	// 用一行「更晚的、换了 identity 的」同 (session_id, user_id, key) 行来触发。
	replacement := "marker-sid-rebind"
	sessionIDKind := "session_id"
	insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
		UserID: f.ownerID, Key: f.keyAName, ProviderID: f.namedID,
		SessionID: "marker-sid-a", SessionIdentity: &replacement,
		IdentityKind: &sessionIDKind, StatusCode: &status, CreatedAt: f.at(20),
	})
	rebound, err := f.pools.ListPhysicalSessionSourcesForIdentity(ctx, "marker-sid-a", 0)
	if err != nil {
		t.Fatalf("枚举物理来源失败: %v", err)
	}
	// 主密钥那一支现在归到 marker-sid-rebind，故只剩副密钥那一支。
	if len(rebound) != 1 || rebound[0].KeyID != f.keyBID {
		t.Fatalf("换绑后只剩副密钥来源，实际 %#v", rebound)
	}
}

// TestIntegrationListPhysicalSessionSourcesProviderChainShapes 覆盖 provider_chain 末元素的
// 形状判定：非数组 / 空数组 / 末元素缺 id / 非数字 id 一律回退到 provider_id；合法才取用。
func TestIntegrationListPhysicalSessionSourcesProviderChainShapes(t *testing.T) {
	f := seedAdminSessionFixture(t)
	ctx := context.Background()
	status := 200

	shapes := []string{
		"",
		`{"a":1}`,
		`[]`,
		`[{"name":"x"}]`,
		`[{"id":"42"}]`,
		`[{"id":"7"},{"id":"42"}]`,
	}
	for index, shape := range shapes {
		insertAdminSessionTestRequest(t, f.pool, adminSessionTestRequest{
			UserID: f.ownerID, Key: f.keyAName, ProviderID: int64(1000 + index + 1),
			SessionID: "marker-sid-chain", ProviderChain: shape, StatusCode: &status,
			CreatedAt: f.at(index),
		})
	}

	sources, err := f.pools.ListPhysicalSessionSourcesForIdentity(ctx, "marker-sid-chain", 0)
	if err != nil {
		t.Fatalf("枚举物理来源失败: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("应只有一个来源，实际 %#v", sources)
	}
	// 前五行的 chain 都不算数 → 各自回退到 provider_id；最后一行取 chain 末元素 42。
	want := []int64{1001, 1002, 1003, 1004, 1005, 42, 1006}
	got := sources[0].ProviderIDs
	if len(got) != len(want) {
		t.Fatalf("providerIds = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("providerIds = %#v, want %#v", got, want)
		}
	}
}
