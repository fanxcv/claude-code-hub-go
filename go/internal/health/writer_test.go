package health

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// fakeRedis 是最小 Redis 替身：只实现本包用到的 HGetAll / HSet / Expire。
//
// 它同时记住 TTL，供「TTL 是否写对」的断言使用。
type fakeRedis struct {
	redis.UniversalClient
	hashes  map[string]map[string]string
	ttl     map[string]time.Duration
	hsetErr error
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{hashes: map[string]map[string]string{}, ttl: map[string]time.Duration{}}
}

func (f *fakeRedis) HGetAll(_ context.Context, key string) *redis.MapStringStringCmd {
	cmd := redis.NewMapStringStringCmd(context.Background())
	cmd.SetVal(f.hashes[key])
	return cmd
}

func (f *fakeRedis) HSet(_ context.Context, key string, values ...interface{}) *redis.IntCmd {
	cmd := redis.NewIntCmd(context.Background())
	if f.hsetErr != nil {
		cmd.SetErr(f.hsetErr)
		return cmd
	}
	if f.hashes[key] == nil {
		f.hashes[key] = map[string]string{}
	}
	// go-redis 允许把整个 map 当唯一参数传入（生产写入用的就是这个形式）。
	if len(values) == 1 {
		switch single := values[0].(type) {
		case map[string]interface{}:
			for name, value := range single {
				f.hashes[key][name], _ = value.(string)
			}
			cmd.SetVal(1)
			return cmd
		case map[string]string:
			for name, value := range single {
				f.hashes[key][name] = value
			}
			cmd.SetVal(1)
			return cmd
		}
	}
	for index := 0; index+1 < len(values); index += 2 {
		name, _ := values[index].(string)
		value, ok := values[index+1].(string)
		if !ok {
			continue
		}
		f.hashes[key][name] = value
	}
	cmd.SetVal(1)
	return cmd
}

func (f *fakeRedis) Expire(_ context.Context, key string, ttl time.Duration) *redis.BoolCmd {
	cmd := redis.NewBoolCmd(context.Background())
	f.ttl[key] = ttl
	cmd.SetVal(true)
	return cmd
}

// settingsStub 是 SettingsSource 的替身，只控制高并发开关。
type settingsStub struct {
	highConcurrency bool
	err             error
}

func (s settingsStub) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &store.SystemSettings{EnableHighConcurrencyMode: s.highConcurrency}, nil
}

func testWriter(t *testing.T, client redis.UniversalClient, endpointEnabled bool, settings SettingsSource) (*Writer, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	return NewWriter(Options{
		Redis:                         client,
		EndpointCircuitBreakerEnabled: endpointEnabled,
		Settings:                      settings,
		Now:                           func() time.Time { return now },
	}), &now
}

func providerKey(id int64) string { return route.ProviderStateKeyPrefix + strconv.FormatInt(id, 10) }

func endpointKey(id int64) string { return route.EndpointStateKeyPrefix + strconv.FormatInt(id, 10) }

