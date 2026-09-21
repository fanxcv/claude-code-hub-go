package terminal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住会话绑定写回在**两条终态路径**上的发放。它与亲和写回同纪律，却是另一套键、
// 另一套语义（CAS 指向 winner / 失败写会话冷却），故另立替身与用例。
//
// 为什么必须有它：生产走异步写模式（MESSAGE_REQUEST_WRITE_MODE=async），而会话绑定写回
// 原先只在同步路径的 affinityWriteback 里发放 ⇒ 生产上**一次都没调过**：绑定键永远没有
// 胜出渠道，选路层的会话提名恒放弃（session_reuse 恒 0），粘性静默失效。
//
// 该缺陷的特征是「配置面、单测、日志全都看不见」：写回不发放不报错、不写日志，
// 只有对比「选了哪家」与「回访了哪家」才看得出。故这里按路径×结局列全，缺一即红。

// sessionBindingRecorder 是 pctx.SessionBindingWriteback 的最小替身：只记事件与参数。
type sessionBindingRecorder struct {
	mu          sync.Mutex
	events      []string
	casIDs      []int64
	cooldownIDs []int64
	clearIDs    []int64
	ctxErrs     []error
}

func (r *sessionBindingRecorder) CompareAndSet(ctx context.Context, providerID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "binding_cas")
	r.casIDs = append(r.casIDs, providerID)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	return true
}

func (r *sessionBindingRecorder) CooldownOnFailure(ctx context.Context, providerID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "binding_cooldown")
	r.cooldownIDs = append(r.cooldownIDs, providerID)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	return true
}

func (r *sessionBindingRecorder) ClearBinding(ctx context.Context, providerID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "binding_clear")
	r.clearIDs = append(r.clearIDs, providerID)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	return true
}

// clearedIDs 单开一个取数口，不改 snapshot 的返回形状（它有 11 处调用点）。
func (r *sessionBindingRecorder) clearedIDs() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int64(nil), r.clearIDs...)
}

func (r *sessionBindingRecorder) snapshot() (events []string, casIDs []int64, cooldownIDs []int64, ctxErrs []error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...),
		append([]int64(nil), r.casIDs...),
		append([]int64(nil), r.cooldownIDs...),
		append([]error(nil), r.ctxErrs...)
}

// newSessionBindingContext 造一个带行标识与绑定写回替身的请求上下文；recorder 为 nil 表示
// 本次请求**没有**会话绑定写回能力（无会话身份或会话包未接线）。
func newSessionBindingContext(t *testing.T, recorder pctx.SessionBindingWriteback, rowID int64) *pctx.Context {
	t.Helper()
	pc := newTestContext(t)
	if err := pc.SetMessageRequestID(rowID); err != nil {
		t.Fatalf("写入行标识失败: %v", err)
	}
	if recorder != nil {
		pc.SetSessionBindingWriteback(recorder)
	}
	return pc
}

// flushQueue 冲一次队列并等待批次落库。
func flushQueue(t *testing.T, queue *WriteQueue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.Flush(ctx); err != nil {
		t.Fatalf("冲队列失败: %v", err)
	}
}

// 异步 + 提交成功：会话绑定 CAS 必须在 flush 之后发出。这是本缺陷的正面钉子——
// 摘掉异步闭包里的那一次发放，本用例即红（生产症状就是这里一次都不发）。
func TestAsyncSessionBindingWinnerWrittenAfterCommit(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 8, FlushInterval: time.Hour,
	})
	recorder := &sessionBindingRecorder{}
	pc := newSessionBindingContext(t, recorder, 77)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{WinnerProviderID: 7}
	result, err := settler.SettleContext(context.Background(), pc, settlement, nil)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if !result.Queued {
		t.Fatalf("异步模式下终态应先入队：%+v", result)
	}
	if events, _, _, _ := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("提交结论出来之前不得写会话绑定，收到 %v", events)
	}

	flushQueue(t, queue)

	events, casIDs, _, ctxErrs := recorder.snapshot()
	if len(events) != 1 || events[0] != "binding_cas" {
		t.Fatalf("终态提交后必须写会话绑定 CAS，收到 %v。"+
			"该调用原先只在同步路径发放，异步（生产）路径一次都不发——"+
			"症状是绑定无胜出渠道、会话提名恒放弃", events)
	}
	if casIDs[0] != 7 {
		t.Fatalf("CAS 指向的供应商 = %d，期望 7", casIDs[0])
	}
	if ctxErrs[0] != nil {
		t.Fatalf("写回不得携带已取消的请求上下文（真实 Redis CAS 会静默失败）：%v", ctxErrs[0])
	}
}

