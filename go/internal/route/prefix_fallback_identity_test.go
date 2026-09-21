package route

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「前缀兜底按**客户端身份是否缺失**判定，而不是按 SessionID 是否为空」。
//
// 为什么必须单独钉：会话包在客户端未带 id 时总会生成或恢复出一个非空 SessionID，于是
// 「SessionID == ""」这个条件在生产链上永不成立——前缀兜底整层死掉。设计稿 §2 把它定义为
// 「无 session id 客户端的兜底」，§9 验收项 2 要求这类请求记 prefix_affinity；少了这条钉子，
// 无 id 的客户端（curl、旧客户端）会静默失去旧版已有的前缀粘性，而所有既有用例照绿。

// TestPrefixFallbackRunsOnlyWhenClientIdentityMissing 主线：四种身份组合各自该不该走前缀层。
func TestPrefixFallbackRunsOnlyWhenClientIdentityMissing(t *testing.T) {
	cases := []struct {
		name       string
		sessionID  string
		identity   SessionIdentity
		wantPrefix bool
	}{
		{name: "客户端带 id", sessionID: "sess_client", identity: SessionIdentityClient, wantPrefix: false},
		{name: "客户端未带、网关生成", sessionID: "sess_gen", identity: SessionIdentityGenerated, wantPrefix: true},
		{name: "客户端未带、按哈希找回", sessionID: "sess_rec", identity: SessionIdentityRecovered, wantPrefix: true},
		{name: "会话包未接线（SessionID 为空）", sessionID: "", identity: SessionIdentityClient, wantPrefix: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			nominated := baseProvider(7, convert.ProviderClaude)
			other := baseProvider(8, convert.ProviderClaude)
			selector := NewSelector(Options{
				Source:   &stubSource{providers: []Provider{nominated, other}, byID: map[int64]Provider{7: nominated, 8: other}},
				Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
				Rand:     (&scriptedRand{values: []float64{0}}).next,
			})

			request, _ := affinityNominationRequest(t)
			request.SessionID = testCase.sessionID
			request.SessionIdentity = testCase.identity

			result, err := selector.Select(context.Background(), request)
			if err != nil {
				t.Fatalf("选路失败: %v", err)
			}
			isPrefix := result.Method == MethodPrefixAffinity
			if isPrefix != testCase.wantPrefix {
				t.Fatalf("method = %q，期望走前缀兜底=%v（SessionID=%q identity=%v）",
					result.Method, testCase.wantPrefix, testCase.sessionID, testCase.identity)
			}
			if isPrefix && (result.Provider == nil || result.Provider.ID != nominated.ID) {
				t.Fatalf("前缀兜底应命中枢 id=7，实际 %+v", result.Provider)
			}
		})
	}
}
