package pubstatus

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// 本文件钉住 public-api-contract.ts 的纯逻辑：查询校验（逐码）、过滤、路由状态映射与装配开关。
// 这些都是「非法输入怎么处置」的语义，Node 与 Go 一旦分叉，症状是前端筛选项静默失效——
// 故每条分支都留一条用例。

func parseQuery(t *testing.T, rawQuery string, defaults PublicStatusQueryDefaults) (PublicStatusParsedQuery, *PublicStatusQueryValidationError) {
	t.Helper()
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("解析查询串失败: %v", err)
	}
	return ParsePublicStatusQuery(values, defaults)
}

func defaultQueryDefaults() PublicStatusQueryDefaults {
	return PublicStatusQueryDefaults{IntervalMinutes: 5, RangeHours: 24}
}

func TestParseQueryDefaultsWhenNoParams(t *testing.T) {
	query, validationErr := parseQuery(t, "", PublicStatusQueryDefaults{IntervalMinutes: 15, RangeHours: 72})
	if validationErr != nil {
		t.Fatalf("无条件参数不该报错：%+v", validationErr.Issues)
	}
	if query.IntervalMinutes != 15 || query.RangeHours != 72 {
		t.Fatalf("应取默认窗口 15/72，收到 %d/%d", query.IntervalMinutes, query.RangeHours)
	}
	if len(query.Include) != 4 {
		t.Fatalf("include 缺省应全开，收到 %v", query.Include)
	}
	if query.Filters.Q != nil || len(query.Filters.GroupSlugs) != 0 || len(query.Filters.Models) != 0 || len(query.Filters.Statuses) != 0 {
		t.Fatalf("默认不该有任何过滤：%+v", query.Filters)
	}
}

func TestParseQueryWindowNumberSemantics(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		wantInterval int
		wantRange    int
	}{
		{name: "interval 接受 15m 形态", raw: "interval=15m", wantInterval: 15},
		{name: "interval 大写 M 也接受", raw: "interval=30M", wantInterval: 30},
		{name: "interval 就近吸附到 5", raw: "interval=7", wantInterval: 5},
		{name: "interval 距离相同时取更大者（10 → 15）", raw: "interval=10", wantInterval: 15},
		{name: "interval 45 同为 15 距离时取 60", raw: "interval=45", wantInterval: 60},
		{name: "interval 超范围吸附到 60", raw: "interval=999", wantInterval: 60},
		{name: "rangeHours 饱和到上限 168", raw: "rangeHours=999", wantRange: 168},
		{name: "rangeHours 取下限 1", raw: "rangeHours=1", wantRange: 1},
		{name: "两者同时给", raw: "interval=30&rangeHours=48", wantInterval: 30, wantRange: 48},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			query, validationErr := parseQuery(t, testCase.raw, defaultQueryDefaults())
			if validationErr != nil {
				t.Fatalf("不该报错：%+v", validationErr.Issues)
			}
			wantInterval := testCase.wantInterval
			if wantInterval == 0 {
				wantInterval = 5
			}
			wantRange := testCase.wantRange
			if wantRange == 0 {
				wantRange = 24
			}
			if query.IntervalMinutes != wantInterval || query.RangeHours != wantRange {
				t.Fatalf("窗口不符：want %d/%d got %d/%d", wantInterval, wantRange, query.IntervalMinutes, query.RangeHours)
			}
		})
	}
}

