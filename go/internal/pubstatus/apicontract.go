package pubstatus

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// 本文件是 Node `src/lib/public-status/public-api-contract.ts`（544 行）的转写：查询参数校验、
// 过滤与响应装配。它是**纯逻辑**（除了取 `url.Values`），故与读路径分开，可单测每条分支。
//
// 三处必须逐字对齐、且都属于「看起来无关紧要但会改变服务语义」的点：
//  1. **`interval` 接受 `15m` 形态**并**就近吸附**到 {5,15,30,60}（距离相同时取**更大的**）——
//     不是拒绝非法值也不是向上取整；
//  2. **`rangeHours` 饱和到 [1,168]**，而 `interval` 是吸附：两者对非法输入的处置**不同**；
//  3. **`include` 缺省即全开**；且 `include` 不含 `groups` 时响应里 `groups` 是**空数组**
//     （不是省略该字段——前端按恒有字段解析）。
//
// 字符串长度按 **UTF-16 码元**计（Node 的 `String.prototype.length`）：Go 的 `len()` 是字节、
// `utf8.RuneCountInString` 是码点，两者都与 Node 不同。astral 字符（emoji）在 Node 算 2，
// 用码点算会放过一个本该判越界的输入。

// 过滤与投影的取值域（public-api-contract.ts:19-35）。
var (
	PublicStatusIntervalOptions = []int{5, 15, 30, 60}
	PublicStatusFilterStatuses  = []string{"operational", "degraded", "failed", "no_data"}
	PublicStatusIncludeValues   = []string{"meta", "defaults", "groups", "timeline"}
	PublicStatusRouteStatuses   = []string{"ready", "stale", "rebuilding", "no_snapshot", "no_data"}
)

// PublicStatusQueryDefaults 是默认窗口（public-api-contract.ts:37-40）。
type PublicStatusQueryDefaults struct {
	IntervalMinutes int `json:"intervalMinutes"`
	RangeHours      int `json:"rangeHours"`
}

// PublicStatusRouteMeta 是响应里的 meta（public-api-contract.ts:84-88）。
type PublicStatusRouteMeta struct {
	SiteTitle       *string `json:"siteTitle"`
	SiteDescription *string `json:"siteDescription"`
	TimeZone        *string `json:"timeZone"`
}

// PublicStatusRouteRebuildState 是响应里的 rebuildState（public-api-contract.ts:90-94）。
//
// Reason **恒为 null**：Node 的装配处就是这么写的（contract.ts:513），重建原因只作为提示写进
// Redis（见 SchedulePublicStatusRebuild），不回给访客。
type PublicStatusRouteRebuildState struct {
	State       ServeState `json:"state"`
	HasSnapshot bool       `json:"hasSnapshot"`
	Reason      *string    `json:"reason"`
}

// PublicStatusRouteStatus 是**路由级**状态，比服务态多两档（public-api-contract.ts:27-33）。
type PublicStatusRouteStatus string

const (
	RouteStatusReady      PublicStatusRouteStatus = "ready"
	RouteStatusStale      PublicStatusRouteStatus = "stale"
	RouteStatusRebuilding PublicStatusRouteStatus = "rebuilding"
	RouteStatusNoSnapshot PublicStatusRouteStatus = "no_snapshot"
	RouteStatusNoData     PublicStatusRouteStatus = "no_data"
)

// PublicStatusValidationIssue 是一条校验失败（public-api-contract.ts:42-48）。
//
// Code 是**契约自定义**的五个码，不是 zod 的码：`invalid_number` / `invalid_enum` /
// `invalid_text` / `too_many_values` / `value_too_long`。响应里的 invalidParams 直接用它。
type PublicStatusValidationIssue struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Value   string `json:"value,omitempty"`
}

// PublicStatusQueryValidationError 是校验失败的聚合错误（public-api-contract.ts:50-60）。
type PublicStatusQueryValidationError struct {
	Issues []PublicStatusValidationIssue
}

func (e *PublicStatusQueryValidationError) Error() string {
	return "Invalid public status query parameters"
}

// PublicStatusQueryFilters 是三类过滤 + 一个模糊搜索（public-api-contract.ts:63-68）。
type PublicStatusQueryFilters struct {
	GroupSlugs []string
	Models     []string
	Statuses   []string
	Q          *string
}

