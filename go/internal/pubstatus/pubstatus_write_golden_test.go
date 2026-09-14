package pubstatus

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// 本文件是投影**写侧**的 golden 对照：夹具与期望值全部来自 Node 原函数
// （`scripts/public-status-write-golden.ts`），不是人手写的期望。
//
// 为什么必须这样测：写侧的每条规则（增量折算、桶键与字段编码、coverage-start 的 NX 语义、
// 投影记录的键序与 TTL、manifest 提升规则）都是**跨语言契约**——双跑期 Node 与 Go 读写同一批
// 键。任一处分歧都会让两侧互相把对方的投影判成「另一代」，从而在公开页上表现为数据抖动。
// golden 里落的是 Node 写出的**逐字节正文**，故对照就是逐字节。

const writeGoldenPath = "testdata/node_public_status_write_golden.json"

type writeGolden struct {
	NowISO        string           `json:"nowIso"`
	ConfigVersion string           `json:"configVersion"`
	Prefix        string           `json:"prefix"`
	RollupTTLSecs int              `json:"rollupTtlSeconds"`
	Groups        []map[string]any `json:"groups"`
	Events        []struct {
		Name  string          `json:"name"`
		Event json.RawMessage `json:"event"`
	} `json:"events"`
	Increments []struct {
		Name       string `json:"name"`
		Increments []struct {
			GroupID  string  `json:"groupId"`
			ModelKey string  `json:"modelKey"`
			Metric   string  `json:"metric"`
			Value    float64 `json:"value"`
		} `json:"increments"`
	} `json:"increments"`
	BucketStarts []struct {
		RangeHours      int      `json:"rangeHours"`
		IntervalMinutes int      `json:"intervalMinutes"`
		BucketStarts    []string `json:"bucketStarts"`
	} `json:"bucketStarts"`
	WriteResults []struct {
		Name           string  `json:"name"`
		Written        bool    `json:"written"`
		Retryable      bool    `json:"retryable"`
		Reason         *string `json:"reason"`
		IncrementCount int     `json:"incrementCount"`
		Key            *string `json:"key"`
	} `json:"writeResults"`
	RollupRedis struct {
		Keys map[string]string  `json:"keys"`
		TTLs map[string]float64 `json:"ttls"`
	} `json:"rollupRedis"`
	Payload struct {
		GeneratedAt string           `json:"generatedAt"`
		CoveredFrom string           `json:"coveredFrom"`
		CoveredTo   string           `json:"coveredTo"`
		Groups      []map[string]any `json:"groups"`
	} `json:"payload"`
	RebuildResult struct {
		Status           string `json:"status"`
		SourceGeneration string `json:"sourceGeneration"`
	} `json:"rebuildResult"`
	ProjectionRedis struct {
		Keys map[string]string  `json:"keys"`
		TTLs map[string]float64 `json:"ttls"`
	} `json:"projectionRedis"`
}

func loadWriteGolden(t *testing.T) *writeGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(writeGoldenPath))
	if err != nil {
		t.Fatalf("读 golden 失败（先跑 `bun scripts/public-status-write-golden.ts`）: %v", err)
	}
	var golden writeGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("解析 golden 失败: %v", err)
	}
	return &golden
}

