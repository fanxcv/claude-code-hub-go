package adminapi

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 **`GET /api/availability`（按时间桶聚合）** 的 Go 落点。
//
// 唯一真源：src/app/api/availability/route.ts（参数校验）+ src/lib/availability/availability-service.ts
// 的 queryProviderAvailability（聚合口径）。它与同目录那三条根级可用性端点**不是一回事**：
// 那三条是自足查询，这一条要把 1 分钟投影桶 date_bin 成展示桶、按 provider 截断 maxBuckets、
// 再从「返回的桶」里推导当前状态（注意：截断后的子窗口，不是整个查询窗）。
//
// 作答形状照 Node：裸 JSON（NoManagementEnvelope），字段顺序同 availability-service 的对象字面量。
// 一处沿用既有登记差异：非管理员由守卫作答（problem 信封），Node 回 `{"error":"Unauthorized"}` + 401。

const (
	// availabilityMinBucketSizeMinutes / Max 照 availability-service.ts 的两个导出常量。
	availabilityMinBucketSizeMinutes = 0.25
	availabilityMaxBucketSizeMinutes = 1440
	// availabilityDefaultMaxBuckets 是 DEFAULT_MAX_BUCKETS；硬上限另有一份同值常量（Node 刻意分开）：
	// 将来调默认值时不该连带把守卫也放松。
	availabilityDefaultMaxBuckets = 100
	// availabilityMaxBucketsHardLimit 照 MAX_BUCKETS_HARD_LIMIT。
	availabilityMaxBucketsHardLimit = 100
	// availabilityBucketsTargetCount 是 determineOptimalBucketSize 的目标桶数。
	availabilityBucketsTargetCount = 50
	// availabilityDefaultWindowHours 是 Node 的默认窗口（最近 24 小时）。
	availabilityDefaultWindowHours = 24
)

// availabilityMaxRangeMS 照 MAX_AVAILABILITY_QUERY_RANGE_MS = 100 * 1440 * 60 * 1000（= 100 天）。
var availabilityMaxRangeMS = float64(availabilityMaxBucketsHardLimit) *
	availabilityMaxBucketSizeMinutes * 60 * 1000

// availabilityStandardBucketSizes 是 determineOptimalBucketSize 的标准桶宽档位。
var availabilityStandardBucketSizes = []float64{1, 5, 15, 60, 1440}

// registerBucketedAvailability 把 `/api/availability` 挂进可用性那组（Store 未装配时整组不注册）。
func registerBucketedAvailability(router *Router, api *availabilityAPI) {
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/availability",
		Access:               AccessAdmin,
		Module:               "availability",
		OperationID:          "queryProviderAvailability",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleBucketedAvailability),
	})
}

// availabilityQueryOptions 是 queryProviderAvailability 的入参（Node 的 AvailabilityQueryOptions）。
type availabilityQueryOptions struct {
	startTime         *time.Time
	endTime           *time.Time
	providerIDs       []int64
	bucketSizeMinutes *float64
	includeDisabled   bool
	maxBuckets        *int
}