// PublicStatusResolvedQuery 回显给调用方的解析结果（public-api-contract.ts:72-80）。
type PublicStatusResolvedQuery struct {
	IntervalMinutes int      `json:"intervalMinutes"`
	RangeHours      int      `json:"rangeHours"`
	GroupSlugs      []string `json:"groupSlugs"`
	Models          []string `json:"models"`
	Statuses        []string `json:"statuses"`
	Q               *string  `json:"q"`
	Include         []string `json:"include"`
}

// PublicStatusParsedQuery 是解析结果（public-api-contract.ts:62-81）。
type PublicStatusParsedQuery struct {
	IntervalMinutes int
	RangeHours      int
	Filters         PublicStatusQueryFilters
	Include         []string
	Defaults        PublicStatusQueryDefaults
	ResolvedQuery   PublicStatusResolvedQuery
}

// PublicStatusRouteResponse 是响应正文（public-api-contract.ts:96-106）。
//
// **字段顺序即 JSON 键序**，对拍台逐字节比对，故顺序照抄 Node 的对象字面量顺序：
// generatedAt → freshUntil → status → rebuildState → defaults → resolvedQuery → meta → groups。
type PublicStatusRouteResponse struct {
	GeneratedAt   *string                       `json:"generatedAt"`
	FreshUntil    *string                       `json:"freshUntil"`
	Status        PublicStatusRouteStatus       `json:"status"`
	RebuildState  PublicStatusRouteRebuildState `json:"rebuildState"`
	Defaults      *PublicStatusQueryDefaults    `json:"defaults"`
	ResolvedQuery PublicStatusResolvedQuery     `json:"resolvedQuery"`
	Meta          *PublicStatusRouteMeta        `json:"meta"`
	Groups        []PublicStatusPayloadGroup    `json:"groups"`
}

// utf16Length 返回 Node `String.prototype.length` 的口径（UTF-16 码元数）。
func utf16Length(value string) int {
	length := 0
	for _, character := range value {
		if character > 0xFFFF {
			length += 2
			continue
		}
		length++
	}
	return length
}

func containsControlCharacters(value string) bool {
	for _, character := range value {
		// Node 判的是 UTF-16 码元 <= 31 或 === 127；代理对的两个码元都 > 127，故按 rune 判等价。
		if character <= 31 || character == 127 {
			return true
		}
	}
	return false
}

func dedupePreservingOrder(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	deduped := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		deduped = append(deduped, value)
	}
	return deduped
}

// clampIntervalMinutes 复刻 clampIntervalMinutes（public-api-contract.ts:118-133）：就近吸附，
// 距离相同时取更大者（避免 5↔15 之间摇摆时向下取整）。
func clampIntervalMinutes(rawMinutes int) int {
	bestValue := PublicStatusIntervalOptions[0]
	bestDistance := -1
	for _, candidate := range PublicStatusIntervalOptions {
		distance := candidate - rawMinutes
		if distance < 0 {
			distance = -distance
		}
		if bestDistance < 0 || distance < bestDistance ||
			(distance == bestDistance && candidate > bestValue) {
			bestValue = candidate
			bestDistance = distance
		}
	}
	return bestValue
}

// parseWindowNumber 复刻 parseWindowNumber（public-api-contract.ts:135-176）。
//
// `interval` 允许 `15m` 形态（正则 `^(\d+)(m)?$`，大小写无关）；`rangeHours` 只接受纯数字。
// 非法一律记为一条 issue 并返回 null——**不做静默兜底**：静默兜底之下，前端把 `interval=abc`
// 发过来会得到一个「看起来正常」的响应，问题会拖到很久以后才被发现。
func parseWindowNumber(
	rawValue *string,
	field string,
	issues *[]PublicStatusValidationIssue,
) *int {
	if rawValue == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*rawValue)
	fail := func() *int {
		*issues = append(*issues, PublicStatusValidationIssue{
			Field:   field,
			Code:    "invalid_number",
			Message: field + " must be a positive integer",
			Value:   *rawValue,
		})
		return nil
	}
	if trimmed == "" {
		return fail()
	}

	numberSource := trimmed
	if field == "interval" {
		digits, ok := stripOptionalMinuteSuffix(trimmed)
		if !ok {
			return fail()
		}
		numberSource = digits
	} else if !isAllDigits(trimmed) {
		return fail()
	}

	parsed, err := strconv.Atoi(numberSource)
	if err != nil || parsed <= 0 {
		// 超 int 范围的数字也走这一支（Node 的 Number.isSafeInteger 判据）。
		return fail()
	}
	if parsed > 1<<53 {
		return fail()
	}
	return &parsed
}