// goldenConfiguredGroups 把 golden 里的 groups（Node 形状）转成 Go 的 ConfiguredGroup。
func goldenConfiguredGroups(t *testing.T, raw []map[string]any) []ConfiguredGroup {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("重新编码 groups 失败: %v", err)
	}
	var decoded []struct {
		SourceGroupID   *int64  `json:"sourceGroupId"`
		SourceGroupName string  `json:"sourceGroupName"`
		PublicGroupSlug string  `json:"publicGroupSlug"`
		DisplayName     string  `json:"displayName"`
		ExplanatoryCopy *string `json:"explanatoryCopy"`
		SortOrder       float64 `json:"sortOrder"`
		Models          []struct {
			PublicModelKey   string `json:"publicModelKey"`
			Label            string `json:"label"`
			VendorIconKey    any    `json:"vendorIconKey"`
			RequestTypeBadge string `json:"requestTypeBadge"`
		} `json:"models"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("解析 groups 失败: %v", err)
	}
	groups := make([]ConfiguredGroup, 0, len(decoded))
	for _, group := range decoded {
		models := make([]ConfiguredModel, 0, len(group.Models))
		for _, model := range group.Models {
			models = append(models, ConfiguredModel{
				PublicModelKey:   model.PublicModelKey,
				Label:            model.Label,
				RequestTypeBadge: model.RequestTypeBadge,
			})
		}
		groups = append(groups, ConfiguredGroup{
			SourceGroupID:   group.SourceGroupID,
			SourceGroupName: group.SourceGroupName,
			PublicGroupSlug: group.PublicGroupSlug,
			DisplayName:     group.DisplayName,
			ExplanatoryCopy: group.ExplanatoryCopy,
			SortOrder:       group.SortOrder,
			Models:          models,
		})
	}
	return groups
}

// goldenEvent 把 golden 里的单个事件（Node 形状）转成 Go 的 RollupEvent。
func goldenEvent(t *testing.T, raw json.RawMessage) RollupEvent {
	t.Helper()
	var decoded struct {
		CreatedAt     string   `json:"createdAt"`
		Model         *string  `json:"model"`
		OriginalModel *string  `json:"originalModel"`
		DurationMs    *float64 `json:"durationMs"`
		TTFTMs        *float64 `json:"ttftMs"`
		FirstByteMs   *float64 `json:"firstByteMs"`
		OutputTokens  *int64   `json:"outputTokens"`
		ProviderChain []struct {
			StatusCode   *int    `json:"statusCode"`
			Reason       *string `json:"reason"`
			ErrorMessage *string `json:"errorMessage"`
			GroupTag     *string `json:"groupTag"`
			ErrorDetails *struct {
				MatchedRule any `json:"matchedRule"`
			} `json:"errorDetails"`
		} `json:"providerChain"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("解析事件失败: %v", err)
	}
	createdAt, err := ParseISOMilli(decoded.CreatedAt)
	if err != nil {
		t.Fatalf("解析事件时刻失败: %v", err)
	}
	chain := make([]ProviderChainItem, 0, len(decoded.ProviderChain))
	for _, item := range decoded.ProviderChain {
		entry := ProviderChainItem{
			StatusCode: item.StatusCode, Reason: item.Reason,
			ErrorMessage: item.ErrorMessage, GroupTag: item.GroupTag,
		}
		if item.ErrorDetails != nil {
			entry.ErrorDetails = &ProviderChainErrorDetails{MatchedRule: item.ErrorDetails.MatchedRule}
		}
		chain = append(chain, entry)
	}
	return RollupEvent{
		CreatedAt: createdAt, Model: decoded.Model, OriginalModel: decoded.OriginalModel,
		DurationMs: decoded.DurationMs, TTFTMs: decoded.TTFTMs, FirstByteMs: decoded.FirstByteMs,
		OutputTokens: decoded.OutputTokens, ProviderChain: chain,
	}
}

// TestWriteGoldenRollupIncrementsComparesNode 对照一次事件的增量清单。
func TestWriteGoldenRollupIncrementsComparesNode(t *testing.T) {
	golden := loadWriteGolden(t)
	groups := goldenConfiguredGroups(t, golden.Groups)
	for index, item := range golden.Increments {
		event := goldenEvent(t, golden.Events[index].Event)
		got := BuildRollupIncrements(event, groups)
		if len(got) != len(item.Increments) {
			t.Fatalf("事件 %s：增量条数 Go=%d Node=%d（go=%+v node=%+v）",
				item.Name, len(got), len(item.Increments), got, item.Increments)
		}
		for position, want := range item.Increments {
			actual := got[position]
			if actual.GroupID != want.GroupID || actual.ModelKey != want.ModelKey ||
				string(actual.Metric) != want.Metric || actual.Value != want.Value {
				t.Fatalf("事件 %s 第 %d 条增量不一致：Go=%+v Node=%+v", item.Name, position, actual, want)
			}
		}
	}
}

// TestWriteGoldenBucketStartsComparesNode 对照窗口与桶起点。
func TestWriteGoldenBucketStartsComparesNode(t *testing.T) {
	golden := loadWriteGolden(t)
	now, err := ParseISOMilli(golden.NowISO)
	if err != nil {
		t.Fatalf("解析 now 失败: %v", err)
	}
	for _, combo := range golden.BucketStarts {
		coveredFrom, coveredTo, starts, err := BuildRollupBucketStarts(now, combo.RangeHours, combo.IntervalMinutes)
		if err != nil {
			t.Fatalf("构造桶起点失败: %v", err)
		}
		if len(starts) != len(combo.BucketStarts) {
			t.Fatalf("%dh/%dm：桶数 Go=%d Node=%d", combo.RangeHours, combo.IntervalMinutes, len(starts), len(combo.BucketStarts))
		}
		for index := range starts {
			if starts[index] != combo.BucketStarts[index] {
				t.Fatalf("%dh/%dm 第 %d 个桶起点：Go=%s Node=%s",
					combo.RangeHours, combo.IntervalMinutes, index, starts[index], combo.BucketStarts[index])
			}
		}
		// coveredFrom/coveredTo 由同一函数产出，golden 的 payload 段里带同样口径，这里用对齐结果兜底校验。
		if _, err := AlignBucketStartUTC(coveredTo, combo.IntervalMinutes); err != nil {
			t.Fatalf("coveredTo 不是合法 ISO: %v", err)
		}
		if _, err := AlignBucketStartUTC(coveredFrom, PublicStatusRollupBucketMinutes); err != nil {
			t.Fatalf("coveredFrom 不是合法 ISO: %v", err)
		}
	}
}

