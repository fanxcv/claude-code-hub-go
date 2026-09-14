package guard

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ingress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件覆盖适配器自身的路径：正文访问器、依赖注入，以及需要真实库的归类与写入路径。
// 需要库的部分走同一套门控变量（未设置即跳过）。

// gzipBody 把正文压成 gzip 并返回可读流（模拟带 content-encoding 的入站）。
func gzipBody(t *testing.T, payload []byte) io.ReadCloser {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(payload); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭压缩器失败: %v", err)
	}
	return io.NopCloser(bytes.NewReader(buffer.Bytes()))
}

// contextWithEncoding 造一个带 content-encoding 的上下文。
func contextWithEncoding(t *testing.T, encoding string, body io.ReadCloser) *pctx.Context {
	t.Helper()
	header := http.Header{}
	if encoding != "" {
		header.Set("Content-Encoding", encoding)
	}
	init := pctx.Init{Method: "POST", Path: "/v1/messages", Headers: header, Body: body}
	ctx, err := pctx.New(init)
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	return ctx
}

// TestBodyAccessorDecodesAndSharesTree 校验：解压后解析、多处共用同一棵树、Store 后重新序列化。
func TestBodyAccessorDecodesAndSharesTree(t *testing.T) {
	payload := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	ctx := contextWithEncoding(t, "gzip", gzipBody(t, payload))

	accessor, err := NewBodyAccessor(ctx, BodyAccessOptions{Ingress: ingress.DefaultOptions()})
	if err != nil {
		t.Fatalf("构造正文访问器失败: %v", err)
	}

	tree, err := accessor.JSON()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if tree["model"] != "claude-sonnet-4-5" {
		t.Fatalf("解压后的正文不对: %v", tree)
	}

	// 同一棵树：就地改一处，另一处视角可见（守卫改正文、转发器读正文靠这条不变量）。
	tree["model"] = "改写后的模型"
	again, err := accessor.JSON()
	if err != nil {
		t.Fatalf("二次解析失败: %v", err)
	}
	if again["model"] != "改写后的模型" {
		t.Fatalf("JSON 未返回同一棵树: %v", again)
	}

	// 未 Store 前返回原始字节；Store 之后重新序列化。
	raw, err := accessor.Bytes()
	if err != nil {
		t.Fatalf("取字节失败: %v", err)
	}
	if !bytes.Contains(raw, []byte("claude-sonnet-4-5")) {
		t.Fatalf("未修改时应返回原始字节: %s", string(raw))
	}
	if err := accessor.Store(again); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	rewritten, err := accessor.Bytes()
	if err != nil {
		t.Fatalf("取改写字节失败: %v", err)
	}
	if !bytes.Contains(rewritten, []byte("改写后的模型")) {
		t.Fatalf("改写后应重新序列化: %s", string(rewritten))
	}

	factory := accessor.Factory()
	bound, err := factory(nil)
	if err != nil {
		t.Fatalf("工厂返回失败: %v", err)
	}
	if bound != BodyAccess(accessor) {
		t.Fatalf("工厂应返回同一个访问器")
	}
	if got := accessor.EncodedHeaders().Get("Content-Type"); got != "application/json" {
		t.Fatalf("改写后的内容类型应是未压缩的 JSON，得到 %q", got)
	}
}

// TestBodyAccessorFailures 校验失败路径都带上 ErrBodyUnavailable，供守卫按 fail-open 处理。
func TestBodyAccessorFailures(t *testing.T) {
	t.Run("空正文解析成空对象", func(t *testing.T) {
		ctx := contextWithEncoding(t, "", io.NopCloser(bytes.NewReader(nil)))
		accessor, err := NewBodyAccessor(ctx, BodyAccessOptions{})
		if err != nil {
			t.Fatalf("空正文不应失败: %v", err)
		}
		tree, err := accessor.JSON()
		if err != nil || len(tree) != 0 {
			t.Fatalf("空正文应解析成空对象: %v %v", tree, err)
		}
	})

	t.Run("非 JSON 正文", func(t *testing.T) {
		ctx := contextWithEncoding(t, "", io.NopCloser(bytes.NewReader([]byte("not-json"))))
		if _, err := NewBodyAccessor(ctx, BodyAccessOptions{}); !errors.Is(err, ErrBodyUnavailable) {
			t.Fatalf("应返回 ErrBodyUnavailable，得到 %v", err)
		}
	})

	t.Run("超出解压上限", func(t *testing.T) {
		payload := bytes.Repeat([]byte("a"), 4096)
		ctx := contextWithEncoding(t, "", io.NopCloser(bytes.NewReader(payload)))
		if _, err := NewBodyAccessor(ctx, BodyAccessOptions{MaxBytes: 128}); !errors.Is(err, ErrBodyUnavailable) {
			t.Fatalf("超限应返回 ErrBodyUnavailable，得到 %v", err)
		}
	})

	t.Run("正文已被取走", func(t *testing.T) {
		ctx := contextWithEncoding(t, "", io.NopCloser(bytes.NewReader([]byte(`{}`))))
		if _, err := ctx.TakeBody(); err != nil {
			t.Fatalf("首次取正文失败: %v", err)
		}
		if _, err := NewBodyAccessor(ctx, BodyAccessOptions{}); !errors.Is(err, ErrBodyUnavailable) {
			t.Fatalf("二次取正文应返回 ErrBodyUnavailable，得到 %v", err)
		}
	})

	t.Run("Store 拒绝 nil 正文", func(t *testing.T) {
		accessor := &BodyAccessor{}
		if err := accessor.Store(nil); err == nil {
			t.Fatalf("nil 正文应被拒绝")
		}
		var nilAccessor *BodyAccessor
		if _, err := nilAccessor.JSON(); !errors.Is(err, ErrBodyUnavailable) {
			t.Fatalf("空访问器应返回 ErrBodyUnavailable，得到 %v", err)
		}
	})
}

