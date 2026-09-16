package pubstatus

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是 public-status **投影写侧的「事件捕获」半场**：把一次已终态的请求折算成
// rollup 桶的增量并写入 Redis。
//
// Node 侧的对应实现与调用点：
//   - `src/lib/public-status/rollup-store.ts` 的 `buildPublicStatusRollupIncrements`（:289-388）
//     与 `writePublicStatusRollupEvent`（:389-470）；
//   - 调用点在**请求落库路径**：`src/repository/message.ts:246` 的 `queuePublicStatusRollupWrite`。
//
// 为什么这一半是「唯一前置」：投影 worker 的 payload **只从 rollup 桶构建**
// （`rebuild-worker.ts:161` 的 `buildPublicStatusPayloadFromRollups`），并不直查
// `message_request`。也就是说，若 Go 侧只移植 worker 而不写桶，公开状态页在 Node 下线后
// 会永远停在「无数据」。`aggregation.ts` 里那条直查 SQL（`queryPublicStatusRequests`）
// 在 Node 生产里**没有任何调用者**（已 grep 全仓确认），故不是本次要移植的口径。

const (
	// RollupFieldSeparator 是 rollup 哈希字段的分隔符（rollup-store.ts:26）。
	RollupFieldSeparator = "|"
	// RollupTTLSeconds 是 rollup 桶与覆盖起点的存活时长（rollup-store.ts:27，32 天）。
	RollupTTLSeconds = 60 * 60 * 24 * 32
)

// RollupMetric 是 rollup 哈希里的六个指标（rollup-store.ts:29-36）。
type RollupMetric string

const (
	RollupMetricSuccess   RollupMetric = "success"
	RollupMetricFailure   RollupMetric = "failure"
	RollupMetricTTFbSum   RollupMetric = "ttfb_sum"
	RollupMetricTTFbCount RollupMetric = "ttfb_count"
	RollupMetricTPSSum    RollupMetric = "tps_sum"
	RollupMetricTPSCount  RollupMetric = "tps_count"
)

func isRollupMetric(value string) bool {
	switch RollupMetric(value) {
	case RollupMetricSuccess, RollupMetricFailure,
		RollupMetricTTFbSum, RollupMetricTTFbCount,
		RollupMetricTPSSum, RollupMetricTPSCount:
		return true
	default:
		return false
	}
}

// BuildRollupKey 复刻 buildPublicStatusRollupKey（redis-contract.ts:139-155）。
//
// 形如 `<prefix>:rollup:<bucketMinutes>m:<对齐后的桶起点>`。桶起点按 UTC 对齐到
// bucketMinutes 边界——与 Node 的 `alignBucketStartUtc` 同义（本包已有一份
// `AlignBucketStartUTC`，读路径复用同一实现，避免两份对齐口径）。
func BuildRollupKey(bucketStartISO string, bucketMinutes int, prefix string) (string, error) {
	if bucketMinutes == 0 {
		bucketMinutes = PublicStatusRollupBucketMinutes
	}
	if err := assertPositiveInt(bucketMinutes, "bucketMinutes"); err != nil {
		return "", err
	}
	aligned, err := AlignBucketStartUTC(bucketStartISO, bucketMinutes)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		prefixOr(prefix),
		"rollup",
		fmt.Sprintf("%dm", bucketMinutes),
		encodeKeyPart(aligned),
	}, ":"), nil
}

// BuildRollupCoverageStartKey 复刻 buildPublicStatusRollupCoverageStartKey（redis-contract.ts:157-165）。
//
// 这个键记录「桶从哪一刻开始有覆盖」，worker 用它判断本代的覆盖是否完整
// （`isRollupCoverageComplete`：覆盖起点早于窗口起点才算完整）。
func BuildRollupCoverageStartKey(bucketMinutes int, prefix string) (string, error) {
	if bucketMinutes == 0 {
		bucketMinutes = PublicStatusRollupBucketMinutes
	}
	if err := assertPositiveInt(bucketMinutes, "bucketMinutes"); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:rollup:coverage-start:%dm", prefixOr(prefix), bucketMinutes), nil
}