func TestProviderOpensAtThresholdAndWritesNodeShape(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, false, nil)

	var opened []int64
	writer.onProviderOpen = func(providerID int64, failureCount int64, openUntilMS int64, _ error) {
		opened = append(opened, providerID)
		if openUntilMS != now.UnixMilli()+route.DefaultOpenDurationMS {
			t.Errorf("开闸时刻应为 now+openDuration，得到 %d", openUntilMS)
		}
	}

	// 阈值 5（默认）：前 4 次不开闸。
	for attempt := 0; attempt < 4; attempt++ {
		if err := writer.RecordProviderFailure(context.Background(), 21, errors.New("boom")); err != nil {
			t.Fatalf("第 %d 次记账失败: %v", attempt+1, err)
		}
	}
	state := client.hashes[providerKey(21)]
	if state["circuitState"] != "closed" {
		t.Fatalf("未达阈值不应开闸，得到 %q", state["circuitState"])
	}
	if state["failureCount"] != "4" {
		t.Fatalf("失败计数应为 4，得到 %q", state["failureCount"])
	}

	if err := writer.RecordProviderFailure(context.Background(), 21, errors.New("boom")); err != nil {
		t.Fatalf("第 5 次记账失败: %v", err)
	}
	state = client.hashes[providerKey(21)]
	if state["circuitState"] != "open" {
		t.Fatalf("达阈值应开闸，得到 %q", state["circuitState"])
	}
	if len(opened) != 1 || opened[0] != 21 {
		t.Fatalf("应恰好告警一次，得到 %v", opened)
	}
	// Node 形状：五个字段齐备，空串即 null。
	for _, field := range []string{"failureCount", "lastFailureTime", "circuitState", "circuitOpenUntil", "halfOpenSuccessCount"} {
		if _, ok := state[field]; !ok {
			t.Fatalf("缺少 Node 字段 %q: %v", field, state)
		}
	}
	if state["lastFailureTime"] != strconv.FormatInt(now.UnixMilli(), 10) {
		t.Fatalf("lastFailureTime 应为当时毫秒，得到 %q", state["lastFailureTime"])
	}
	if client.ttl[providerKey(21)] != 86400*time.Second {
		t.Fatalf("TTL 应为 86400s，得到 %v", client.ttl[providerKey(21)])
	}

	// 已开闸后再失败：不重复开闸、不推后 openUntil、不重复告警。
	openUntilBefore := state["circuitOpenUntil"]
	if err := writer.RecordProviderFailure(context.Background(), 21, errors.New("boom")); err != nil {
		t.Fatalf("开闸后再记账失败: %v", err)
	}
	if client.hashes[providerKey(21)]["circuitOpenUntil"] != openUntilBefore {
		t.Fatalf("开闸后再失败不得重置 openUntil")
	}
	if len(opened) != 1 {
		t.Fatalf("开闸只应告警一次，得到 %v", opened)
	}
}

func TestProviderHalfOpenSuccessClosesAfterThreshold(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, false, nil)
	// 半开态（越窗后选路器会按 half-open 放行试探）。
	client.hashes[providerKey(31)] = map[string]string{
		"failureCount":         "7",
		"circuitState":         "half-open",
		"circuitOpenUntil":     "1",
		"halfOpenSuccessCount": "0",
		"lastFailureTime":      "1",
	}

	// 默认半开阈值 2：第一次成功只计数。
	if err := writer.RecordProviderSuccess(context.Background(), 31); err != nil {
		t.Fatalf("半开成功记账失败: %v", err)
	}
	state := client.hashes[providerKey(31)]
	if state["halfOpenSuccessCount"] != "1" || state["circuitState"] != "half-open" {
		t.Fatalf("首次半开成功应只计数，得到 %v", state)
	}

	if err := writer.RecordProviderSuccess(context.Background(), 31); err != nil {
		t.Fatalf("半开成功记账失败: %v", err)
	}
	state = client.hashes[providerKey(31)]
	if state["circuitState"] != "closed" {
		t.Fatalf("达半开阈值应归闭，得到 %q", state["circuitState"])
	}
	for field, want := range map[string]string{
		"failureCount":         "0",
		"halfOpenSuccessCount": "0",
		"lastFailureTime":      "",
		"circuitOpenUntil":     "",
	} {
		if state[field] != want {
			t.Fatalf("归闭后 %s 应为 %q，得到 %q", field, want, state[field])
		}
	}
}

func TestProviderSuccessResetsFailureCountWhenClosed(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, false, nil)
	client.hashes[providerKey(41)] = map[string]string{
		"failureCount":     "3",
		"circuitState":     "closed",
		"lastFailureTime":  "123",
		"circuitOpenUntil": "",
	}

	if err := writer.RecordProviderSuccess(context.Background(), 41); err != nil {
		t.Fatalf("成功记账失败: %v", err)
	}
	state := client.hashes[providerKey(41)]
	if state["failureCount"] != "0" || state["lastFailureTime"] != "" {
		t.Fatalf("闭态成功应清零失败计数，得到 %v", state)
	}
}