// handleBucketedAvailability 复刻 GET /api/availability。
//
// 参数校验（route.ts）与聚合校验（availability-service.ts）分两段：前者是「格式不合法」，
// 后者是「范围/桶预算不合法」，两段的正文都是 `{"error": "<原话>"}` + 400。
func (api *availabilityAPI) handleBucketedAvailability(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()

	options := availabilityQueryOptions{}
	// 注意 startTime/endTime 的「给了但为空」与「没给」不同：前者 400，后者走默认窗口。
	for _, field := range []struct {
		name  string
		value **time.Time
	}{{"startTime", &options.startTime}, {"endTime", &options.endTime}} {
		if !query.Has(field.name) {
			continue
		}
		raw := query.Get(field.name)
		if strings.TrimSpace(raw) == "" {
			availabilityError(writer, http.StatusBadRequest,
				"Invalid "+field.name+": expected a valid Date or ISO timestamp")
			return
		}
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
		if err != nil {
			availabilityError(writer, http.StatusBadRequest,
				"Invalid "+field.name+": expected a valid Date or ISO timestamp")
			return
		}
		*field.value = &parsed
	}

	if query.Has("providerIds") {
		providerIDs, message := availabilityParseProviderIDs(query.Get("providerIds"))
		if message != "" {
			availabilityError(writer, http.StatusBadRequest, message)
			return
		}
		options.providerIDs = providerIDs
	}

	if query.Has("bucketSizeMinutes") {
		size, message := availabilityParseBucketSize(query.Get("bucketSizeMinutes"))
		if message != "" {
			availabilityError(writer, http.StatusBadRequest, message)
			return
		}
		options.bucketSizeMinutes = &size
	}

	if query.Has("includeDisabled") {
		switch query.Get("includeDisabled") {
		case "true":
			options.includeDisabled = true
		case "false":
			options.includeDisabled = false
		default:
			availabilityError(writer, http.StatusBadRequest,
				"Invalid includeDisabled: expected true or false")
			return
		}
	}

	if query.Has("maxBuckets") {
		maxBuckets, message := availabilityParseMaxBuckets(query.Get("maxBuckets"))
		if message != "" {
			availabilityError(writer, http.StatusBadRequest, message)
			return
		}
		options.maxBuckets = &maxBuckets
	}

	result, err := api.queryBucketedAvailability(request, options)
	if err != nil {
		var invalid *availabilityQueryValidationError
		if asValidationError(err, &invalid) {
			availabilityError(writer, http.StatusBadRequest, invalid.message)
			return
		}
		api.logger.Error("admin_availability_query_failed", map[string]any{"error": err.Error()})
		availabilityError(writer, http.StatusInternalServerError, "Internal server error")
		return
	}
	availabilityWriteJSON(writer, http.StatusOK, result)
}

// availabilityQueryValidationError 是聚合层的校验失败（Node 的 AvailabilityQueryValidationError）。
type availabilityQueryValidationError struct{ message string }

func (e *availabilityQueryValidationError) Error() string { return e.message }

// asValidationError 把 err 归到校验失败上（等价 errors.As，但只关心本类型，省一层反射）。
func asValidationError(err error, target **availabilityQueryValidationError) bool {
	invalid, ok := err.(*availabilityQueryValidationError)
	if ok {
		*target = invalid
	}
	return ok
}

// availabilityParseProviderIDs 复刻 parseProviderIdsQueryParam：逗号分隔、逐项正整数、去重保序。
func availabilityParseProviderIDs(raw string) ([]int64, string) {
	const invalidList = "Invalid providerIds: expected comma-separated positive integers"
	const invalidItem = "Invalid providerIds: expected a positive integer"

	tokens := strings.Split(raw, ",")
	providerIDs := make([]int64, 0, len(tokens))
	seen := make(map[int64]struct{}, len(tokens))
	for _, token := range tokens {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" {
			return nil, invalidList
		}
		if !availabilityIsDigits(trimmed) {
			return nil, invalidItem
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, invalidItem
		}
		if _, dup := seen[parsed]; dup {
			continue
		}
		seen[parsed] = struct{}{}
		providerIDs = append(providerIDs, parsed)
	}
	return providerIDs, ""
}

// availabilityParseBucketSize 复刻 parsePositiveNumberQueryParam(bucketSizeMinutes) 的两道界。
func availabilityParseBucketSize(raw string) (float64, string) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	// JS 的 Number("") 是 0、Number("abc") 是 NaN，两者都落在「期望正数」这一条上。
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed <= 0 {
		return 0, "Invalid bucketSizeMinutes: expected a positive number"
	}
	if parsed < availabilityMinBucketSizeMinutes {
		return 0, "Invalid bucketSizeMinutes: expected a positive number not less than 0.25"
	}
	if parsed > availabilityMaxBucketSizeMinutes {
		return 0, "Invalid bucketSizeMinutes: expected a positive number not greater than 1440"
	}
	return parsed, ""
}

// availabilityParseMaxBuckets 复刻 parsePositiveIntegerQueryParam(maxBuckets, 100)。
func availabilityParseMaxBuckets(raw string) (int, string) {
	trimmed := strings.TrimSpace(raw)
	if !availabilityIsDigits(trimmed) {
		return 0, "Invalid maxBuckets: expected a positive integer"
	}
	parsed, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, "Invalid maxBuckets: expected a positive integer"
	}
	if parsed > availabilityMaxBucketsHardLimit {
		return 0, "Invalid maxBuckets: expected a positive integer not greater than 100"
	}
	return int(parsed), ""
}