func TestParseQueryRejectsInvalidNumbers(t *testing.T) {
	cases := []struct{ name, raw, field string }{
		{name: "interval 非数字", raw: "interval=abc", field: "interval"},
		{name: "interval 带小数", raw: "interval=5.5", field: "interval"},
		{name: "interval 为 0", raw: "interval=0", field: "interval"},
		{name: "interval 空串", raw: "interval=", field: "interval"},
		{name: "interval 带后缀但无数字", raw: "interval=m", field: "interval"},
		{name: "rangeHours 负数", raw: "rangeHours=-3", field: "rangeHours"},
		{name: "rangeHours 为 0", raw: "rangeHours=0", field: "rangeHours"},
		{name: "rangeHours 带单位", raw: "rangeHours=24h", field: "rangeHours"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, validationErr := parseQuery(t, testCase.raw, defaultQueryDefaults())
			if validationErr == nil {
				t.Fatal("应报校验失败")
			}
			if len(validationErr.Issues) != 1 || validationErr.Issues[0].Code != "invalid_number" ||
				validationErr.Issues[0].Field != testCase.field {
				t.Fatalf("issue 不符：%+v", validationErr.Issues)
			}
		})
	}
}

func TestParseQueryListAliasesDedupeAndOrder(t *testing.T) {
	query, validationErr := parseQuery(
		t, "groupSlug=a,b&groupSlugs=b,c&model=m1&models=m2,m1", defaultQueryDefaults(),
	)
	if validationErr != nil {
		t.Fatalf("不该报错：%+v", validationErr.Issues)
	}
	if got := strings.Join(query.Filters.GroupSlugs, ","); got != "a,b,c" {
		t.Fatalf("分组别名合并与去重不符：%q", got)
	}
	if got := strings.Join(query.Filters.Models, ","); got != "m1,m2" {
		t.Fatalf("模型别名合并与去重不符：%q", got)
	}
	// 去重必须保序（前端按回显顺序展示已选筛选）。
	if query.ResolvedQuery.GroupSlugs[0] != "a" || query.ResolvedQuery.GroupSlugs[2] != "c" {
		t.Fatalf("去重后顺序不对：%v", query.ResolvedQuery.GroupSlugs)
	}
}

func TestParseQueryRejectsTooManyValues(t *testing.T) {
	values := make([]string, 0, 101)
	for index := 0; index < 101; index++ {
		values = append(values, "g")
	}
	_, validationErr := parseQuery(t, "groupSlug="+strings.Join(values, ","), defaultQueryDefaults())
	if validationErr == nil || validationErr.Issues[0].Code != "too_many_values" {
		t.Fatalf("超过 100 个值应报 too_many_values：%+v", validationErr)
	}
}

func TestParseQueryTextValidation(t *testing.T) {
	// 控制字符（%01）必须被拒。
	_, validationErr := parseQuery(t, "groupSlug=ab%01cd", defaultQueryDefaults())
	if validationErr == nil || validationErr.Issues[0].Code != "invalid_text" {
		t.Fatalf("控制字符应报 invalid_text：%+v", validationErr)
	}

	// 超长（121 个 ASCII 字符）。
	long := strings.Repeat("x", 121)
	_, validationErr = parseQuery(t, "groupSlug="+long, defaultQueryDefaults())
	if validationErr == nil || validationErr.Issues[0].Code != "value_too_long" {
		t.Fatalf("超长值应报 value_too_long：%+v", validationErr)
	}

	// 长度按 **UTF-16 码元** 计：60 个 emoji = 120 码元（合法），61 个 = 122（越界）。
	sixtyEmoji := strings.Repeat("\U0001F600", 60)
	query, validationErr := parseQuery(t, "groupSlug="+url.QueryEscape(sixtyEmoji), defaultQueryDefaults())
	if validationErr != nil {
		t.Fatalf("60 个 emoji（120 码元）应合法：%+v", validationErr.Issues)
	}
	if len(query.Filters.GroupSlugs) != 1 {
		t.Fatalf("合法值应保留：%+v", query.Filters.GroupSlugs)
	}
	_, validationErr = parseQuery(t, "groupSlug="+url.QueryEscape(sixtyEmoji+"\U0001F600"), defaultQueryDefaults())
	if validationErr == nil || validationErr.Issues[0].Code != "value_too_long" {
		t.Fatalf("61 个 emoji（122 码元）应越界：%+v", validationErr)
	}
}

