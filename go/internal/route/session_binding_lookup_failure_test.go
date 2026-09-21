package route

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「读绑定行**失败**」与「绑定行**已不存在**」必须分开处置这条契约。
//
// 为什么必须单独钉：两者的返回形态都是 error，但处置相反——读失败是**临时**原因（绑定必须
// 保留、成功侧不得改绑，设计稿 §4「待恢复后仍粘回去」），行不存在是**结构性**失效（旧绑定
// 已死，允许改绑）。压成一支的后果是：一次 DB 抖动就把会话永久搬到备用渠道，且单测、日志、
// 配置面全都看不见——只有对比「本次选了谁」与「绑定还指向谁」才看得出。

// lookupErrorSource 是「按 id 直读会失败」的数据源：列表照常给，只有直读某家时报错。
//
// 为何要单独一个夹具而不扩 stubSource：stubSource 被全包多处用例共用，给它加错误注入会
// 让「哪条用例在测哪条分支」变模糊；本文件的分支判据正是「直读的返回形态」。
type lookupErrorSource struct {
	providers []Provider
	byID      map[int64]Provider
	// err 非 nil 时，Provider 一律返回它（模拟读失败：连接池不可用/超时/快照装载失败）。
	err error
	// nilProvider 为真时，Provider 返回 (nil, nil)（模拟「源里没有这一家」且不报错）。
	nilProvider bool
}

func (s *lookupErrorSource) Providers(context.Context) ([]Provider, error) { return s.providers, nil }

func (s *lookupErrorSource) Provider(_ context.Context, id int64) (*Provider, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.nilProvider {
		return nil, nil
	}
	provider, ok := s.byID[id]
	if !ok {
		return nil, nil
	}
	return &provider, nil
}

func (s *lookupErrorSource) Endpoints(context.Context, int64, convert.ProviderType) ([]Endpoint, error) {
	return nil, nil
}

// newLookupFailureSelector 造一个「绑定指向 7、读 7 会按 source 的形态返回」的选路器。
func newLookupFailureSelector(source Source) *Selector {
	return NewSelector(Options{
		Source:   source,
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})
}

// TestSessionBindingBypassTransientWhenLookupFails 是主线：读绑定行**失败**时本次照常回落选
// 备用，但 bypass 必须判为临时 ⇒ 终态成功侧跳过 CAS ⇒ 绑定仍指向原 provider。
func TestSessionBindingBypassTransientWhenLookupFails(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	backup := baseProvider(8, convert.ProviderClaude)

	selector := newLookupFailureSelector(&lookupErrorSource{
		// 备用排在首位：加权随机的脚本取首家，便于断言「回落到备用」而非恰好抽中绑定家。
		providers: []Provider{backup, bound},
		byID:      map[int64]Provider{7: bound, 8: backup},
		err:       errors.New("lookup: 连接池暂时不可用"),
	})

	result, err := selector.Select(context.Background(), sessionBindingRequest(bound.ID))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == MethodSessionReuse {
		t.Fatal("前置条件不成立：读不到绑定行时不得短路为会话复用")
	}
	if result.Provider == nil || result.Provider.ID != backup.ID {
		t.Fatalf("读不到绑定行时应照常回落选备用，实际 %+v", result.Provider)
	}
	if result.SessionBindingBypass != SessionBindingBypassTransient {
		t.Errorf("bypass = %v，期望 transient（读失败是临时原因，不得改绑）", result.SessionBindingBypass)
	}
	if !result.SessionBindingBypass.KeepsBinding() {
		t.Error("KeepsBinding 应为真：终态成功侧据此跳过 CAS")
	}
}