// availabilityIsDigits 复刻 `/^\d+$/`（只认 ASCII 数字，空串为假）。
func availabilityIsDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// availabilityDetermineOptimalBucketSize 复刻 determineOptimalBucketSize：目标 50 桶取最近的档位。
func availabilityDetermineOptimalBucketSize(timeRangeMinutes float64) float64 {
	ideal := timeRangeMinutes / availabilityBucketsTargetCount
	for _, size := range availabilityStandardBucketSizes {
		if ideal <= size {
			return size
		}
	}
	return availabilityMaxBucketSizeMinutes
}

// availabilitySanitizeMaxBuckets 复刻 sanitizeMaxBuckets：缺省 100，夹到 [1, 100] 并向下取整。
func availabilitySanitizeMaxBuckets(maxBuckets *int) int {
	if maxBuckets == nil || *maxBuckets <= 0 {
		return availabilityDefaultMaxBuckets
	}
	// 下面的 math.Floor 是空操作（maxBuckets 是 int，转 float64 精确），保留它只为与 Node 的
	// sanitizeMaxBuckets 逐句对应；staticcheck 的 SA4015 正是报在这里，属已知误报，不必改。
	value := float64(*maxBuckets)
	return int(math.Min(availabilityMaxBucketsHardLimit, math.Max(1, math.Floor(value))))
}

// availabilitySanitizeBucketSize 复刻 sanitizeBucketSizeMinutes 的三条分支。
//
// 显式桶宽为「缺省/非法值」时取 档位值与「预算下限桶宽」的较大者；显式合法时若窗口超出
// `桶宽 × maxBuckets` 的预算则直接拒绝（这是 Node 唯一一条 400 的聚合层校验）。
func availabilitySanitizeBucketSize(
	explicit *float64,
	timeRangeMinutes float64,
	maxBuckets int,
) (float64, error) {
	fallback := availabilityDetermineOptimalBucketSize(timeRangeMinutes)
	safeFallback := fallback
	if !(safeFallback > 0) {
		safeFallback = 60
	}
	minimumBudget := availabilityMinBucketSizeMinutes
	if timeRangeMinutes > 0 {
		minimumBudget = timeRangeMinutes / math.Max(1, float64(maxBuckets))
	}
	clampedMinimumBudget := math.Min(
		availabilityMaxBucketSizeMinutes,
		math.Max(availabilityMinBucketSizeMinutes, minimumBudget),
	)

	if explicit == nil || !(*explicit > 0) {
		return math.Min(
			availabilityMaxBucketSizeMinutes,
			math.Max(math.Max(availabilityMinBucketSizeMinutes, safeFallback), clampedMinimumBudget),
		), nil
	}

	normalized := math.Min(
		availabilityMaxBucketSizeMinutes,
		math.Max(availabilityMinBucketSizeMinutes, *explicit),
	)
	if timeRangeMinutes > normalized*float64(maxBuckets) {
		return 0, &availabilityQueryValidationError{
			message: "Invalid bucket configuration: requested range exceeds the bucket budget " +
				"implied by bucketSizeMinutes and maxBuckets",
		}
	}
	return normalized, nil
}

// availabilityFloorToUTCMinute 复刻 floorToUtcMinute：按 UTC 落到整分钟。
func availabilityFloorToUTCMinute(value time.Time) time.Time {
	utc := value.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), utc.Hour(), utc.Minute(), 0, 0, time.UTC)
}

// availabilityBucketMetrics 是返回给 UI 的单桶（Node 的 TimeBucketMetrics）。
type availabilityBucketMetrics struct {
	BucketStart       string  `json:"bucketStart"`
	BucketEnd         string  `json:"bucketEnd"`
	TotalRequests     int64   `json:"totalRequests"`
	GreenCount        int64   `json:"greenCount"`
	RedCount          int64   `json:"redCount"`
	AvailabilityScore float64 `json:"availabilityScore"`
	AvgLatencyMS      float64 `json:"avgLatencyMs"`
	P50LatencyMS      float64 `json:"p50LatencyMs"`
	P95LatencyMS      float64 `json:"p95LatencyMs"`
	P99LatencyMS      float64 `json:"p99LatencyMs"`
}