func TestParseQueryEnumValidation(t *testing.T) {
	_, validationErr := parseQuery(t, "status=operational,weird", defaultQueryDefaults())
	if validationErr == nil || validationErr.Issues[0].Code != "invalid_enum" ||
		validationErr.Issues[0].Field != "status" {
		t.Fatalf("非法枚举应报 invalid_enum：%+v", validationErr)
	}
	if !strings.Contains(validationErr.Issues[0].Message, "operational, degraded, failed, no_data") {
		t.Fatalf("枚举错误消息应列出全部合法值：%q", validationErr.Issues[0].Message)
	}

	_, validationErr = parseQuery(t, "include=meta,bogus", defaultQueryDefaults())
	if validationErr == nil || validationErr.Issues[0].Field != "include" {
		t.Fatalf("include 非法值应报错：%+v", validationErr)
	}

	query, validationErr := parseQuery(t, "status=degraded&include=meta,timeline", defaultQueryDefaults())
	if validationErr != nil {
		t.Fatalf("合法枚举不该报错：%+v", validationErr.Issues)
	}
	if len(query.Include) != 2 || query.Include[0] != "meta" {
		t.Fatalf("include 应保序：%v", query.Include)
	}
}

func TestParseQuerySearchQueryRules(t *testing.T) {
	// 空/纯空白归一为 nil。
	for _, raw := range []string{"q=", "q=%20%20"} {
		query, validationErr := parseQuery(t, raw, defaultQueryDefaults())
		if validationErr != nil {
			t.Fatalf("%q 不该报错：%+v", raw, validationErr.Issues)
		}
		if query.Filters.Q != nil {
			t.Fatalf("%q 应归一为 nil，收到 %q", raw, *query.Filters.Q)
		}
	}

	query, validationErr := parseQuery(t, "q=%20hello%20", defaultQueryDefaults())
	if validationErr != nil {
		t.Fatalf("不该报错：%+v", validationErr.Issues)
	}
	if query.Filters.Q == nil || *query.Filters.Q != "hello" {
		t.Fatalf("q 应被 trim：%v", query.Filters.Q)
	}

	_, validationErr = parseQuery(t, "q=a%01b", defaultQueryDefaults())
	if validationErr == nil || validationErr.Issues[0].Field != "q" {
		t.Fatalf("q 含控制字符应报错：%+v", validationErr)
	}

	_, validationErr = parseQuery(t, "q="+strings.Repeat("y", 121), defaultQueryDefaults())
	if validationErr == nil || validationErr.Issues[0].Code != "value_too_long" {
		t.Fatalf("q 超长应报错：%+v", validationErr)
	}
}

func TestParseQueryAccumulatesEveryIssue(t *testing.T) {
	// Node 一次性收集全部 issue（而不是遇错即停）：前端的表单校验要靠整份清单。
	_, validationErr := parseQuery(t, "interval=abc&status=nope&q=a%01b", defaultQueryDefaults())
	if validationErr == nil {
		t.Fatal("应报错")
	}
	if len(validationErr.Issues) != 3 {
		t.Fatalf("应收集 3 条 issue，收到 %d：%+v", len(validationErr.Issues), validationErr.Issues)
	}
}

// --- 过滤与状态推导 ---

func timelineBucket(state PublicStatusTimelineState, availability *float64) PublicStatusTimelineBucket {
	return PublicStatusTimelineBucket{
		BucketStart: "2026-09-13T00:00:00.000Z", BucketEnd: "2026-09-13T00:05:00.000Z",
		State: state, AvailabilityPct: availability, SampleCount: 1,
	}
}

func modelFixture(key string, latest PublicStatusTimelineState, buckets ...PublicStatusTimelineBucket) PublicStatusPayloadModel {
	return PublicStatusPayloadModel{
		PublicModelKey: key, Label: key + " label", VendorIconKey: "vendor", RequestTypeBadge: "openaiCompatible",
		LatestState: latest, Timeline: buckets,
	}
}

