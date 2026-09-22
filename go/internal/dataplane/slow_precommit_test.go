package dataplane

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件钉「提交前判废 ⇒ 标慢」的事实归集：只认 ProbeSlow、按渠道去重、作用域与速率样本同源。
//
// 闭环背景：判废事实只挂在尝试留痕上（forward.AttemptOutcome.ProbeSlow），而速率样本只采
// **作答者**。没有这一步，判废后由别家作答成功时被判废的慢家不会被标慢。

func precommitSettler(t *testing.T, requestID int64) (*storeSettler, *pctx.Context) {
	t.Helper()
	pc, err := pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages"})
	if err != nil {
		t.Fatalf("建 pctx 失败: %v", err)
	}
	if requestID > 0 {
		if err := pc.SetMessageRequestID(requestID); err != nil {
			t.Fatalf("写请求行 id 失败: %v", err)
		}
	}
	return &storeSettler{state: &RequestState{Model: "deepseek-v4.1-flash", sessionID: "sess-a"}}, pc
}

// 钉子：只认 ProbeSlow（普通失败不构成「这家慢」），按渠道去重（同一请求对同一家的
// 多次尝试只标一次），且归集的是**被判废的那家**（不是作答的那家）。
func TestSlowPrecommitCollectsProbedProvidersOnly(t *testing.T) {
	settler, pc := precommitSettler(t, 4242)

	got := settler.slowPrecommit(pc, []forward.AttemptOutcome{
		{ProviderID: 167, ProbeSlow: true},
		{ProviderID: 167, ProbeSlow: true}, // 同家第二次尝试 ⇒ 去重
		{ProviderID: 145, StatusCode: 500}, // 普通失败 ⇒ 不记
		{ProviderID: 121, Category: forward.CategoryProviderError},
		{ProviderID: 121, ProbeSlow: true},
	})

	if len(got) != 2 {
		t.Fatalf("应归集 2 家（167 与 121），实际 %d 条：%+v", len(got), got)
	}
	if got[0].ProviderID != 167 || got[1].ProviderID != 121 {
		t.Fatalf("归集顺序应与尝试顺序一致（167、121），实际 %d、%d", got[0].ProviderID, got[1].ProviderID)
	}
	for _, fact := range got {
		if fact.RequestID != 4242 {
			t.Fatalf("滑窗成员应是请求行 id（幂等键），实际 %d", fact.RequestID)
		}
		if fact.ModelKey == "" {
			t.Fatal("模型键必须与速率样本同源（空则读侧查的不是同一把键）")
		}
		if fact.SessionID != "sess-a" {
			t.Fatalf("会话身份应随事实传递，实际 %q", fact.SessionID)
		}
	}
}

// 钉子：无请求行 id 时整段跳过——它是滑窗成员的幂等键，缺了就无从保证「同一请求只计一次」。
func TestSlowPrecommitSkipsWithoutRowID(t *testing.T) {
	settler, pc := precommitSettler(t, 0)

	if got := settler.slowPrecommit(pc, []forward.AttemptOutcome{{ProviderID: 167, ProbeSlow: true}}); got != nil {
		t.Fatalf("无请求行 id 时不应产出事实，实际 %+v", got)
	}
}

// 钉子：无尝试留痕时零成本跳过（每次终态都会调它）。
func TestSlowPrecommitSkipsWithoutAttempts(t *testing.T) {
	settler, pc := precommitSettler(t, 4242)

	if got := settler.slowPrecommit(pc, nil); got != nil {
		t.Fatalf("无尝试时不应产出事实，实际 %+v", got)
	}
}

// 源码结构性钉子：断言「判废事实真的从尝试留痕接到了终态旁路」。
//
// 为什么必须有它：slowPrecommit 本身有单测、slowrate.RecordPrecommit 也有单测，但
// 「settlement 到底有没有把这份事实交给结算器」谁都盖不住——摘掉接线时上面两组用例全绿，
// 闭环却依旧是断的。这正是本仓反复出现的「已定义≠已接线」盲区（同类做法见
// slow_probe_wiring_nail_test.go）。
//
// 必须**两条终态路径各一次**：闸门跑在 forward 的尝试处理里，非流形态（JSON）被 gated/ForceGate
// 命中时同样会走到探测，故只接流式会让半数路径静默漏标。
var slowPrecommitCollectorWiring = regexp.MustCompile(
	`(?m)^\s*settlement\.SlowPrecommit = s\.slowPrecommit\(pc, (result|outcome)\.Attempts\)\s*$`)

func TestBothTerminalPathsHandPrecommitFactsToSettlement(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("upstream.go"))
	if err != nil {
		t.Fatalf("读取 upstream.go 失败：%v", err)
	}
	matched := slowPrecommitCollectorWiring.FindAllString(string(source), -1)
	if len(matched) != 2 {
		t.Fatalf("upstream.go 应有 2 处「尝试留痕 → settlement.SlowPrecommit」接线（非流式与流式各一处），"+
			"实际 %d 处：%v\n  缺失时判废事实永不进入低速状态，闭环静默断裂。", len(matched), matched)
	}
}

// 第二段钉子：结算器侧必须真的把 settlement 里的这份事实发放给旁路（否则第一段是空转）。
var slowPrecommitDispatch = regexp.MustCompile(
	`(?m)^\s*s\.recordSlowPrecommit\(ctx, settlement\.SlowPrecommit\)\s*$`)

func TestSettlerDispatchesPrecommitFacts(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "terminal", "settle.go"))
	if err != nil {
		t.Fatalf("读取 terminal/settle.go 失败：%v", err)
	}
	matched := slowPrecommitDispatch.FindAllString(string(source), -1)
	if len(matched) != 2 {
		t.Fatalf("terminal/settle.go 应有 2 处 recordSlowPrecommit 发放（入队分支与同步分支各一处），实际 %d 处",
			len(matched))
	}
}

// 事实类型必须与 terminal 侧同一份（防两处各定义一份、字段悄悄漂移）。
func TestSlowPrecommitFactTypeIsTerminalOwned(t *testing.T) {
	var fact terminal.SlowPrecommit
	fact.ProviderID = 1
	fact.ModelKey = "m"
	fact.RequestID = 2
	fact.SessionID = "s"
	fact.KeyID = 3
	if fact.ProviderID != 1 || fact.ModelKey != "m" || fact.RequestID != 2 || fact.SessionID != "s" || fact.KeyID != 3 {
		t.Fatal("terminal.SlowPrecommit 字段应逐字可用（本断言只是编译期与赋值期钉子）")
	}
}