// availabilityProviderSummary 是返回给 UI 的单个供应商汇总（Node 的 ProviderAvailabilitySummary）。
//
// 字段顺序照 Node 的对象字面量；`currentAvailability` 与 `successRate` 值相同（后者是兼容别名）。
type availabilityProviderSummary struct {
	ProviderID          int64                       `json:"providerId"`
	ProviderName        string                      `json:"providerName"`
	ProviderType        string                      `json:"providerType"`
	IsEnabled           bool                        `json:"isEnabled"`
	CurrentStatus       string                      `json:"currentStatus"`
	CurrentAvailability float64                     `json:"currentAvailability"`
	TotalRequests       int64                       `json:"totalRequests"`
	SuccessRate         float64                     `json:"successRate"`
	AvgLatencyMS        float64                     `json:"avgLatencyMs"`
	LastRequestAt       *string                     `json:"lastRequestAt"`
	TimeBuckets         []availabilityBucketMetrics `json:"timeBuckets"`
}

// availabilityBucketedResult 是 `/api/availability` 的正文（Node 的 AvailabilityQueryResult）。
type availabilityBucketedResult struct {
	QueriedAt          string                        `json:"queriedAt"`
	StartTime          string                        `json:"startTime"`
	EndTime            string                        `json:"endTime"`
	BucketSizeMinutes  float64                       `json:"bucketSizeMinutes"`
	Providers          []availabilityProviderSummary `json:"providers"`
	SystemAvailability float64                       `json:"systemAvailability"`
}

