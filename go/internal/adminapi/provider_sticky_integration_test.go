package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**粘性会话终止**在管理面写路径上的端到端用例：真路由表 + 真库 + 真 Redis
// （`CCH_TEST_DSN` / `CCH_TEST_REDIS_URL` 未设置时跳过）。
//
// 为什么要真 Redis：这一批的要点不是「调了哪个方法」，而是**写完库之后亲和到底还在不在**
// ——范围（只动该厂该类型下已启用的供应商）、时机（写成功后）、触发条件（哪些字段算数）
// 三者错一个，后果就是「管理员改了配置，老会话继续落旧配置直到 TTL 到期」这种静默错。
// 桩只能证明调用发生过，证明不了亲和真的清掉了。
//
// 每条用例都带一个**对照组**（另一个 provider_type 下的供应商会话）：只断言「目标的亲和没了」
// 会在「把所有会话都终止」这种过度杀伤实现下同样通过。

// stickySessionsBinder 建真绑定门面（供 Deps.StickySessions 注入）。
func stickySessionsBinder(t *testing.T, client *redis.Client) StickySessionTerminator {
	t.Helper()
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("加载脚本注册表失败: %v", err)
	}
	scriptClient, err := ratelimit.New(client, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	return session.NewBinder(scriptClient)
}

// stickySessionSeed 是一个「已建立亲和」的会话现场。
type stickySessionSeed struct {
	sessionID  string
	keyID      int64
	userID     int64
	providerID int64
}

// seedStickySession 在真 Redis 上造出与数据面同形的会话现场：legacy 镜像（provider/key）、
// info（userId）、以及供应商维度的活跃索引 —— 终止路径正是靠这个索引发现候选会话。
func seedStickySession(
	t *testing.T,
	client *redis.Client,
	suffix string,
	providerID int64,
	keyID int64,
	userID int64,
) stickySessionSeed {
	t.Helper()
	ctx := context.Background()
	seed := stickySessionSeed{
		sessionID:  fmt.Sprintf("sess_goit_%s_%d", suffix, time.Now().UnixNano()),
		keyID:      keyID,
		userID:     userID,
		providerID: providerID,
	}
	pipe := client.Pipeline()
	pipe.Set(ctx, session.LegacyProviderKey(seed.sessionID), fmt.Sprintf("%d", providerID), 0)
	pipe.Set(ctx, session.LegacyOwnerKey(seed.sessionID), fmt.Sprintf("%d", keyID), 0)
	pipe.HSet(ctx, session.InfoKey(seed.sessionID), "userId", fmt.Sprintf("%d", userID))
	pipe.ZAdd(ctx, session.ProviderActiveSessionsKey(providerID),
		redis.Z{Score: 1, Member: seed.sessionID})
	pipe.HSet(ctx, session.ProviderActiveSessionRefsKey(providerID), seed.sessionID, "1")
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("造粘性会话现场失败: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = client.Del(cleanupCtx,
			session.LegacyProviderKey(seed.sessionID),
			session.LegacyOwnerKey(seed.sessionID),
			session.InfoKey(seed.sessionID),
			session.BuildBindingKeys(seed.sessionID, seed.keyID).Canonical,
		).Err()
		_ = client.ZRem(cleanupCtx, session.ProviderActiveSessionsKey(seed.providerID),
			seed.sessionID).Err()
		_ = client.HDel(cleanupCtx, session.ProviderActiveSessionRefsKey(seed.providerID),
			seed.sessionID).Err()
	})
	return seed
}