func TestProviderSuccessWithoutChangeSkipsWrite(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, false, nil)
	// 已经干净：Node 只在状态变化时落库（recordSuccess 的 stateChanged 判定）。
	if err := writer.RecordProviderSuccess(context.Background(), 51); err != nil {
		t.Fatalf("成功记账失败: %v", err)
	}
	if len(client.hashes[providerKey(51)]) != 0 {
		t.Fatalf("无状态变化不应写入 Redis，得到 %v", client.hashes[providerKey(51)])
	}
}

func TestProviderDisabledConfigForcesClosed(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, false, nil)
	// failureThreshold = 0 表示熔断器关闭（Node isCircuitBreakerDisabled）。
	client.hashes[route.ProviderConfigKeyPrefix+"61"] = map[string]string{
		"failureThreshold":            "0",
		"openDuration":                "1800000",
		"halfOpenSuccessThreshold":    "2",
		"circuitOpenUntilPlaceholder": "",
	}
	client.hashes[providerKey(61)] = map[string]string{
		"failureCount":     "9",
		"circuitState":     "open",
		"circuitOpenUntil": "999",
	}

	if err := writer.RecordProviderFailure(context.Background(), 61, errors.New("boom")); err != nil {
		t.Fatalf("禁用态记账失败: %v", err)
	}
	state := client.hashes[providerKey(61)]
	if state["circuitState"] != "closed" || state["failureCount"] != "0" {
		t.Fatalf("禁用态应强制归闭，得到 %v", state)
	}
}

func TestProviderConfigFailureThresholdFromRedis(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, false, nil)
	// 阈值配成 2：第二次失败即开闸（证明读了 Redis 而非只用默认 5）。
	client.hashes[route.ProviderConfigKeyPrefix+"71"] = map[string]string{
		"failureThreshold":         "2",
		"openDuration":             "60000",
		"halfOpenSuccessThreshold": "2",
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := writer.RecordProviderFailure(context.Background(), 71, errors.New("boom")); err != nil {
			t.Fatalf("记账失败: %v", err)
		}
	}
	if client.hashes[providerKey(71)]["circuitState"] != "open" {
		t.Fatalf("配置阈值 2 应在第二次失败开闸，得到 %v", client.hashes[providerKey(71)])
	}
}

func TestEndpointOpensAndHalfOpenCloses(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, true, nil)
	var opened []int64
	writer.onEndpointOpen = func(endpointID int64, _ int64, _ int64, _ error) {
		opened = append(opened, endpointID)
	}

	// 端点阈值 3（Node DEFAULT_ENDPOINT_CIRCUIT_BREAKER_CONFIG）。
	for attempt := 0; attempt < 3; attempt++ {
		if err := writer.RecordEndpointFailure(context.Background(), 101, errors.New("boom")); err != nil {
			t.Fatalf("端点记账失败: %v", err)
		}
	}
	state := client.hashes[endpointKey(101)]
	if state["circuitState"] != "open" {
		t.Fatalf("端点达阈值应开闸，得到 %v", state)
	}
	if state["circuitOpenUntil"] != strconv.FormatInt(now.UnixMilli()+300000, 10) {
		t.Fatalf("端点开闸时长应为 5 分钟，得到 %q", state["circuitOpenUntil"])
	}
	if len(opened) != 1 {
		t.Fatalf("端点开闸应告警一次，得到 %v", opened)
	}
	if client.ttl[endpointKey(101)] != 86400*time.Second {
		t.Fatalf("端点状态 TTL 应为 86400s，得到 %v", client.ttl[endpointKey(101)])
	}

	// 半开：端点阈值 1，一次成功即归闭。
	client.hashes[endpointKey(101)]["circuitState"] = "half-open"
	if err := writer.RecordEndpointSuccess(context.Background(), 101); err != nil {
		t.Fatalf("端点成功记账失败: %v", err)
	}
	state = client.hashes[endpointKey(101)]
	if state["circuitState"] != "closed" || state["failureCount"] != "0" {
		t.Fatalf("端点半开成功应归闭，得到 %v", state)
	}
}