// queryBucketedAvailability 复刻 queryProviderAvailability。
//
// 顺序与 Node 逐条对齐：定窗口与桶宽 → 取供应商列表（空则提前返回）→ 取展示桶 →
// 逐供应商汇总（含「用最近 3 个返回桶推导当前状态」）→ 按返回桶加权算全站可用性。
func (api *availabilityAPI) queryBucketedAvailability(
	request *http.Request,
	options availabilityQueryOptions,
) (*availabilityBucketedResult, error) {
	ctx := request.Context()
	now := time.Now()

	startDate := now.Add(-availabilityDefaultWindowHours * time.Hour)
	if options.startTime != nil {
		startDate = *options.startTime
	}
	endDate := now
	if options.endTime != nil {
		endDate = *options.endTime
	}

	rangeMS := float64(endDate.Sub(startDate)) / float64(time.Millisecond)
	if rangeMS < 0 {
		return nil, &availabilityQueryValidationError{
			message: "Invalid time range: endTime must be greater than or equal to startTime",
		}
	}
	if rangeMS > availabilityMaxRangeMS {
		return nil, &availabilityQueryValidationError{
			message: "Invalid time range: requested range must not exceed 100 days",
		}
	}

	timeRangeMinutes := rangeMS / (1000 * 60)
	maxBuckets := availabilitySanitizeMaxBuckets(options.maxBuckets)
	bucketSizeMinutes, err := availabilitySanitizeBucketSize(
		options.bucketSizeMinutes, timeRangeMinutes, maxBuckets)
	if err != nil {
		return nil, err
	}

	providers, err := api.pools.AdminListAvailabilityProviders(
		ctx, options.providerIDs, options.includeDisabled)
	if err != nil {
		return nil, err
	}

	result := &availabilityBucketedResult{
		QueriedAt:         availabilityISOMillis(now),
		StartTime:         availabilityISOMillis(startDate),
		EndTime:           availabilityISOMillis(endDate),
		BucketSizeMinutes: bucketSizeMinutes,
		Providers:         []availabilityProviderSummary{},
	}
	if len(providers) == 0 {
		return result, nil
	}

	ids := make([]int64, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.ID)
	}
	// 窗口两端都先落到整分钟：上界用 floor 后的桶起点，与 Node 的 floorToUtcMinute 一致。
	rangeStartBucket := availabilityFloorToUTCMinute(startDate)
	rangeEndBucket := availabilityFloorToUTCMinute(endDate)

	rows, err := api.pools.AdminListAvailabilityBuckets(
		ctx, ids, bucketSizeMinutes, rangeStartBucket, rangeEndBucket, maxBuckets)
	if err != nil {
		return nil, err
	}

	bucketsByProvider := make(map[int64][]store.AdminAvailabilityBucketRow, len(providers))
	for _, row := range rows {
		bucketsByProvider[row.ProviderID] = append(bucketsByProvider[row.ProviderID], row)
	}

	bucketSizeMS := bucketSizeMinutes * 60 * 1000
	summaries := make([]availabilityProviderSummary, 0, len(providers))
	for _, provider := range providers {
		summary := availabilityProviderSummary{
			ProviderID:    provider.ID,
			ProviderName:  provider.Name,
			ProviderType:  availabilityProviderTypeOrDefault(provider.ProviderType),
			IsEnabled:     availabilityProviderEnabledOrDefault(provider.Enabled),
			CurrentStatus: "unknown",
			TimeBuckets:   []availabilityBucketMetrics{},
		}

		var totalGreen, totalRed, totalLatencyCount int64
		var totalLatencySumMS float64
		var lastRequestAtMS int64

		for _, bucket := range bucketsByProvider[provider.ID] {
			greenCount := bucket.GreenCount
			redCount := bucket.RedCount
			totalGreen += greenCount
			totalRed += redCount
			totalLatencyCount += bucket.LatencyCount
			totalLatencySumMS += bucket.LatencySumMS
			if ms := bucket.LastRequestAt.UnixMilli(); ms > lastRequestAtMS {
				lastRequestAtMS = ms
			}

			bucketEnd := bucket.BucketStart.Add(
				time.Duration(math.Trunc(bucketSizeMS)) * time.Millisecond)
			summary.TimeBuckets = append(summary.TimeBuckets, availabilityBucketMetrics{
				BucketStart:       availabilityISOMillis(bucket.BucketStart),
				BucketEnd:         availabilityISOMillis(bucketEnd),
				TotalRequests:     greenCount + redCount,
				GreenCount:        greenCount,
				RedCount:          redCount,
				AvailabilityScore: availabilityScore(greenCount, redCount),
				AvgLatencyMS:      bucket.AvgLatencyMS,
				P50LatencyMS:      bucket.P50LatencyMS,
				P95LatencyMS:      bucket.P95LatencyMS,
				P99LatencyMS:      bucket.P99LatencyMS,
			})
		}

		summary.TotalRequests = totalGreen + totalRed
		summary.CurrentAvailability = availabilityScore(totalGreen, totalRed)
		summary.SuccessRate = summary.CurrentAvailability
		if totalLatencyCount > 0 {
			summary.AvgLatencyMS = totalLatencySumMS / float64(totalLatencyCount)
		}
		if lastRequestAtMS > 0 {
			formatted := availabilityISOMillis(time.UnixMilli(lastRequestAtMS).UTC())
			summary.LastRequestAt = &formatted
		}

		// 当前状态只看**返回桶**的最后 3 个：更早的非空桶可能已被 maxBuckets 截掉，
		// 故它反映的是截断后的尾窗而不是整段查询窗（Node 的注释同样如此交代）。
		// 无数据一律 unknown，不能默认绿。
		if len(summary.TimeBuckets) > 0 {
			recent := summary.TimeBuckets
			if len(recent) > 3 {
				recent = recent[len(recent)-3:]
			}
			var recentGreen, recentRed int64
			for _, bucket := range recent {
				recentGreen += bucket.GreenCount
				recentRed += bucket.RedCount
			}
			switch {
			case recentGreen+recentRed == 0:
				summary.CurrentStatus = "unknown"
			case availabilityScore(recentGreen, recentRed) >= 0.5:
				summary.CurrentStatus = "green"
			default:
				summary.CurrentStatus = "red"
			}
		}

		summaries = append(summaries, summary)
	}

	// 全站可用性按「返回桶里的请求数」加权；被截断时它同样只反映子窗口。
	var totalSystemRequests int64
	for _, summary := range summaries {
		totalSystemRequests += summary.TotalRequests
	}
	if totalSystemRequests > 0 {
		var weighted float64
		for _, summary := range summaries {
			weighted += summary.CurrentAvailability * float64(summary.TotalRequests)
		}
		result.SystemAvailability = weighted / float64(totalSystemRequests)
	}

	result.Providers = summaries
	return result, nil
}

// availabilityProviderTypeOrDefault 是 Node 的 `provider.providerType ?? "claude"`。
func availabilityProviderTypeOrDefault(value *string) string {
	if value == nil || *value == "" {
		return "claude"
	}
	return *value
}

// availabilityProviderEnabledOrDefault 是 Node 的 `provider.enabled ?? true`。
func availabilityProviderEnabledOrDefault(value *bool) bool {
	return value == nil || *value
}

// availabilityISOMillis 复刻 `Date.prototype.toISOString()`：毫秒三位定长、UTC、Z 结尾。
func availabilityISOMillis(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