// TestAdaptersApplyFillsSeams 校验注入覆盖了哪些缝隙，且不覆盖调用方已按请求绑定的正文工厂。
func TestAdaptersApplyFillsSeams(t *testing.T) {
	pools := guardIntegrationPools(t)
	adapters := guardIntegrationAdapters(t, pools)

	deps := Deps{Logger: quietLogger(), Locale: "en"}
	adapters.Apply(&deps)
	if deps.Auth == nil || deps.Users == nil || deps.ExpiryMarker == nil || deps.Settings == nil ||
		deps.Sensitive == nil || deps.Filters == nil || deps.BlockedLog == nil || deps.WarmupLog == nil ||
		deps.MessageContext == nil || deps.Provider == nil || deps.ProviderGroupTag == nil || deps.IP == nil {
		t.Fatalf("注入后仍有缝隙未接线: %+v", deps)
	}
	// 仍留空、由后续波次接线的缝隙（会话绑定与回放挂在数据面装配上；版本检查与限流未落地）。
	if deps.RateLimit != nil || deps.Replay != nil || deps.Sessions != nil || deps.Versions != nil {
		t.Fatalf("本波不该接线这些缝隙: rate=%v replay=%v session=%v version=%v",
			deps.RateLimit != nil, deps.Replay != nil, deps.Sessions != nil, deps.Versions != nil)
	}
	if deps.Locale != "en" {
		t.Fatalf("注入不应覆盖调用方的语种设置")
	}

	// 调用方已绑定的正文工厂不被覆盖。
	custom := func(*pctx.Context) (BodyAccess, error) { return newFakeBody(map[string]any{}), nil }
	withBody := Deps{Body: custom}
	adapters.Apply(&withBody)
	if withBody.Body == nil {
		t.Fatalf("正文工厂被清空")
	}

	// 链上分组取值函数确实取到了真实密钥的分组。
	apiKey, _ := seedIdentity(t, pools, context.Background(), nil, testGroupTag)
	request := assembledContext(t, apiKey, map[string]any{"model": "m"})
	if _, err := deps.Auth.ResolveAPIKey(context.Background(), apiKey); err != nil {
		t.Fatalf("解析密钥失败: %v", err)
	}
	request.SetAuth(pctx.AuthState{KeyID: 1, UserID: 1, APIKey: apiKey})
	if got := adapters.Auth.ProviderGroup(context.Background(), request); got != testGroupTag {
		t.Fatalf("有效分组应为 %s，得到 %q", testGroupTag, got)
	}
	if got := adapters.Provider.ProviderGroupTag(pctx.ProviderSelection{ProviderID: 2}); got != testGroupTag {
		t.Fatalf("供应商分组标签应为 %s，得到 %q", testGroupTag, got)
	}
	if _, err := NewAdapters(AdapterOptions{}); err == nil {
		t.Fatalf("缺 Pools 应拒绝构造")
	}
}