func TestDeriveModelFilterStateWalksTimelineBackwards(t *testing.T) {
	degradedAvailability := 42.0
	healthyAvailability := 99.0
	cases := []struct {
		name  string
		model PublicStatusPayloadModel
		want  PublicStatusTimelineState
	}{
		{
			name:  "末尾 failed 直接判 failed",
			model: modelFixture("m", TimelineStateOperational, timelineBucket(TimelineStateFailed, &healthyAvailability)),
			want:  TimelineStateFailed,
		},
		{
			name: "末尾 no_data 跳过，由前一个桶决定",
			model: modelFixture("m", TimelineStateOperational,
				timelineBucket(TimelineStateDegraded, &healthyAvailability),
				timelineBucket(TimelineStateNoData, nil)),
			want: TimelineStateDegraded,
		},
		{
			name:  "可用率低于 50% 判 degraded",
			model: modelFixture("m", TimelineStateOperational, timelineBucket(TimelineStateOperational, &degradedAvailability)),
			want:  TimelineStateDegraded,
		},
		{
			name:  "可用率正常且桶为 operational 判 operational",
			model: modelFixture("m", TimelineStateFailed, timelineBucket(TimelineStateOperational, &healthyAvailability)),
			want:  TimelineStateOperational,
		},
		{
			name:  "无时间线时回落 latestState",
			model: modelFixture("m", TimelineStateDegraded),
			want:  TimelineStateDegraded,
		},
		{
			name:  "latestState 也缺时判 no_data",
			model: modelFixture("m", ""),
			want:  TimelineStateNoData,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := deriveModelFilterState(testCase.model); got != testCase.want {
				t.Fatalf("want %q got %q", testCase.want, got)
			}
		})
	}
}

func groupFixture(slug string, displayName string, models ...PublicStatusPayloadModel) PublicStatusPayloadGroup {
	copy_ := "explain " + slug
	return PublicStatusPayloadGroup{
		PublicGroupSlug: slug, DisplayName: displayName, ExplanatoryCopy: &copy_, Models: models,
	}
}

func queryWith(include []string, groupSlugs, models, statuses []string, q *string) PublicStatusParsedQuery {
	return PublicStatusParsedQuery{
		IntervalMinutes: 5,
		RangeHours:      24,
		Filters:         PublicStatusQueryFilters{GroupSlugs: groupSlugs, Models: models, Statuses: statuses, Q: q},
		Include:         include,
		Defaults:        defaultQueryDefaults(),
	}
}

func TestFilterGroupsIncludeGate(t *testing.T) {
	groups := []PublicStatusPayloadGroup{groupFixture("a", "A", modelFixture("m1", TimelineStateOperational))}
	if got := FilterPublicStatusGroups(groups, queryWith([]string{"meta"}, nil, nil, nil, nil)); len(got) != 0 {
		t.Fatalf("include 不含 groups 时必须返回空数组：%+v", got)
	}
}

func TestFilterGroupsBySlugModelAndStatus(t *testing.T) {
	groups := []PublicStatusPayloadGroup{
		groupFixture("a", "A", modelFixture("m1", TimelineStateOperational), modelFixture("m2", TimelineStateFailed)),
		groupFixture("b", "B", modelFixture("m1", TimelineStateDegraded)),
	}

	bySlug := FilterPublicStatusGroups(groups, queryWith([]string{"groups"}, []string{"b"}, nil, nil, nil))
	if len(bySlug) != 1 || bySlug[0].PublicGroupSlug != "b" {
		t.Fatalf("分组过滤不符：%+v", bySlug)
	}

	// 模型过滤按 key 或 label 命中。
	byLabel := FilterPublicStatusGroups(groups, queryWith([]string{"groups"}, nil, []string{"m1 label"}, nil, nil))
	if len(byLabel) != 2 {
		t.Fatalf("按 label 过滤应命中两组：%+v", byLabel)
	}

	// 状态过滤走 deriveModelFilterState：只剩 failed 的 m2。
	byStatus := FilterPublicStatusGroups(groups, queryWith([]string{"groups"}, nil, nil, []string{"failed"}, nil))
	if len(byStatus) != 1 || len(byStatus[0].Models) != 1 || byStatus[0].Models[0].PublicModelKey != "m2" {
		t.Fatalf("状态过滤不符：%+v", byStatus)
	}
}

