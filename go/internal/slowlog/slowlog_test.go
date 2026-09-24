package slowlog

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件钉住 slowlog 的六条硬约束。
//
// 为什么用替身而不是真 Redis：本包断言的是**命令形态与容量语义**（写的是 XADD + EXPIRE、
// 裁剪是精确 MAXLEN、读的是倒序），这些在真 Redis 上也能测，但会让 CI 依赖外部服务；
// 而「真 Redis 语义」那一层已由 adminapi 的集成用例覆盖。故这里替身只管「发了什么命令」。
//
// go-redis 的 pipeline 陷阱：`Pipelined` 的闭包形式与 `Pipeline()+Exec()` 是**两条路**，
// 替身只实现被用的那一条。本包用的是 `Pipeline()+Exec()`，故替换的也是它。

// fakeRedis 是只实现本包用到的那几个命令的替身。
//
// 内嵌 redis.UniversalClient（nil 接口）：未实现的方法调用即 panic，等于把「本包用了什么命令」
// 变成一条隐式钉子——多用一个命令就会在测试里炸出来，逼作者显式补齐语义。
type fakeRedis struct {
	redis.UniversalClient

	mu      sync.Mutex
	entries map[string][]string // key -> 按写入顺序的载荷
	added   []string            // 记录 XADD 的参数，供「精确 MAXLEN」断言
	xaddErr error
	// execErr 令 Exec 失败，用于造「写日志失败」这一态。
	execErr error
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{entries: map[string][]string{}}
}

func (f *fakeRedis) Pipelined(
	_ context.Context,
	fn func(redis.Pipeliner) error,
) ([]redis.Cmder, error) {
	pipe := &fakePipeline{client: f}
	if err := fn(pipe); err != nil {
		return pipe.cmds, err
	}
	if f.execErr != nil {
		return pipe.cmds, f.execErr
	}
	return pipe.cmds, nil
}

type fakePipeline struct {
	redis.Pipeliner
	client *fakeRedis
	cmds   []redis.Cmder
}

func (p *fakePipeline) XAdd(_ context.Context, args *redis.XAddArgs) *redis.StringCmd {
	cmd := redis.NewStringCmd(context.Background())
	if p.client.xaddErr != nil {
		cmd.SetErr(p.client.xaddErr)
		p.cmds = append(p.cmds, cmd)
		return cmd
	}
	// XAddArgs.Values 是 interface{}（支持三种形态）；生产侧只用 map[string]any 这一种。
	values, _ := args.Values.(map[string]any)
	payload, _ := values["event"].(string)
	p.client.mu.Lock()
	entries := append(p.client.entries[args.Stream], payload)
	// 精确裁剪（MAXLEN =N）：只留最近 N 条。近似裁剪会留更多，本包刻意不用。
	if args.MaxLen > 0 && int64(len(entries)) > args.MaxLen {
		entries = entries[int64(len(entries))-args.MaxLen:]
	}
	p.client.entries[args.Stream] = entries
	p.client.added = append(p.client.added, strconv.FormatInt(args.MaxLen, 10))
	p.client.mu.Unlock()
	cmd.SetVal(strconv.Itoa(len(entries)) + "-0")
	p.cmds = append(p.cmds, cmd)
	return cmd
}

func (p *fakePipeline) Expire(_ context.Context, _ string, _ time.Duration) *redis.BoolCmd {
	cmd := redis.NewBoolCmd(context.Background())
	cmd.SetVal(true)
	p.cmds = append(p.cmds, cmd)
	return cmd
}

// XRevRangeN 倒序取最近 count 条（与真 Redis 的语义一致）。
func (f *fakeRedis) XRevRangeN(
	_ context.Context, stream, _, _ string, count int64,
) *redis.XMessageSliceCmd {
	cmd := redis.NewXMessageSliceCmd(context.Background())
	f.mu.Lock()
	stored := append([]string(nil), f.entries[stream]...)
	f.mu.Unlock()
	messages := make([]redis.XMessage, 0, len(stored))
	for index := len(stored) - 1; index >= 0 && int64(len(messages)) < count; index-- {
		messages = append(messages, redis.XMessage{
			ID:     strconv.Itoa(index) + "-0",
			Values: map[string]any{"event": stored[index]},
		})
	}
	cmd.SetVal(messages)
	return cmd
}

// recordingLogger 收集 warn 事件名。
type recordingLogger struct {
	mu     sync.Mutex
	events []string
}