// stripOptionalMinuteSuffix 复刻 `/^(\d+)(m)?$/i`。
func stripOptionalMinuteSuffix(value string) (string, bool) {
	candidate := value
	if len(candidate) > 0 && (candidate[len(candidate)-1] == 'm' || candidate[len(candidate)-1] == 'M') {
		candidate = candidate[:len(candidate)-1]
	}
	if candidate == "" || !isAllDigits(candidate) {
		return "", false
	}
	return candidate, true
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// parseCSVList 复刻 parseCsvList（public-api-contract.ts:178-190）：多个别名键全部取出、逗号切分、
// 去空白、丢空项。注意 `searchParams.get` 只取**第一个**同名参数，Go 侧同样只取首个 Value。
func parseCSVList(values url.Values, keys []string) []string {
	collected := make([]string, 0)
	for _, key := range keys {
		if !values.Has(key) {
			continue
		}
		for _, piece := range strings.Split(values.Get(key), ",") {
			trimmed := strings.TrimSpace(piece)
			if trimmed == "" {
				continue
			}
			collected = append(collected, trimmed)
		}
	}
	return collected
}

// validateTextList 复刻 validateTextList（public-api-contract.ts:192-236）。
func validateTextList(
	rawValues []string,
	field string,
	issues *[]PublicStatusValidationIssue,
	maxLength int,
) []string {
	if len(rawValues) > 100 {
		*issues = append(*issues, PublicStatusValidationIssue{
			Field:   field,
			Code:    "too_many_values",
			Message: field + " accepts at most 100 values",
			Value:   strconv.Itoa(len(rawValues)),
		})
		return []string{}
	}

	normalized := make([]string, 0, len(rawValues))
	for _, rawValue := range rawValues {
		if containsControlCharacters(rawValue) {
			*issues = append(*issues, PublicStatusValidationIssue{
				Field:   field,
				Code:    "invalid_text",
				Message: field + " cannot contain control characters",
				Value:   rawValue,
			})
			continue
		}
		if utf16Length(rawValue) > maxLength {
			*issues = append(*issues, PublicStatusValidationIssue{
				Field:   field,
				Code:    "value_too_long",
				Message: fmt.Sprintf("%s values must be at most %d characters", field, maxLength),
				Value:   rawValue,
			})
			continue
		}
		normalized = append(normalized, rawValue)
	}
	return dedupePreservingOrder(normalized)
}

// validateEnumList 复刻 validateEnumList（public-api-contract.ts:238-262）。
func validateEnumList(
	rawValues []string,
	field string,
	issues *[]PublicStatusValidationIssue,
	allowedValues []string,
) []string {
	allowed := make(map[string]struct{}, len(allowedValues))
	for _, value := range allowedValues {
		allowed[value] = struct{}{}
	}
	normalized := make([]string, 0, len(rawValues))
	for _, rawValue := range rawValues {
		if _, ok := allowed[rawValue]; !ok {
			*issues = append(*issues, PublicStatusValidationIssue{
				Field:   field,
				Code:    "invalid_enum",
				Message: field + " must be one of: " + strings.Join(allowedValues, ", "),
				Value:   rawValue,
			})
			continue
		}
		normalized = append(normalized, rawValue)
	}
	return dedupePreservingOrder(normalized)
}

// parseSearchQuery 复刻 parseSearchQuery（public-api-contract.ts:264-300）：空/纯空白归一为 null。
func parseSearchQuery(rawValue *string, issues *[]PublicStatusValidationIssue) *string {
	if rawValue == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*rawValue)
	if trimmed == "" {
		return nil
	}
	if containsControlCharacters(trimmed) {
		*issues = append(*issues, PublicStatusValidationIssue{
			Field:   "q",
			Code:    "invalid_text",
			Message: "q cannot contain control characters",
			Value:   *rawValue,
		})
		return nil
	}
	if utf16Length(trimmed) > 120 {
		*issues = append(*issues, PublicStatusValidationIssue{
			Field:   "q",
			Code:    "value_too_long",
			Message: "q must be at most 120 characters",
			Value:   *rawValue,
		})
		return nil
	}
	return &trimmed
}