// assertStickyAffinityCleared 断言「旧亲和不再命中」：legacy provider 镜像被清、
// 供应商活跃索引与引用哈希都不再含该会话。这是数据面选路读到的那个「无绑定」状态。
func assertStickyAffinityCleared(t *testing.T, client *redis.Client, seed stickySessionSeed) {
	t.Helper()
	ctx := context.Background()
	if err := client.Get(ctx, session.LegacyProviderKey(seed.sessionID)).Err(); err != redis.Nil {
		t.Fatalf("会话 %s 的 legacy provider 镜像仍在（err=%v）", seed.sessionID, err)
	}
	if score, err := client.ZScore(ctx, session.ProviderActiveSessionsKey(seed.providerID),
		seed.sessionID).Result(); err != redis.Nil {
		t.Fatalf("会话 %s 仍在供应商 %d 的活跃索引里（score=%v err=%v）",
			seed.sessionID, seed.providerID, score, err)
	}
	if _, err := client.HGet(ctx, session.ProviderActiveSessionRefsKey(seed.providerID),
		seed.sessionID).Result(); err != redis.Nil {
		t.Fatalf("会话 %s 仍在供应商 %d 的引用哈希里（err=%v）",
			seed.sessionID, seed.providerID, err)
	}
}

// assertStickyAffinityIntact 断言对照组会话完全没被动过。
func assertStickyAffinityIntact(t *testing.T, client *redis.Client, seed stickySessionSeed) {
	t.Helper()
	ctx := context.Background()
	if err := client.Get(ctx, session.LegacyProviderKey(seed.sessionID)).Err(); err != nil {
		t.Fatalf("对照组会话 %s 的亲和不该被清: %v", seed.sessionID, err)
	}
	if score, err := client.ZScore(ctx, session.ProviderActiveSessionsKey(seed.providerID),
		seed.sessionID).Result(); err != nil || score == 0 {
		t.Fatalf("对照组会话 %s 仍应在自己的供应商索引里（score=%v err=%v）",
			seed.sessionID, score, err)
	}
}

// stickyFixture 是一次用例的现场：一个厂（两种类型的端点）+ 两种类型的启用供应商
// + 各自一个带亲和的会话。
type stickyFixture struct {
	endpoints *endpointWriteFixture
	prefix    string
	vendorID  int64
	claudeID  int64
	codexID   int64
	claudeSes stickySessionSeed
	codexSes  stickySessionSeed
}

// endpointIDOfType 读该厂某类型的端点 id（转发到端点夹具的读法，避免用例里到处拆结构）。
func (f *stickyFixture) endpointIDOfType(t *testing.T, providerType string) int64 {
	t.Helper()
	return endpointIDOf(t, f.endpoints, providerType)
}

// seedStickyFixture 复用端点写路径的厂/端点夹具，再补两种类型的启用供应商与两个亲和会话。
//
// 对照组（codex）刻意与目标（claude）**同厂不同类**：端点写路径的范围是
// 「该厂该类型下已启用的供应商」，只有这样一个对照组才能把「按 vendor+type 收窄」与
// 「把所有会话都终止」区分开。
func seedStickyFixture(t *testing.T, pools *store.Pools, client *redis.Client) *stickyFixture {
	t.Helper()
	endpoints := seedEndpointWriteFixture(t, pools)
	ctx := context.Background()

	fixture := &stickyFixture{endpoints: endpoints, prefix: endpoints.prefix, vendorID: endpoints.vendorID}
	for index, providerType := range []string{"claude", "codex"} {
		var providerID int64
		if err := endpoints.pool.QueryRow(ctx,
			`INSERT INTO providers (name, url, key, provider_vendor_id, provider_type, is_enabled)
			 VALUES ($1, $2, $3, $4, $5, true) RETURNING id`,
			fmt.Sprintf("%s-%s", endpoints.prefix, providerType),
			"https://"+endpoints.domain+"/"+providerType+"-direct",
			"sk-"+endpoints.prefix+"-"+providerType,
			endpoints.vendorID, providerType,
		).Scan(&providerID); err != nil {
			t.Fatalf("建供应商夹具失败: %v", err)
		}
		if providerType == "claude" {
			fixture.claudeID = providerID
		} else {
			fixture.codexID = providerID
		}
		// 亲和存在性由 Redis 决定，与库无关；key/user 维度只作为现场的一部分。
		seed := seedStickySession(t, client, fmt.Sprintf("%d-%d", providerID, index),
			providerID, int64(900000+index), int64(700+index))
		if providerType == "claude" {
			fixture.claudeSes = seed
		} else {
			fixture.codexSes = seed
		}
	}
	return fixture
}

