package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// goldenDir 是语言中立黄金样本目录，与 internal/convert 的语料读取保持同一相对路径约定。
const goldenDir = "../../../tests/load/redis-parity/golden"

// 与 cfgsync 的集成测试同一条隔离纪律：只允许 DB index >= 13，未设门控变量则整组跳过。
const (
	testRedisEnv      = "CCH_TEST_REDIS_URL"
	testRedisMinDB    = 13
	testRedisDeadline = 10 * time.Second
	// ttlToleranceSec 吸收「执行」与「读 TTL」之间的真实时间流逝。
	// 黄金样本记录的是秒级 TTL，跨秒执行会让 60 变成 59，因此正数 TTL 按容差比较；
	// 负数（-1 无过期、-2 不存在）是 Redis 哨兵值，须精确比较；
	// 注意 go-redis 把秒级哨兵原样放进 time.Duration（即 -1ns 而非 -1s），因此不能用 Seconds() 取整。
	ttlToleranceSec = 2
)

type keyState struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
	TTL   int64           `json:"ttl"`
}

type goldenCall struct {
	ID       string               `json:"id"`
	Keys     []string             `json:"keys"`
	Argv     []string             `json:"argv"`
	Setup    [][]string           `json:"setup"`
	Returned json.RawMessage      `json:"returned"`
	After    map[string]*keyState `json:"after"`
}

type goldenFile struct {
	ConstName string       `json:"constName"`
	File      string       `json:"file"`
	SHA256    string       `json:"sha256"`
	Calls     []goldenCall `json:"calls"`
}

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()

	raw := os.Getenv(testRedisEnv)
	if raw == "" {
		t.Skipf("未设置 %s，跳过 Redis 集成测试", testRedisEnv)
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", testRedisEnv, err)
	}
	if options.DB < testRedisMinDB {
		t.Fatalf("%s 必须使用 DB index >= %d，收到 %d", testRedisEnv, testRedisMinDB, options.DB)
	}
	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), testRedisDeadline)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("连接 Redis 失败：%v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestGoldenReplay 是本包的验收主体：对每段 Lua、每个场景断言
// 「同一 Lua 原文 + 同一 KEYS/ARGV」在 Go 侧得到与 Node 侧黄金样本相同的返回值与键终态。
func TestGoldenReplay(t *testing.T) {
	rdb := testRedisClient(t)
	ctx := context.Background()

	registry, err := Load()
	if err != nil {
		t.Fatalf("加载脚本失败: %v", err)
	}
	caller, err := New(rdb, registry)
	if err != nil {
		t.Fatalf("组装调用层失败: %v", err)
	}

	paths, err := filepath.Glob(filepath.Join(goldenDir, "*.json"))
	if err != nil {
		t.Fatalf("列黄金样本失败: %v", err)
	}
	if len(paths) != registry.Len() {
		t.Fatalf("黄金样本数 %d 与脚本数 %d 不符", len(paths), registry.Len())
	}
	sort.Strings(paths)

	scenarios := 0
	for _, path := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			golden := readGolden(t, path)
			script, ok := registry.LookupFile(golden.File)
			if !ok {
				t.Fatalf("脚本表中缺少 %s", golden.File)
			}
			if script.ConstName != golden.ConstName {
				t.Fatalf("常量名不符: 清单 %s, 黄金 %s", script.ConstName, golden.ConstName)
			}
			// 脚本原文的同一性：黄金样本采集自该摘要的正文，摘要不符即基准失效。
			if script.SHA256 != golden.SHA256 {
				t.Fatalf("脚本摘要不符: 内嵌 %s, 黄金 %s", script.SHA256, golden.SHA256)
			}
			if len(golden.Calls) == 0 {
				t.Fatal("黄金样本没有场景")
			}
			scenarios += len(golden.Calls)
			for _, call := range golden.Calls {
				t.Run(call.ID, func(t *testing.T) {
					replayCall(t, ctx, rdb, caller, script, call)
				})
			}
		})
	}
	if scenarios == 0 {
		t.Fatal("未执行任何黄金场景")
	}
	t.Logf("黄金场景总数: %d", scenarios)
}

func readGolden(t *testing.T, path string) goldenFile {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取黄金样本失败: %v", err)
	}
	var golden goldenFile
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("解析黄金样本失败: %v", err)
	}
	return golden
}

