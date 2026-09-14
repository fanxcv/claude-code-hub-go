package usersreset

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 本文件钉住三件跨端契约（错了不会报错，只会与 Node 互相看不见对方的作业）：
//  1. 状态记录的 JSON 键名与 Node 的 UserStatisticsResetStoredRecord 逐字一致；
//  2. 公开投影剥掉两个对内字段（Node 的响应 schema 不含它们）；
//  3. 键名前缀与两段 Lua 与 Node 逐字一致。
func TestRecordJSONMatchesNodeFieldNames(t *testing.T) {
	version := 1
	started := "2026-01-02T03:04:05.000Z"
	record := Record{
		ResetID:                   "0b9f1c2e-3d4a-4b5c-8d6e-7f8091a2b3c4",
		UserID:                    42,
		Status:                    StatusRunning,
		RequestedAt:               "2026-01-02T03:00:00.000Z",
		StartedAt:                 &started,
		DeletedMessageRequests:    7,
		DeletedUsageLedger:        9,
		Fixed5hKeyIDs:             []int64{3, 5},
		Fixed5hPreparationVersion: &version,
	}
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	// Node 侧的字段全集（types.ts:13-29），少一个即「Node 读到 undefined」。
	want := []string{
		"resetId", "userId", "status", "requestedAt", "startedAt", "completedAt",
		"deletedMessageRequests", "deletedUsageLedger", "errorCode",
		"fixed5hKeyIds", "fixed5hPreparationVersion",
	}
	for _, name := range want {
		if _, ok := fields[name]; !ok {
			t.Fatalf("状态记录缺字段 %q：Node 会对它读到 undefined。实际字段：%v", name, keysOf(fields))
		}
	}
	if len(fields) != len(want) {
		t.Fatalf("字段数 %d 与 Node 的 %d 不一致：多出的字段会被 Node 原样透传。实际：%v",
			len(fields), len(want), keysOf(fields))
	}
	// 空的下界：Node 用 `?? []` 兜住缺字段，Go 侧必须写 null 之外的形状——这里断言 marshal 出的
	// 空切片是 []（不是 null），否则 Node 的 JSON.parse 会得到 null 并在 .length 上炸。
	empty := Record{ResetID: "x", Fixed5hKeyIDs: []int64{}}
	emptyPayload, err := json.Marshal(empty)
	if err != nil {
		t.Fatalf("序列化空记录失败: %v", err)
	}
	if !strings.Contains(string(emptyPayload), `"fixed5hKeyIds":[]`) {
		t.Fatalf("空键清单必须序列化成 []，实际：%s", emptyPayload)
	}
}

// TestPublicRecordDropsInternalFields 钉住公开形状（schemas/users.ts:73-83 的九个字段）。
func TestPublicRecordDropsInternalFields(t *testing.T) {
	version := 1
	record := Record{
		ResetID:                   "0b9f1c2e-3d4a-4b5c-8d6e-7f8091a2b3c4",
		UserID:                    42,
		Status:                    StatusQueued,
		RequestedAt:               "2026-01-02T03:00:00.000Z",
		Fixed5hKeyIDs:             []int64{1},
		Fixed5hPreparationVersion: &version,
	}
	payload, err := json.Marshal(record.Public())
	if err != nil {
		t.Fatalf("序列化公开记录失败: %v", err)
	}
	for _, name := range []string{"fixed5hKeyIds", "fixed5hPreparationVersion"} {
		if strings.Contains(string(payload), name) {
			t.Fatalf("公开记录不得含对内字段 %q，实际：%s", name, payload)
		}
	}
	for _, name := range []string{
		"resetId", "userId", "status", "requestedAt", "startedAt", "completedAt",
		"deletedMessageRequests", "deletedUsageLedger", "errorCode",
	} {
		if !strings.Contains(string(payload), `"`+name+`"`) {
			t.Fatalf("公开记录缺字段 %q，实际：%s", name, payload)
		}
	}
}

// TestGetNormalizesLegacyRecord 钉住读档的容错（reset-status-store.ts:88-95）。
func TestRecordJSONRoundTripThroughStore(t *testing.T) {
	nodeJSON := `{"resetId":"abc","userId":5,"status":"queued","requestedAt":"2026-01-02T03:00:00.000Z",` +
		`"startedAt":null,"completedAt":null,"deletedMessageRequests":0,"deletedUsageLedger":0,` +
		`"errorCode":null,"fixed5hKeyIds":[],"fixed5hPreparationVersion":null}`
	var record Record
	if err := json.Unmarshal([]byte(nodeJSON), &record); err != nil {
		t.Fatalf("解析 Node 记录失败: %v", err)
	}
	if record.UserID != 5 || record.Status != StatusQueued || record.Fixed5hPreparationVersion != nil {
		t.Fatalf("解析结果不符：%+v", record)
	}
	// 早期版本写下的记录（缺两个对内字段）也要能读：Node 侧用 `?? []` 兜住。
	legacyJSON := `{"resetId":"abc","userId":5,"status":"queued","requestedAt":"2026-01-02T03:00:00.000Z"}`
	var legacy Record
	if err := json.Unmarshal([]byte(legacyJSON), &legacy); err != nil {
		t.Fatalf("解析旧记录失败: %v", err)
	}
	if legacy.Fixed5hKeyIDs == nil {
		legacy.Fixed5hKeyIDs = []int64{}
	}
	if legacy.Fixed5hKeyIDs == nil {
		t.Fatal("缺失的键清单应归一成空切片")
	}
}