// stickyTestRouter 造真路由表：providers 读写 + provider-endpoints 写路径，注入真绑定门面。
func stickyTestRouter(t *testing.T, pools *store.Pools, client *redis.Client) *Router {
	t.Helper()
	deps := Deps{
		Logger:         logx.New(nil),
		Guard:          principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Problems:       NewProblems(nil),
		Store:          pools,
		ProviderUndoKV: NewRedisProviderUndoKV(client),
		StickySessions: stickySessionsBinder(t, client),
	}
	router := New(Options{Deps: deps})
	RegisterProviders(router, deps)
	RegisterProvidersWrite(router, deps)
	RegisterProviderEndpointRoutes(router, deps)
	return router
}

// TestProviderStickySessionsNeedInvalidation 钉住触发字段集：
// Node 的 STICKY_SESSION_INVALIDATING_PROVIDER_KEYS 有 11 个键（providers.ts:218-230），
// 少一个就有一类配置改动不会让旧亲和失效（静默落旧配置）。
func TestProviderStickySessionsNeedInvalidation(t *testing.T) {
	sticky := []string{
		"url", "websiteUrl", "providerType", "groupTag", "isEnabled",
		"allowedModels", "allowedClients", "blockedClients", "modelRedirects",
		"activeTimeStart", "activeTimeEnd",
	}
	for _, key := range sticky {
		if !providerStickySessionsNeedInvalidation(map[string]any{key: "x"}) {
			t.Fatalf("%s 应触发粘性会话失效", key)
		}
	}
	nonSticky := []string{"name", "weight", "priority", "costMultiplier", "limit5hUsd", "description"}
	for _, key := range nonSticky {
		if providerStickySessionsNeedInvalidation(map[string]any{key: "x"}) {
			t.Fatalf("%s 不该触发粘性会话失效（不动选路）", key)
		}
	}
	if providerStickySessionsNeedInvalidation(nil) {
		t.Fatal("空前像不该触发粘性会话失效")
	}
}

// TestProviderStickySessionsUnwiredStillSucceeds 钉住 fail-open：没装配终止面时
// 写操作**照常成功**（Node 内部 try/catch 只记 warn），并留下可检索的日志。
func TestProviderStickySessionsUnwiredStillSucceeds(t *testing.T) {
	pools := endpointWritePools(t)
	endpoints := seedEndpointWriteFixture(t, pools)
	ctx := context.Background()

	var providerID int64
	if err := endpoints.pool.QueryRow(ctx,
		`INSERT INTO providers (name, url, key, provider_vendor_id, provider_type, is_enabled)
		 VALUES ($1, $2, $3, $4, 'claude', true) RETURNING id`,
		endpoints.prefix+"-unwired", "https://"+endpoints.domain+"/claude-direct",
		"sk-"+endpoints.prefix+"-unwired", endpoints.vendorID,
	).Scan(&providerID); err != nil {
		t.Fatalf("建供应商夹具失败: %v", err)
	}

	var logs strings.Builder
	deps := Deps{
		Logger:         logx.New(&logs),
		Guard:          principalGuard{principal: Principal{UserID: 1, IsAdmin: true}},
		Problems:       NewProblems(nil),
		Store:          pools,
		ProviderUndoKV: NewRedisProviderUndoKV(providerWriteRedis(t)),
	}
	router := New(Options{Deps: deps})
	RegisterProviders(router, deps)
	RegisterProvidersWrite(router, deps)

	status, body := call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", providerID), `{"is_enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("未装配终止面时写操作仍应 200，实得 %d：%s", status, body)
	}
	if !strings.Contains(logs.String(), "editProvider:terminate_provider_sessions_skipped") {
		t.Fatalf("应留下跳过终止的 warn 日志：%s", logs.String())
	}
}