func replayCall(t *testing.T, ctx context.Context, rdb *redis.Client, caller *Client, script *Script, call goldenCall) {
	t.Helper()

	touched := touchedKeys(call)
	for _, key := range touched {
		if err := rdb.Del(ctx, key).Err(); err != nil {
			t.Fatalf("清理键 %s 失败: %v", key, err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), testRedisDeadline)
		defer cancel()
		_ = rdb.Del(cleanupCtx, touched...).Err()
	})

	for _, command := range call.Setup {
		if len(command) == 0 {
			t.Fatalf("场景 %s 的 setup 含空命令", call.ID)
		}
		args := make([]any, 0, len(command))
		for _, item := range command {
			args = append(args, item)
		}
		if err := rdb.Do(ctx, args...).Err(); err != nil {
			t.Fatalf("执行 setup %v 失败: %v", command, err)
		}
	}

	argv := make([]any, 0, len(call.Argv))
	for _, item := range call.Argv {
		argv = append(argv, item)
	}

	actual, evalErr := caller.Eval(ctx, script, call.Keys, argv)
	compareReturned(t, call.ID, call.Returned, actual, evalErr)
	verifyAfter(t, ctx, rdb, call)
}

// compareReturned 把「脚本返回值」与黄金样本逐结构比对。
// 类型必须一致：{"ok","updated","101","7"} 与 "101" 不可互换，整数 1 与字符串 "1" 也不可互换。
func compareReturned(t *testing.T, id string, expected json.RawMessage, actual any, evalErr error) {
	t.Helper()

	var expectedErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(expected, &expectedErr); err == nil && expectedErr.Error != "" {
		if evalErr == nil {
			t.Fatalf("场景 %s 期望脚本报错 %q，实际返回值 %#v", id, expectedErr.Error, actual)
		}
		var wrapped *EvalError
		if !errors.As(evalErr, &wrapped) {
			t.Fatalf("场景 %s 的错误未经 EvalError 包装: %v", id, evalErr)
		}
		if got := errorMessage(evalErr); got != expectedErr.Error {
			t.Fatalf("场景 %s 错误文本不符: 期望 %q, 实际 %q", id, expectedErr.Error, got)
		}
		if wrapped.Kind != ErrorReply {
			t.Fatalf("场景 %s 的脚本主动报错应归类为 %s，实际 %s", id, ErrorReply, wrapped.Kind)
		}
		return
	}

	if evalErr != nil {
		t.Fatalf("场景 %s 执行失败: %v", id, evalErr)
	}

	normalized, err := canonicalJSON(actual)
	if err != nil {
		t.Fatalf("场景 %s 的返回值无法序列化: %v", id, err)
	}
	var want, got any
	if err := json.Unmarshal(expected, &want); err != nil {
		t.Fatalf("场景 %s 的期望值不是合法 JSON: %v", id, err)
	}
	if err := json.Unmarshal(normalized, &got); err != nil {
		t.Fatalf("场景 %s 的实际值不是合法 JSON: %v", id, err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("场景 %s 返回值不符:\n  期望 %s\n  实际 %s", id, expected, normalized)
	}
}

// verifyAfter 断言调用后的键终态（类型、取值、TTL）与黄金样本一致。
func verifyAfter(t *testing.T, ctx context.Context, rdb *redis.Client, call goldenCall) {
	t.Helper()

	for key, want := range call.After {
		gotType, err := rdb.Type(ctx, key).Result()
		if err != nil {
			t.Fatalf("场景 %s 读取键 %s 类型失败: %v", call.ID, key, err)
		}
		if want == nil {
			if gotType != "none" {
				t.Fatalf("场景 %s 期望键 %s 不存在，实际类型 %s", call.ID, key, gotType)
			}
			continue
		}
		if gotType != want.Type {
			t.Fatalf("场景 %s 键 %s 类型不符: 期望 %s, 实际 %s", call.ID, key, want.Type, gotType)
		}

		switch want.Type {
		case "string":
			value, err := rdb.Get(ctx, key).Result()
			if err != nil {
				t.Fatalf("场景 %s 读取键 %s 失败: %v", call.ID, key, err)
			}
			compareKeyValue(t, call.ID, key, want.Value, value)
		case "hash":
			value, err := rdb.HGetAll(ctx, key).Result()
			if err != nil {
				t.Fatalf("场景 %s 读取 hash %s 失败: %v", call.ID, key, err)
			}
			compareKeyValue(t, call.ID, key, want.Value, value)
		case "zset":
			members, err := rdb.ZRangeWithScores(ctx, key, 0, -1).Result()
			if err != nil {
				t.Fatalf("场景 %s 读取 zset %s 失败: %v", call.ID, key, err)
			}
			value := make(map[string]string, len(members))
			for _, member := range members {
				name, ok := member.Member.(string)
				if !ok {
					t.Fatalf("场景 %s 的 zset %s 成员不是字符串: %#v", call.ID, key, member.Member)
				}
				value[name] = strconv.FormatFloat(member.Score, 'f', -1, 64)
			}
			compareKeyValue(t, call.ID, key, want.Value, value)
		default:
			t.Fatalf("场景 %s 键 %s 出现未支持的黄金类型 %s", call.ID, key, want.Type)
		}

		ttl, err := rdb.TTL(ctx, key).Result()
		if err != nil {
			t.Fatalf("场景 %s 读取键 %s TTL 失败: %v", call.ID, key, err)
		}
		gotTTL := int64(ttl)
		if ttl >= 0 {
			gotTTL = int64(ttl / time.Second)
		}
		if want.TTL < 0 {
			if gotTTL != want.TTL {
				t.Fatalf("场景 %s 键 %s TTL 不符: 期望 %d, 实际 %d", call.ID, key, want.TTL, gotTTL)
			}
			continue
		}
		if diff := gotTTL - want.TTL; diff > ttlToleranceSec || diff < -ttlToleranceSec {
			t.Fatalf("场景 %s 键 %s TTL 超出容差: 期望 %d, 实际 %d", call.ID, key, want.TTL, gotTTL)
		}
	}
}

func compareKeyValue(t *testing.T, id, key string, want json.RawMessage, actual any) {
	t.Helper()
	normalized, err := canonicalJSON(actual)
	if err != nil {
		t.Fatalf("场景 %s 键 %s 的值无法序列化: %v", id, key, err)
	}
	var wantValue, gotValue any
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("场景 %s 键 %s 的期望值非法: %v", id, key, err)
	}
	if err := json.Unmarshal(normalized, &gotValue); err != nil {
		t.Fatalf("场景 %s 键 %s 的实际值非法: %v", id, key, err)
	}
	if !reflect.DeepEqual(wantValue, gotValue) {
		t.Fatalf("场景 %s 键 %s 取值不符:\n  期望 %s\n  实际 %s", id, key, want, normalized)
	}
}