func TestEndpointWritesSkippedWhenSwitchOff(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, false, nil)
	if err := writer.RecordEndpointFailure(context.Background(), 111, errors.New("boom")); err != nil {
		t.Fatalf("记账失败: %v", err)
	}
	if len(client.hashes[endpointKey(111)]) != 0 {
		t.Fatalf("开关关闭时不应写端点状态，得到 %v", client.hashes[endpointKey(111)])
	}
	if err := writer.RecordVendorTypeAllEndpointsTimeout(
		context.Background(), 7, convert.ProviderCodex, time.Minute,
	); err != nil {
		t.Fatalf("厂级记账失败: %v", err)
	}
	if len(client.hashes) != 0 {
		t.Fatalf("开关关闭时不应写任何状态，得到 %v", client.hashes)
	}
}

func TestVendorTypeAllEndpointsTimeoutOpensWithNodeKey(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, true, nil)
	key := route.VendorTypeStateKeyPrefix + "8:" + string(convert.ProviderCodex)

	if err := writer.RecordVendorTypeAllEndpointsTimeout(
		context.Background(), 8, convert.ProviderCodex, 10*time.Minute,
	); err != nil {
		t.Fatalf("厂级记账失败: %v", err)
	}
	state := client.hashes[key]
	if state["circuitState"] != "open" {
		t.Fatalf("厂级应开闸，得到 %v", state)
	}
	if state["circuitOpenUntil"] != strconv.FormatInt(now.UnixMilli()+(10*time.Minute).Milliseconds(), 10) {
		t.Fatalf("厂级 openUntil 应取传入时长，得到 %q", state["circuitOpenUntil"])
	}
	if client.ttl[key] != 2592000*time.Second {
		t.Fatalf("厂级 TTL 应为 2592000s，得到 %v", client.ttl[key])
	}

	// 时长下限 1000ms（Node 的 max(1000, x)）。
	if err := writer.RecordVendorTypeAllEndpointsTimeout(
		context.Background(), 9, convert.ProviderCodex, 0,
	); err != nil {
		t.Fatalf("厂级记账失败: %v", err)
	}
	if got := client.hashes[route.VendorTypeStateKeyPrefix+"9:"+string(convert.ProviderCodex)]["circuitOpenUntil"]; got != strconv.FormatInt(now.UnixMilli()+1000, 10) {
		t.Fatalf("厂级开闸时长下限应为 1000ms，得到 %q", got)
	}
}

func TestVendorTypeTTLShrinksInHighConcurrencyMode(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, true, settingsStub{highConcurrency: true})
	key := route.VendorTypeStateKeyPrefix + "10:" + string(convert.ProviderCodex)
	if err := writer.RecordVendorTypeAllEndpointsTimeout(
		context.Background(), 10, convert.ProviderCodex, time.Minute,
	); err != nil {
		t.Fatalf("厂级记账失败: %v", err)
	}
	if client.ttl[key] != 86400*time.Second {
		t.Fatalf("高并发模式下厂级 TTL 应收缩到 86400s，得到 %v", client.ttl[key])
	}
}

func TestManualOpenIsNotOverwritten(t *testing.T) {
	client := newFakeRedis()
	writer, _ := testWriter(t, client, true, nil)
	key := route.VendorTypeStateKeyPrefix + "11:" + string(convert.ProviderCodex)
	client.hashes[key] = map[string]string{"manualOpen": "1", "circuitState": "open", "circuitOpenUntil": ""}
	if err := writer.RecordVendorTypeAllEndpointsTimeout(
		context.Background(), 11, convert.ProviderCodex, time.Minute,
	); err != nil {
		t.Fatalf("厂级记账失败: %v", err)
	}
	if client.hashes[key]["circuitOpenUntil"] != "" {
		t.Fatalf("手工开闸状态下不应被自动写入改写，得到 %v", client.hashes[key])
	}
}