// TestWriteGoldenRollupWriteComparesNode 对照事件写入 Redis 后的**全部键值**。
func TestWriteGoldenRollupWriteComparesNode(t *testing.T) {
	golden := loadWriteGolden(t)
	groups := goldenConfiguredGroups(t, golden.Groups)
	redis := newFakeProjectionRedis()
	for index, item := range golden.Events {
		event := goldenEvent(t, item.Event)
		result, err := WriteRollupEvent(context.Background(), redis, event, groups, golden.Prefix)
		if err != nil {
			t.Fatalf("事件 %s 写入失败: %v", item.Name, err)
		}
		want := golden.WriteResults[index]
		if result.Written != want.Written || result.Retryable != want.Retryable ||
			result.IncrementCount != want.IncrementCount {
			t.Fatalf("事件 %s 写入结果不一致：Go=%+v Node=%+v", item.Name, result, want)
		}
		if want.Key != nil && result.Key != *want.Key {
			t.Fatalf("事件 %s 桶键不一致：Go=%s Node=%s", item.Name, result.Key, *want.Key)
		}
		if !want.Written {
			if want.Reason != nil && string(result.Reason) != *want.Reason {
				t.Fatalf("事件 %s 未写入原因不一致：Go=%s Node=%s", item.Name, result.Reason, *want.Reason)
			}
		}
	}

	gotKeys, gotTTLs := redis.dump()
	if diff := diffStringMaps(gotKeys, golden.RollupRedis.Keys); diff != "" {
		t.Fatalf("rollup 键值不一致：%s", diff)
	}
	// TTL：Node 记的是秒（expire 的单位），这里逐键比整数秒。
	for key, wantSeconds := range golden.RollupRedis.TTLs {
		gotSeconds, ok := gotTTLs[key]
		if !ok {
			t.Fatalf("缺少 TTL: %s（应为 %v 秒）", key, wantSeconds)
		}
		if gotSeconds != wantSeconds {
			t.Fatalf("TTL 不一致 %s：Go=%v Node=%v", key, gotSeconds, wantSeconds)
		}
	}
	if RollupTTLSeconds != golden.RollupTTLSecs {
		t.Fatalf("rollup TTL 常量不一致：Go=%d Node=%d", RollupTTLSeconds, golden.RollupTTLSecs)
	}
}

// TestWriteGoldenPayloadComparesNode 对照从桶聚合出的 payload（逐字段）。
func TestWriteGoldenPayloadComparesNode(t *testing.T) {
	golden := loadWriteGolden(t)
	groups := goldenConfiguredGroups(t, golden.Groups)
	now, err := ParseISOMilli(golden.NowISO)
	if err != nil {
		t.Fatalf("解析 now 失败: %v", err)
	}
	// 用 golden 里 Node 写出的桶作为输入（同一批数据、两条独立实现各自聚合）。
	buckets := bucketsFromGolden(golden, golden.Prefix)
	rangeHours, intervalMinutes := 24, 15
	result, err := BuildPayloadFromRollups(now, rangeHours, intervalMinutes, groups, buckets)
	if err != nil {
		t.Fatalf("聚合失败: %v", err)
	}
	if result.GeneratedAt != golden.Payload.GeneratedAt ||
		result.CoveredFrom != golden.Payload.CoveredFrom ||
		result.CoveredTo != golden.Payload.CoveredTo {
		t.Fatalf("窗口不一致：Go=(%s,%s,%s) Node=(%s,%s,%s)",
			result.GeneratedAt, result.CoveredFrom, result.CoveredTo,
			golden.Payload.GeneratedAt, golden.Payload.CoveredFrom, golden.Payload.CoveredTo)
	}
	// 语义比对（不是逐字节）：payload 的契约是 JSON **对象**，键序不进契约；
	// Node 侧的 golden 经 map 解码后键序已丢，逐字节比字符串只会假红。
	// 逐字节那一层由 TestWriteGoldenRebuildProjectionComparesNode 负责（那里比的是落盘正文）。
	gotGroups := decodeJSONAny(t, mustJSON(t, result.Groups))
	wantGroups := decodeJSONAny(t, mustJSON(t, golden.Payload.Groups))
	if !reflect.DeepEqual(gotGroups, wantGroups) {
		t.Fatalf("payload.groups 语义不一致：%s", firstDifference("groups", gotGroups, wantGroups))
	}
}

