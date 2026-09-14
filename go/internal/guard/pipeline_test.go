package guard

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是守卫链的**装配级**集成测试：真实 store（门控 env）+ 真实 pctx，跑通四条链的
// 短路路径。
//
// 为什么必须用真实库：这一波的价值就在于「适配器真的接到数据面上了」。用假 store 断言
// 「链会短路」不校验任何东西——那条路径已经被 edges_test.go 覆盖。真正需要验证的是
// SQL 形状、列名、类型与缓存行为能否支撑起链条。
//
// 数据纪律（共享测试库）：
//   - 禁 DDL。只做 DML，且全部用 cch-guard-it- 前缀的唯一名字，t.Cleanup 兜底清理；
//     每次开工前先按前缀清理一次历史残留（上次崩溃留下的行不会累积）。
//   - 不清、不改库里已有的任何行：本文件只读既有数据（密钥、供应商、设置），写入的行都是自己建的。
//
// 与库中既有数据的关系：本库只有一行用户（allowed_models 为空、provider_group=default），
// 因此「模型白名单」路径无法用既有数据触发，测试自行建一个受限用户；供应商则用库里既有的
// e2e-anth 分组供应商（provider_group=e2e-anth），以验证选路真的从快照里选出了它。

const (
	testDSNEnv   = "CCH_TEST_DSN"
	testPrefix   = "cch-guard-it-"
	testGroupTag = "e2e-anth"
)

// guardIntegrationPools 建真实池；未门控时跳过。
func guardIntegrationPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过守卫链装配集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-guard-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// guardIntegrationAdapters 建真实适配器集合（不接 Redis 总线：本测试不验证失效广播，
// 那属于 cfgsync 自己的集成测试；这里要证的是「快照能撑住热路径」）。
func guardIntegrationAdapters(t *testing.T, pools *store.Pools) *Adapters {
	t.Helper()
	adapters, err := NewAdapters(AdapterOptions{Pools: pools, Logger: quietLogger()})
	if err != nil {
		t.Fatalf("构造适配器失败: %v", err)
	}
	t.Cleanup(adapters.Close)
	return adapters
}

// cleanupStaleRows 清理历史残留（按前缀）。
func cleanupStaleRows(t *testing.T, pools *store.Pools, ctx context.Context) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	statements := []string{
		`DELETE FROM message_request WHERE user_id IN (SELECT id FROM users WHERE name LIKE $1)`,
		`DELETE FROM keys WHERE user_id IN (SELECT id FROM users WHERE name LIKE $1)`,
		`DELETE FROM users WHERE name LIKE $1`,
		`DELETE FROM sensitive_words WHERE word LIKE $1`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement, testPrefix+"%"); err != nil {
			t.Fatalf("清理历史残留失败（%s）: %v", statement, err)
		}
	}
}

// seedIdentity 建一个临时用户 + 密钥，返回密钥明文与用户 id。
//
// allowedModels 为空表示不限模型；providerGroup 决定选路分组。
func seedIdentity(
	t *testing.T,
	pools *store.Pools,
	ctx context.Context,
	allowedModels []string,
	providerGroup string,
) (apiKey string, userID int64) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	suffix := randomSuffix(t)
	name := testPrefix + "user-" + suffix
	apiKey = "sk-" + testPrefix + suffix

	models := "[]"
	if len(allowedModels) > 0 {
		encoded := make([]string, 0, len(allowedModels))
		for _, model := range allowedModels {
			encoded = append(encoded, `"`+model+`"`)
		}
		models = "[" + strings.Join(encoded, ",") + "]"
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (name, provider_group, is_enabled, allowed_models, allowed_clients, blocked_clients)
		 VALUES ($1, $2, true, $3::jsonb, '[]'::jsonb, '[]'::jsonb) RETURNING id`,
		name, providerGroup, models).Scan(&userID); err != nil {
		t.Fatalf("建临时用户失败: %v", err)
	}
	// 密钥的 provider_group 必须显式写：该列有 default 'default'，而有效分组是
	// key.provider_group 优先于 user.provider_group（写死默认值会把分组过滤锁进 default）。
	if _, err := pool.Exec(ctx,
		`INSERT INTO keys (user_id, key, name, is_enabled, provider_group) VALUES ($1, $2, $3, true, $4)`,
		userID, apiKey, name+"-key", providerGroup); err != nil {
		t.Fatalf("建临时密钥失败: %v", err)
	}

	t.Cleanup(func() {
		cleanup := context.Background()
		// 先删请求日志：它会因本测试的走链而新增行。
		_, _ = pool.Exec(cleanup, `DELETE FROM message_request WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM keys WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanup, `DELETE FROM users WHERE id = $1`, userID)
	})
	return apiKey, userID
}

