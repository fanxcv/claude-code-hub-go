package route

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「亲和总闸与模式开关是**逐请求**读的」。
//
// 为什么必须单独钉：设计稿 §7 把管理面 affinityIgnoreClientSessionId 明定为**配置回滚手段**，
// 总闸同样可在运行时改。构造期快照会让「管理面返回 200 且广播失效、运行中选路却不变」——
// 运维据此回滚必然失败，而界面上一切正常。判据只能是「同一进程、同一选器，改了开关之后
// 下一次选路的结果就变」。

// TestAffinitySwitchesReadPerRequest 主线：同一个 selector 连续三次选路，只改开关返回值。
func TestAffinitySwitchesReadPerRequest(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)
	switches := AffinitySwitches{Enabled: true}
	selector := NewSelector(Options{
		Source:           &stubSource{providers: []Provider{bound, other}, byID: map[int64]Provider{7: bound, 8: other}},
		Affinity:         NewAffinityStore(AffinityOptions{Window: 8}),
		AffinitySwitches: func(context.Context) AffinitySwitches { return switches },
		Rand:             (&scriptedRand{values: []float64{0}}).next,
	})

	// 请求同时带「会话绑定指向 7」与「已完成的亲和查找（hint 也指向 7）」：两条短路路径
	// 都可命中，故结果能区分「走了哪一层」。
	request, _ := affinityNominationRequest(t)
	request.SessionID = "sess_switch"
	request.SessionBinding = &SessionBindingSnapshot{
		SessionID: "sess_switch", KeyID: 42, Generation: "g", ProviderID: bound.ID,
	}

	selectMethod := func(t *testing.T) SelectionMethod {
		t.Helper()
		result, err := selector.Select(context.Background(), request)
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		if result.Provider == nil {
			t.Fatal("应选中枢，实际无候选")
		}
		return result.Method
	}

	if got := selectMethod(t); got != MethodSessionReuse {
		t.Fatalf("总闸开且不强制前缀时应走会话绑定，实际 %q", got)
	}

	// 只改模式开关（模拟管理面把 affinityIgnoreClientSessionId 置真）：下一次必须改走前缀层。
	switches.ForcePrefix = true
	if got := selectMethod(t); got != MethodPrefixAffinity {
		t.Fatalf("模式改成强制前缀后应走前缀层（会话绑定层整层跳过），实际 %q", got)
	}

	// 只改总闸：两层都不进，退回加权随机。
	switches = AffinitySwitches{Enabled: false}
	if got := selectMethod(t); got == MethodSessionReuse || got == MethodPrefixAffinity {
		t.Fatalf("总闸关时两层都不得参与，实际 %q", got)
	}
}

// TestAffinitySwitchesFallBackToStaticOption 未注入逐请求读取面时回落到静态值
// （接线前的行为）：既有测试与无请求上下文的调用方（模拟器）靠它。
func TestAffinitySwitchesFallBackToStaticOption(t *testing.T) {
	selector := NewSelector(Options{AffinityIgnoreClientSessionID: true})
	if got := selector.affinitySwitches(context.Background()); !got.Enabled || !got.ForcePrefix {
		t.Fatalf("静态回落 = %+v，期望总闸开且强制前缀", got)
	}
}