// BuildSeriesChunkKey 复刻 buildPublicStatusSeriesChunkKey（redis-contract.ts:119-137）。
func BuildSeriesChunkKey(
	intervalMinutes int,
	generation string,
	bucketStartISO string,
	bucketEndISO string,
	prefix string,
) (string, error) {
	if err := assertPositiveInt(intervalMinutes, "intervalMinutes"); err != nil {
		return "", err
	}
	start, err := AlignBucketStartUTC(bucketStartISO, intervalMinutes)
	if err != nil {
		return "", err
	}
	end, err := AlignBucketStartUTC(bucketEndISO, intervalMinutes)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{
		prefixOr(prefix),
		"series",
		encodeKeyPart(generation),
		fmt.Sprintf("%dm", intervalMinutes),
		encodeKeyPart(start),
		encodeKeyPart(end),
	}, ":"), nil
}

// BuildRebuildLockKey 复刻 buildPublicStatusRebuildLockKey（redis-contract.ts:167-175）。
func BuildRebuildLockKey(flightKey string, prefix string) string {
	return fmt.Sprintf("%s:rebuild-lock:%s", prefixOr(prefix), encodeKeyPart(flightKey))
}

// BuildTempKey 复刻 buildPublicStatusTempKey（redis-contract.ts:179-181）。
//
// 临时键的意义：先写临时键、再写正式键，是「读者永远看不到半成品投影」的写法
// （正式键要么是上一代完整内容，要么是这一代完整内容）。
func BuildTempKey(baseKey string, nonce string) string {
	return fmt.Sprintf("%s:tmp:%s", baseKey, encodeKeyPart(nonce))
}

// BuildRollupField 复刻 buildPublicStatusRollupField（rollup-store.ts:137-147）。
func BuildRollupField(groupID string, modelKey string, metric RollupMetric) string {
	return strings.Join([]string{
		encodeKeyPart(groupID),
		encodeKeyPart(modelKey),
		encodeKeyPart(string(metric)),
	}, RollupFieldSeparator)
}

// ParseRollupField 复刻 parsePublicStatusRollupField（rollup-store.ts:149-174）。
func ParseRollupField(field string) (string, string, RollupMetric, bool) {
	parts := strings.Split(field, RollupFieldSeparator)
	if len(parts) != 3 {
		return "", "", "", false
	}
	metric := decodeKeyPart(parts[2])
	if !isRollupMetric(metric) {
		return "", "", "", false
	}
	return decodeKeyPart(parts[0]), decodeKeyPart(parts[1]), RollupMetric(metric), true
}

// decodeKeyPart 是 encodeKeyPart 的逆运算；解不开时原样返回（rollup-store.ts:115-121）。
func decodeKeyPart(value string) string {
	decoded, err := url.PathUnescape(value)
	if err != nil {
		return value
	}
	return decoded
}

// ProviderChainItem 是 rollup 事件里用到的链项子集，字段名对齐 Node 的 `ProviderChainItem`
// （`@/types/message`）。只声明分类需要读到的字段：多声明会把「Node 读了什么」这件事
// 变得不可核对。
type ProviderChainItem struct {
	StatusCode   *int
	Reason       *string
	ErrorMessage *string
	GroupTag     *string
	ErrorDetails *ProviderChainErrorDetails
}

// ProviderChainErrorDetails 是链项 errorDetails 里分类要读的一处（matchedRule）。
type ProviderChainErrorDetails struct {
	MatchedRule any
}

// RollupEvent 是一次终态请求折算 rollup 所需的字段（rollup-store.ts:37-46）。
type RollupEvent struct {
	CreatedAt     time.Time
	Model         *string
	OriginalModel *string
	DurationMs    *float64
	TTFTMs        *float64
	FirstByteMs   *float64
	OutputTokens  *int64
	ProviderChain []ProviderChainItem
}

// RollupIncrement 是一条待累加的增量（rollup-store.ts:48-53）。
type RollupIncrement struct {
	GroupID  string
	ModelKey string
	Metric   RollupMetric
	Value    float64
}

// ---------- 结果分类（request-outcome.ts 的移植）----------

// RequestOutcome 是成功率的三种结局（request-outcome.ts:3）。
type RequestOutcome string

const (
	OutcomeSuccess  RequestOutcome = "success"
	OutcomeFailure  RequestOutcome = "failure"
	OutcomeExcluded RequestOutcome = "excluded"
)

// ExclusionFamily 是被排除项的原因族（request-outcome.ts:5-16）。
type ExclusionFamily string