// TestEvalFallsBackAfterScriptFlush 验证「EVALSHA 命中失败即回退 EVAL」这条必需语义：
// 预热后走 EVALSHA，SCRIPT FLUSH 清空缓存后同一调用必须仍然成功，而不是报 NOSCRIPT。
func TestEvalFallsBackAfterScriptFlush(t *testing.T) {
	rdb := testRedisClient(t)
	ctx := context.Background()

	registry, err := Load()
	if err != nil {
		t.Fatalf("加载脚本失败: %v", err)
	}
	caller, err := New(rdb, registry)
	if err != nil {
		t.Fatalf("组装调用层失败: %v", err)
	}
	script, ok := registry.LookupConst("DELETE_LEGACY_PROVIDER_IF_VALUE")
	if !ok {
		t.Fatal("缺少 DELETE_LEGACY_PROVIDER_IF_VALUE")
	}

	loaded, err := caller.Preload(ctx)
	if err != nil {
		t.Fatalf("预热失败: %v", err)
	}
	if loaded != registry.Len() {
		t.Fatalf("预热脚本数不符: 期望 %d, 实际 %d", registry.Len(), loaded)
	}

	key := "go-ratelimit-it-fallback"
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })
	if err := rdb.Del(ctx, key).Err(); err != nil {
		t.Fatalf("清理键失败: %v", err)
	}

	if value, err := caller.Eval(ctx, script, []string{key}, []any{"7"}); err != nil || value != int64(0) {
		t.Fatalf("预热后调用失败: value=%#v err=%v", value, err)
	}

	if err := rdb.ScriptFlush(ctx).Err(); err != nil {
		t.Fatalf("SCRIPT FLUSH 失败: %v", err)
	}

	if err := rdb.Set(ctx, key, "7", 0).Err(); err != nil {
		t.Fatalf("写前置键失败: %v", err)
	}
	value, err := caller.Eval(ctx, script, []string{key}, []any{"7"})
	if err != nil {
		t.Fatalf("SCRIPT FLUSH 后调用必须回退 EVAL 并成功，实际失败: %v", err)
	}
	if value != int64(1) {
		t.Fatalf("回退后返回值不符: %#v", value)
	}
	if exists, err := rdb.Exists(ctx, key).Result(); err != nil || exists != 0 {
		t.Fatalf("脚本应删除命中键: exists=%d err=%v", exists, err)
	}
}

func touchedKeys(call goldenCall) []string {
	seen := map[string]bool{}
	keys := make([]string, 0, len(call.Keys)+len(call.After))
	for _, key := range call.Keys {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	for key := range call.After {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

// canonicalJSON 把任意 Redis 返回值规范化为紧凑 JSON，供结构比对。
func canonicalJSON(value any) ([]byte, error) {
	if value == nil {
		return []byte("null"), nil
	}
	return json.Marshal(value)
}

// errorMessage 取 Redis 返回的原始错误文本（剥掉本包包装）。
func errorMessage(err error) string {
	var wrapped *EvalError
	if errors.As(err, &wrapped) {
		return wrapped.Err.Error()
	}
	return err.Error()
}