func (l *recordingLogger) Warn(event string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *recordingLogger) has(event string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, got := range l.events {
		if got == event {
			return true
		}
	}
	return false
}

// TestRecordIsBypassOnRedisFailure 钉住旁路纪律：写失败只 warn，不返回错误、不 panic。
//
// 为什么这条最要紧：Record 的调用点在终态结算路径上（recorder.Record）与基线任务里
// （slowrate_baseline）。若把错误冒泡成结算错误，等于「日志写不进去 ⇒ 请求结算失败」——
// 这正是设计稿明令禁止的（日志是纯旁路）。
func TestRecordIsBypassOnRedisFailure(t *testing.T) {
	client := newFakeRedis()
	client.execErr = errors.New("redis: connection refused")
	logger := &recordingLogger{}

	// 不 panic、无返回值可断言——能跑到下一行即证明「没有冒泡」。
	Record(context.Background(), client, logger, Event{
		Kind:       KindPenaltyUp,
		ProviderID: 167,
		ModelKey:   "deepseek-v4.1-flash",
	})

	if !logger.has("slowlog.write_failed") {
		t.Errorf("写失败必须落一条 slowlog.write_failed，实际事件：%v", logger.events)
	}
}

// TestRecordPenaltyChangeDeduplicates 钉住去重：档位没变就不记。
//
// 为什么必须去重：写侧在窗内计数达阈值后**每个慢样本**都重写状态 Hash，若照写不误，
// 日志会退化成逐请求的样本日志（写放大）。用户要看的是「什么时候被压了」，不是每条样本。
func TestRecordPenaltyChangeDeduplicates(t *testing.T) {
	client := newFakeRedis()
	logger := &recordingLogger{}

	// 旧值与新值相同 ⇒ 不应产生任何条目。
	RecordPenaltyChange(context.Background(), client, logger, 167, "model", "10", 10)
	if got := len(client.entries[Key(167)]); got != 0 {
		t.Fatalf("档位未变时不该记事件，实际写入 %d 条", got)
	}

	// 0 -> 10 是升档，必须记。
	RecordPenaltyChange(context.Background(), client, logger, 167, "model", "", 10)
	// 10 -> 30 也是升档。
	RecordPenaltyChange(context.Background(), client, logger, 167, "model", "10", 30)
	// 30 -> 10 是降档。
	RecordPenaltyChange(context.Background(), client, logger, 167, "model", "30", 10)
	// 10 -> 0 归零（恢复策略删掉样本后，下一次读到的旧值即 0）也是降档，必须记。
	RecordPenaltyChange(context.Background(), client, logger, 167, "model", "10", 0)

	if got := len(client.entries[Key(167)]); got != 3 {
		t.Fatalf("应记 3 条（空->10、10->30、30->10），实际 %d 条", got)
	}
	// current <= 0 一律不记：惩罚为 0 不是「被降权」，它是「没被降权」。
	if got := len(client.entries[Key(167)]); got != 3 {
		t.Fatalf("current=0 不该新增条目，实际 %d 条", got)
	}
}

// TestRecordPenaltyChangeRoundTrip 钉住①升档/降档分档正确 ②序列化往返一致。
//
// 为什么往返单独钉：事件要经 JSON 存进 Redis，字段名或指针语义改坏时，编译不会红、
// 界面会静默显示错数（把「不含此维」渲染成 0）。
func TestRecordPenaltyChangeRoundTrip(t *testing.T) {
	client := newFakeRedis()
	logger := &recordingLogger{}
	RecordPenaltyChange(context.Background(), client, logger, 167, "deepseek-v4.1-flash", "10", 30)

	reader := NewReader(client, logger)
	events, err := reader.Recent(context.Background(), 167, DefaultLimit)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("应有 1 条，实际 %d 条", len(events))
	}
	event := events[0]
	if event.Kind != KindPenaltyUp {
		t.Errorf("10 -> 30 应为升档 %q，收到 %q", KindPenaltyUp, event.Kind)
	}
	if event.ProviderID != 167 || event.ModelKey != "deepseek-v4.1-flash" {
		t.Errorf("渠道与模型键应往返一致，收到 %+v", event)
	}
	if event.PenaltyFrom == nil || *event.PenaltyFrom != 10 {
		t.Errorf("PenaltyFrom 应为 10，收到 %v", event.PenaltyFrom)
	}
	if event.PenaltyTo == nil || *event.PenaltyTo != 30 {
		t.Errorf("PenaltyTo 应为 30，收到 %v", event.PenaltyTo)
	}
	// 惩罚事件不含基线读数：必须是 nil 而不是 0——界面据此决定渲不渲染那一列。
	if event.Median != nil || event.Samples != nil {
		t.Errorf("惩罚事件不该带基线读数，收到 median=%v samples=%v", event.Median, event.Samples)
	}
}

