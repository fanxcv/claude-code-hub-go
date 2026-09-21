package route_test

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件钉住**跨包键形制必须逐字节一致**。这是本仓最容易静默失效的一类接缝：
// 同一段键在两侧各拼一遍，任一侧改了而另一侧没跟上，表现是「写了但读不到」——
// 不报错、不告警，只是功能静默失效。
//
// 为什么必然有两份：`route` 不能 import `slowrate`（slowrate → session → guard → route，成环），
// 也不能 import `session`（同一条环）。只能靠本测试在**外部测试包**里把两者拉到一起比对
// ——外部测试包不受内部 import 环约束。
//
// 为什么能 import jobs：jobs 不依赖 route（已核，无环），且它导出了 BaselineKey，
// 于是「基线键」这一侧有了真实的对照物，而不是我抄一份字面量自己对自己。
func TestBaselineKeyMirrorsWriteSide(t *testing.T) {
	for _, tc := range []struct {
		providerID int64
		modelKey   string
	}{
		{167, "deepseek-v4.1-flash"},
		{1, "claude-opus-5"},
		{999, "m"},
	} {
		want := jobs.BaselineKey(tc.providerID, tc.modelKey)
		got := route.SlowRateBaselineKey(tc.providerID, tc.modelKey)
		if got != want {
			t.Errorf("基线键不一致：\n  route = %q\n  jobs  = %q", got, want)
		}
	}
}

// TestStateKeySharesBaselineScope 钉住 state 与 baseline 共用同一个 hash tag。
//
// 依据：B2 的 samples/state 与 B3 的 baseline 用同一个 scopeTag，多键操作才不会被
// Redis Cluster 以 CROSSSLOT 拒绝。读侧要同时读 state 与 baseline，同槽是它能压进
// 一次 pipeline 的前提。
func TestStateKeySharesBaselineScope(t *testing.T) {
	const (
		providerID = int64(167)
		modelKey   = "deepseek-v4.1-flash"
	)
	baseline := jobs.BaselineKey(providerID, modelKey)
	state := route.SlowRateStateKey(providerID, modelKey)
	// 同 tag 即「去掉各自后缀后的前缀相同」。
	const baselineSuffix = ":baseline"
	tag := baseline[:len(baseline)-len(baselineSuffix)]
	if want := tag + ":state"; state != want {
		t.Errorf("状态键与基线键不同槽：\n  基线 = %q\n  状态 = %q\n  期望 %q", baseline, state, want)
	}
}

// TestCooldownKeyMirrorsSession 钉住冷却键与 session.ProviderCooldownKey 逐字节相同。
//
// 写侧在 internal/slowrate 里**直接调** session.ProviderCooldownKey，而读侧因成环只能自拼。
// 这条断言就是那份自拼的唯一防线。
func TestCooldownKeyMirrorsSession(t *testing.T) {
	for _, tc := range []struct {
		sessionID  string
		keyID      int64
		providerID int64
	}{
		{"sess_muamjk3d_ebc790c9a5c9", 7, 167},
		{"sess_01a0c1e3_359d707a", 16, 1},
	} {
		want := session.ProviderCooldownKey(tc.sessionID, tc.keyID, tc.providerID)
		got := route.SlowRateCooldownKey(tc.sessionID, tc.keyID, tc.providerID)
		if got != want {
			t.Errorf("冷却键不一致：\n  route   = %q\n  session = %q", got, want)
		}
	}
}

// TestModelKeyMirrorsUpstream 钉住归一口径与上游同源。
//
// 读侧（route）、写侧（slowrate 经 dataplane）、基线任务（jobs）必须用同一个归一函数，
// 否则同一模型的不同别名各算一套样本与基线，而界面上看不出「基线为何是空的」。
// pubstatus.ResolveSuccessRateModelKey 是那几处的共同上游，故比对它。
func TestModelKeyMirrorsUpstream(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash", " claude-opus-5 ", "", "dsf4"} {
		trimmed := model
		want := pubstatus.ResolveSuccessRateModelKey(&trimmed, nil)
		if got := route.SlowRateModelKey(model); got != want {
			t.Errorf("模型键不一致：route = %q，pubstatus = %q（输入 %q）", got, want, model)
		}
	}
}