// TestWriteGoldenRebuildProjectionComparesNode 对照 worker 写出的**每个键与逐字节正文**。
func TestWriteGoldenRebuildProjectionComparesNode(t *testing.T) {
	golden := loadWriteGolden(t)
	now, err := ParseISOMilli(golden.NowISO)
	if err != nil {
		t.Fatalf("解析 now 失败: %v", err)
	}
	redis := newFakeProjectionRedis()
	// 夹具：只搬入 Node 写好的配置快照与 rollup 桶（与 golden 脚本同一批）。
	for key, value := range golden.ProjectionRedis.Keys {
		if strings.Contains(key, ":rollup:") && !strings.Contains(key, "coverage-start") {
			continue
		}
		if strings.Contains(key, ":config") {
			redis.seedRaw(key, value)
		}
	}
	for key, value := range golden.RollupRedis.Keys {
		redis.seedHash(key, value)
	}
	for key, ttl := range golden.RollupRedis.TTLs {
		redis.seedTTL(key, ttl)
	}

	result, err := RebuildProjection(context.Background(), RebuildOptions{
		Redis:           redis,
		IntervalMinutes: 15,
		RangeHours:      24,
		Prefix:          golden.Prefix,
		Now:             func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("重建失败: %v", err)
	}
	if string(result.Status) != golden.RebuildResult.Status {
		t.Fatalf("重建状态不一致：Go=%s Node=%s", result.Status, golden.RebuildResult.Status)
	}
	if result.SourceGeneration != golden.RebuildResult.SourceGeneration {
		t.Fatalf("代指纹不一致：Go=%s Node=%s", result.SourceGeneration, golden.RebuildResult.SourceGeneration)
	}

	gotAllKeys, gotTTLs := redis.dump()
	// golden 里也包含**输入侧夹具**（配置快照与 rollup 桶），比对时两侧都排除它们，
	// 只比**本代产出**：正式快照/序列键与两份 manifest（临时键写完即删，故不该出现）。
	isInputKey := func(key string) bool {
		// 输入侧夹具（配置快照与 rollup 桶）不算产出。
		if strings.Contains(key, ":config") || strings.Contains(key, ":rollup:") {
			return true
		}
		// 分布式重建锁是**瞬态键**（60s TTL，释放即删），它的 TTL 不进投影契约；
		// 且两侧假 Redis 对「Lua 释放」的处理不同（Node 的假 Redis 只删值不删 TTL 记录，
		// Go 的替身两者都删）——那是对拍夹具的差异，不是实现差异。
		return strings.Contains(key, ":rebuild-lock:")
	}
	gotKeys := map[string]string{}
	for key, value := range gotAllKeys {
		if isInputKey(key) {
			continue
		}
		gotKeys[key] = value
	}
	wantKeys := map[string]string{}
	wantTTLs := map[string]float64{}
	for key, value := range golden.ProjectionRedis.Keys {
		if isInputKey(key) {
			continue
		}
		wantKeys[key] = value
	}
	for key, ttl := range golden.ProjectionRedis.TTLs {
		if isInputKey(key) {
			continue
		}
		wantTTLs[key] = ttl
	}
	if diff := diffStringMaps(gotKeys, wantKeys); diff != "" {
		t.Fatalf("投影键值不一致：%s", diff)
	}
	for key, wantSeconds := range wantTTLs {
		if gotTTLs[key] != wantSeconds {
			t.Fatalf("投影 TTL 不一致 %s：Go=%v Node=%v", key, gotTTLs[key], wantSeconds)
		}
	}
}

// bucketsFromGolden 从 golden 的 Node 写入结果里还原桶（键 → 桶 → 字段值）。
func bucketsFromGolden(golden *writeGolden, prefix string) []RollupBucket {
	now, _ := ParseISOMilli(golden.NowISO)
	starts, _ := BuildRollupBucketStartsOnly(now, 24, 15)
	byStart := map[string]map[string]float64{}
	for key, value := range golden.RollupRedis.Keys {
		marker := prefix + ":rollup:5m:"
		if !strings.HasPrefix(key, marker) {
			continue
		}
		rest := strings.TrimPrefix(key, marker)
		// 桶键形如 `<encodedISO>`；哈希字段在 golden 里用 \u0000 拼接。
		parts := strings.SplitN(rest, "\u0000", 2)
		if len(parts) != 2 {
			continue
		}
		bucketStart := decodeKeyPart(parts[0])
		fields := byStart[bucketStart]
		if fields == nil {
			fields = map[string]float64{}
			byStart[bucketStart] = fields
		}
		fields[parts[1]] = parseFloatOrZero(value)
	}
	buckets := make([]RollupBucket, 0, len(starts))
	for _, start := range starts {
		values := byStart[start]
		if values == nil {
			values = map[string]float64{}
		}
		buckets = append(buckets, RollupBucket{BucketStart: start, Values: values})
	}
	return buckets
}

func parseFloatOrZero(raw string) float64 {
	var value float64
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return 0
	}
	return value
}