const (
	ExclusionWarmup            ExclusionFamily = "warmup"
	ExclusionSensitiveWord     ExclusionFamily = "sensitive_word"
	ExclusionBlockedRequest    ExclusionFamily = "blocked_request"
	ExclusionMatchedRule       ExclusionFamily = "matched_rule"
	ExclusionResourceNotFound  ExclusionFamily = "resource_not_found"
	ExclusionClientAbort       ExclusionFamily = "client_abort"
	ExclusionLocalCapacity     ExclusionFamily = "local_capacity"
	ExclusionLocalNonRetryable ExclusionFamily = "local_non_retryable"
	// ExclusionProviderUnsupportedInput 是「当前供应商不支持该输入形态」的族名（Go 侧新增，
	// 对应 forward.CategoryProviderUnsupportedInput 与链上 reason `unsupported`）。
	//
	// 为何必须排除：这一档不计供应商熔断器（拒一种它不支持的输入形态不是健康度问题），
	// 若落入 failure 兜底，就会把「同一份输入换一家可能就成」的请求计进供应商可用率。
	ExclusionProviderUnsupportedInput ExclusionFamily = "provider_unsupported_input"
	ExclusionHedgeLoser               ExclusionFamily = "hedge_loser"
	ExclusionQuotaOrRateLimit         ExclusionFamily = "quota_or_rate_limit"
	ExclusionNoAvailableProv          ExclusionFamily = "no_available_provider"
)

// RequestOutcomeTaxonomy 是分类结果（request-outcome.ts:18-24）。
type RequestOutcomeTaxonomy struct {
	Outcome         RequestOutcome
	Locus           string // "upstream" | "non_upstream"
	Countability    string // "countable" | "excluded"
	Result          string // "success" | "failure" | "n/a"
	ExclusionFamily ExclusionFamily
}

var (
	successTaxonomy = RequestOutcomeTaxonomy{
		Outcome: OutcomeSuccess, Locus: "upstream", Countability: "countable", Result: "success",
	}
	failureTaxonomy = RequestOutcomeTaxonomy{
		Outcome: OutcomeFailure, Locus: "upstream", Countability: "countable", Result: "failure",
	}
)

func excludedTaxonomy(family ExclusionFamily) RequestOutcomeTaxonomy {
	return RequestOutcomeTaxonomy{
		Outcome: OutcomeExcluded, Locus: "non_upstream", Countability: "excluded", Result: "n/a",
		ExclusionFamily: family,
	}
}

// neutralReasons 复刻 NEUTRAL_REASONS（request-outcome.ts:26-33）：这些 reason 本身
// 既不算成功也不算失败（它们是选路过程的痕迹，不是结果）。
var neutralReasons = map[string]struct{}{
	"session_reuse":               {},
	"initial_selection":           {},
	"hedge_triggered":             {},
	"hedge_launched":              {},
	"client_restriction_filtered": {},
	"http2_fallback":              {},
}

// successReasons 复刻 SUCCESS_REASONS（request-outcome.ts:44-48）。
var successReasons = map[string]struct{}{
	"request_success": {},
	"retry_success":   {},
	"hedge_winner":    {},
}

// excludedReasons 复刻 EXCLUDED_REASONS（request-outcome.ts:50-60）。
var excludedReasons = map[string]ExclusionFamily{
	"resource_not_found":         ExclusionResourceNotFound,
	"concurrent_limit_failed":    ExclusionLocalCapacity,
	"hedge_loser_cancelled":      ExclusionHedgeLoser,
	"hedge_loser_billed":         ExclusionHedgeLoser,
	"client_error_non_retryable": ExclusionLocalNonRetryable,
	"client_abort":               ExclusionClientAbort,
	// Go 侧新增档（Node 的 EXCLUDED_REASONS 里没有这个词）：上游声明不支持该输入形态。
	"unsupported": ExclusionProviderUnsupportedInput,
}

// quotaOrRateLimitPatterns 复刻 QUOTA_OR_RATE_LIMIT_PATTERNS（request-outcome.ts:62-70）。
var quotaOrRateLimitPatterns = []string{
	"insufficient quota",
	"quota exceeded",
	"rate limit",
	"rate_limit",
	"concurrency limit",
	"concurrent limit",
	"limit exceeded",
}