// TestRecordQuarantineEnteredRoundTrip 钉住「进入隔离」事件的往返（含 kind / modelKey / reason）。
//
// 为什么单独钉：这条事件是「今日隔离多少次」的唯一数据源，kind 或 reason 写错都不会报错，
// 只会让界面数不到、或把两个成因混成一个。
func TestRecordQuarantineEnteredRoundTrip(t *testing.T) {
	client := newFakeRedis()
	logger := &recordingLogger{}
	RecordQuarantineEntered(context.Background(), client, logger, 167, "deepseek-v4.1-flash", "precommit")

	events, err := NewReader(client, logger).Recent(context.Background(), 167, DefaultLimit)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("应有 1 条，实际 %d 条", len(events))
	}
	event := events[0]
	if event.Kind != KindQuarantineEntered {
		t.Errorf("应为进入隔离 %q，收到 %q", KindQuarantineEntered, event.Kind)
	}
	if event.ProviderID != 167 || event.ModelKey != "deepseek-v4.1-flash" {
		t.Errorf("渠道与模型键应往返一致，收到 %+v", event)
	}
	if event.Reason != "precommit" {
		t.Errorf("Reason 应带触发原因 precommit，收到 %q", event.Reason)
	}
	// 进入隔离不含惩罚/基线读数：必须是 nil 而不是 0——界面据此决定渲不渲染那一列。
	if event.PenaltyFrom != nil || event.PenaltyTo != nil || event.Median != nil || event.Samples != nil {
		t.Errorf("进入隔离事件不该带惩罚或基线读数，收到 %+v", event)
	}
}

// TestRecordBaselineRoundTrip 钉住基线发布的往返（含 median / samples / source）。
func TestRecordBaselineRoundTrip(t *testing.T) {
	client := newFakeRedis()
	logger := &recordingLogger{}
	RecordBaselinePublished(
		context.Background(), client, logger, 167, "deepseek-v4.1-flash",
		239.68, 7215, "primary", time.UnixMilli(1789993274606),
	)

	events, err := NewReader(client, logger).Recent(context.Background(), 167, DefaultLimit)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("应有 1 条，实际 %d 条", len(events))
	}
	event := events[0]
	if event.Kind != KindBaselinePublished {
		t.Errorf("应为基线发布，收到 %q", event.Kind)
	}
	if event.Median == nil || *event.Median != 239.68 {
		t.Errorf("Median 应为 239.68，收到 %v", event.Median)
	}
	if event.Samples == nil || *event.Samples != 7215 {
		t.Errorf("Samples 应为 7215，收到 %v", event.Samples)
	}
	if event.Reason != "primary" {
		t.Errorf("Reason 应带基线来源 primary，收到 %q", event.Reason)
	}
	if event.At != 1789993274606 {
		t.Errorf("At 应用传入时刻（不取 now），收到 %d", event.At)
	}
	// 基线事件不含惩罚读数，同样必须是 nil。
	if event.PenaltyFrom != nil || event.PenaltyTo != nil {
		t.Errorf("基线事件不该带惩罚读数，收到 %v / %v", event.PenaltyFrom, event.PenaltyTo)
	}
}

// TestRecordTrimsToMaxEntries 钉住容量有界：连续写入超过上限后**最旧的被裁掉**。
//
// 变异反证（去掉 MaxLen ⇒ 本用例必红）：不裁剪的话条目数会一直涨，
// 键就成了无界增长——而这是本包选 Stream + MAXLEN 的唯一理由。
func TestRecordTrimsToMaxEntries(t *testing.T) {
	client := newFakeRedis()
	logger := &recordingLogger{}
	key := Key(167)

	// 写 MaxEntries + 5 条。
	for index := 0; index < MaxEntries+5; index++ {
		Record(context.Background(), client, logger, Event{
			Kind:       KindPenaltyUp,
			ProviderID: 167,
			ModelKey:   "model-" + strconv.Itoa(index),
		})
	}

	client.mu.Lock()
	stored := len(client.entries[key])
	// 同时钉住「裁剪用的是精确 MAXLEN 而不是近似」：每条 XADD 的参数都是本包的 MaxEntries。
	maxLens := append([]string(nil), client.added...)
	client.mu.Unlock()

	if stored != MaxEntries {
		t.Fatalf("条目数应被裁到 %d，实际 %d", MaxEntries, stored)
	}
	for _, got := range maxLens {
		if got != strconv.Itoa(MaxEntries) {
			t.Fatalf("XADD 的 MaxLen 应为 %d（精确裁剪），收到 %s", MaxEntries, got)
		}
	}

	// 被裁掉的是**最旧**的：读回最后一条应是 index = MaxEntries+4。
	events, err := NewReader(client, logger).Recent(context.Background(), 167, 1)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(events) != 1 || events[0].ModelKey != "model-"+strconv.Itoa(MaxEntries+4) {
		t.Fatalf("最旧的应被裁掉、最新在前，实际 %+v", events)
	}
}