// ParsePublicStatusQuery 复刻 parsePublicStatusQuery（public-api-contract.ts:302-382）。
//
// 任一条 issue 即整体失败（Node 抛 PublicStatusQueryValidationError）：**不做部分接受**——
// 部分接受会让「我筛了三个分组」的调用方拿到一个只筛了两个的结果。
func ParsePublicStatusQuery(
	values url.Values,
	defaults PublicStatusQueryDefaults,
) (PublicStatusParsedQuery, *PublicStatusQueryValidationError) {
	issues := make([]PublicStatusValidationIssue, 0)

	parsedInterval := parseWindowNumber(optionalValue(values, "interval"), "interval", &issues)
	parsedRangeHours := parseWindowNumber(optionalValue(values, "rangeHours"), "rangeHours", &issues)

	groupSlugs := validateTextList(
		parseCSVList(values, []string{"groupSlug", "groupSlugs"}), "groupSlug", &issues, 120,
	)
	models := validateTextList(
		parseCSVList(values, []string{"model", "models"}), "model", &issues, 200,
	)
	statuses := validateEnumList(
		parseCSVList(values, []string{"status"}), "status", &issues, PublicStatusFilterStatuses,
	)
	include := validateEnumList(
		parseCSVList(values, []string{"include"}), "include", &issues, PublicStatusIncludeValues,
	)
	q := parseSearchQuery(optionalValue(values, "q"), &issues)

	if len(issues) > 0 {
		return PublicStatusParsedQuery{}, &PublicStatusQueryValidationError{Issues: issues}
	}

	intervalMinutes := defaults.IntervalMinutes
	if parsedInterval != nil {
		intervalMinutes = clampIntervalMinutes(*parsedInterval)
	}
	rangeHours := defaults.RangeHours
	if parsedRangeHours != nil {
		rangeHours = *parsedRangeHours
		if rangeHours < 1 {
			rangeHours = 1
		}
		if rangeHours > maxPublicStatusRangeHours {
			rangeHours = maxPublicStatusRangeHours
		}
	}
	includeValues := include
	if len(includeValues) == 0 {
		includeValues = append([]string{}, PublicStatusIncludeValues...)
	}

	return PublicStatusParsedQuery{
		IntervalMinutes: intervalMinutes,
		RangeHours:      rangeHours,
		Filters: PublicStatusQueryFilters{
			GroupSlugs: groupSlugs,
			Models:     models,
			Statuses:   statuses,
			Q:          q,
		},
		Include:  includeValues,
		Defaults: defaults,
		ResolvedQuery: PublicStatusResolvedQuery{
			IntervalMinutes: intervalMinutes,
			RangeHours:      rangeHours,
			GroupSlugs:      groupSlugs,
			Models:          models,
			Statuses:        statuses,
			Q:               q,
			Include:         includeValues,
		},
	}, nil
}

// optionalValue 取可选查询参数（Node 的 `searchParams.get` 语义：不存在即 nil）。
func optionalValue(values url.Values, key string) *string {
	if !values.Has(key) {
		return nil
	}
	value := values.Get(key)
	return &value
}

// includesInsensitive 复刻 includesInsensitive（public-api-contract.ts:384-387）。
func includesInsensitive(haystacks []*string, needle string) bool {
	normalizedNeedle := strings.ToLower(needle)
	for _, haystack := range haystacks {
		if haystack == nil {
			continue
		}
		if strings.Contains(strings.ToLower(*haystack), normalizedNeedle) {
			return true
		}
	}
	return false
}

// deriveModelFilterState 复刻 deriveModelFilterState（public-api-contract.ts:389-411）。
//
// **从最后一个桶往回找**：最近的可用证据优先。末尾的 failed 直接判 failed（不是 degraded）；
// no_data 跳过（没有证据就不下结论）；availabilityPct < 50 判 degraded；否则按桶自身状态。
// 全无可用桶时回落模型的 latestState，再缺就是 no_data。
func deriveModelFilterState(model PublicStatusPayloadModel) PublicStatusTimelineState {
	for index := len(model.Timeline) - 1; index >= 0; index-- {
		bucket := model.Timeline[index]
		if bucket.State == TimelineStateFailed {
			return TimelineStateFailed
		}
		if bucket.State == TimelineStateNoData {
			continue
		}
		if bucket.AvailabilityPct != nil && *bucket.AvailabilityPct < 50 {
			return TimelineStateDegraded
		}
		if bucket.State == TimelineStateDegraded {
			return TimelineStateDegraded
		}
		return TimelineStateOperational
	}
	if model.LatestState == "" {
		return TimelineStateNoData
	}
	return model.LatestState
}

