package route_test

import (
	"os"
	"regexp"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/slowrate"
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

// TestCooldownMarkerMirrorsWriteSide 钉住冷却键的**值**标记逐字节一致。
//
// 为何本仓需要这条：同一个冷却键有两个写入者，读侧只能按值把它们分开——低速写侧写标记
// `slow`（受低速监控开关约束），绑定写侧写下一代 generation（正整数字符串，**不受**该开关
// 约束，见 `route.CooldownKind`）。读侧把「非标记值」一律算故障冷却，故**标记值一旦在写侧
// 被改名而读侧没跟上，低速冷却就会被误判成故障冷却**——表现是「关掉监控后低速冷却仍生效」，
// 静默、无报错。
//
// 为何是源码结构性钉子而不是常量比对：`slowrate` 没有导出这个标记（写侧是 `writeCooldown`
// 里的字面量），而本包不允许 import `slowrate`（成环）。只能读它的源码，把「那一处确实是这个
// 值」钉住——与键形制那几条同一道防线，只是没法用常量对照。
//
// 正则容许 gofmt 换行（`r.redis.Set(` 与参数之间可能断行），但不容许值本身不同。
func TestCooldownMarkerMirrorsWriteSide(t *testing.T) {
	const path = "../slowrate/recorder.go"
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	pattern := regexp.MustCompile(
		`r\.redis\.Set\(\s*ctx,\s*key,\s*"` + regexp.QuoteMeta(route.SlowRateCooldownMarker) + `"\s*,`)
	if !pattern.Match(source) {
		t.Fatalf("低速写侧写的冷却标记与读侧常量不一致：\n"+
			"  %s 里找不到形如 r.redis.Set(ctx, key, %q, ...) 的写入\n"+
			"  读侧常量 route.SlowRateCooldownMarker = %q\n"+
			"  两侧不一致时，低速冷却会被读侧误判成故障冷却（关掉监控仍生效）。",
			path, route.SlowRateCooldownMarker, route.SlowRateCooldownMarker)
	}
}

// TestSamplesKeyMirrorsWriteSide 钉住慢样本滑窗键逐字节一致。
//
// 读侧（route）因 import 环只能自拼这个键，而它现在是**惩罚的唯一真源**：读侧据窗内活成员
// 数当场派生惩罚。拼错一个字符的表现是「窗永远为空 ⇒ 惩罚恒为 0」——静默、无报错。
func TestSamplesKeyMirrorsWriteSide(t *testing.T) {
	for _, tc := range []struct {
		providerID int64
		modelKey   string
	}{
		{167, "deepseek-v4.1-flash"},
		{1, "claude-opus-5"},
		{999, "m"},
	} {
		want := slowrate.SamplesKey(tc.providerID, tc.modelKey)
		got := route.SlowRateSamplesKey(tc.providerID, tc.modelKey)
		if got != want {
			t.Errorf("滑窗键不一致：\n  route    = %q\n  slowrate = %q", got, want)
		}
	}
}

// TestStateFieldNamesMirrorWriteSide 钉住状态 Hash 的字段名逐字节一致。
//
// 写侧把**生效参数**（窗长/阈值/步长/上限）随状态一起落 Hash，读侧据它们把滑窗计数折成惩罚。
// 字段名在两侧各写一遍（import 环），任一侧改名都会让读侧判成「参数缺失」——那会静默退化成
// 「回退读快照」，即本次刚修掉的那个 bug。故逐项比对。
func TestStateFieldNamesMirrorWriteSide(t *testing.T) {
	for _, tc := range []struct {
		name      string
		writeSide string
		readSide  string
	}{
		{"penalty", slowrate.StateFieldPenalty, route.SlowRateStateFieldPenalty},
		{"windowSeconds", slowrate.StateFieldWindowSeconds, route.SlowRateStateFieldWindowSeconds},
		{"triggerCount", slowrate.StateFieldTriggerCount, route.SlowRateStateFieldTriggerCount},
		{"penaltyStep", slowrate.StateFieldPenaltyStep, route.SlowRateStateFieldPenaltyStep},
		{"penaltyMax", slowrate.StateFieldPenaltyMax, route.SlowRateStateFieldPenaltyMax},
	} {
		if tc.readSide != tc.writeSide {
			t.Errorf("状态字段 %s 不一致：\n  route    = %q\n  slowrate = %q",
				tc.name, tc.readSide, tc.writeSide)
		}
	}
}