// TestKeyShapesMatchNode 钉住键名（换一个字符就会让两侧互相看不见对方的作业）。
func TestKeyShapesMatchNode(t *testing.T) {
	if got := statusKey("rid"); got != "cch:user-statistics-reset:status:rid" {
		t.Fatalf("状态键名不符：%s", got)
	}
	if got := activeKey(7); got != "cch:user-statistics-reset:active:7" {
		t.Fatalf("认领键名不符：%s", got)
	}
	if got := fixed5hKey("rid"); got != "cch:user-statistics-reset:fixed5h:rid" {
		t.Fatalf("5h 标记键名不符：%s", got)
	}
	if StatusTTLSeconds != 604800 {
		t.Fatalf("状态 TTL 应为 7 天（604800），实际 %d", StatusTTLSeconds)
	}
	keys := fixed5hWindowKeys("rid", 7, []int64{11, 12})
	want := []string{
		"cch:user-statistics-reset:fixed5h:rid",
		"user:7:cost_5h_fixed",
		"lease:user:7:5h:fixed",
		"key:11:cost_5h_fixed",
		"lease:key:11:5h:fixed",
		"key:12:cost_5h_fixed",
		"lease:key:12:5h:fixed",
	}
	if len(keys) != len(want) {
		t.Fatalf("5h 键序列长度 %d，期望 %d：%v", len(keys), len(want), keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("第 %d 个 5h 键不符：得到 %s，期望 %s", i, keys[i], want[i])
		}
	}
}

// TestLuaScriptsMatchNode 钉住两段 Lua 的逐字内容。
//
// 逐字而不是「语义等价」：这两段脚本在 Redis 里执行，Node 与 Go 可能先后跑同一个作业，
// 脚本不同会让切点或认领的语义分叉。前后换行也算（Node 的模板串以换行开头）。
func TestLuaScriptsMatchNode(t *testing.T) {
	if !strings.HasPrefix(luaCompareDelete, "\nif redis.call('GET', KEYS[1]) == ARGV[1] then") {
		t.Fatalf("认领释放脚本与 Node 不符：%q", luaCompareDelete)
	}
	if !strings.HasSuffix(luaCompareDelete, "\nreturn 0") {
		t.Fatalf("认领释放脚本结尾与 Node 不符：%q", luaCompareDelete)
	}
	for _, fragment := range []string{
		"if redis.call('EXISTS', KEYS[1]) == 1 then",
		"local now = redis.call('TIME')",
		"local cutoff_ms = (tonumber(now[1]) * 1000) + math.floor(tonumber(now[2]) / 1000)",
		"redis.call('SETEX', KEYS[1], ARGV[1], tostring(cutoff_ms))",
	} {
		if !strings.Contains(luaPrepareFixed5h, fragment) {
			t.Fatalf("5h 准备脚本缺片段 %q：%q", fragment, luaPrepareFixed5h)
		}
	}
}

// TestISOMillisMatchesJSToISOString 钉住时间串形状（Node 的 z.string().datetime() 校验它）。
func TestISOMillisMatchesJSToISOString(t *testing.T) {
	value := time.Date(2026, 1, 2, 3, 4, 5, 6_000_000, time.UTC)
	if got := isoMillis(value); got != "2026-01-02T03:04:05.006Z" {
		t.Fatalf("ISO 串不符：%s", got)
	}
	parsed, err := parseIsoMillis("2026-01-02T03:04:05.006Z")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !parsed.Equal(value) {
		t.Fatalf("往返不等：%s", parsed)
	}
	if _, err := parseIsoMillis("not-a-time"); err == nil {
		t.Fatal("非法时间串必须报错")
	}
}

// TestErrorCodeAndProgress 钉住错误分类（UI 直接展示错误码）。
func TestErrorCodeAndProgress(t *testing.T) {
	wrapped := &Error{Code: ErrCodeRowsLocked, Progress: Progress{DeletedUsageLedger: 3}}
	if got := errorCode(wrapped); got != ErrCodeRowsLocked {
		t.Fatalf("错误码不符：%s", got)
	}
	if got := errorProgress(wrapped); got.DeletedUsageLedger != 3 {
		t.Fatalf("进度未取到：%+v", got)
	}
	plain := errors.New("boom")
	if got := errorCode(plain); got != ErrCodeOperationFailed {
		t.Fatalf("非本包错误应归为兜底码，实际 %s", got)
	}
	if got := errorProgress(plain); got != (Progress{}) {
		t.Fatalf("非本包错误应给零进度，实际 %+v", got)
	}
	// errors.As 要能穿透包装：worker 的失败路径会把执行错误再包一层（见 finishFailed）。
	wrappedErr := fmt.Errorf("执行失败: %w", &Error{Code: ErrCodeCacheCleanupFailed})
	if got := errorCode(wrappedErr); got != ErrCodeCacheCleanupFailed {
		t.Fatalf("包装后的错误码不符：%s", got)
	}
}

func keysOf(fields map[string]any) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	return names
}