func TestFilterGroupsDropsEmptyGroupsAndTrimsTimeline(t *testing.T) {
	groups := []PublicStatusPayloadGroup{
		groupFixture("a", "A", modelFixture("m1", TimelineStateOperational, timelineBucket(TimelineStateOperational, nil))),
		groupFixture("b", "B"),
	}
	filtered := FilterPublicStatusGroups(groups, queryWith([]string{"groups"}, nil, nil, nil, nil))
	if len(filtered) != 1 || filtered[0].PublicGroupSlug != "a" {
		t.Fatalf("空模型组应被丢弃：%+v", filtered)
	}
	if len(filtered[0].Models[0].Timeline) != 0 {
		t.Fatal("include 不含 timeline 时时间线必须清空")
	}

	withTimeline := FilterPublicStatusGroups(groups, queryWith([]string{"groups", "timeline"}, nil, nil, nil, nil))
	if len(withTimeline[0].Models[0].Timeline) != 1 {
		t.Fatal("include 含 timeline 时应保留时间线")
	}
}

func TestFilterGroupsSearchMatchesGroupThenModel(t *testing.T) {
	groups := []PublicStatusPayloadGroup{
		groupFixture("alpha", "Alpha Group", modelFixture("m1", TimelineStateOperational)),
		groupFixture("beta", "Beta Group", modelFixture("special", TimelineStateOperational)),
	}
	needle := "alpha"
	got := FilterPublicStatusGroups(groups, queryWith([]string{"groups"}, nil, nil, nil, &needle))
	if len(got) != 1 || got[0].PublicGroupSlug != "alpha" {
		t.Fatalf("按组名搜索不符：%+v", got)
	}

	needle = "SPECIAL"
	got = FilterPublicStatusGroups(groups, queryWith([]string{"groups"}, nil, nil, nil, &needle))
	if len(got) != 1 || got[0].Models[0].PublicModelKey != "special" {
		t.Fatalf("按模型名搜索应大小写无关：%+v", got)
	}
}

// --- 路由状态与响应装配 ---

func TestMapRouteStatusCoversEveryState(t *testing.T) {
	generatedAt := fixtureNowISO
	redisUnavailable := "redis-unavailable"
	otherReason := "manifest-missing"
	cases := []struct {
		name          string
		payload       PublicStatusPayload
		rebuildReason *string
		want          PublicStatusRouteStatus
	}{
		{name: "fresh → ready", payload: PublicStatusPayload{RebuildState: ServeStateFresh}, want: RouteStatusReady},
		{name: "stale → stale", payload: PublicStatusPayload{RebuildState: ServeStateStale}, want: RouteStatusStale},
		{name: "no-data → no_data", payload: PublicStatusPayload{RebuildState: ServeStateNoData}, want: RouteStatusNoData},
		{
			name:    "rebuilding 但有旧快照 → stale",
			payload: PublicStatusPayload{RebuildState: ServeStateRebuilding, GeneratedAt: &generatedAt},
			want:    RouteStatusStale,
		},
		{
			name:          "rebuilding 且原因为 redis-unavailable → rebuilding",
			payload:       PublicStatusPayload{RebuildState: ServeStateRebuilding},
			rebuildReason: &redisUnavailable,
			want:          RouteStatusRebuilding,
		},
		{
			name:          "rebuilding 且原因无关 → no_snapshot（200 而不是 503）",
			payload:       PublicStatusPayload{RebuildState: ServeStateRebuilding},
			rebuildReason: &otherReason,
			want:          RouteStatusNoSnapshot,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := mapRouteStatus(testCase.payload, testCase.rebuildReason); got != testCase.want {
				t.Fatalf("want %q got %q", testCase.want, got)
			}
		})
	}
}