// TestProviderEditStickySessionsOnRealRedis 覆盖单家编辑的三条：命中粘性字段要终止、
// 不命中（只改权重）不终止、对照组不受牵连。
func TestProviderEditStickySessionsOnRealRedis(t *testing.T) {
	pools := endpointWritePools(t)
	client := providerWriteRedis(t)
	router := stickyTestRouter(t, pools, client)
	fixture := seedStickyFixture(t, pools, client)

	// ① 只改 weight：不动选路，亲密和必须留着。
	status, body := call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", fixture.claudeID), `{"weight": 5}`)
	if status != http.StatusOK {
		t.Fatalf("改权重应 200，实得 %d：%s", status, body)
	}
	assertStickyAffinityIntact(t, client, fixture.claudeSes)

	// ② 改 is_enabled（命中粘性字段集）：亲和必须清掉，对照组不动。
	status, body = call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", fixture.claudeID), `{"is_enabled": false}`)
	if status != http.StatusOK {
		t.Fatalf("改启用态应 200，实得 %d：%s", status, body)
	}
	assertStickyAffinityCleared(t, client, fixture.claudeSes)
	assertStickyAffinityIntact(t, client, fixture.codexSes)
}

// TestProviderEditTerminatesStickySessionsOnURLChange 覆盖最要紧的那条触发：换 URL
// （数据面的选路输入）必须让旧亲和失效。
func TestProviderEditTerminatesStickySessionsOnURLChange(t *testing.T) {
	pools := endpointWritePools(t)
	client := providerWriteRedis(t)
	router := stickyTestRouter(t, pools, client)
	fixture := seedStickyFixture(t, pools, client)

	newURL := fmt.Sprintf("https://%s-v2.example.com/claude", fixture.prefix)
	status, body := call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", fixture.claudeID),
		fmt.Sprintf(`{"url":%q}`, newURL))
	if status != http.StatusOK {
		t.Fatalf("换 URL 应 200，实得 %d：%s", status, body)
	}
	assertStickyAffinityCleared(t, client, fixture.claudeSes)
	assertStickyAffinityIntact(t, client, fixture.codexSes)
}

// TestProviderDeleteTerminatesStickySessions 覆盖删除：删掉的供应商名下不该还有在跑的会话
// （Node removeProvider:1036 无条件终止）。
func TestProviderDeleteTerminatesStickySessions(t *testing.T) {
	pools := endpointWritePools(t)
	client := providerWriteRedis(t)
	router := stickyTestRouter(t, pools, client)
	fixture := seedStickyFixture(t, pools, client)

	status, body := call(router, http.MethodDelete,
		fmt.Sprintf("/api/v1/providers/%d", fixture.claudeID), "")
	if status != http.StatusNoContent {
		t.Fatalf("删供应商应 204，实得 %d：%s", status, body)
	}
	assertStickyAffinityCleared(t, client, fixture.claudeSes)
	assertStickyAffinityIntact(t, client, fixture.codexSes)
}

// TestProviderBatchUpdateStickySessionsOnRealRedis 覆盖批量：五个选路字段命中即终止，
// 其余字段（权重）不终止——批量改权重时把在跑的会话全踢掉是明确的过度杀伤。
func TestProviderBatchUpdateStickySessionsOnRealRedis(t *testing.T) {
	pools := endpointWritePools(t)
	client := providerWriteRedis(t)
	router := stickyTestRouter(t, pools, client)
	fixture := seedStickyFixture(t, pools, client)

	status, body := call(router, http.MethodPost, "/api/v1/providers:batchUpdate",
		fmt.Sprintf(`{"providerIds":[%d],"updates":{"weight":3}}`, fixture.claudeID))
	if status != http.StatusOK {
		t.Fatalf("批量改权重应 200，实得 %d：%s", status, body)
	}
	assertStickyAffinityIntact(t, client, fixture.claudeSes)

	status, body = call(router, http.MethodPost, "/api/v1/providers:batchUpdate",
		fmt.Sprintf(`{"providerIds":[%d],"updates":{"group_tag":"it-group"}}`, fixture.claudeID))
	if status != http.StatusOK {
		t.Fatalf("批量改分组应 200，实得 %d：%s", status, body)
	}
	assertStickyAffinityCleared(t, client, fixture.claudeSes)
	assertStickyAffinityIntact(t, client, fixture.codexSes)
}

