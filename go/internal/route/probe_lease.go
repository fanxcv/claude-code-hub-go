package route

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"time"
)

// 本文件是「探针租约」——被隔离的组合只允许**少量试探请求**进入，租约就是那个少量。
//
// 为什么必须是 Redis 上的全局租约而不是进程内计数或随机百分比：本进程可以有多个实例，
// 而「最多 1 个在飞 + 每 30 秒最多 1 个」是**跨实例**的上界；随机百分比既不能限制高 QPS 下
// 的绝对请求数，也管不住跨实例并发（用户 2026-09-22 明确排除该做法）。
//
// 为什么放在本包（route）而不是 slowrate：租约的**消费点**在选路上——命中租约的请求要
// 被定向到被隔离的渠道（绕过隔离排除），这个决定与「谁被隔离」必须在同一处做出，否则会
// 出现「按隔离排除了它、又没有任何人能把请求送进去」的死锁。slowrate 可以 import route，
// 反向成环，故只有本包能同时看见两侧。

// SlowProbeLeaseKey 是该组合的探针租约键（String，SET NX PX）。
//
// 与慢状态同用一个 hash tag：同组合的键落同一槽，便于排障时用一次 SCAN/管道取齐。
func SlowProbeLeaseKey(providerID int64, modelKey string) string {
	return "cch:slowprobe:" + slowRateScopeTag(providerID, modelKey)
}

// slowProbeLeaseTTL 是探针租约的存活期，同时承担用户给的两个上界：
//
//   - 「每 30 秒最多 1 个」——键存在期间不再发放新租约，故两次发放至少间隔一个 TTL；
//   - 「全实例最多 1 个在飞」——同一时刻只有一个持有者。
//
// 为什么用 TTL 而不是终态显式释放：显式释放要求把租约身份从选路层一路带到终态层
// （pctx 槽位 + 守卫适配器一行），那是跨 lane 的改动；而 TTL 对**所有异常路径**
// （连接中断、上游报错、请求取消、进程崩溃）一律有效——这正是「包括异常路径都要释放」
// 想要的语义。代价是探针节奏等于 TTL 而非「上一个完成即下一个」（8 秒结束的探针也占满
// 30 秒），已记入报告的待改进项。
const slowProbeLeaseTTL = 30 * time.Second

// SlowProbeGrant 是一次成功的探针租约授予。
type SlowProbeGrant struct {
	// ProviderID 与 ModelKey 是被探的组合。
	ProviderID int64
	ModelKey   string
	// Holder 是持有者标识（请求身份哈希），写进租约值供排障辨识是谁占着。
	Holder string
}

// AcquireSlowProbe 为某组合尝试取得探针租约；未取得（已有持有者、或 Redis 故障）时返回 nil。
//
// 失败一律 fail-open：取不到租约只是「这次不探」，绝不能把请求变成失败；但必须**可观测**——
// Redis 故障时记一条 warn（用户明示要求），否则「探针再也不发」这件事没有任何痕迹。
func (r *SlowRateReader) AcquireSlowProbe(
	ctx context.Context,
	providerID int64,
	modelKey string,
	holder string,
) *SlowProbeGrant {
	if r == nil || r.redis == nil || providerID <= 0 || modelKey == "" {
		return nil
	}
	key := SlowProbeLeaseKey(providerID, modelKey)
	acquired, err := r.redis.SetNX(ctx, key, holder, slowProbeLeaseTTL).Result()
	if err != nil {
		r.warn("route.slow_probe_lease_acquire_failed", modelKey, err)
		return nil
	}
	if !acquired {
		return nil
	}
	return &SlowProbeGrant{ProviderID: providerID, ModelKey: modelKey, Holder: holder}
}

// slowProbeHolder 由请求身份派生租约持有者标识。
//
// 与隔离准入闸门刻意用不同的哈希输入：准入是「这次请求能不能进」，租约是「这次请求能不能当
// 探针」，两者独立，共用输入会让某个渠道的准入结论与探针结论在观测上难以区分。
func slowProbeHolder(keyID int64, sessionID string, now time.Time) string {
	sum := sha256.Sum256([]byte(
		strconv.FormatInt(keyID, 10) + "\x00" + sessionID + "\x00" +
			strconv.FormatInt(now.UnixNano(), 10),
	))
	return hex.EncodeToString(sum[:8])
}

// nominateSlowProbe 尝试为本次请求取得一个探针租约；取得则返回该租约与被探的渠道。
//
// 三个前置条件缺一不可：
//
//  1. 有**仅因隔离**被排除的候选；
//  2. 本次还有**替代候选**（否则软信号 fail-open 已经把它放回 healthy，请求本就会打到它，
//     再占一次租约只是白白消耗探针配额）；
//  3. 读侧观察到该组合的租约空闲（避开每请求一次 SET NX 的写放大）。
//
// 多个被隔离组合时按 id 升序依次尝试，**取第一个成功的**（一个请求只能去一家）。这会带来
// 「id 小的组合优先被探」的偏差，但被探成功的组合会随干净样本逐档解除隔离，不会永久饿死。
func (s *Selector) nominateSlowProbe(
	ctx context.Context,
	req Request,
	filtered filterResult,
	states map[int64]SlowRateQuarantine,
	excluded map[int64]bool,
	now time.Time,
) (*SlowProbeGrant, *Provider) {
	if s.opts.SlowRate == nil || len(filtered.quarantined) == 0 {
		return nil, nil
	}
	alternatives := 0
	for _, provider := range filtered.healthy {
		if !excluded[provider.ID] {
			alternatives++
		}
	}
	if alternatives == 0 {
		return nil, nil
	}
	modelKey := SlowRateModelKey(req.Model)
	if modelKey == "" {
		return nil, nil
	}
	targets := make([]Provider, len(filtered.quarantined))
	copy(targets, filtered.quarantined)
	slices.SortFunc(targets, func(left, right Provider) int { return int(left.ID - right.ID) })
	holder := slowProbeHolder(req.KeyID, req.SessionID, now)
	for index := range targets {
		state, ok := states[targets[index].ID]
		if !ok || !state.ProbeLeaseFree {
			continue
		}
		if grant := s.opts.SlowRate.AcquireSlowProbe(ctx, targets[index].ID, modelKey, holder); grant != nil {
			provider := targets[index]
			return grant, &provider
		}
	}
	return nil, nil
}
