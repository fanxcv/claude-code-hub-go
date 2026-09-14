package adminapi

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住撤销快照的**键形制、payload 字段名与 TTL**——三样错一样就出静默错行为：
// 键名错了 Node 读不到 Go 写的快照（切换期两端并存），payload 字段名错了 operationId 恒不匹配
// （撤销恒 UNDO_CONFLICT），TTL 错了用户在界面上看到撤销按钮却点不动。
//
// 真 Redis 段落未设置 CCH_TEST_REDIS_URL 时跳过（与 auth_issue_redis_test.go 同一门控）；
// 夹具自钉：token 用固定前缀，测试前后按精确键清理。

const providerUndoRedisGate = "CCH_TEST_REDIS_URL"

// fakeUndoKV 是内存替身，用于「过期」「不匹配」这类不必真等 TTL 的分支。
type fakeUndoKV struct {
	values   map[string][]byte
	ttls     map[string]time.Duration
	setCalls int
}

func newFakeUndoKV() *fakeUndoKV {
	return &fakeUndoKV{values: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (f *fakeUndoKV) SetEx(
	_ context.Context,
	key string,
	payload []byte,
	ttl time.Duration,
) error {
	f.setCalls++
	f.values[key] = payload
	f.ttls[key] = ttl
	return nil
}

func (f *fakeUndoKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	payload, found := f.values[key]
	return payload, found, nil
}

func (f *fakeUndoKV) GetDel(_ context.Context, key string) ([]byte, bool, error) {
	payload, found := f.values[key]
	delete(f.values, key)
	return payload, found, nil
}

func (f *fakeUndoKV) Del(_ context.Context, keys ...string) error {
	for _, key := range keys {
		delete(f.values, key)
	}
	return nil
}

func TestProviderUndoTokenShapes(t *testing.T) {
	undoToken := newProviderUndoToken()
	if len(undoToken) <= len("provider_patch_undo_") ||
		undoToken[:len("provider_patch_undo_")] != "provider_patch_undo_" {
		t.Fatalf("撤销 token 前缀不符: %q", undoToken)
	}
	// 删除撤销复用同一个 token 生成器（Node 的 removeProvider 就是这样）；
	// 这里钉住的是「v4 UUID 形状」，避免哪天换成自增数字导致跨语言不可解析。
	uuid := undoToken[len("provider_patch_undo_"):]
	if len(uuid) != 36 || uuid[14] != '4' || uuid[8] != '-' || uuid[13] != '-' || uuid[18] != '-' ||
		uuid[23] != '-' {
		t.Fatalf("token 里的 uuid 不是 v4 形状: %q", uuid)
	}

	operationID := newProviderOperationID()
	if len(operationID) <= len("provider_patch_apply_") ||
		operationID[:len("provider_patch_apply_")] != "provider_patch_apply_" {
		t.Fatalf("operationId 前缀不符: %q", operationID)
	}
}

func TestProviderUndoKeysAndTTLs(t *testing.T) {
	kv := newFakeUndoKV()
	ctx := context.Background()

	deleteSnapshot := ProviderDeleteUndo{
		UndoToken:   "provider_patch_undo_delete_fixture",
		OperationID: "provider_patch_apply_delete_fixture",
		ProviderIDs: []int64{7, 9},
	}
	if err := putProviderDeleteUndo(ctx, kv, deleteSnapshot); err != nil {
		t.Fatalf("写删除撤销快照失败: %v", err)
	}
	deleteKey := "cch:prov:undo-del:provider_patch_undo_delete_fixture"
	if _, found := kv.values[deleteKey]; !found {
		t.Fatalf("删除撤销快照键不符，现有键: %v", keysOf(kv.values))
	}
	if got := kv.ttls[deleteKey]; got != 60*time.Second {
		t.Fatalf("删除撤销 TTL = %v，期望 60s（Node 的 PROVIDER_DELETE_UNDO_TTL_SECONDS）", got)
	}
	// 字段名逐字对齐 Node：Node 侧读的是 undoToken / operationId / providerIds。
	var rawDelete map[string]any
	if err := json.Unmarshal(kv.values[deleteKey], &rawDelete); err != nil {
		t.Fatalf("删除撤销快照不是合法 JSON: %v", err)
	}
	for _, field := range []string{"undoToken", "operationId", "providerIds"} {
		if _, ok := rawDelete[field]; !ok {
			t.Fatalf("删除撤销快照缺字段 %q: %s", field, kv.values[deleteKey])
		}
	}

	patchSnapshot := ProviderPatchUndo{
		UndoToken:   "provider_patch_undo_patch_fixture",
		OperationID: "provider_patch_apply_patch_fixture",
		ProviderIDs: []int64{11},
		Preimage: map[string]map[string]any{
			"11": {"name": "before-name", "priority": 3},
		},
	}
	if err := putProviderPatchUndo(ctx, kv, patchSnapshot); err != nil {
		t.Fatalf("写更新撤销快照失败: %v", err)
	}
	patchKey := "cch:prov:undo-patch:provider_patch_undo_patch_fixture"
	if _, found := kv.values[patchKey]; !found {
		t.Fatalf("更新撤销快照键不符，现有键: %v", keysOf(kv.values))
	}
	if got := kv.ttls[patchKey]; got != 10*time.Second {
		t.Fatalf("更新撤销 TTL = %v，期望 10s（Node 的 PROVIDER_PATCH_UNDO_TTL_SECONDS）", got)
	}
	var rawPatch map[string]any
	if err := json.Unmarshal(kv.values[patchKey], &rawPatch); err != nil {
		t.Fatalf("更新撤销快照不是合法 JSON: %v", err)
	}
	for _, field := range []string{"undoToken", "operationId", "providerIds", "preimage"} {
		if _, ok := rawPatch[field]; !ok {
			t.Fatalf("更新撤销快照缺字段 %q: %s", field, kv.values[patchKey])
		}
	}
	// 未标 durable 的快照不得把 durable 写进 payload：Node 侧靠 durable 的**存在与否**
	// 决定「消费 token 后再写」还是「回退持久账本」，写出 durable:false 会走进账本分支。
	if _, ok := rawPatch["durable"]; ok {
		t.Fatalf("非 durable 快照不应含 durable 字段: %s", kv.values[patchKey])
	}
}

func TestTakeProviderPatchUndoConsumesToken(t *testing.T) {
	kv := newFakeUndoKV()
	ctx := context.Background()
	snapshot := ProviderPatchUndo{
		UndoToken:   "provider_patch_undo_take_fixture",
		OperationID: "provider_patch_apply_take_fixture",
		ProviderIDs: []int64{3},
		Preimage:    map[string]map[string]any{"3": {"name": "old"}},
	}
	if err := putProviderPatchUndo(ctx, kv, snapshot); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}

	first, err := takeProviderPatchUndo(ctx, kv, snapshot.UndoToken)
	if err != nil {
		t.Fatalf("首次读取消失败: %v", err)
	}
	if first == nil || first.OperationID != snapshot.OperationID {
		t.Fatalf("首次读取消得到 %+v", first)
	}
	second, err := takeProviderPatchUndo(ctx, kv, snapshot.UndoToken)
	if err != nil {
		t.Fatalf("二次读取消失败: %v", err)
	}
	if second != nil {
		t.Fatalf("token 应已被消费，二次读到 %+v", second)
	}
}

func TestLoadProviderDeleteUndoKeepsToken(t *testing.T) {
	kv := newFakeUndoKV()
	ctx := context.Background()
	snapshot := ProviderDeleteUndo{
		UndoToken:   "provider_patch_undo_keep_fixture",
		OperationID: "provider_patch_apply_keep_fixture",
		ProviderIDs: []int64{5},
	}
	if err := putProviderDeleteUndo(ctx, kv, snapshot); err != nil {
		t.Fatalf("写快照失败: %v", err)
	}
	loaded, err := loadProviderDeleteUndo(ctx, kv, snapshot.UndoToken)
	if err != nil {
		t.Fatalf("读快照失败: %v", err)
	}
	if loaded == nil || loaded.ProviderIDs[0] != 5 {
		t.Fatalf("读到的快照 %+v", loaded)
	}
	// 删除撤销是「先校验 operationId，再删 token」：读操作不得消费 token，
	// 否则一次 operationId 不匹配的撤销会白白烧掉用户仅有的 60 秒窗口。
	if _, found := kv.values[providerDeleteUndoKey(snapshot.UndoToken)]; !found {
		t.Fatal("读删除撤销快照不应删除 token")
	}
}

// TestProviderUndoRoundTripOnRealRedis 钉住真实 Redis 上的键布局与 TTL——
// 用替身看不出 `GetDel` 在真客户端上的语义，也看不出 TTL 是否真被设置。
func TestProviderUndoRoundTripOnRealRedis(t *testing.T) {
	rawURL := os.Getenv(providerUndoRedisGate)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}

	// 自钉夹具：固定 token，前后按精确键清理。
	const (
		deleteToken = "provider_patch_undo_00000000-0000-4000-8000-0000000undodel"
		patchToken  = "provider_patch_undo_00000000-0000-4000-8000-00000000undopt"
	)
	deleteKey := providerDeleteUndoKey(deleteToken)
	patchKey := providerPatchUndoKey(patchToken)
	t.Cleanup(func() { _ = client.Del(ctx, deleteKey, patchKey).Err() })
	if err := client.Del(ctx, deleteKey, patchKey).Err(); err != nil {
		t.Fatalf("清理夹具键失败: %v", err)
	}

	kv := NewRedisProviderUndoKV(client)
	if kv == nil {
		t.Fatal("NewRedisProviderUndoKV 对非 nil 客户端不应返回 nil")
	}

	deleteSnapshot := ProviderDeleteUndo{
		UndoToken:   deleteToken,
		OperationID: "provider_patch_apply_00000000-0000-4000-8000-00000000undodel",
		ProviderIDs: []int64{101, 102},
	}
	if err := putProviderDeleteUndo(ctx, kv, deleteSnapshot); err != nil {
		t.Fatalf("写删除撤销快照失败: %v", err)
	}
	// TTL 用真客户端读，确认是 60 秒而不是「被默认 0 变成永久」。
	if ttl := client.TTL(ctx, deleteKey).Val(); ttl <= 0 || ttl > 60*time.Second {
		t.Fatalf("删除撤销键 TTL = %v，期望 (0,60s]", ttl)
	}
	// Node 侧要能按同样的键名读到 Go 写的快照（跨语言兼容的最小证据：键名 + payload 形状）。
	stored := client.Get(ctx, deleteKey).Val()
	var asNode map[string]any
	if err := json.Unmarshal([]byte(stored), &asNode); err != nil {
		t.Fatalf("真 Redis 上的快照不是合法 JSON: %v", err)
	}
	if asNode["undoToken"] != deleteToken {
		t.Fatalf("快照 undoToken = %v", asNode["undoToken"])
	}
	for _, field := range []string{"operationId", "providerIds"} {
		if _, ok := asNode[field]; !ok {
			t.Fatalf("快照缺字段 %q: %s", field, stored)
		}
	}

	patchSnapshot := ProviderPatchUndo{
		UndoToken:   patchToken,
		OperationID: "provider_patch_apply_00000000-0000-4000-8000-00000000undopt",
		ProviderIDs: []int64{103},
		Preimage:    map[string]map[string]any{"103": {"name": "老名字"}},
	}
	if err := putProviderPatchUndo(ctx, kv, patchSnapshot); err != nil {
		t.Fatalf("写更新撤销快照失败: %v", err)
	}
	if ttl := client.TTL(ctx, patchKey).Val(); ttl <= 0 || ttl > 10*time.Second {
		t.Fatalf("更新撤销键 TTL = %v，期望 (0,10s]", ttl)
	}
	taken, err := takeProviderPatchUndo(ctx, kv, patchToken)
	if err != nil {
		t.Fatalf("读取消失败: %v", err)
	}
	if taken == nil || taken.Preimage["103"]["name"] != "老名字" {
		t.Fatalf("读到的 preimage 不符: %+v", taken)
	}
	if exists := client.Exists(ctx, patchKey).Val(); exists != 0 {
		t.Fatal("getAndDelete 之后键应不存在")
	}
	// 缺失的键读出来必须是 found=false 而不是错误：撤销窗口过了是正常业务分支。
	if snapshot, err := takeProviderPatchUndo(ctx, kv, patchToken); err != nil || snapshot != nil {
		t.Fatalf("过期 token 应得到 (nil,nil)，实得 (%+v,%v)", snapshot, err)
	}
}