// firstDifference 递归找出两份 JSON 值的**首个差异路径**并描述它。
//
// 为什么不直接打印两份 JSON 的前 200 字：payload 很长，差异往往在后半段，
// 截断输出会把「哪一字段错了」藏起来——那种失败信息等于没有。
func firstDifference(path string, got, want any) string {
	switch wantValue := want.(type) {
	case map[string]any:
		gotValue, ok := got.(map[string]any)
		if !ok {
			return path + "：类型不一致（Go=" + typeName(got) + " Node=object）"
		}
		for _, key := range sortedAnyKeys(wantValue) {
			gotChild, exists := gotValue[key]
			if !exists {
				return path + "." + key + "：Go 缺失（Node=" + mustJSONNoT(wantValue[key]) + "）"
			}
			if diff := firstDifference(path+"."+key, gotChild, wantValue[key]); diff != "" {
				return diff
			}
		}
		for key := range gotValue {
			if _, exists := wantValue[key]; !exists {
				return path + "." + key + "：Go 多出（Node 无此键）"
			}
		}
		return ""
	case []any:
		gotValue, ok := got.([]any)
		if !ok {
			return path + "：类型不一致（Go=" + typeName(got) + " Node=array）"
		}
		if len(gotValue) != len(wantValue) {
			return path + "：长度不一致 Go=" + itoa(len(gotValue)) + " Node=" + itoa(len(wantValue))
		}
		for index := range wantValue {
			if diff := firstDifference(path+"["+itoa(index)+"]", gotValue[index], wantValue[index]); diff != "" {
				return diff
			}
		}
		return ""
	default:
		if !reflect.DeepEqual(got, want) {
			return path + "：Go=" + mustJSONNoT(got) + " Node=" + mustJSONNoT(want)
		}
		return ""
	}
}

func typeName(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	default:
		return "scalar"
	}
}

func sortedAnyKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func mustJSONNoT(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return "<序列化失败>"
	}
	return string(raw)
}

// decodeJSONAny 把 JSON 文本解成 any（对象为 map、数组为 slice），用于语义比对。
func decodeJSONAny(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("解码 JSON 失败: %v", err)
	}
	return value
}

// diffStringMaps 逐键比对，返回首个差异的可读描述（空串表示一致）。
func diffStringMaps(got map[string]string, want map[string]string) string {
	gotKeys := make([]string, 0, len(got))
	for key := range got {
		gotKeys = append(gotKeys, key)
	}
	wantKeys := make([]string, 0, len(want))
	for key := range want {
		wantKeys = append(wantKeys, key)
	}
	sort.Strings(gotKeys)
	sort.Strings(wantKeys)
	for _, key := range wantKeys {
		if _, ok := got[key]; !ok {
			return "缺少键 " + key
		}
	}
	for _, key := range gotKeys {
		if _, ok := want[key]; !ok {
			return "多出键 " + key
		}
	}
	for _, key := range wantKeys {
		if got[key] != want[key] {
			gotValue, wantValue := got[key], want[key]
			index := 0
			for index < len(gotValue) && index < len(wantValue) && gotValue[index] == wantValue[index] {
				index++
			}
			start := index - 60
			if start < 0 {
				start = 0
			}
			return "键 " + key + " 正文不一致（首个差异在第 " + itoa(index) + " 字节）:\n  Go  =" +
				sliceAround(gotValue, start) + "\n  Node=" + sliceAround(wantValue, start)
		}
	}
	return ""
}

func sliceAround(value string, start int) string {
	end := start + 200
	if end > len(value) {
		end = len(value)
	}
	return value[start:end]
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := []byte{}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}
