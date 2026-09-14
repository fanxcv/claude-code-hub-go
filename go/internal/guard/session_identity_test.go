package guard

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// newIdentityContext 造一个最小请求上下文（只需支持亲和身份槽位）。
func newIdentityContext(t *testing.T) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{
		Method:       "POST",
		Path:         "/v1/messages",
		ProtocolFrom: egress.FamilyAnthropicMessages,
		Logger:       quietLogger(),
	})
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	pc.SetAuth(pctx.AuthState{KeyID: 42, APIKey: "sk-identity"})
	return pc
}

// TestSessionIdentityColumns 钉住四条分支的取值。
//
// 语义（Node 的 getSessionIdentityMetadata + message-service 落库）：
// 忽略客户端会话 id 且亲和身份存在 → prefix_affinity（跨会话复用同一渠道）；
// 否则一律 session_id（新会话新渠道）；无会话 id 时形制仍是 session_id、身份留空。
func TestSessionIdentityColumns(t *testing.T) {
	cases := []struct {
		name               string
		sessionID          string
		ignoreClientSessID bool
		withAffinity       bool
		wantIdentity       string
		wantKind           string
	}{
		{
			name:         "无会话 id：形制仍为 session_id、身份留空（Node 的兜底元数据）",
			sessionID:    "",
			wantIdentity: "",
			wantKind:     sessionIdentityKindSessionID,
		},
		{
			name:               "平文会话 id 且不忽略：身份原样落库",
			sessionID:          "plain-session-id",
			ignoreClientSessID: false,
			wantIdentity:       "plain-session-id",
			wantKind:           sessionIdentityKindSessionID,
		},
		{
			name:               "忽略 + 亲和身份存在：记 prefix_affinity，身份为 pfx:scope:fp",
			sessionID:          "plain-session-id",
			ignoreClientSessID: true,
			withAffinity:       true,
			wantIdentity:       "pfx:scope-tag-1:fp-tip-1",
			wantKind:           sessionIdentityKindPrefixAffinity,
		},
		{
			name:               "忽略但无亲和身份（未启用/不可指纹化）：回落 session_id",
			sessionID:          "plain-session-id",
			ignoreClientSessID: true,
			withAffinity:       false,
			wantIdentity:       "plain-session-id",
			wantKind:           sessionIdentityKindSessionID,
		},
		{
			name:               "有亲和身份但不忽略：Node 的 skipSessionBinding 为假 → session_id",
			sessionID:          "plain-session-id",
			ignoreClientSessID: false,
			withAffinity:       true,
			wantIdentity:       "plain-session-id",
			wantKind:           sessionIdentityKindSessionID,
		},
		{
			name:               "pfx: 前缀的会话 id：走摘要（不把上游命名空间明文落库）",
			sessionID:          "pfx:sc:fp",
			ignoreClientSessID: false,
			wantIdentity:       "sid:ec949408ac29ad888a8c0ebeabeb1a7a",
			wantKind:           sessionIdentityKindSessionID,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := newIdentityContext(t)
			if tc.withAffinity {
				pc.SetAffinityIdentity("scope-tag-1", "fp-tip-1")
			}
			identity, kind := sessionIdentityColumns(tc.sessionID, 42, pc, tc.ignoreClientSessID)
			if identity != tc.wantIdentity {
				t.Fatalf("identity = %q，期望 %q", identity, tc.wantIdentity)
			}
			if kind != tc.wantKind {
				t.Fatalf("kind = %q，期望 %q", kind, tc.wantKind)
			}
		})
	}
}

// TestBuildPublicSessionIdentityMatchesNode 用 **Node 原函数**产出的黄金值钉住摘要口径。
//
// 黄金值由 `bun -e "import {buildPublicSessionIdentity} from './src/lib/request-identity.ts'"` 实跑得到：
//
//	"pfx:sc:fp"        42         => sid:ec949408ac29ad888a8c0ebeabeb1a7a
//	"sid:abc"          7          => sid:1eb80332d1ead7c1bfddd1d24c11fc5e
//	"plain-session-id" 99         => plain-session-id
//	"pfx:x:y"          "unbound"  => sid:afdaa0a829b3c0477049f00bf6b90951
//
// 最后一条即 keyID 为 0 时 Node 的 `keyId ?? "unbound"` 分支。
func TestBuildPublicSessionIdentityMatchesNode(t *testing.T) {
	cases := []struct {
		sessionID string
		keyID     int64
		want      string
	}{
		{"pfx:sc:fp", 42, "sid:ec949408ac29ad888a8c0ebeabeb1a7a"},
		{"sid:abc", 7, "sid:1eb80332d1ead7c1bfddd1d24c11fc5e"},
		{"plain-session-id", 99, "plain-session-id"},
		{"", 1, ""},
		{"pfx:x:y", 0, "sid:afdaa0a829b3c0477049f00bf6b90951"},
	}
	for _, tc := range cases {
		if got := buildPublicSessionIdentity(tc.sessionID, tc.keyID); got != tc.want {
			t.Fatalf("buildPublicSessionIdentity(%q, %d) = %q，期望 %q", tc.sessionID, tc.keyID, got, tc.want)
		}
	}
}

// TestAffinityIdentitySlotIsPerRequest 钉住身份槽位的「未装即未参与」语义：
// 不装时第二个返回值为 false（调用方据此写 session_id，而不是猜测）。
func TestAffinityIdentitySlotIsPerRequest(t *testing.T) {
	pc := newIdentityContext(t)
	if _, ok := pc.AffinityIdentity(); ok {
		t.Fatal("未装身份时不应报告已参与")
	}
	pc.SetAffinityIdentity("sc", "fp")
	aff, ok := pc.AffinityIdentity()
	if !ok || aff.ScopeTag != "sc" || aff.Fingerprint != "fp" {
		t.Fatalf("装入后应取回同一事实，得到 %+v ok=%v", aff, ok)
	}
	// 空 scopeTag 视为未参与（防空值把日志写成 prefix_affinity）。
	pc2 := newIdentityContext(t)
	pc2.SetAffinityIdentity("", "fp")
	if _, ok := pc2.AffinityIdentity(); ok {
		t.Fatal("空 scopeTag 不应被当作已参与")
	}
}
