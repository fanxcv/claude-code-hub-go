package limit

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/redis/go-redis/v9"
)

// commandHook 按命令名记录 Redis 命令，并区分「管道内」与「单发」——用来钉住成本窗口的读法
// 是一次批量取，而不是逐维单发。
type commandHook struct {
	mu       sync.Mutex
	single   map[string]int
	batched  map[string]int
	pipeline int
}

func newCommandHook() *commandHook {
	return &commandHook{single: map[string]int{}, batched: map[string]int{}}
}

func (h *commandHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *commandHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.count(h.single, cmd.Name())
		return next(ctx, cmd)
	}
}

func (h *commandHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.mu.Lock()
		h.pipeline++
		h.mu.Unlock()
		for _, cmd := range cmds {
			h.count(h.batched, cmd.Name())
		}
		return next(ctx, cmds)
	}
}

func (h *commandHook) count(bucket map[string]int, name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	bucket[name]++
}

// snapshot 返回单发与管道内的命令计数快照（含管道执行次数）。
func (h *commandHook) snapshot() (map[string]int, map[string]int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	single := make(map[string]int, len(h.single))
	for name, count := range h.single {
		single[name] = count
	}
	batched := make(map[string]int, len(h.batched))
	for name, count := range h.batched {
		batched[name] = count
	}
	return single, batched, h.pipeline
}

// unreachableRedis 建一个带命令 hook 的调用层：Ready() 为真、命令一律失败（地址不可达），
// 用来观察读命令的形状（批量 vs 单发），同时覆盖 Redis 故障回退账本的分支。
func unreachableRedis(t *testing.T) (*ratelimit.Client, *commandHook) {
	t.Helper()
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	raw := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  200 * time.Millisecond,
		ReadTimeout:  200 * time.Millisecond,
		WriteTimeout: 200 * time.Millisecond,
		MaxRetries:   -1,
	})
	hook := newCommandHook()
	raw.AddHook(hook)
	t.Cleanup(func() { _ = raw.Close() })
	client, err := ratelimit.New(raw, registry)
	if err != nil {
		t.Fatalf("组装调用层失败: %v", err)
	}
	return client, hook
}

// TestCheckReadsCostWindowsInOneBatch 钉住成本窗口的读法：一次判定只发一个管道（含所有窗口键），
// 不再逐维单发 GET/EVAL；Redis 故障时仍逐维回退账本（既有语义不变）。
func TestCheckReadsCostWindowsInOneBatch(t *testing.T) {
	client, hook := unreachableRedis(t)
	ledger := &fakeLedger{
		totals: map[string]float64{"key/sk-batch": 1, "user/2": 1},
		ranges: map[string]float64{"key/sk-batch": 9},
	}
	service, err := New(Config{
		Quotas: &fakeQuotaSource{
			key: KeyQuota{
				KeyID: 1, KeyHash: "sk-batch",
				Limit5hUSD: floatPtr(20), Limit5hResetMode: ResetRolling,
				LimitDailyUSD: floatPtr(20), LimitWeeklyUSD: floatPtr(1),
				LimitMonthlyUSD: floatPtr(20), LimitTotalUSD: floatPtr(20),
			},
			user: UserQuota{
				UserID:     2,
				Limit5hUSD: floatPtr(20), LimitDailyUSD: floatPtr(20),
				LimitWeeklyUSD: floatPtr(20), LimitMonthlyUSD: floatPtr(20),
				LimitTotalUSD: floatPtr(20),
			},
		},
		Ledger:   ledger,
		Redis:    client,
		Location: time.UTC,
		Now:      func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}

	block, err := service.Check(context.Background(), requestWithAuth(t))
	if err != nil {
		t.Fatalf("Check 出错: %v", err)
	}
	if block == nil || !strings.Contains(block.BlockedReason, `"limit_type":"usd_weekly"`) {
		t.Fatalf("周维度应先行拦截: block=%+v", block)
	}

	single, batched, pipelines := hook.snapshot()
	if pipelines != 1 {
		t.Errorf("成本窗口应一次批量取回，实际管道执行 %d 次", pipelines)
	}
	for _, name := range []string{"get", "ttl", "exists", "eval"} {
		if count := single[name]; count != 0 {
			t.Errorf("窗口读命令 %q 不应单发，实际 %d 次", name, count)
		}
	}
	// 6 个固定窗口维度 + 2 条总额度 = 8 次 GET；两个 5h 滚动维度 = 2 次 EVAL + 2 次 EXISTS。
	if batched["get"] < 8 || batched["eval"] != 2 || batched["exists"] != 2 {
		t.Errorf("管道内窗口读命令形状不符: %+v", batched)
	}
	if len(ledger.calls) == 0 {
		t.Error("Redis 不可用时仍应回退账本")
	}
}

// TestCostReadsUsePrefetchedSnapshot 钉住缓存命中路径：预取快照命中时不再回账本、不再发 Redis 命令。
func TestCostReadsUsePrefetchedSnapshot(t *testing.T) {
	client, hook := unreachableRedis(t)
	ledger := &fakeLedger{}
	service, err := New(Config{
		Quotas:   &fakeQuotaSource{},
		Ledger:   ledger,
		Redis:    client,
		Location: time.UTC,
		Now:      func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("组装服务失败: %v", err)
	}
	ctx := context.Background()
	dimension := costDimension{
		entity: EntityKey, id: 1, keyHash: "sk-hit",
		period: PeriodWeekly, amount: floatPtr(5), resetMode: ResetFixed,
	}
	key := FixedWindowKey(EntityKey, 1, PeriodWeekly, "")

	current, exceeded, err := service.costLimit(ctx, dimension, fixedNow, windowCache{key: {Value: 3, Exists: true}})
	if err != nil || exceeded || current != 3 {
		t.Fatalf("预取命中应直接用缓存值: current=%v exceeded=%v err=%v", current, exceeded, err)
	}
	if _, exceeded, err := service.costLimit(ctx, dimension, fixedNow, windowCache{key: {Value: 7, Exists: true}}); err != nil || !exceeded {
		t.Errorf("预取值超限应判触顶: exceeded=%v err=%v", exceeded, err)
	}

	totalKey := TotalCostCacheKey(EntityKey, 1, "sk-hit", nil)
	total, err := service.totalCost(ctx, EntityKey, 1, "sk-hit", nil, windowCache{totalKey: {Value: 4, Exists: true}})
	if err != nil || total != 4 {
		t.Fatalf("总额度预取命中应直接用缓存值: total=%v err=%v", total, err)
	}

	if len(ledger.calls) != 0 || ledger.totalCalls != 0 {
		t.Errorf("命中预取快照时不应回账本: 周期 %d 次、总额 %d 次", len(ledger.calls), ledger.totalCalls)
	}
	if single, batched, pipelines := hook.snapshot(); pipelines != 0 || len(single) != 0 || len(batched) != 0 {
		t.Errorf("命中预取快照时不应再发 Redis 命令: single=%v batched=%v pipelines=%d", single, batched, pipelines)
	}
}