// 异步 + 供应商侧失败：冷却与墓碑同机，**入队处立即发出**，且只发一次（不得因 flush 再发）。
func TestAsyncSessionBindingFailureFiresBeforeFlush(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 8, FlushInterval: time.Hour,
	})
	recorder := &sessionBindingRecorder{}
	pc := newSessionBindingContext(t, recorder, 66)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{TombstoneProviderID: 9}
	if _, err := settler.SettleContext(context.Background(), pc, settlement, nil); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	events, _, cooldownIDs, _ := recorder.snapshot()
	if len(events) != 1 || events[0] != "binding_cooldown" {
		t.Fatalf("失败侧冷却应与墓碑同机、在入队处立即发出，收到 %v", events)
	}
	if cooldownIDs[0] != 9 {
		t.Fatalf("冷却指向的供应商 = %d，期望 9", cooldownIDs[0])
	}

	flushQueue(t, queue)

	events, _, _, _ = recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("冷却只应发放一次（异步两条路径不得重复发放），收到 %v", events)
	}
}

// 异步 + 未赢得终态：不得写 CAS——否则会把粘性指向一个本次并未真正服务成功的供应商。
func TestAsyncSessionBindingWinnerSkippedWhenNotCommitted(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 1, FlushInterval: time.Hour,
	})
	recorder := &sessionBindingRecorder{}
	pc := newSessionBindingContext(t, recorder, 55)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{WinnerProviderID: 5}
	if _, err := settler.SettleContext(context.Background(), pc, settlement, nil); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	flushQueue(t, queue)

	if events, _, _, _ := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("未赢得终态时不得写 CAS，收到 %v", events)
	}
}

// 同步路径（未装配队列）是本缺陷的反向护栏：三种结局各自恰好一次，行为与接线前逐字一致。
func TestSyncSessionBindingWritebackStaysOncePerTerminal(t *testing.T) {
	cases := []struct {
		name      string
		directive AffinityDirective
		committed bool
		want      string
	}{
		{name: "成功且提交", directive: AffinityDirective{WinnerProviderID: 7}, committed: true, want: "binding_cas"},
		{name: "成功但未提交", directive: AffinityDirective{WinnerProviderID: 7}, committed: false, want: ""},
		{name: "失败写冷却", directive: AffinityDirective{TombstoneProviderID: 9}, committed: true, want: "binding_cooldown"},
		{name: "客户端中断只写前缀不发会话动作", directive: AffinityDirective{
			TombstoneProviderID: 9,
			TombstoneKind:       AffinityTombstonePrefixOnly,
		}, committed: true, want: ""},
		{name: "资源类失效只清绑定", directive: AffinityDirective{
			TombstoneProviderID: 9,
			TombstoneKind:       AffinityTombstoneResourceNotFound,
		}, committed: true, want: "binding_clear"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: tc.committed}}}
			recorder := &sessionBindingRecorder{}
			pc := newSessionBindingContext(t, recorder, 44)
			settlement := okSettlement(nil)
			settlement.Affinity = tc.directive
			_, err := New(writer, Options{}).SettleContext(context.Background(), pc, settlement, nil)
			if err != nil && !errors.Is(err, ErrNotSettled) {
				t.Fatalf("同步结算失败: %v", err)
			}
			events, _, _, _ := recorder.snapshot()
			if tc.want == "" {
				if len(events) != 0 {
					t.Fatalf("期望不发动作，收到 %v", events)
				}
				return
			}
			if len(events) != 1 || events[0] != tc.want {
				t.Fatalf("期望恰好一次 %s，收到 %v", tc.want, events)
			}
		})
	}
}

