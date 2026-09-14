package guard

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testBody 是最小的 BodyAccess：EnsureContext 只需要「取得到模型名」与消息条数。
type testBody struct{ obj map[string]any }

func (b *testBody) JSON() (map[string]any, error) { return b.obj, nil }
func (b *testBody) Store(m map[string]any) error  { b.obj = m; return nil }

func guardTestPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(4),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// TestIntegrationEnsureContextWritesSessionIdentityKind 是本次修复的核心验收：
// 建行时把「会话身份形制」如实写进 message_request，使用记录页的
// 「渠道复用 / 新会话新渠道」一列才有据可读（此前 Go 侧该列恒为 NULL）。
//
// 两条路径都要有真库证据：
//   - 忽略客户端会话 id 且亲和身份存在 → prefix_affinity（跨会话复用同一渠道）
//   - 不忽略 → session_id（新会话新渠道），身份为客户端会话 id
func TestIntegrationEnsureContextWritesSessionIdentityKind(t *testing.T) {
	pools := guardTestPools(t)
	ctx := context.Background()

	db, err := pools.Data()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}

	prefix := "go-guard-it-" + time.Now().Format("20060102150405.000000000")
	var vendorID, providerID, userID int64
	if err := db.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		prefix+".invalid", prefix).Scan(&vendorID); err != nil {
		t.Fatalf("建厂商失败: %v", err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_vendor_id, is_enabled, weight, priority,
			cost_multiplier, group_tag, provider_type)
		VALUES ($1, $2, $3, $4, true, 1, 0, '1.0'::numeric, $1, 'claude')
		RETURNING id`,
		prefix+"-provider", "https://"+prefix+".invalid", "sk-guard-it", vendorID).Scan(&providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`,
		prefix+"-user").Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = db.Exec(cleanup, `DELETE FROM message_request WHERE user_id = $1`, userID)
		_, _ = db.Exec(cleanup, `DELETE FROM usage_ledger WHERE user_id = $1`, userID)
		_, _ = db.Exec(cleanup, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = db.Exec(cleanup, `DELETE FROM providers WHERE id = $1`, providerID)
		_, _ = db.Exec(cleanup, `DELETE FROM provider_vendors WHERE id = $1`, vendorID)
	})

	const sessionID = "it-session-plain-1"
	cases := []struct {
		name                 string
		ignoreClientSession  bool
		withAffinityIdentity bool
		wantKind             string
		wantIdentity         string
	}{
		{
			name:                 "忽略客户端会话 id：记 prefix_affinity（跨会话复用同一渠道）",
			ignoreClientSession:  true,
			withAffinityIdentity: true,
			wantKind:             "prefix_affinity",
			wantIdentity:         "pfx:it-scope:it-fp",
		},
		{
			name:                "不忽略：记 session_id（新会话新渠道）",
			ignoreClientSession: false,
			wantKind:            "session_id",
			wantIdentity:        sessionID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc, err := pctx.New(pctx.Init{
				Method:       "POST",
				Path:         "/v1/messages",
				ProtocolFrom: egress.FamilyAnthropicMessages,
				Logger:       quietLogger(),
			})
			if err != nil {
				t.Fatalf("构造上下文失败: %v", err)
			}
			pc.SetAuth(pctx.AuthState{KeyID: 1, UserID: userID, APIKey: "sk-guard-it"})
			pc.SetProvider(pctx.ProviderSelection{ProviderID: providerID, Name: prefix + "-provider"})
			if tc.withAffinityIdentity {
				pc.SetAffinityIdentity("it-scope", "it-fp")
			}

			writer := newMessageWriter(pools, quietLogger(), tc.ignoreClientSession).WithBody(
				func(*pctx.Context) (BodyAccess, error) {
					return &testBody{obj: map[string]any{
						"model":    "claude-sonnet-4-5",
						"messages": []any{map[string]any{"role": "user", "content": "hi"}},
					}}, nil
				},
				func(*pctx.Context) (SessionResult, bool) {
					return SessionResult{SessionID: sessionID, Sequence: 1}, true
				},
			)
			if err := writer.EnsureContext(ctx, pc); err != nil {
				t.Fatalf("建行失败: %v", err)
			}
			rowID, ok := pc.MessageRequestID()
			if !ok {
				t.Fatal("建行后应能取到行标识")
			}

			var kind, identity *string
			if err := db.QueryRow(ctx,
				`SELECT session_identity_kind, session_identity FROM message_request WHERE id = $1`,
				rowID).Scan(&kind, &identity); err != nil {
				t.Fatalf("读回行失败: %v", err)
			}
			if kind == nil || *kind != tc.wantKind {
				t.Fatalf("session_identity_kind = %v，期望 %q", deref(kind), tc.wantKind)
			}
			if identity == nil || *identity != tc.wantIdentity {
				t.Fatalf("session_identity = %v，期望 %q", deref(identity), tc.wantIdentity)
			}
		})
	}
}