// TestReaderReturnsEmptySliceWhenNoData 钉住「查了，没有记录」不是错误。
//
// 为什么这条要有：端点据此返回空数组而不是 500，界面才能把「24 小时内无降权」
// 与「读不到」分开渲染（同族 circuitLogsResponse 的 Errors 也是 [] 而不是 null）。
func TestReaderReturnsEmptySliceWhenNoData(t *testing.T) {
	reader := NewReader(newFakeRedis(), &recordingLogger{})
	events, err := reader.Recent(context.Background(), 999999, DefaultLimit)
	if err != nil {
		t.Fatalf("无数据不该报错: %v", err)
	}
	if events == nil {
		t.Fatal("无数据应返回空切片而不是 nil（前端不必为 null 与 [] 各写一条分支）")
	}
	if len(events) != 0 {
		t.Fatalf("应为空，实际 %d 条", len(events))
	}
}

// TestReaderClampsLimit 钉住条数上限：越界入参被夹到硬上限，而不是把整条流读出来。
func TestReaderClampsLimit(t *testing.T) {
	client := newFakeRedis()
	logger := &recordingLogger{}
	for index := 0; index < MaxLimit+20; index++ {
		Record(context.Background(), client, logger, Event{
			Kind:       KindPenaltyUp,
			ProviderID: 167,
			ModelKey:   "m" + strconv.Itoa(index),
		})
	}
	events, err := NewReader(client, logger).Recent(context.Background(), 167, MaxLimit+1000)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if len(events) != MaxLimit {
		t.Fatalf("越界 limit 应夹到 %d，实际 %d", MaxLimit, len(events))
	}
}

// TestNewReaderNilWhenNoRedis 钉住装配闸门：无 Redis 时读面为 nil，端点据此不注册。
func TestNewReaderNilWhenNoRedis(t *testing.T) {
	if reader := NewReader(nil, &recordingLogger{}); reader != nil {
		t.Fatal("无 Redis 命令连接时应返回 nil（该路由不注册、回退 Node）")
	}
	// nil 读面不得 panic：调用方可能在装配竞态下先拿到 nil 再调。
	var reader *Reader
	events, err := reader.Recent(context.Background(), 167, DefaultLimit)
	if err != nil || len(events) != 0 {
		t.Fatalf("nil 读面应安全返回空，收到 events=%v err=%v", events, err)
	}
}

// TestRecordSkipsInvalidProviderID 钉住入参校验：无渠道 id 的事件无从归属，必须丢弃。
func TestRecordSkipsInvalidProviderID(t *testing.T) {
	client := newFakeRedis()
	Record(context.Background(), client, &recordingLogger{}, Event{Kind: KindPenaltyUp, ProviderID: 0})
	if len(client.entries) != 0 {
		t.Fatalf("providerID 为 0 时不该写任何键，实际 %v", client.entries)
	}
}

// TestKeyIsHashTagged 钉住键形制：花括号是 Redis Cluster 的 hash tag，缺了会让同一渠道的
// 事件分散到不同槽、pipeline 与按渠道读都失效。
func TestKeyIsHashTagged(t *testing.T) {
	key := Key(167)
	if !strings.HasPrefix(key, "cch:slowlog:{") || !strings.HasSuffix(key, "}") {
		t.Fatalf("键形制应为 cch:slowlog:{<id>}，收到 %q", key)
	}
	if key == Key(168) {
		t.Fatal("不同渠道必须不同键")
	}
	// 刻意不与 cch:slow: 同族：管理面按 `cch:slow:*:state` 扫描那族键，同族会被误匹配。
	if strings.HasPrefix(key, "cch:slow:") {
		t.Fatalf("键不该落在 cch:slow: 前缀下，收到 %q", key)
	}
}