// 异步 + 资源类失效：只清绑定、**不写冷却**（设计稿 §4：模型不支持是配置决策而非故障，
// 写冷却会把「缺模型」记成「慢」，等它补上模型还会白背一段冷却）。
func TestAsyncSessionBindingResourceNotFoundClearsWithoutCooldown(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 8, FlushInterval: time.Hour,
	})
	recorder := &sessionBindingRecorder{}
	pc := newSessionBindingContext(t, recorder, 63)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{
		TombstoneProviderID: 9,
		TombstoneKind:       AffinityTombstoneResourceNotFound,
	}
	if _, err := settler.SettleContext(context.Background(), pc, settlement, nil); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	events, _, _, _ := recorder.snapshot()
	if len(events) != 1 || events[0] != "binding_clear" {
		t.Fatalf("资源类失效应与墓碑同机、在入队处只清绑定，收到 %v", events)
	}

	flushQueue(t, queue)

	events, _, _, _ = recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("清绑定只应发放一次，收到 %v", events)
	}
	if containsEvent(events, "binding_cooldown") {
		t.Fatalf("资源类失效不得写冷却，收到 %v", events)
	}
}

// 异步 + 供应商故障：写冷却。与上一用例成对，证明分流确实按 TombstoneKind 走，
// 而不是「一律清绑定」或「一律冷却」。
func TestAsyncSessionBindingProviderErrorWritesCooldown(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 8, FlushInterval: time.Hour,
	})
	recorder := &sessionBindingRecorder{}
	pc := newSessionBindingContext(t, recorder, 62)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{
		TombstoneProviderID: 9,
		TombstoneKind:       AffinityTombstoneProviderError,
	}
	if _, err := settler.SettleContext(context.Background(), pc, settlement, nil); err != nil {
		t.Fatalf("入队失败: %v", err)
	}

	events, _, cooldownIDs, _ := recorder.snapshot()
	if len(events) != 1 || events[0] != "binding_cooldown" {
		t.Fatalf("供应商故障应写冷却，收到 %v", events)
	}
	if cooldownIDs[0] != 9 {
		t.Fatalf("冷却指向的供应商 = %d，期望 9", cooldownIDs[0])
	}

	flushQueue(t, queue)

	events, _, _, _ = recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("冷却只应发放一次，收到 %v", events)
	}
}

// 未装配写回能力（无会话身份、会话包未接线）时整段跳过：结算照常成立、不 panic。
func TestSessionBindingWritebackSkippedWhenNotWired(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	pc := newSessionBindingContext(t, nil, 33)
	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{WinnerProviderID: 7}
	if _, err := New(writer, Options{}).SettleContext(context.Background(), pc, settlement, nil); err != nil {
		t.Fatalf("未装配写回能力时结算应照常成立，收到 %v", err)
	}
}

// 无请求上下文（Settle 路径）时同样整段跳过：不得因取写回句柄而空指针。
// 异步 + 客户端主动中断（PrefixOnly）：**一个会话动作都不许发**——入队处与 flush 之后都不得有。
//
// 这条钉住 P1：客户端按停不是供应商故障，而这条失败半在 f1ce0bf 之后才在异步（生产）路径
// 首次真正可达。若照默认种类走，用户按一次停就会给一家健康渠道写 60 秒冷却，
// 下一请求无故换家、丢粘性与缓存。
func TestAsyncSessionBindingClientAbortFiresNothing(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 8, FlushInterval: time.Hour,
	})
	recorder := &sessionBindingRecorder{}
	pc := newSessionBindingContext(t, recorder, 61)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{
		TombstoneProviderID: 9,
		TombstoneKind:       AffinityTombstonePrefixOnly,
	}
	if _, err := settler.SettleContext(context.Background(), pc, settlement, nil); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if events, _, _, _ := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("客户端中断不得在入队处发会话动作，收到 %v", events)
	}

	flushQueue(t, queue)

	events, _, cooldownIDs, _ := recorder.snapshot()
	if len(events) != 0 || len(cooldownIDs) != 0 {
		t.Fatalf("客户端中断不得发任何会话动作（flush 后也不得），收到 %v（冷却 %v）", events, cooldownIDs)
	}
	if ids := recorder.clearedIDs(); len(ids) != 0 {
		t.Fatalf("客户端中断不得清绑定，收到 %v", ids)
	}
}

func TestSessionBindingWritebackToleratesNilContext(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{WinnerProviderID: 7, TombstoneProviderID: 9}
	if _, err := New(writer, Options{}).Settle(context.Background(), 21, settlement); err != nil {
		t.Fatalf("无 pctx 的结算应照常成立，收到 %v", err)
	}
}