func TestRedisFailureIsFailOpen(t *testing.T) {
	client := newFakeRedis()
	client.hsetErr = errors.New("redis down")
	writer, _ := testWriter(t, client, false, nil)
	// 返回错误供调用方记日志；调用方（forward 钩子）不因此改变请求结论。
	if err := writer.RecordProviderFailure(context.Background(), 81, errors.New("boom")); err == nil {
		t.Fatalf("落库失败应返回错误以便记录")
	}
	// 无 Redis 时一律静默跳过（比照 Node 的 fail-open）。
	noRedis, _ := testWriter(t, nil, false, nil)
	if err := noRedis.RecordProviderFailure(context.Background(), 81, errors.New("boom")); err != nil {
		t.Fatalf("无 Redis 时应静默跳过，得到 %v", err)
	}
}

// TestWriterImplementsHealthSink 钉住接口实现：route 只定义接口，写入面在本包。
//
// 钉子就是下面那行**赋值**：Writer 若不再满足 route.HealthSink，这里编译不过。
// 不要再写 `if sink == nil`——接口里装的是具体类型（NewWriter 返回 *Writer），
// 该比较是常量假（SA4023），这种断言永远不会失败。
func TestWriterImplementsHealthSink(t *testing.T) {
	var _ route.HealthSink = NewWriter(Options{})
}

// TestIntegrationStateIsSharedWithNodeRedis 用真实 Redis 验证键形制与 TTL，并清理自己写的键。
func TestIntegrationStateIsSharedWithNodeRedis(t *testing.T) {
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败: %v", err)
	}
	client := redis.NewClient(options)
	defer client.Close()

	ctx := context.Background()
	writer := NewWriter(Options{Redis: client, EndpointCircuitBreakerEnabled: true})
	const providerID = int64(900000001)
	const endpointID = int64(900000002)
	keys := []string{providerKey(providerID), endpointKey(endpointID)}
	defer func() {
		for _, key := range keys {
			client.Del(ctx, key)
		}
	}()

	if err := writer.RecordProviderFailure(ctx, providerID, errors.New("boom")); err != nil {
		t.Fatalf("真实 Redis 记账失败: %v", err)
	}
	if ttl := client.TTL(ctx, providerKey(providerID)).Val(); ttl <= 0 || ttl > 86400*time.Second {
		t.Fatalf("真实 Redis 上 TTL 应为 (0,86400s]，得到 %v", ttl)
	}
	if state := client.HGetAll(ctx, providerKey(providerID)).Val(); state["failureCount"] != "1" {
		t.Fatalf("真实 Redis 上状态形状不符: %v", state)
	}
	if err := writer.RecordEndpointFailure(ctx, endpointID, errors.New("boom")); err != nil {
		t.Fatalf("真实 Redis 端点记账失败: %v", err)
	}
	if state := client.HGetAll(ctx, endpointKey(endpointID)).Val(); state["failureCount"] != "1" {
		t.Fatalf("真实 Redis 上端点状态形状不符: %v", state)
	}
}