func keysOf(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// TestProviderCircuitConfigSyncOnRealRedis 钉住熔断配置哈希的键与字段名——
// 数据面的熔断读（route.ReadProviderCircuitConfig）只认这三个字段名，写错一个就等于
// 「管理员改了阈值而闸门不变」，且没有任何错误可看。
func TestProviderCircuitConfigSyncOnRealRedis(t *testing.T) {
	rawURL := os.Getenv(providerUndoRedisGate)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}

	// 自钉夹具：用一个不会与真实供应商 id 相撞的高位 id（int 序号不可能到此）。
	const fixtureProviderID int64 = 2_000_000_001
	key := "circuit_breaker:config:" + strconv.FormatInt(fixtureProviderID, 10)
	t.Cleanup(func() { _ = client.Del(ctx, key).Err() })
	if err := client.Del(ctx, key).Err(); err != nil {
		t.Fatalf("清理夹具键失败: %v", err)
	}

	writer := NewRedisProviderCircuitConfig(client)
	if writer == nil {
		t.Fatal("非 nil 客户端不应返回 nil")
	}
	if err := writer.WriteProviderCircuitConfig(ctx, fixtureProviderID, 9, 60000, 3, 600000, 3); err != nil {
		t.Fatalf("写熔断配置失败: %v", err)
	}
	hash := client.HGetAll(ctx, key).Val()
	want := map[string]string{
		"failureThreshold":         "9",
		"openDuration":             "60000",
		"halfOpenSuccessThreshold": "3",
		// 等待阶梯两项：与阈值同一次 HSET 写进哈希（数据面读的就是这两个字段名）。
		"releaseIncrement": "600000",
		"maxOpenCount":     "3",
	}
	if len(hash) != len(want) {
		t.Fatalf("哈希字段数 = %d，期望 %d：%v", len(hash), len(want), hash)
	}
	for field, expected := range want {
		if hash[field] != expected {
			t.Fatalf("字段 %s = %q，期望 %q", field, hash[field], expected)
		}
	}
	// 键无 TTL（Node 的 saveProviderCircuitConfig 也没设过期）。
	if ttl := client.TTL(ctx, key).Val(); ttl > 0 {
		t.Fatalf("熔断配置键不应有过期时间，实得 %v", ttl)
	}
}

// TestProviderThresholdsFromRow 钉住「列为空取出厂默认」这条（否则同步会把 0 写进哈希，
// 而 route 侧把非正阈值解读为「熔断被关闭」——一次改名就能把熔断关掉）。
func TestProviderThresholdsFromRow(t *testing.T) {
	empty := providerThresholdsFromRow(nil)
	if empty.FailureThreshold != 5 || empty.OpenDurationMS != 1800000 || empty.HalfOpenSuccessThreshold != 2 {
		t.Fatalf("零值行应取出厂默认，实得 %+v", empty)
	}
	threshold := 9
	openDuration := 1234
	halfOpen := 7
	row := &store.AdminProvider{
		CircuitFailureThreshold:  &threshold,
		CircuitOpenDuration:      &openDuration,
		CircuitHalfOpenThreshold: &halfOpen,
	}
	got := providerThresholdsFromRow(row)
	if got.FailureThreshold != 9 || got.OpenDurationMS != 1234 || got.HalfOpenSuccessThreshold != 7 {
		t.Fatalf("从行取阈值不符: %+v", got)
	}
}