// RequestOutcomeSignal 是分类的输入（request-outcome.ts:35-42）。
type RequestOutcomeSignal struct {
	BlockedBy    *string
	StatusCode   *int
	Reason       *string
	ErrorMessage *string
	MatchedRule  any
}

// IsSuccessStatusCode 复刻 isSuccessStatusCode（request-outcome.ts:257-259）。
func IsSuccessStatusCode(statusCode *int) bool {
	return statusCode != nil && *statusCode >= 200 && *statusCode < 400
}

// ClassifyRequestOutcomeSignal 复刻 classifyRequestOutcomeSignal（request-outcome.ts:120-186）。
//
// 判定顺序即语义：先看本地拦截（blockedBy / matchedRule），再看客户端主动放弃与 404，
// 再查 reason 表，最后才按错误文案与状态码兜底。顺序错一个分支就会把「被排除」算成
// 「失败」，从而污染公开页的可用率——这类差异对拍比形状时看不见，故按行照抄。
func ClassifyRequestOutcomeSignal(signal RequestOutcomeSignal) (RequestOutcomeTaxonomy, bool) {
	blockedBy := normalizeBlockedBy(signal.BlockedBy)
	switch blockedBy {
	case "warmup":
		return excludedTaxonomy(ExclusionWarmup), true
	case "sensitive_word":
		return excludedTaxonomy(ExclusionSensitiveWord), true
	case "":
	default:
		return excludedTaxonomy(ExclusionBlockedRequest), true
	}

	if signal.MatchedRule != nil {
		return excludedTaxonomy(ExclusionMatchedRule), true
	}

	reason := ""
	if signal.Reason != nil {
		reason = *signal.Reason
	}
	if signal.StatusCode != nil && *signal.StatusCode == 499 && reason != "client_abort_no_first_byte" {
		return excludedTaxonomy(ExclusionClientAbort), true
	}
	if reason == "client_abort" {
		return excludedTaxonomy(ExclusionClientAbort), true
	}
	if (signal.StatusCode != nil && *signal.StatusCode == 404) || reason == "resource_not_found" {
		return excludedTaxonomy(ExclusionResourceNotFound), true
	}
	if family, ok := excludedReasons[reason]; ok {
		return excludedTaxonomy(family), true
	}

	normalizedError := ""
	if signal.ErrorMessage != nil {
		normalizedError = strings.ToLower(*signal.ErrorMessage)
	}
	if strings.Contains(normalizedError, "no available provider") {
		return excludedTaxonomy(ExclusionNoAvailableProv), true
	}
	if matchesQuotaOrRateLimit(signal.ErrorMessage) {
		return excludedTaxonomy(ExclusionQuotaOrRateLimit), true
	}
	if reason == "response_incomplete" {
		return failureTaxonomy, true
	}

	if _, ok := successReasons[reason]; ok {
		return successTaxonomy, true
	}
	if IsSuccessStatusCode(signal.StatusCode) {
		return successTaxonomy, true
	}

	_, neutral := neutralReasons[reason]
	if neutral && signal.StatusCode == nil && (signal.ErrorMessage == nil || *signal.ErrorMessage == "") {
		return RequestOutcomeTaxonomy{}, false
	}

	if signal.StatusCode != nil || reason != "" || normalizedError != "" {
		return failureTaxonomy, true
	}
	return RequestOutcomeTaxonomy{}, false
}

// ClassifyProviderChainItemOutcome 复刻 classifyProviderChainItemOutcome
// （request-outcome.ts:188-197）：把链项投影成信号再分类。
func ClassifyProviderChainItemOutcome(item ProviderChainItem) (RequestOutcomeTaxonomy, bool) {
	var matchedRule any
	if item.ErrorDetails != nil {
		matchedRule = item.ErrorDetails.MatchedRule
	}
	return ClassifyRequestOutcomeSignal(RequestOutcomeSignal{
		StatusCode:   item.StatusCode,
		Reason:       item.Reason,
		ErrorMessage: item.ErrorMessage,
		MatchedRule:  matchedRule,
	})
}

// IsExcludedFromPublicStatusFailure 复刻 isExcludedFromPublicStatusFailure（aggregation.ts:98-107）。
func IsExcludedFromPublicStatusFailure(item ProviderChainItem) bool {
	taxonomy, ok := ClassifyProviderChainItemOutcome(item)
	return ok && taxonomy.Outcome == OutcomeExcluded
}