// seedSensitiveWord 插一条敏感词，返回词面。
func seedSensitiveWord(t *testing.T, pools *store.Pools, ctx context.Context) string {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	word := testPrefix + "word-" + randomSuffix(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO sensitive_words (word, match_type, is_enabled) VALUES ($1, 'contains', true)`,
		word); err != nil {
		t.Fatalf("建临时敏感词失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sensitive_words WHERE word = $1`, word)
	})
	return word
}

// randomSuffix 生成唯一后缀（避免并行测试与残留行冲突）。
func randomSuffix(t *testing.T) string {
	t.Helper()
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("生成随机后缀失败: %v", err)
	}
	return hex.EncodeToString(raw[:])
}

// assembledContext 造一个走真实链路的请求上下文（真实 pctx + 真实正文流）。
//
// 入口在该写的三件事这里手动补上：协议族（选路的格式兼容判定要用）、客户端 IP
// （请求日志列）、以及正文（由 newContextAtPath 灌进 pctx 的流）。
func assembledContext(t *testing.T, apiKey string, body map[string]any) *pctx.Context {
	t.Helper()
	headers := map[string]string{"authorization": "Bearer " + apiKey, "user-agent": "claude-cli/1.0.0"}
	ctx := newContextAtPath(t, "/v1/messages", headers, body)
	ctx.SetProtocolFrom(egress.FamilyAnthropicMessages)
	ctx.SetClientIP("203.0.113.10")
	return ctx
}

// TestAssembleChatPipelineStepOrder 打印并钉住四条链的实际步骤顺序。
//
// 顺序是语义（错误优先级、计费时点都由它决定），因此断言的是**字面顺序**而不是长度。
func TestAssembleChatPipelineStepOrder(t *testing.T) {
	deps := Deps{
		Auth:           &fakeAuth{},
		Users:          fakeUsers{},
		Settings:       fakeSettings{},
		Sensitive:      fakeSensitive{},
		Filters:        fakeFilters{},
		Provider:       fakeProvider{},
		MessageContext: &fakeMessageContext{},
		Body:           func(*pctx.Context) (BodyAccess, error) { return newFakeBody(map[string]any{}), nil },
	}

	chat, err := Assemble(deps, ChatPolicy())
	if err != nil {
		t.Fatalf("对话链装配失败: %v", err)
	}
	want := "auth -> sensitive -> client -> model -> version -> probe -> session -> warmup -> " +
		"requestFilter -> replayAttach -> rateLimit -> provider -> providerRequestFilter -> messageContext"
	if got := StepSequence(chat); got != want {
		t.Fatalf("对话链步骤顺序不符:\n got %s\nwant %s", got, want)
	}
	t.Logf("CHAT_PIPELINE: %s", StepSequence(chat))

	raw, err := Assemble(deps, Policy{Preset: PresetRawPassthrough})
	if err != nil {
		t.Fatalf("原始透传链装配失败: %v", err)
	}
	if got := StepSequence(raw); got != "auth -> client -> model -> version -> probe -> provider" {
		t.Fatalf("原始透传链步骤顺序不符: %s", got)
	}
	t.Logf("RAW_PASSTHROUGH_PIPELINE: %s", StepSequence(raw))

	rawSafe, err := Assemble(deps, Policy{Preset: PresetRawPassthrough, RawCrossProviderFallback: true})
	if err != nil {
		t.Fatalf("安全会话链装配失败: %v", err)
	}
	if got := StepSequence(rawSafe); got != "auth -> client -> model -> version -> probe -> session -> provider -> messageContext" {
		t.Fatalf("安全会话链步骤顺序不符: %s", got)
	}
	t.Logf("RAW_SAFE_SESSION_PIPELINE: %s", StepSequence(rawSafe))

	countTokens, err := Assemble(deps, Policy{Preset: PresetChat, RequestType: RequestTypeCountTokens})
	if err != nil {
		t.Fatalf("count_tokens 链装配失败: %v", err)
	}
	t.Logf("COUNT_TOKENS_PIPELINE: %s", StepSequence(countTokens))
	if StepSequence(countTokens) != StepSequence(rawSafe) {
		t.Fatalf("count_tokens 应复用安全会话链: %s", StepSequence(countTokens))
	}
}