// TestProviderRecoversFromExpiredOpenWindow 是本轮机制修复的验收：
// 「open 且 circuitOpenUntil 已过期」的供应商，必须能经由 half-open 计数**回到 closed**。
//
// 改前的真实缺陷：窗口过期只在选路侧（ProviderOpen）被当作 half-open 放行，**从不写回**；
// 而成功路径只认 half-open ⇒ 成功一次次被忽略、failureCount 也不清 ⇒ 状态永远停在 open，
// 熔断再也回不到 closed（生产现象：一批供应商永远显示「已熔断」，请求却照样通过）。
// Node 在 isCircuitOpen 里做 open->half-open 的迁移与持久化，本包补上同一步。
func TestProviderRecoversFromExpiredOpenWindow(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, true, nil)
	ctx := context.Background()
	const providerID = int64(88001)
	key := providerKey(providerID)

	// 夹具：open 且窗口已过期（生产 156/145 的同款形态）。
	*now = now.Add(time.Hour)
	expired := now.Add(-time.Minute).UnixMilli()
	client.hashes[key] = map[string]string{
		"failureCount":         "11",
		"lastFailureTime":      strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10),
		"circuitState":         "open",
		"circuitOpenUntil":     strconv.FormatInt(expired, 10),
		"halfOpenSuccessCount": "0",
	}

	// 第一次成功：落成 half-open 并把这次成功记入 halfOpenSuccessCount（阈值 2，未到闭）。
	if err := writer.RecordProviderSuccess(ctx, providerID); err != nil {
		t.Fatalf("记录成功失败: %v", err)
	}
	state := client.hashes[key]
	if state["circuitState"] != "half-open" {
		t.Fatalf("窗口过期的 open 应落成 half-open，实际 %q（这就是「永远回不到 closed」的根因）",
			state["circuitState"])
	}
	if state["halfOpenSuccessCount"] != "1" {
		t.Fatalf("halfOpenSuccessCount 应为 1，实际 %q", state["halfOpenSuccessCount"])
	}
	if state["failureCount"] != "11" {
		t.Fatalf("迁移不得重置 failureCount（Node 也不重置），实际 %q", state["failureCount"])
	}
	if state["circuitOpenUntil"] != strconv.FormatInt(expired, 10) {
		t.Fatalf("迁移不得改 circuitOpenUntil，实际 %q", state["circuitOpenUntil"])
	}

	// 第二次成功：达到默认半开阈值 2 ⇒ 归 closed。
	if err := writer.RecordProviderSuccess(ctx, providerID); err != nil {
		t.Fatalf("记录成功失败: %v", err)
	}
	state = client.hashes[key]
	if state["circuitState"] != "closed" {
		t.Fatalf("半开成功达阈值后应归 closed，实际 %q", state["circuitState"])
	}
	if state["failureCount"] != "0" || state["circuitOpenUntil"] != "" {
		t.Fatalf("归闭应清空 failureCount 与 circuitOpenUntil，实际 %v", state)
	}
}

// TestProviderFailureInExpiredWindowReopensWithFreshWindow 钉住另一半：
// 窗口过期后的失败不落进「已开闸」分支（那条分支不更新 openUntil），而要以**新窗口**重新开闸。
func TestProviderFailureInExpiredWindowReopensWithFreshWindow(t *testing.T) {
	client := newFakeRedis()
	writer, now := testWriter(t, client, true, nil)
	ctx := context.Background()
	const providerID = int64(88002)
	key := providerKey(providerID)

	*now = now.Add(time.Hour)
	expired := now.Add(-time.Minute).UnixMilli()
	client.hashes[key] = map[string]string{
		"failureCount":         "5",
		"circuitState":         "open",
		"circuitOpenUntil":     strconv.FormatInt(expired, 10),
		"halfOpenSuccessCount": "0",
	}

	if err := writer.RecordProviderFailure(ctx, providerID, errors.New("still broken")); err != nil {
		t.Fatalf("记录失败失败: %v", err)
	}
	state := client.hashes[key]
	if state["circuitState"] != "open" {
		t.Fatalf("半开下的失败应以 open 重新开闸，实际 %q", state["circuitState"])
	}
	if state["circuitOpenUntil"] == strconv.FormatInt(expired, 10) {
		t.Fatalf("重新开闸必须刷新 openUntil（改前正是卡在这里，导致永不恢复）")
	}
	openUntil, err := strconv.ParseInt(state["circuitOpenUntil"], 10, 64)
	if err != nil {
		t.Fatalf("circuitOpenUntil 不是整数: %v", err)
	}
	if openUntil <= now.UnixMilli() {
		t.Fatalf("新窗口应在未来，实际 %d（now=%d）", openUntil, now.UnixMilli())
	}
}