// TestProviderBatchDeleteKeepsStickySessions 钉住 Node 的**不对称**：batchDeleteProviders
// （providers.ts:2951-3013）删完不终止粘性会话，而单个 removeProvider 会。
// 照抄这条不是认同它，而是「完全替代」的定义就是行为一致；将来要改，得两侧一起改。
func TestProviderBatchDeleteKeepsStickySessions(t *testing.T) {
	pools := endpointWritePools(t)
	client := providerWriteRedis(t)
	router := stickyTestRouter(t, pools, client)
	fixture := seedStickyFixture(t, pools, client)

	status, body := call(router, http.MethodPost, "/api/v1/providers:batchDelete",
		fmt.Sprintf(`{"providerIds":[%d]}`, fixture.claudeID))
	if status != http.StatusOK {
		t.Fatalf("批量删除应 200，实得 %d：%s", status, body)
	}
	assertStickyAffinityIntact(t, client, fixture.claudeSes)
}

// TestEndpointEditStickySessionsOnRealRedis 覆盖端点编辑的三条：改 URL/排序/启用态要终止，
// 只改标签不终止；范围必须收窄到该厂该类型。
func TestEndpointEditStickySessionsOnRealRedis(t *testing.T) {
	pools := endpointWritePools(t)
	client := providerWriteRedis(t)
	router := stickyTestRouter(t, pools, client)
	fixture := seedStickyFixture(t, pools, client)
	endpointID := fixture.endpointIDOfType(t, "claude")

	// ① 只改标签：Node 的触发条件看「给没给 url/sortOrder/isEnabled」，标签不在其中。
	status, body := call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/provider-endpoints/%d", endpointID), `{"label":"新标签"}`)
	if status != http.StatusOK {
		t.Fatalf("改标签应 200，实得 %d：%s", status, body)
	}
	assertStickyAffinityIntact(t, client, fixture.claudeSes)

	// ② 改排序（命中触发条件）：与该厂该类型下启用供应商的亲和必须清掉。
	status, body = call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/provider-endpoints/%d", endpointID), `{"sortOrder":7}`)
	if status != http.StatusOK {
		t.Fatalf("改排序应 200，实得 %d：%s", status, body)
	}
	assertStickyAffinityCleared(t, client, fixture.claudeSes)
	// 对照组是同厂**另一类型**的供应商：按 vendor+type 收窄的正确实现不该碰它。
	assertStickyAffinityIntact(t, client, fixture.codexSes)
}

// TestEndpointDeleteTerminatesStickySessions 覆盖端点删除：删掉的地址不该还留在亲和里。
func TestEndpointDeleteTerminatesStickySessions(t *testing.T) {
	pools := endpointWritePools(t)
	client := providerWriteRedis(t)
	router := stickyTestRouter(t, pools, client)
	fixture := seedStickyFixture(t, pools, client)
	endpointID := fixture.endpointIDOfType(t, "claude")

	status, body := call(router, http.MethodDelete,
		fmt.Sprintf("/api/v1/provider-endpoints/%d", endpointID), "")
	if status != http.StatusNoContent {
		t.Fatalf("删端点应 204，实得 %d：%s", status, body)
	}
	assertStickyAffinityCleared(t, client, fixture.claudeSes)
	assertStickyAffinityIntact(t, client, fixture.codexSes)
}