// TestAssembleRejectsMissingRequiredDependency 校验构造期的必需依赖判定。
func TestAssembleRejectsMissingRequiredDependency(t *testing.T) {
	complete := Deps{
		Auth:           &fakeAuth{},
		Users:          fakeUsers{},
		Settings:       fakeSettings{},
		Sensitive:      fakeSensitive{},
		Filters:        fakeFilters{},
		Provider:       fakeProvider{},
		MessageContext: &fakeMessageContext{},
		Body:           func(*pctx.Context) (BodyAccess, error) { return newFakeBody(map[string]any{}), nil },
	}
	if _, err := Assemble(complete, ChatPolicy()); err != nil {
		t.Fatalf("完整依赖应能装配: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Deps)
		policy Policy
	}{
		{name: "缺 Auth", mutate: func(d *Deps) { d.Auth = nil }, policy: ChatPolicy()},
		{name: "缺 Users", mutate: func(d *Deps) { d.Users = nil }, policy: ChatPolicy()},
		{name: "缺 Provider", mutate: func(d *Deps) { d.Provider = nil }, policy: ChatPolicy()},
		{name: "缺 Settings", mutate: func(d *Deps) { d.Settings = nil }, policy: ChatPolicy()},
		{name: "缺 Sensitive", mutate: func(d *Deps) { d.Sensitive = nil }, policy: ChatPolicy()},
		{name: "缺 Filters", mutate: func(d *Deps) { d.Filters = nil }, policy: ChatPolicy()},
		{name: "缺 Body", mutate: func(d *Deps) { d.Body = nil }, policy: ChatPolicy()},
		{name: "缺 MessageContext", mutate: func(d *Deps) { d.MessageContext = nil }, policy: ChatPolicy()},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			deps := complete
			testCase.mutate(&deps)
			if _, err := Assemble(deps, testCase.policy); err == nil {
				t.Fatalf("%s 时应拒绝装配", testCase.name)
			}
		})
	}

	// 原始透传链不含 sensitive/filters/settings/正文步骤，缺它们不应阻断构造。
	lean := Deps{
		Auth:     &fakeAuth{},
		Users:    fakeUsers{},
		Provider: fakeProvider{},
	}
	if _, err := Assemble(lean, Policy{Preset: PresetRawPassthrough}); err != nil {
		t.Fatalf("原始透传链不应要求对话链专属依赖: %v", err)
	}
}