// FilterPublicStatusGroups 复刻 filterPublicStatusGroups（public-api-contract.ts:413-487）。
func FilterPublicStatusGroups(
	groups []PublicStatusPayloadGroup,
	query PublicStatusParsedQuery,
) []PublicStatusPayloadGroup {
	includeTimeline := containsString(query.Include, "timeline")
	if !containsString(query.Include, "groups") {
		return []PublicStatusPayloadGroup{}
	}

	groupFilter := make(map[string]struct{}, len(query.Filters.GroupSlugs))
	for _, slug := range query.Filters.GroupSlugs {
		groupFilter[slug] = struct{}{}
	}
	modelFilter := make(map[string]struct{}, len(query.Filters.Models))
	for _, model := range query.Filters.Models {
		modelFilter[model] = struct{}{}
	}
	statusFilter := make(map[string]struct{}, len(query.Filters.Statuses))
	for _, status := range query.Filters.Statuses {
		statusFilter[status] = struct{}{}
	}
	searchQuery := query.Filters.Q

	filtered := make([]PublicStatusPayloadGroup, 0, len(groups))
	for _, group := range groups {
		if len(groupFilter) > 0 {
			if _, ok := groupFilter[group.PublicGroupSlug]; !ok {
				continue
			}
		}

		groupMatchesSearch := false
		if searchQuery != nil {
			groupMatchesSearch = includesInsensitive(
				[]*string{&group.DisplayName, &group.PublicGroupSlug}, *searchQuery,
			)
		}

		models := make([]PublicStatusPayloadModel, 0, len(group.Models))
		for _, model := range group.Models {
			if len(modelFilter) > 0 {
				_, byKey := modelFilter[model.PublicModelKey]
				_, byLabel := modelFilter[model.Label]
				if !byKey && !byLabel {
					continue
				}
			}

			if len(statusFilter) > 0 {
				if _, ok := statusFilter[string(deriveModelFilterState(model))]; !ok {
					continue
				}
			}

			if searchQuery != nil && !groupMatchesSearch &&
				!includesInsensitive([]*string{&model.PublicModelKey, &model.Label}, *searchQuery) {
				continue
			}

			filteredModel := model
			if !includeTimeline {
				filteredModel.Timeline = []PublicStatusTimelineBucket{}
			}
			models = append(models, filteredModel)
		}

		if len(models) == 0 {
			continue
		}
		filteredGroup := group
		filteredGroup.Models = models
		filtered = append(filtered, filteredGroup)
	}
	return filtered
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// mapRouteStatus 复刻 mapRouteStatus（public-api-contract.ts:489-507）。
func mapRouteStatus(payload PublicStatusPayload, rebuildReason *string) PublicStatusRouteStatus {
	switch payload.RebuildState {
	case ServeStateFresh:
		return RouteStatusReady
	case ServeStateStale:
		return RouteStatusStale
	case ServeStateNoData:
		return RouteStatusNoData
	}
	if payload.GeneratedAt != nil && *payload.GeneratedAt != "" {
		return RouteStatusStale
	}
	if rebuildReason != nil && *rebuildReason == "redis-unavailable" {
		return RouteStatusRebuilding
	}
	return RouteStatusNoSnapshot
}

// PublicStatusRouteResponseInput 是装配输入（public-api-contract.ts:509-515）。
type PublicStatusRouteResponseInput struct {
	Payload       PublicStatusPayload
	Query         PublicStatusParsedQuery
	Defaults      PublicStatusQueryDefaults
	Meta          *PublicStatusRouteMeta
	RebuildReason *string
}

// BuildPublicStatusRouteResponse 复刻 buildPublicStatusRouteResponse（public-api-contract.ts:509-533）。
func BuildPublicStatusRouteResponse(input PublicStatusRouteResponseInput) PublicStatusRouteResponse {
	response := PublicStatusRouteResponse{
		GeneratedAt: input.Payload.GeneratedAt,
		FreshUntil:  input.Payload.FreshUntil,
		Status:      mapRouteStatus(input.Payload, input.RebuildReason),
		RebuildState: PublicStatusRouteRebuildState{
			State:       input.Payload.RebuildState,
			HasSnapshot: input.Payload.GeneratedAt != nil && *input.Payload.GeneratedAt != "",
		},
		ResolvedQuery: input.Query.ResolvedQuery,
		Groups:        FilterPublicStatusGroups(input.Payload.Groups, input.Query),
	}
	if containsString(input.Query.Include, "defaults") {
		defaults := input.Defaults
		response.Defaults = &defaults
	}
	if containsString(input.Query.Include, "meta") {
		response.Meta = input.Meta
	}
	return response
}
