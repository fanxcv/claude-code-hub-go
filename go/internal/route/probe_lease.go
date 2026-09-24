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
// 而「同一组合最多 1 个探针在飞」是**跨实例**的上界；随机百分比既不能限制高 QPS 下的
// 绝对请求数，也管不住跨实例并发（用户 2026-09-22 明确排除该做法）。
//
// 这个上界由三件事共同成立：holder token 写进租约值（排障可见是谁占着）、请求存活期间
// 续租（探针可长达上游拨号超时，只靠固定 TTL 会在飞时先过期）、请求结束做
// compare-and-delete 释放（旧持有者不得删掉新持有者的租约）。固定 TTL 退居崩溃兜底，
// 不再兼任「每 30 秒最多 1 个」的节奏闸门——探针节奏改为「上一个完成即下一个」。
//
// 为什么放在本包（route）而不是 slowrate：租约的**消费点**在选路上——命中租约的请求要
// 被定向到被隔离的渠道（绕过隔离排除），这个决定与「谁被隔离」必须在同一处做出，否则会
// 出现「按隔离排除了它、又没有任何人能把请求送进去」的死锁。slowrate 可以 import route，
// 反向成环，故只有本包能同时看见两侧。

// SlowProbeLeaseKey 是该组合的探针租约键（String，SET NX PX 取得，持有时 PEXPIRE 续期，
// 终态按持有者 compare-and-delete 释放）。
//
// 与慢状态同用一个 hash tag：同组合的键落同一槽，便于排障时用一次 SCAN/管道取齐。
func SlowProbeLeaseKey(providerID int64, modelKey string) string {
	return "cch:slowprobe:" + slowRateScopeTag(providerID, modelKey)
}

// slowProbeLeaseTTL 是探针租约的**崩溃兜底**存活期，不再是节奏闸门。
//
// 持有者在请求存活期间每 slowProbeLeaseRenewEvery 续租一次（compare-and-renew），故正常
// 路径上租约不会中途过期——这正是「全实例最多 1 个在飞」的保证：探针最长可达上游拨号超时
// （FETCH_HEADERS_TIMEOUT 默认 600s），远大于本 TTL，只靠 TTL 会让探针还在飞时租约先到期，
// 第二个实例随即拿到租约，出现多探针重叠。
//
// TTL 仍然必要：持有进程崩溃或续租整体失效时，键会在至多一个 TTL 内自愈，不会永久占着。
// 请求结束（ctx 取消，即终态返回）时持有者做一次 compare-and-delete 释放，于是探针节奏是
// 「上一个完成即下一个」，不再固定等满一个 TTL。
const slowProbeLeaseTTL = 30 * time.Second

// slowProbeLeaseRenewEvery 是续租间隔：TTL 的三分之一，容忍两次连续续租失败仍不丢租约。
const slowProbeLeaseRenewEvery = slowProbeLeaseTTL / 3

// slowProbeLeaseOpTimeout 是续租与释放两条 Redis 往返的上界。
//
// 释放发生在请求 ctx 已取消之后，必须自建超时；取值与终态旁路同量级。
const slowProbeLeaseOpTimeout = 3 * time.Second

// 两条脚本都先比对值再动作（compare-and-*）：只动**自己**的租约。
// KEYS[1] 是租约键，ARGV[1] 是持有者；续租脚本的 ARGV[2] 是新的毫秒 TTL。
//
// 为什么必须原子比对：租约可能已因 TTL 过期被别人重新取得，此时无条件 PEXPIRE/DEL 会动到
// 别人的租约——续租把别人占着的时间拉长，删除则直接放第三个探针挤进来，正是单飞要防的重叠。
const (
	slowProbeRenewLua   = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) end return 0`
	slowProbeReleaseLua = `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`
)

// SlowProbeGrant 是一次成功的探针租约授予。
type SlowProbeGrant struct {
	// ProviderID 与 ModelKey 是被探的组合。
	ProviderID int64
	ModelKey   string
	// Holder 是持有者标识（请求身份哈希），写进租约值供排障辨识是谁占着。
	Holder string
	// key 是租约键，续租与释放都按它操作；调用方不需要拼键，故不导出。
	key string
}

// AcquireSlowProbe 为某组合尝试取得探针租约；未取得（已有持有者、或 Redis 故障）时返回 nil。
//
// 取得后启动一个续租 goroutine（见 keepSlowProbeAlive），请求 ctx 结束即自动释放租约。
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
	grant := &SlowProbeGrant{ProviderID: providerID, ModelKey: modelKey, Holder: holder, key: key}
	go r.keepSlowProbeAlive(ctx, grant, slowProbeLeaseRenewEvery)
	return grant
}

// releaseSlowProbe 释放租约：仅当键仍是**本持有者**写的才删除（compare-and-delete）。
//
// 未导出：调用点只有 keepSlowProbeAlive 一处。若将来终态层要在请求未结束时就回收租约（例如
// 探针被故障转移换家后原渠道已无意义），再把它导出并接线。
//
// ctx 需自带超时：本方法常被已取消的请求 ctx 触发（见 keepSlowProbeAlive），直接用那个 ctx
// 会让释放当场失败，租约只能等 TTL 自愈。
func (r *SlowRateReader) releaseSlowProbe(ctx context.Context, grant *SlowProbeGrant) {
	if r == nil || r.redis == nil || grant == nil || grant.key == "" {
		return
	}
	if err := r.redis.Eval(ctx, slowProbeReleaseLua, []string{grant.key}, grant.Holder).Err(); err != nil {
		r.warn("route.slow_probe_lease_release_failed", grant.ModelKey, err)
	}
}

// keepSlowProbeAlive 在请求存活期间续租，请求一结束即释放租约。
//
// 用请求 ctx（守卫链把 HTTP 请求 ctx 一路传进来）而不是另建生命周期：探针就是这次请求本身，
// 请求结束即探针结束。ctx 取消后另用无取消父的短超时 ctx 做释放——否则释放会因「ctx 已取消」
// 直接失败，租约只能等 TTL 自愈。
//
// every 是续租间隔，由调用方传常量；作为形参是为了让用例能把它压到毫秒级（否则每个用例
// 都要真等 10 秒）。
func (r *SlowRateReader) keepSlowProbeAlive(ctx context.Context, grant *SlowProbeGrant, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slowProbeLeaseOpTimeout)
			r.releaseSlowProbe(releaseCtx, grant)
			cancel()
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slowProbeLeaseOpTimeout)
			grantTTL := strconv.FormatInt(slowProbeLeaseTTL.Milliseconds(), 10)
			if err := r.redis.Eval(renewCtx, slowProbeRenewLua, []string{grant.key}, grant.Holder, grantTTL).Err(); err != nil {
				r.warn("route.slow_probe_lease_renew_failed", grant.ModelKey, err)
			}
			cancel()
		}
	}
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