// TestPipelineShortCircuitsWithRealStore 跑通四条链的短路路径（门控 env）。
//
// 用真实正文访问器（NewBodyAccessor + ingress 解压）而不是手工塞一棵 map：正文路径正是
// 「谁持有字节」的接缝，用假的就绕开了本波最想验证的那一段。
func TestPipelineShortCircuitsWithRealStore(t *testing.T) {
	pools := guardIntegrationPools(t)
	adapters := guardIntegrationAdapters(t, pools)
	ctx := context.Background()
	cleanupStaleRows(t, pools, ctx)

	t.Run("鉴权失败返回 401 且负结果不缓存", func(t *testing.T) {
		body := map[string]any{
			"model":    "claude-sonnet-4-5",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}
		before := adapters.Auth.KeyLoads()
		response, err := runChainWithRequest(t, adapters, assembledContext(t, "sk-"+testPrefix+"missing-key", body))
		if err != nil {
			t.Fatalf("链条不应把认证失败当步骤故障: %v", err)
		}
		if response == nil || response.Status != 401 {
			t.Fatalf("期望 401，得到 %+v", response)
		}
		if got := errorField(t, response, "type"); got != "invalid_api_key" {
			t.Fatalf("期望 error.type=invalid_api_key，得到 %q（体: %s）", got, string(response.Body))
		}

		// 再跑一次：不存在的密钥不得进缓存（否则管理面刚建的密钥会有一段时间不可用）。
		if _, err := runChainWithRequest(t, adapters, assembledContext(t, "sk-"+testPrefix+"missing-key", body)); err != nil {
			t.Fatalf("第二次运行失败: %v", err)
		}
		if got := adapters.Auth.KeyLoads() - before; got != 2 {
			t.Fatalf("负结果应每次打库：期望 2 次装载，得到 %d", got)
		}
	})

	t.Run("模型白名单拒绝并命中缓存", func(t *testing.T) {
		apiKey, _ := seedIdentity(t, pools, ctx, []string{"cch-guard-it-allowed-model"}, "default")
		before := adapters.Auth.UserLoads()

		response, err := runChainWithRequest(t, adapters, assembledContext(t, apiKey, map[string]any{
			"model":    "not-in-whitelist",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("模型拒绝不应是步骤故障: %v", err)
		}
		if response == nil || response.Status != 400 {
			t.Fatalf("期望 400，得到 %+v", response)
		}
		if got := errorField(t, response, "type"); got != "invalid_request_error" {
			t.Fatalf("期望 error.type=invalid_request_error，得到 %q", got)
		}
		if !containsSubstring(string(response.Body), "not-in-whitelist") {
			t.Fatalf("错误体应回显被拒模型: %s", string(response.Body))
		}
		// 用户属性随密钥解析一并回填缓存，模型步不应再打库。
		if got := adapters.Auth.UserLoads() - before; got != 0 {
			t.Fatalf("模型步不应触发用户查询：期望 0 次装载，得到 %d", got)
		}

		// 白名单内的模型应通过模型步，并一路走到最后一步：分组用真实存在的 e2e-anth，
		// 确保后续选路也能成功，从而证明上面那次拒绝确实来自模型步。
		passKey, _ := seedIdentity(t, pools, ctx, []string{"cch-guard-it-allowed-model"}, testGroupTag)
		passed, err := runChainWithRequest(t, adapters, assembledContext(t, passKey, map[string]any{
			"model":    "cch-guard-it-allowed-model",
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		}))
		if err != nil {
			t.Fatalf("白名单内模型运行失败: %v", err)
		}
		if passed != nil {
			t.Fatalf("白名单内模型应通过全链，被 %d 拦下: %s", passed.Status, string(passed.Body))
		}
	})

	t.Run("敏感词命中返回 400 并落库", func(t *testing.T) {
		word := seedSensitiveWord(t, pools, ctx)
		apiKey, userID := seedIdentity(t, pools, ctx, nil, "default")
		// 新建的词必须立刻生效：清一次快照，等价于失效消息到达（广播本身由 cfgsync 的集成测试覆盖）。
		adapters.Sensitive.Invalidate()

		response, err := runChainWithRequest(t, adapters, assembledContext(t, apiKey, map[string]any{
			"model": "claude-sonnet-4-5",
			"messages": []any{
				map[string]any{"role": "user", "content": "口令是 " + word},
			},
		}))
		if err != nil {
			t.Fatalf("敏感词拒绝不应是步骤故障: %v", err)
		}
		if response == nil || response.Status != 400 {
			t.Fatalf("期望 400，得到 %+v", response)
		}
		if !containsSubstring(string(response.Body), word) {
			t.Fatalf("错误体应回显命中词: %s", string(response.Body))
		}

		// 落库：provider_id = 0、blocked_by = sensitive_word、状态码 400。
		pool, err := pools.Control()
		if err != nil {
			t.Fatalf("取控制分道失败: %v", err)
		}
		var providerID, statusCode int
		var blockedBy string
		if err := pool.QueryRow(ctx,
			`SELECT provider_id, status_code, blocked_by FROM message_request
			 WHERE user_id = $1 ORDER BY id DESC LIMIT 1`,
			userID).Scan(&providerID, &statusCode, &blockedBy); err != nil {
			t.Fatalf("读取拦截行失败: %v", err)
		}
		if providerID != 0 || statusCode != 400 || blockedBy != "sensitive_word" {
			t.Fatalf("拦截行不符: provider_id=%d status=%d blocked_by=%q", providerID, statusCode, blockedBy)
		}
		if rows := adapters.Blocked.Rows(); rows < 1 {
			t.Fatalf("拦截记录器未落库：%d 行", rows)
		}
	})

	t.Run("通过链并到达最后一步且不每请求查库", func(t *testing.T) {
		apiKey, userID := seedIdentity(t, pools, ctx, nil, testGroupTag)

		// 预热一次：让三份快照装载完成、密钥与供应商快照进缓存。
		if response, err := runChainWithRequest(t, adapters, assembledContext(t, apiKey, chatBody())); err != nil {
			t.Fatalf("预热运行失败: %v", err)
		} else if response != nil {
			t.Fatalf("预热运行被 %d 拦下: %s", response.Status, string(response.Body))
		}
		baseline := loadBaseline{
			key:       adapters.Auth.KeyLoads(),
			user:      adapters.Auth.UserLoads(),
			settings:  adapters.Settings.Loads(),
			sensitive: adapters.Sensitive.Loads(),
			filters:   adapters.Filters.Loads(),
			snapshot:  adapters.Provider.SnapshotLoads(),
		}

		const runs = 3
		start := time.Now()
		for index := 0; index < runs; index++ {
			request := assembledContext(t, apiKey, chatBody())
			response, err := runChainWithRequest(t, adapters, request)
			if err != nil {
				t.Fatalf("第 %d 次运行失败: %v", index, err)
			}
			if response != nil {
				t.Fatalf("第 %d 次应通过全链，被 %d 拦下: %s", index, response.Status, string(response.Body))
			}
			if _, ok := request.Auth(); !ok {
				t.Fatalf("第 %d 次运行后上下文没有鉴权结果", index)
			}
			selection, ok := request.Provider()
			if !ok || selection.ProviderID == 0 {
				t.Fatalf("第 %d 次运行后上下文没有选路结果", index)
			}
			if selection.Type != "claude" {
				t.Fatalf("分组 %s 应选出 claude 供应商，得到 %s", testGroupTag, selection.Type)
			}
		}
		elapsed := time.Since(start)
		t.Logf("通过链 %d 次耗时 %s（每次 %s）", runs, elapsed, elapsed/runs)

		// 关键断言：热路径上没有任何一次快照或密钥装载。
		if got := adapters.Auth.KeyLoads() - baseline.key; got != 0 {
			t.Fatalf("密钥每请求查库：%d 次装载", got)
		}
		if got := adapters.Auth.UserLoads() - baseline.user; got != 0 {
			t.Fatalf("用户每请求查库：%d 次装载", got)
		}
		if got := adapters.Settings.Loads() - baseline.settings; got != 0 {
			t.Fatalf("设置每请求查库：%d 次装载", got)
		}
		if got := adapters.Sensitive.Loads() - baseline.sensitive; got != 0 {
			t.Fatalf("敏感词每请求查库：%d 次装载", got)
		}
		if got := adapters.Filters.Loads() - baseline.filters; got != 0 {
			t.Fatalf("过滤规则每请求查库：%d 次装载", got)
		}
		if got := adapters.Provider.SnapshotLoads() - baseline.snapshot; got != 0 {
			t.Fatalf("选路快照每请求查库：%d 次装载", got)
		}

		// 每次通过全链都应开一行请求日志（messageContext 是最后一步）。
		if rows := adapters.Message.Rows(); rows < runs+1 {
			t.Fatalf("请求日志开行数不足：期望 >= %d，得到 %d", runs+1, rows)
		}
		pool, err := pools.Control()
		if err != nil {
			t.Fatalf("取控制分道失败: %v", err)
		}
		var storedUserID int64
		if err := pool.QueryRow(ctx,
			`SELECT user_id FROM message_request WHERE user_id = $1 ORDER BY id DESC LIMIT 1`,
			userID).Scan(&storedUserID); err != nil {
			t.Fatalf("读取请求日志行失败: %v", err)
		}
		if storedUserID != userID {
			t.Fatalf("请求日志行归属不符: %d", storedUserID)
		}
	})
}

// runChainWithRequest 装配并执行一次对话链。
//
// 正文工厂按请求构造：这是本包推荐的接线方式（见 adapters.go 的纪律第 3 条），
// 也是唯一不需要「按上下文指针索引的全局表」的做法。
func runChainWithRequest(t *testing.T, adapters *Adapters, request *pctx.Context) (*Response, error) {
	t.Helper()
	deps := Deps{Logger: quietLogger()}
	adapters.Apply(&deps)
	accessor, err := NewBodyAccessor(request, BodyAccessOptions{})
	if err != nil {
		t.Fatalf("构造正文访问器失败: %v", err)
	}
	deps.Body = accessor.Factory()
	chain, err := Assemble(deps, ChatPolicy())
	if err != nil {
		t.Fatalf("装配失败: %v", err)
	}
	t.Logf("链条: %s", StepSequence(chain))
	return chain.Run(request)
}

// loadBaseline 是一组装载计数的快照。
type loadBaseline struct {
	key       int64
	user      int64
	settings  int64
	sensitive int64
	filters   int64
	snapshot  int64
}

// chatBody 造一个最小合法对话正文。
func chatBody() map[string]any {
	return map[string]any{
		"model":      "claude-sonnet-4-5",
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
	}
}

// TestAdapterCachesInvalidateOnBus 校验失效订阅真的接到了适配器上。
//
// 用真实 Redis 总线：这是「配置改了要立刻生效」的唯一保证，用假总线测不出订阅是否装上。
func TestAdapterCachesInvalidateOnBus(t *testing.T) {
	pools := guardIntegrationPools(t)
	client := guardIntegrationRedis(t)
	bus := cfgsync.NewBus(client, nil)
	t.Cleanup(func() { _ = bus.Close() })
	registry := cfgsync.NewRegistry(bus)
	t.Cleanup(registry.Close)

	adapters, err := NewAdapters(AdapterOptions{
		Pools:    pools,
		Bus:      bus,
		Registry: registry,
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatalf("构造适配器失败: %v", err)
	}
	t.Cleanup(adapters.Close)

	ctx := context.Background()
	if _, err := adapters.Settings.FindSystemSettings(ctx); err != nil {
		t.Fatalf("装载设置失败: %v", err)
	}
	before := registry.Invalidations(cfgsync.DomainSystemSettings)

	bus.Publish(ctx, cfgsync.ChannelSystemSettingsUpdated, "test")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if registry.Invalidations(cfgsync.DomainSystemSettings) > before {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := registry.Invalidations(cfgsync.DomainSystemSettings); got <= before {
		t.Fatalf("失效订阅未生效：期望收到至少一次失效，得到 %d（之前 %d）", got, before)
	}
	// 失效后再次读取会重新装载。
	loadsBefore := adapters.Settings.Loads()
	if _, err := adapters.Settings.FindSystemSettings(ctx); err != nil {
		t.Fatalf("失效后装载失败: %v", err)
	}
	if adapters.Settings.Loads() <= loadsBefore {
		t.Fatalf("失效后未重新装载：%d -> %d", loadsBefore, adapters.Settings.Loads())
	}
	if !registry.Loaded(cfgsync.DomainSystemSettings) {
		t.Fatalf("装载后应记录该域已装载")
	}
}

// factory 把 fakeBody 包成 BodyFactory。
func (b *fakeBody) factory() BodyFactory {
	return func(*pctx.Context) (BodyAccess, error) { return b, nil }
}

// quietLogger 让集成测试不刷屏：链上那些 warn/debug 在「部分接线」的中间态里是预期行为。
func quietLogger() *logx.Logger { return logx.New(io.Discard) }

// guardIntegrationRedis 读门控变量建 Redis 客户端；未设置时跳过。
//
// 库号固定在 13（URL 自带库号时以其为准），避免碰生产键空间。
func guardIntegrationRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}