// TestAuthClassificationAndExpiry 用真实库覆盖密钥归类、用户不存在与惰性过期标记。
func TestAuthClassificationAndExpiry(t *testing.T) {
	pools := guardIntegrationPools(t)
	adapters := guardIntegrationAdapters(t, pools)
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}

	t.Run("禁用密钥归为 key_disabled", func(t *testing.T) {
		apiKey, userID := seedIdentity(t, pools, ctx, nil, "default")
		if _, err := pool.Exec(ctx, `UPDATE keys SET is_enabled = false WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("禁用密钥失败: %v", err)
		}
		adapters.Auth.Invalidate()
		if _, err := adapters.Auth.ResolveAPIKey(ctx, apiKey); !errors.Is(err, ErrKeyDisabled) {
			t.Fatalf("期望 ErrKeyDisabled，得到 %v", err)
		}
	})

	t.Run("过期密钥归为 key_expired", func(t *testing.T) {
		apiKey, userID := seedIdentity(t, pools, ctx, nil, "default")
		if _, err := pool.Exec(ctx,
			`UPDATE keys SET expires_at = now() - interval '1 hour' WHERE user_id = $1`, userID); err != nil {
			t.Fatalf("置为过期失败: %v", err)
		}
		adapters.Auth.Invalidate()
		if _, err := adapters.Auth.ResolveAPIKey(ctx, apiKey); !errors.Is(err, ErrKeyExpired) {
			t.Fatalf("期望 ErrKeyExpired，得到 %v", err)
		}
	})

	t.Run("用户不存在", func(t *testing.T) {
		// id 列是 serial(int4)，取一个仍在范围内、必定不存在的值。
		if _, err := adapters.Auth.User(ctx, 2147483000); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("期望包装 store.ErrNotFound，得到 %v", err)
		}
	})

	t.Run("惰性过期标记幂等", func(t *testing.T) {
		apiKey, userID := seedIdentity(t, pools, ctx, nil, "default")
		adapters.Auth.Invalidate()
		resolution, err := adapters.Auth.ResolveAPIKey(ctx, apiKey)
		if err != nil {
			t.Fatalf("解析失败: %v", err)
		}
		if resolution.User.ID != userID {
			t.Fatalf("解析出的用户不符: %d != %d", resolution.User.ID, userID)
		}
		if err := adapters.Auth.MarkUserExpired(ctx, userID); err != nil {
			t.Fatalf("标记失败: %v", err)
		}
		if err := adapters.Auth.MarkUserExpired(ctx, userID); err != nil {
			t.Fatalf("重复标记应幂等: %v", err)
		}
		var enabled bool
		if err := pool.QueryRow(ctx, `SELECT is_enabled FROM users WHERE id = $1`, userID).Scan(&enabled); err != nil {
			t.Fatalf("读取用户失败: %v", err)
		}
		if enabled {
			t.Fatalf("标记后用户应为禁用")
		}
	})

	t.Run("用户禁用后的密钥仍能解析但用户属性为禁用", func(t *testing.T) {
		apiKey, userID := seedIdentity(t, pools, ctx, nil, "default")
		if _, err := pool.Exec(ctx, `UPDATE users SET is_enabled = false WHERE id = $1`, userID); err != nil {
			t.Fatalf("禁用用户失败: %v", err)
		}
		adapters.Auth.Invalidate()
		resolution, err := adapters.Auth.ResolveAPIKey(ctx, apiKey)
		if err != nil {
			t.Fatalf("解析应成功（用户状态由守卫判定）: %v", err)
		}
		if resolution.User.IsEnabled {
			t.Fatalf("用户应被判为禁用")
		}
	})
}

// TestWarmupRecorderWritesFinalizedRow 校验抢答日志落到终态行（provider_id = 0、blocked_by = warmup）。
func TestWarmupRecorderWritesFinalizedRow(t *testing.T) {
	pools := guardIntegrationPools(t)
	adapters := guardIntegrationAdapters(t, pools)
	ctx := context.Background()
	apiKey, userID := seedIdentity(t, pools, ctx, nil, "default")

	if err := adapters.Warmup.RecordWarmup(ctx, WarmupRecord{
		KeyID:         1,
		UserID:        userID,
		APIKey:        apiKey,
		Model:         "claude-sonnet-4-5",
		OriginalModel: "claude-sonnet-4-5",
		UserAgent:     "claude-cli/1.0.0",
		Endpoint:      "/v1/messages",
		MessagesCount: 1,
		DurationMS:    7,
	}); err != nil {
		t.Fatalf("写 warmup 日志失败: %v", err)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	var providerID, statusCode int
	var blockedBy string
	var cost *string
	if err := pool.QueryRow(ctx,
		`SELECT provider_id, status_code, blocked_by, cost_usd::text FROM message_request
		 WHERE user_id = $1 ORDER BY id DESC LIMIT 1`, userID).
		Scan(&providerID, &statusCode, &blockedBy, &cost); err != nil {
		t.Fatalf("读取 warmup 行失败: %v", err)
	}
	if providerID != 0 || statusCode != 200 || blockedBy != "warmup" {
		t.Fatalf("warmup 行不符: provider=%d status=%d blocked_by=%q", providerID, statusCode, blockedBy)
	}
	if cost != nil {
		t.Fatalf("warmup 不应计费（cost_usd 应为 NULL），得到 %v", *cost)
	}

	// 拦截记录器的载荷也要能落库（敏感词以外的拦截点会走同一条路）。
	reason := json.RawMessage(`{"word":"x","matchType":"contains"}`)
	if err := adapters.Blocked.RecordBlocked(ctx, BlockedRecord{
		UserID:       userID,
		APIKey:       apiKey,
		Model:        "claude-sonnet-4-5",
		StatusCode:   400,
		BlockedBy:    "sensitive_word",
		Reason:       reason,
		ErrorMessage: "请求包含敏感词：x",
	}); err != nil {
		t.Fatalf("写拦截日志失败: %v", err)
	}
	var blockedReason string
	if err := pool.QueryRow(ctx,
		`SELECT blocked_reason FROM message_request WHERE user_id = $1 ORDER BY id DESC LIMIT 1`,
		userID).Scan(&blockedReason); err != nil {
		t.Fatalf("读取拦截行失败: %v", err)
	}
	if blockedReason == "" {
		t.Fatalf("拦截行应带 blocked_reason")
	}
}