// TestSessionBindingBypassNoneWhenBindingRowGone 反向：绑定行**已不存在**（StoreSource 译出的
// ErrProviderNotFound）时属结构性失效，必须允许改绑——旧绑定已经死了，新 winner 才是该会话
// 该去的地方；钉住它才能证明「读失败」与「行不存在」真的被分开了。
//
// 没有这条反向，把「任何 error 都判 transient」也能让主线变绿，而后果是会话永远钉在一家
// 已删除的渠道上（绑定再也改不掉）。
func TestSessionBindingBypassNoneWhenBindingRowGone(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	backup := baseProvider(8, convert.ProviderClaude)

	selector := newLookupFailureSelector(&lookupErrorSource{
		// 备用排在首位（同前一条：脚本随机取首家）。
		providers: []Provider{backup, bound},
		byID:      map[int64]Provider{7: bound, 8: backup},
		err:       providerLookupError(bound.ID, store.ErrNotFound),
	})

	result, err := selector.Select(context.Background(), sessionBindingRequest(bound.ID))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Method == MethodSessionReuse {
		t.Fatal("前置条件不成立：读不到绑定行时不得短路为会话复用")
	}
	if result.Provider == nil || result.Provider.ID != backup.ID {
		t.Fatalf("绑定行已不存在时应回落选备用，实际 %+v", result.Provider)
	}
	if result.SessionBindingBypass != SessionBindingBypassNone {
		t.Errorf("bypass = %v，期望 none（行已不存在属结构性失效，允许改绑）", result.SessionBindingBypass)
	}
	if result.SessionBindingBypass.KeepsBinding() {
		t.Error("KeepsBinding 应为假：行已不存在不是临时故障")
	}
}

// TestSessionBindingBypassNoneWhenProviderMissingWithoutError 第三支：源返回 (nil, nil)
// （没有这一家且不报错）同样属结构性，不得判为临时。
func TestSessionBindingBypassNoneWhenProviderMissingWithoutError(t *testing.T) {
	bound := baseProvider(7, convert.ProviderClaude)
	backup := baseProvider(8, convert.ProviderClaude)

	selector := newLookupFailureSelector(&lookupErrorSource{
		providers:   []Provider{bound, backup},
		nilProvider: true,
	})

	result, err := selector.Select(context.Background(), sessionBindingRequest(bound.ID))
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.SessionBindingBypass != SessionBindingBypassNone {
		t.Errorf("bypass = %v，期望 none（源里没有这一家属结构性失效）", result.SessionBindingBypass)
	}
}

// TestProviderLookupErrorPreservesNotFoundSentinel 钉住**真实读取面**的哨兵翻译。
//
// 为何必须钉这一层：上面三条用的是 stub 源，真实读取面（StoreSource）若把哨兵压平成普通
// error，stub 三条照样全绿而生产行为已退回缺陷态（本 lane 修的正是这处压平）。纯函数因此
// 不需要真库即可钉住。
func TestProviderLookupErrorPreservesNotFoundSentinel(t *testing.T) {
	notFound := providerLookupError(7, store.ErrNotFound)
	if !errors.Is(notFound, ErrProviderNotFound) {
		t.Errorf("行不存在必须译成 ErrProviderNotFound（调用方靠 errors.Is 分流），实得 %v", notFound)
	}
	if !errors.Is(notFound, store.ErrNotFound) {
		t.Errorf("包装不应丢掉 store 哨兵（多 %%w），实得 %v", notFound)
	}

	transient := errors.New("lookup: 连接池暂时不可用")
	if got := providerLookupError(7, transient); !errors.Is(got, transient) {
		t.Errorf("读失败必须原样透传，实得 %v", got)
	}
	if errors.Is(providerLookupError(7, transient), ErrProviderNotFound) {
		t.Error("读失败不得被译成「行不存在」——那会让一次瞬时读错永久改绑")
	}

	// 包装哨兵：判据用的是 errors.Is 而非 ==，故读取面把 store.ErrNotFound 再包一层
	// （驱动/连接池包装）时，翻译必须仍认得出它。
	wrapped := providerLookupError(7, fmt.Errorf("pgx: %w", store.ErrNotFound))
	if !errors.Is(wrapped, ErrProviderNotFound) {
		t.Errorf("包装过的行不存在必须仍译成 ErrProviderNotFound，实得 %v", wrapped)
	}

	// 两个哨兵的语义相反，绝不可相等（相等即分派失效）。
	if errors.Is(ErrProviderNotFound, store.ErrNotFound) {
		t.Error("ErrProviderNotFound 与 store.ErrNotFound 不应相等：前者是结构性、后者是存储层事实")
	}
}