func normalizeBlockedBy(blockedBy *string) string {
	if blockedBy == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(*blockedBy))
}

func matchesQuotaOrRateLimit(message *string) bool {
	if message == nil {
		return false
	}
	normalized := strings.ToLower(*message)
	for _, pattern := range quotaOrRateLimitPatterns {
		if strings.Contains(normalized, pattern) {
			return true
		}
	}
	return false
}

// ---------- 分组与增量 ----------

// ConfiguredGroup 是「已配置的公开分组」（aggregation-core.ts:1-19）。
type ConfiguredGroup struct {
	SourceGroupID   *int64
	SourceGroupName string
	PublicGroupSlug string
	DisplayName     string
	ExplanatoryCopy *string
	SortOrder       float64
	Models          []ConfiguredModel
}

// ConfiguredModel 是分组里的模型（aggregation-core.ts:8-13）。
type ConfiguredModel struct {
	PublicModelKey   string
	Label            string
	VendorIconKey    string
	RequestTypeBadge string
}

// ConfiguredGroupID 复刻 getPublicStatusGroupId（rollup-store.ts:180-186）：
// 有源分组 id 用 id，否则用源分组名——它是 rollup 字段里的分组键。
func ConfiguredGroupID(group ConfiguredGroup) string {
	if group.SourceGroupID != nil {
		return fmt.Sprintf("%d", *group.SourceGroupID)
	}
	return group.SourceGroupName
}

// ConfiguredGroupsFromSnapshot 复刻 getConfiguredPublicStatusGroups（aggregation.ts:65-97）：
// 过滤掉无名/无模型的分组，映射成公开形状，并按 (sortOrder, displayName) 排序。
//
// 排序必须与 Node 逐字一致：payload 的分组顺序会原样进快照，公开页据此渲染。
func ConfiguredGroupsFromSnapshot(snapshot *InternalPublicStatusConfigSnapshot) []ConfiguredGroup {
	if snapshot == nil {
		return nil
	}
	groups := make([]ConfiguredGroup, 0, len(snapshot.Groups))
	for index := range snapshot.Groups {
		raw := snapshot.Groups[index]
		name := strings.TrimSpace(raw.SourceGroupName)
		if name == "" || len(raw.Models) == 0 {
			continue
		}
		models := make([]ConfiguredModel, 0, len(raw.Models))
		for _, model := range raw.Models {
			models = append(models, ConfiguredModel{
				PublicModelKey:   model.PublicModelKey,
				Label:            model.Label,
				VendorIconKey:    model.VendorIconKey,
				RequestTypeBadge: model.RequestTypeBadge,
			})
		}
		groups = append(groups, ConfiguredGroup{
			SourceGroupID:   raw.SourceGroupID,
			SourceGroupName: name,
			PublicGroupSlug: raw.Slug,
			DisplayName:     raw.DisplayName,
			ExplanatoryCopy: raw.Description,
			SortOrder:       raw.SortOrder,
			Models:          models,
		})
	}
	sort.SliceStable(groups, func(left, right int) bool {
		if groups[left].SortOrder != groups[right].SortOrder {
			return groups[left].SortOrder < groups[right].SortOrder
		}
		return groups[left].DisplayName < groups[right].DisplayName
	})
	return groups
}

