package terminal

import (
	"context"
	"time"
)

// 本文件是低速样本写入的**终态旁路缝**：终态提交之后，把这次请求的速率事实交出去，
// 由 slowrate 包折算成慢样本、推进状态、必要时写会话冷却键（对应设计稿 §4 的写入点）。
//
// 为什么是「缝」而不是直接调 slowrate：terminal 不得 import slowrate——后者要用
// session.ProviderCooldownKey，而 session 依赖 guard，guard 依赖 terminal，直接引用即成环。
// 故本包只定义中性接口与一次调用点，实现在 slowrate 包，装配在 dataplane。

// SlowRateSample 是一次终态的速率事实（本包的中性视图）。
//
// 字段刻意只带「事实」：判据（状态码、样本下限、清洗、与基线比较）全在 slowrate 包，
// 本包不做任何判定，否则判定逻辑会分叉成两处。
type SlowRateSample struct {
	// ProviderID 是实际作答的供应商（行级 provider_id 口径，与 resolveSettlementProviderID 同值）。
	ProviderID int64
	// SessionID 与 KeyID 供会话级冷却键使用；SessionID 为空表示本次没有会话身份。
	SessionID string
	KeyID     int64
	// ModelKey 是跨供应商别名归一后的模型键；空串表示无法归一时不采样。
	//
	// 为什么由调用方算好：归一口径属 public-status（ResolveSuccessRateModelKey），
	// 本包与 slowrate 都不该复制一份。
	ModelKey string
	// RequestID 是请求日志行 id，用作滑窗 ZSET 成员（同一请求只入窗一次）。
	RequestID    int64
	StatusCode   int
	OutputTokens *int64
	DurationMS   *int
	FirstByteMS  *int
}

// SlowRateRecorder 是低速样本的接收面。
//
// 与 RollupRecorder / LeaseSettler 同构的三条约束：
//
//  1. **失败不得影响结算**：接口没有返回值，实现必须自带降级与日志。旁路失败不影响账务，
//     调用方也无所补救（设计稿 §4：样本与行同生同灭，丢了不影响钱）。
//  2. **零开销闸门在实现里**：未开启监控的渠道必须在此返回，不做任何 Redis 读写
//     （设计稿 §8 的「未开启渠道逐请求开销严格为零」是设计约束，不是优化）。
//  3. **时机：终态提交之后**，与 affinity 写回同规矩——未提交就写样本会把「本次并没真正
//     服务成功的速率」记进基线。
//
// 改道计数**不走本接口**：它计的是「被挤掉的那家」而非作答的那家，且 503 路径没有作答者，
// 故它有独立的事实与闸门（见 SlowDivert 与 SetSlowDivertRecorder）。
type SlowRateRecorder interface {
	RecordSlowRate(ctx context.Context, sample SlowRateSample)
}

// SlowDivert 是一次「因低速被改道」的事实：某渠道本会（或本可能）被选中，因低速机制而没轮到。
//
// ProviderID 是**被挤掉的那家**（不是作答的那家）：计数键按它分桶，因为运维要回答的是
// 「这家渠道被低速机制压掉了多少流量」。
//
// Cause 的取值域属 route（DivertCauseCooldown / DivertCausePenalty）；本包只当不透明字符串
// 传递，**不引入 route 类型**（terminal 不 import route，与 AffinityWriteback 同一分层约束）。
type SlowDivert struct {
	ProviderID int64
	Cause      string
}

// SlowDivertRecorder 是低速改道计数的接收面。
//
// 与 SlowRateRecorder 同三条约束（失败不影响结算、自降级、终态之后）。
type SlowDivertRecorder interface {
	RecordSlowDiverts(ctx context.Context, diverts []SlowDivert)
}

// recordSlowRate 是旁路的唯一执行点。未装配（nil）时整段跳过，行为与接线前逐字一致。
//
// 超时自建 `context.WithoutCancel`：异步写模式下本函数由队列 worker 在请求返回之后调用，
// 此刻请求 ctx 已取消（与 affinity 写回同一个坑，见 SettleContext 的注释）。
func (s *Settler) recordSlowRate(ctx context.Context, sample SlowRateSample) {
	if s.slowRate == nil || sample.ProviderID <= 0 || sample.ModelKey == "" {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slowRateTimeout)
	defer cancel()
	s.slowRate.RecordSlowRate(writeCtx, sample)
}

// recordSlowRateCommitted 是「仅终态提交后才采样」的闸门。
//
// 为何要 committed：未赢得终态的尝试（竞速输家、重试失败的那一次）同样带着速率事实，
// 但它的 duration 含重试与切家的额外开销，把它计入基线会系统性压低基线（与 Node 的
// postTerminalSideEffects 同一考虑：只有真正入账的那一次才是真实用户体验）。
func (s *Settler) recordSlowRateCommitted(ctx context.Context, sample SlowRateSample, committed bool) {
	if !committed {
		return
	}
	s.recordSlowRate(ctx, sample)
}

// slowRateTimeout 是旁路写入的上界。
//
// 取 3 秒：与 rollupTimeout 同量级（Redis 默认命令超时），远小于请求超时。即使 Redis 半死，
// 也只让这条旁路多等一小段——结算本身已经完成，这里只是它之后的副作用。
const slowRateTimeout = 3 * time.Second

// recordSlowDiverts 是改道计数的唯一执行点。未装配（nil）或本次无改道时整段跳过。
//
// **不受终态提交闸门约束**（与 recordSlowRateCommitted 相反）：无可用供应商那条 503 路径
// 恰恰是最该被计的形态（唯一候选被会话冷却剔掉），而它settle 之后本就没有可提交的账务事实。
// 与 affinity 墓碑同一口径：不依赖是否赢得终态。
func (s *Settler) recordSlowDiverts(ctx context.Context, diverts []SlowDivert) {
	if s.slowDiverts == nil || len(diverts) == 0 {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slowRateTimeout)
	defer cancel()
	s.slowDiverts.RecordSlowDiverts(writeCtx, diverts)
}