func TestBuildRouteResponseHonorsIncludeSwitches(t *testing.T) {
	generatedAt := fixtureNowISO
	payload := PublicStatusPayload{
		RebuildState: ServeStateFresh, SourceGeneration: "gen-1", GeneratedAt: &generatedAt,
		Groups: []PublicStatusPayloadGroup{groupFixture("a", "A", modelFixture("m1", TimelineStateOperational))},
	}
	meta := &PublicStatusRouteMeta{}

	full := BuildPublicStatusRouteResponse(PublicStatusRouteResponseInput{
		Payload:  payload,
		Query:    queryWith([]string{"meta", "defaults", "groups", "timeline"}, nil, nil, nil, nil),
		Defaults: defaultQueryDefaults(), Meta: meta,
	})
	if full.Defaults == nil || full.Meta == nil {
		t.Fatalf("include 全开时 defaults/meta 都应在：%+v", full)
	}
	if full.Status != RouteStatusReady || !full.RebuildState.HasSnapshot {
		t.Fatalf("装配结果不符：%+v", full)
	}
	if full.RebuildState.Reason != nil {
		t.Fatal("rebuildState.reason 恒为 null（Node 只写 null，原因走 Redis 提示）")
	}
	if full.GeneratedAt == nil || *full.GeneratedAt != fixtureNowISO {
		t.Fatalf("generatedAt 应透传：%v", full.GeneratedAt)
	}

	minimal := BuildPublicStatusRouteResponse(PublicStatusRouteResponseInput{
		Payload:  payload,
		Query:    queryWith([]string{"groups"}, nil, nil, nil, nil),
		Defaults: defaultQueryDefaults(), Meta: meta,
	})
	if minimal.Defaults != nil || minimal.Meta != nil {
		t.Fatalf("include 不含 meta/defaults 时两者都应是 null：%+v", minimal)
	}
}

func TestBuildRouteResponseResolvedQueryEchoesParsedValues(t *testing.T) {
	query, validationErr := parseQuery(t, "interval=30&rangeHours=48&groupSlug=a&status=degraded&q=hi", defaultQueryDefaults())
	if validationErr != nil {
		t.Fatalf("不该报错：%+v", validationErr.Issues)
	}
	response := BuildPublicStatusRouteResponse(PublicStatusRouteResponseInput{
		Payload: PublicStatusPayload{RebuildState: ServeStateNoData, Groups: []PublicStatusPayloadGroup{}},
		Query:   query, Defaults: defaultQueryDefaults(),
	})

	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, want := range []string{
		`"intervalMinutes":30`, `"rangeHours":48`, `"groupSlugs":["a"]`, `"statuses":["degraded"]`,
		`"q":"hi"`, `"status":"no_data"`, `"hasSnapshot":false`, `"groups":[]`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("响应缺 %s：%s", want, string(encoded))
		}
	}
	// 字段顺序也是契约（对拍台逐字节比对）：generatedAt 必须排在 freshUntil 之前，status 跟在后面。
	order := []string{`"generatedAt"`, `"freshUntil"`, `"status"`, `"rebuildState"`, `"defaults"`, `"resolvedQuery"`, `"meta"`, `"groups"`}
	previous := -1
	for _, key := range order {
		index := strings.Index(string(encoded), key)
		if index < previous {
			t.Fatalf("响应键序与 Node 不一致：%s 位置 %d 在 %d 之前\n%s", key, index, previous, string(encoded))
		}
		previous = index
	}
}