// ResolveSuccessRateModelKey 复刻 resolveSuccessRateModelKey（request-outcome.ts:112-118）。
func ResolveSuccessRateModelKey(originalModel *string, model *string) string {
	if originalModel != nil {
		if trimmed := strings.TrimSpace(*originalModel); trimmed != "" {
			return trimmed
		}
	}
	if model != nil {
		if trimmed := strings.TrimSpace(*model); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ResolveProviderGroupsWithDefault 复刻 resolveProviderGroupsWithDefault（provider-group.ts:54-63）：
// 空值算 default 组。
//
// 本包另有一份同语义实现（`internal/route/rules.go:32` 的未导出版本）。两份都不可导出互用，
// 故此处照抄一份并注明出处；合并属跨包整理，见报告。
func ResolveProviderGroupsWithDefault(value *string) []string {
	parsed := parseProviderGroups(value)
	if len(parsed) == 0 {
		return []string{"default"}
	}
	return parsed
}

// parseProviderGroups 复刻 parseProviderGroups（provider-group.ts:23-25）：按中英文逗号与
// 换行切分、trim、丢空、去重保序。
func parseProviderGroups(value *string) []string {
	if value == nil {
		return nil
	}
	fields := strings.FieldsFunc(*value, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == '\r'
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

// ComputeTokensPerSecond 复刻 computeTokensPerSecond（aggregation-core.ts:22-46）。
//
// 分母是「生成窗口」＝ 总时长 − 首字节耗时；firstByteMs 缺失即返回 nil（历史行没有它，
// 用 TTFT 当分母会系统性高估 TPS，故宁可不给数）。
func ComputeTokensPerSecond(outputTokens *int64, durationMs *float64, firstByteMs *float64) *float64 {
	if outputTokens == nil || *outputTokens <= 0 {
		return nil
	}
	if durationMs == nil || *durationMs <= 0 {
		return nil
	}
	if firstByteMs == nil {
		return nil
	}
	generationMs := *durationMs - *firstByteMs
	if generationMs <= 0 {
		return nil
	}
	value := roundTo4(float64(*outputTokens) / (generationMs / 1000))
	return &value
}

// BuildRollupIncrements 复刻 buildPublicStatusRollupIncrements（rollup-store.ts:289-388）。
//
// 语义要点（照 Node 逐条）：
//   - 模型键取 originalModel 优先（跨供应商别名归一），空则整条事件丢弃；
//   - 只有「该模型属于某个已配置公开分组」才产生增量；
//   - 逐个链项分类：**成功一旦出现即锁定**（同一分组内先到者胜），失败只在尚无成功时记录，
//     excluded 不产生增量；
//   - ttfb_sum/ttfb_count 存的是 **TTFT**（键名是历史包袱，改名会作废已积累的桶）；
//   - ttfb/tps 只在**成功**那个分组上累加。
func BuildRollupIncrements(event RollupEvent, groups []ConfiguredGroup) []RollupIncrement {
	modelKey := ResolveSuccessRateModelKey(event.OriginalModel, event.Model)
	if modelKey == "" {
		return nil
	}

	modelToGroups := make(map[string][]int)
	groupsBySourceName := make(map[string]int)
	groupsByRollupID := make(map[string]int)
	for index, group := range groups {
		rollupID := ConfiguredGroupID(group)
		groupsBySourceName[group.SourceGroupName] = index
		if _, exists := groupsByRollupID[rollupID]; !exists {
			groupsByRollupID[rollupID] = index
		}
		for _, model := range group.Models {
			modelToGroups[model.PublicModelKey] = append(modelToGroups[model.PublicModelKey], index)
		}
	}

	configured := modelToGroups[modelKey]
	if len(configured) == 0 {
		return nil
	}

	// groupOutcome 记录每个「源分组名」的结局；顺序由链项顺序决定（先到者胜的语义写在下面）。
	groupOutcome := make(map[string]RequestOutcome)
	groupOrder := make([]string, 0, len(event.ProviderChain))
	for _, item := range event.ProviderChain {
		taxonomy, ok := ClassifyProviderChainItemOutcome(item)
		if !ok {
			continue
		}
		itemGroups := dedupeStrings(ResolveProviderGroupsWithDefault(item.GroupTag))
		for _, sourceGroupName := range itemGroups {
			if _, known := groupsBySourceName[sourceGroupName]; !known {
				continue
			}
			existing, seen := groupOutcome[sourceGroupName]
			if seen && existing == OutcomeSuccess {
				continue
			}
			if taxonomy.Outcome == OutcomeSuccess {
				if !seen {
					groupOrder = append(groupOrder, sourceGroupName)
				}
				groupOutcome[sourceGroupName] = OutcomeSuccess
				continue
			}
			if !seen {
				groupOrder = append(groupOrder, sourceGroupName)
				groupOutcome[sourceGroupName] = taxonomy.Outcome
				continue
			}
			// 已有非成功结局：excluded 可被失败覆盖，失败不被覆盖（Node 的 `!existing || existing === "excluded"`）。
			if existing == OutcomeExcluded {
				groupOutcome[sourceGroupName] = taxonomy.Outcome
			}
		}
	}

	ttftMs := event.TTFTMs
	tps := ComputeTokensPerSecond(event.OutputTokens, event.DurationMs, event.FirstByteMs)

	increments := make([]RollupIncrement, 0, len(groupOutcome)*3)
	for _, sourceGroupName := range groupOrder {
		outcome := groupOutcome[sourceGroupName]
		if outcome == OutcomeExcluded {
			continue
		}
		groupIndex, known := groupsBySourceName[sourceGroupName]
		if !known {
			continue
		}
		group := groups[groupIndex]
		hasModel := false
		for _, model := range group.Models {
			if model.PublicModelKey == modelKey {
				hasModel = true
				break
			}
		}
		if !hasModel {
			continue
		}
		groupID := ConfiguredGroupID(group)
		if index, ok := groupsByRollupID[groupID]; !ok || index != groupIndex {
			continue
		}
		metric := RollupMetricFailure
		if outcome == OutcomeSuccess {
			metric = RollupMetricSuccess
		}
		increments = append(increments, RollupIncrement{
			GroupID: groupID, ModelKey: modelKey, Metric: metric, Value: 1,
		})
		if outcome == OutcomeSuccess && ttftMs != nil {
			increments = append(increments,
				RollupIncrement{GroupID: groupID, ModelKey: modelKey, Metric: RollupMetricTTFbSum, Value: *ttftMs},
				RollupIncrement{GroupID: groupID, ModelKey: modelKey, Metric: RollupMetricTTFbCount, Value: 1},
			)
		}
		if outcome == OutcomeSuccess && tps != nil {
			increments = append(increments,
				RollupIncrement{GroupID: groupID, ModelKey: modelKey, Metric: RollupMetricTPSSum, Value: *tps},
				RollupIncrement{GroupID: groupID, ModelKey: modelKey, Metric: RollupMetricTPSCount, Value: 1},
			)
		}
	}
	return increments
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// ------------- 写入 -------------

// RollupWriteReason 是未写入的原因（rollup-store.ts:67-79）。
type RollupWriteReason string

const (
	RollupIgnored      RollupWriteReason = "ignored"
	RollupRedisUnavail RollupWriteReason = "redis-unavailable"
	RollupWriteFailed  RollupWriteReason = "write-failed"
)

// RollupWriteResult 是写入结果（rollup-store.ts:67-79）。
type RollupWriteResult struct {
	Written        bool
	Retryable      bool
	Reason         RollupWriteReason
	IncrementCount int
	Key            string
}

// RollupWriter 是写路径需要的 Redis 子集。
//
// 与读路径用窄接口同理：桶键布局、字段编码、coverage-start 的 NX 语义都只有真 Redis 才能
// 全量覆盖，窄接口让每条分支都能确定性测试。
type RollupWriter interface {
	// HIncrByFloat 对哈希字段做浮点累加。
	HIncrByFloat(ctx context.Context, key, field string, increment float64) error
	// SetNX 仅当键不存在时写入（coverage-start 用）。
	SetNX(ctx context.Context, key, value string) (bool, error)
	// Expire 设置存活时间。
	Expire(ctx context.Context, key string, ttlSeconds int) error
}

// NewRedisRollupWriter 把 go-redis 客户端包成 RollupWriter；client 为 nil 时返回 nil
// （装配方据此让事件捕获降级为「不写」而不是崩）。
func NewRedisRollupWriter(client redis.UniversalClient) RollupWriter {
	if client == nil {
		return nil
	}
	return &redisRollupWriter{client: client}
}

type redisRollupWriter struct {
	client redis.UniversalClient
}

func (w *redisRollupWriter) HIncrByFloat(ctx context.Context, key, field string, increment float64) error {
	return w.client.HIncrByFloat(ctx, key, field, increment).Err()
}

func (w *redisRollupWriter) SetNX(ctx context.Context, key, value string) (bool, error) {
	return w.client.SetNX(ctx, key, value, 0).Result()
}

func (w *redisRollupWriter) Expire(ctx context.Context, key string, ttlSeconds int) error {
	return w.client.Expire(ctx, key, time.Duration(ttlSeconds)*time.Second).Err()
}

// WriteRollupEvent 复刻 writePublicStatusRollupEvent（rollup-store.ts:389-470）。
//
// 写入语义（照 Node）：
//   - 无增量 → `ignored`，且**不算可重试**（重试也没意义）；
//   - 桶键用事件时刻对齐到 5 分钟边界；
//   - coverage-start 用 `SET NX` 写**首次**覆盖的桶起点，并每次续 TTL；
//   - 累加与 TTL 在同一次往返里完成（Node 用 pipeline；这里逐条发送也保持同一语义，
//     因为只影响吞吐不影响结果——事件捕获在请求路径上，失败即降级，不阻塞请求）。
func WriteRollupEvent(
	ctx context.Context,
	writer RollupWriter,
	event RollupEvent,
	groups []ConfiguredGroup,
	prefix string,
) (RollupWriteResult, error) {
	increments := BuildRollupIncrements(event, groups)
	key, err := BuildRollupKey(event.CreatedAt.UTC().Format(isoMilliLayout), PublicStatusRollupBucketMinutes, prefix)
	if err != nil {
		return RollupWriteResult{Reason: RollupWriteFailed}, err
	}
	if len(increments) == 0 {
		return RollupWriteResult{Written: false, Retryable: false, Reason: RollupIgnored, Key: key}, nil
	}
	coverageKey, err := BuildRollupCoverageStartKey(PublicStatusRollupBucketMinutes, prefix)
	if err != nil {
		return RollupWriteResult{Reason: RollupWriteFailed, IncrementCount: len(increments), Key: key}, err
	}
	bucketStart, err := AlignBucketStartUTC(event.CreatedAt.UTC().Format(isoMilliLayout), PublicStatusRollupBucketMinutes)
	if err != nil {
		return RollupWriteResult{Reason: RollupWriteFailed, IncrementCount: len(increments), Key: key}, err
	}

	if writer == nil {
		return RollupWriteResult{
			Written: false, Retryable: true, Reason: RollupRedisUnavail,
			IncrementCount: len(increments), Key: key,
		}, nil
	}

	for _, increment := range increments {
		if err := writer.HIncrByFloat(ctx, key, BuildRollupField(increment.GroupID, increment.ModelKey, increment.Metric), increment.Value); err != nil {
			return RollupWriteResult{
				Written: false, Retryable: true, Reason: RollupWriteFailed,
				IncrementCount: len(increments), Key: key,
			}, fmt.Errorf("public status rollup hincrbyfloat failed for %s: %w", key, err)
		}
	}
	if _, err := writer.SetNX(ctx, coverageKey, bucketStart); err != nil {
		return RollupWriteResult{
			Written: false, Retryable: true, Reason: RollupWriteFailed,
			IncrementCount: len(increments), Key: key,
		}, fmt.Errorf("public status rollup coverage-start failed for %s: %w", coverageKey, err)
	}
	if err := writer.Expire(ctx, key, RollupTTLSeconds); err != nil {
		return RollupWriteResult{
			Written: false, Retryable: true, Reason: RollupWriteFailed,
			IncrementCount: len(increments), Key: key,
		}, fmt.Errorf("public status rollup expire failed for %s: %w", key, err)
	}
	if err := writer.Expire(ctx, coverageKey, RollupTTLSeconds); err != nil {
		return RollupWriteResult{
			Written: false, Retryable: true, Reason: RollupWriteFailed,
			IncrementCount: len(increments), Key: key,
		}, fmt.Errorf("public status rollup expire failed for %s: %w", coverageKey, err)
	}

	return RollupWriteResult{Written: true, IncrementCount: len(increments), Key: key}, nil
}

// toFixed4 复刻 Node 的 `Number(x.toFixed(4))`（aggregation-core.ts:44、rollup-store.ts:601 等处的
// 统一口径）。
//
// 实现选择：用 strconv 的定点格式再解析回 float64——它同样在**二进制实际值**上做正确舍入。
// 与 JS 的差别只剩「十进制恰好落在半格」时的取整方向（JS 取更大的 n，Go 取偶）。
// 在公开页的可用率/TPS 量级上该情形不出现（半数不在二进制里可精确表示）；
// 若对拍报出末位 1 的差异，这里是第一嫌疑点。
func toFixed4(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	parsed, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', 4, 64), 64)
	if err != nil {
		return value
	}
	return parsed
}

// roundTo4 是 toFixed4 的别名，供调用点表达意图（本包统一用它做 4 位舍入）。
func roundTo4(value float64) float64 { return toFixed4(value) }
