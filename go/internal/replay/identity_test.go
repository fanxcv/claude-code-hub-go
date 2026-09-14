package replay

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 构造带鉴权、带指定逻辑请求体的上下文。
func identityContext(t *testing.T, method, path string, headers map[string]string, message map[string]any, apiKey string) *pctx.Context {
	t.Helper()
	header := http.Header{}
	for key, value := range headers {
		header.Set(key, value)
	}
	init := pctx.Init{Method: method, Path: path, Headers: header}
	if message != nil {
		encoded := mustJSON(message)
		init.Body = io.NopCloser(strings.NewReader(string(encoded)))
	}
	ctx, err := pctx.New(init)
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	if apiKey != "" {
		ctx.SetAuth(pctx.AuthState{KeyID: 7, UserID: 3, APIKey: apiKey})
	}
	return ctx
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

// TestDeriveIdentityDeterministic：相同输入 -> 相同身份；正文变化 -> 身份变化。
func TestDeriveIdentityDeterministic(t *testing.T) {
	message := map[string]any{"model": "gpt-test", "stream": true, "messages": []any{
		map[string]any{"role": "user", "content": "你好"},
	}}
	ctx := identityContext(t, "POST", "/v1/messages", map[string]string{"idempotency-key": "ik-1"}, message, "sk-test")

	first, err := DeriveIdentity(ctx, mustJSON(message), "anthropic")
	if err != nil || first == nil {
		t.Fatalf("推导失败: %v", err)
	}
	second, err := DeriveIdentity(ctx, mustJSON(message), "anthropic")
	if err != nil || second == nil {
		t.Fatalf("二次推导失败: %v", err)
	}
	if first.ReplayID != second.ReplayID || first.Verifier != second.Verifier {
		t.Fatalf("确定性身份被破坏: %s != %s", first.ReplayID, second.ReplayID)
	}
	if len(first.ReplayID) != 32 || len(first.Verifier) != 32 || len(first.ScopeTag) != 16 {
		t.Fatalf("身份长度不对: %d/%d/%d", len(first.ReplayID), len(first.Verifier), len(first.ScopeTag))
	}
	if first.KeyID != 7 || first.UserID != 3 || first.Model != "gpt-test" || first.Endpoint != "/v1/messages" {
		t.Fatalf("身份字段不对: %+v", first)
	}

	changed := map[string]any{"model": "gpt-test", "stream": true, "messages": []any{
		map[string]any{"role": "user", "content": "再见"},
	}}
	third, err := DeriveIdentity(ctx, mustJSON(changed), "anthropic")
	if err != nil || third == nil {
		t.Fatalf("变更推导失败: %v", err)
	}
	if third.ReplayID == first.ReplayID {
		t.Fatal("正文变化后身份必须变化")
	}
}

// TestDeriveIdentityKeyOrderIndependent：同对象不同键序 -> 同身份。
func TestDeriveIdentityKeyOrderIndependent(t *testing.T) {
	a := []byte(`{"stream":true,"model":"m","messages":[{"role":"user","content":"x"}]}`)
	b := []byte(`{"messages":[{"content":"x","role":"user"}],"model":"m","stream":true}`)
	ctxA := identityContext(t, "POST", "/v1/messages", nil, nil, "sk-1")
	ctxB := identityContext(t, "POST", "/v1/messages", nil, nil, "sk-1")

	idA, err := DeriveIdentity(ctxA, a, "anthropic")
	if err != nil || idA == nil {
		t.Fatalf("A 推导失败: %v", err)
	}
	idB, err := DeriveIdentity(ctxB, b, "anthropic")
	if err != nil || idB == nil {
		t.Fatalf("B 推导失败: %v", err)
	}
	if idA.ReplayID != idB.ReplayID || idA.Verifier != idB.Verifier {
		t.Fatalf("键序不应影响身份: %s != %s", idA.ReplayID, idB.ReplayID)
	}
}

// TestDeriveIdentityRejects：非 POST / 非流式 / 无鉴权主体验证失败路径。
func TestDeriveIdentityRejects(t *testing.T) {
	message := map[string]any{"stream": true, "model": "m"}

	if id, _ := DeriveIdentity(identityContext(t, "GET", "/v1/messages", nil, message, "sk-1"), mustJSON(message), "anthropic"); id != nil {
		t.Fatal("GET 不得推导身份")
	}
	nonStream := map[string]any{"stream": false, "model": "m"}
	if id, _ := DeriveIdentity(identityContext(t, "POST", "/v1/messages", nil, nonStream, "sk-1"), mustJSON(nonStream), "anthropic"); id != nil {
		t.Fatal("非流式不得推导身份")
	}
	if id, _ := DeriveIdentity(identityContext(t, "POST", "/v1/messages", nil, message, ""), mustJSON(message), "anthropic"); id != nil {
		t.Fatal("缺鉴权主体不得推导身份")
	}
	if id, _ := DeriveIdentity(nil, mustJSON(message), "anthropic"); id != nil {
		t.Fatal("nil 上下文不得推导身份")
	}
}

// TestStableStringifyNodeParity：字符串转义与 Node JSON.stringify 逐字节一致
// （encoding/json 会把 < > & HTML 转义，Node 不会——身份哈希必须钉住 Node 语义）。
func TestStableStringifyNodeParity(t *testing.T) {
	cases := []struct {
		input  map[string]any
		expect string
	}{
		{
			map[string]any{"k": `a<b&c`},
			`{"k":"a<b&c"}`,
		},
		{
			map[string]any{"k": "q\"b\\t\nl"},
			"{\"k\":\"q\\\"b\\\\t\\nl\"}",
		},
		{
			map[string]any{"z": int64(1), "a": "x", "n": []any{1, 2}},
			`{"a":"x","n":[1,2],"z":1}`,
		},
	}
	for _, tc := range cases {
		if got := stableStringify(tc.input); got != tc.expect {
			t.Fatalf("stableStringify(%v) = %s, 期望 %s", tc.input, got, tc.expect)
		}
	}
}

// TestBypassHeaderConstant：常量与 Node 一致（切换期间两侧共用同一请求头）。
func TestBypassHeaderConstant(t *testing.T) {
	if BypassHeader != "x-cch-no-replay" {
		t.Fatalf("BypassHeader = %q", BypassHeader)
	}
}