// TestIntegrationEnsureContextWritesSessionIDKindWithoutSessionLookup 钉住兜底：
// 会话包未接线（SessionLookup 为 nil）时，Node 的默认元数据形制仍是 session_id，
// 身份留空——不写会让该列恒为 NULL，界面读不出「新会话」。
func TestIntegrationEnsureContextWritesSessionIDKindWithoutSessionLookup(t *testing.T) {
	pools := guardTestPools(t)
	ctx := context.Background()
	db, err := pools.Data()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}

	prefix := "go-guard-it-nolookup-" + time.Now().Format("20060102150405.000000000")
	var vendorID, providerID, userID int64
	if err := db.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		prefix+".invalid", prefix).Scan(&vendorID); err != nil {
		t.Fatalf("建厂商失败: %v", err)
	}
	if err := db.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_vendor_id, is_enabled, weight, priority,
			cost_multiplier, group_tag, provider_type)
		VALUES ($1, $2, $3, $4, true, 1, 0, '1.0'::numeric, $1, 'claude')
		RETURNING id`,
		prefix+"-provider", "https://"+prefix+".invalid", "sk-guard-it", vendorID).Scan(&providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`,
		prefix+"-user").Scan(&userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = db.Exec(cleanup, `DELETE FROM message_request WHERE user_id = $1`, userID)
		_, _ = db.Exec(cleanup, `DELETE FROM usage_ledger WHERE user_id = $1`, userID)
		_, _ = db.Exec(cleanup, `DELETE FROM users WHERE id = $1`, userID)
		_, _ = db.Exec(cleanup, `DELETE FROM providers WHERE id = $1`, providerID)
		_, _ = db.Exec(cleanup, `DELETE FROM provider_vendors WHERE id = $1`, vendorID)
	})

	pc, err := pctx.New(pctx.Init{
		Method:       "POST",
		Path:         "/v1/messages",
		ProtocolFrom: egress.FamilyAnthropicMessages,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	pc.SetAuth(pctx.AuthState{KeyID: 1, UserID: userID, APIKey: "sk-guard-it"})
	pc.SetProvider(pctx.ProviderSelection{ProviderID: providerID, Name: prefix + "-provider"})

	writer := newMessageWriter(pools, quietLogger(), true).WithBody(
		func(*pctx.Context) (BodyAccess, error) {
			return &testBody{obj: map[string]any{
				"model":    "claude-sonnet-4-5",
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}}, nil
		},
		nil, // 会话包未接线
	)
	if err := writer.EnsureContext(ctx, pc); err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	rowID, _ := pc.MessageRequestID()

	var kind, identity *string
	if err := db.QueryRow(ctx,
		`SELECT session_identity_kind, session_identity FROM message_request WHERE id = $1`,
		rowID).Scan(&kind, &identity); err != nil {
		t.Fatalf("读回行失败: %v", err)
	}
	if kind == nil || *kind != "session_id" {
		t.Fatalf("无会话身份时形制应为 session_id，实际 %v", deref(kind))
	}
	if identity != nil {
		t.Fatalf("无会话身份时身份应留空（NULL），实际 %q", *identity)
	}
}

// 让 pgxpool 的导入在本文件内明确（pools.Data 的返回类型）。
var _ *pgxpool.Pool

func deref(s *string) string {
	if s == nil {
		return "(NULL)"
	}
	return *s
}
