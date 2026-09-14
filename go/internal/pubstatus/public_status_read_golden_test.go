package pubstatus

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// 本文件用 **Node 真产物** 做逐字段对拍：golden 由 `bun scripts/public-status-read-golden.ts`
// 跑 Node 的 `read-store.ts` / `rebuild-hints.ts` / `public-api-contract.ts` **原函数**产出
// （testdata/node_public_status_read_golden.json），夹具与下面的 Go 侧逐字同源。
//
// 为什么值得这么做：读路径的价值几乎全在降级分支上，而这些分支**恰恰是最容易「转写走样」**
// 的地方——一处顺序错、一处原因没记，公开页就会在 Redis 抖动时静默变成另一个状态。
// 逐字段对拍能抓住「读得出结果但状态/原因/提示不一样」这类不看输出根本发现不了的走样。
//
// 对照口径：payload / response 全字段深比（键序不参与，值参与）；hints 按**顺序**比；
// 提示键的存在性、TTL（秒）、正文里的 reason、以及读写后 manifest 的 rebuildState 都比。

type readGoldenScenario struct {
	Payload  json.RawMessage `json:"payload"`
	Response json.RawMessage `json:"response"`
	Hints    []struct {
		Reason string `json:"reason"`
	} `json:"hints"`
	HintKeyPresent bool            `json:"hintKeyPresent"`
	HintBody       json.RawMessage `json:"hintBody"`
	HintTTLSeconds *float64        `json:"hintTtlSeconds"`
	ManifestAfter  json.RawMessage `json:"manifestAfter"`
}

type readGolden struct {
	NowISO    string                        `json:"nowIso"`
	Scenarios map[string]readGoldenScenario `json:"scenarios"`
	Queries   []struct {
		RawQuery string          `json:"rawQuery"`
		OK       bool            `json:"ok"`
		Parsed   json.RawMessage `json:"parsed"`
		Issues   []struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"issues"`
	} `json:"queries"`
}

func loadReadGolden(t *testing.T) readGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "node_public_status_read_golden.json"))
	if err != nil {
		t.Fatalf("读读侧 golden 失败（先跑 bun --conditions=react-server scripts/public-status-read-golden.ts）: %v", err)
	}
	var golden readGolden
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("解析 golden 失败: %v", err)
	}
	return golden
}

// normalizeJSON 把任意 JSON 往返成 map/slice，以便「键序无关、值参与」的深比。
func normalizeJSONBytes(t *testing.T, raw []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("解析 JSON 失败: %v（%s）", err, string(raw))
	}
	return value
}

func marshalNormalized(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return normalizeJSONBytes(t, raw)
}

// goldenSnapshotBody 与 scripts/public-status-read-golden.ts 的 snapshotBody 逐字同源：
// 含内部字段（必须被剥）与旧字段名（必须被回落）。
func goldenSnapshotBody() map[string]any {
	return map[string]any{
		"sourceGeneration": fixtureGeneration,
		"generatedAt":      fixtureNowISO,
		"freshUntil":       goldenFreshUntil,
		"groups": []any{
			map[string]any{
				"publicGroupSlug": "group-a",
				"displayName":     "Group A",
				"explanatoryCopy": "copy",
				"sourceGroupId":   float64(42),
				"sourceGroupName": "internal-group-a",
				"secret":          "sk-do-not-leak",
				"models": []any{
					map[string]any{
						"publicModelKey":   "deepseek-v4-flash",
						"label":            "DeepSeek V4 Flash",
						"vendorIconKey":    "deepseek",
						"requestTypeBadge": "openaiCompatible",
						"latestState":      "operational",
						"availabilityPct":  float64(99.5),
						"latestTtfbMs":     float64(120),
						"latestTps":        float64(30.5),
						"providerId":       float64(7),
						"timeline": []any{
							map[string]any{
								"bucketStart":     "2026-09-13T00:00:00.000Z",
								"bucketEnd":       "2026-09-13T00:05:00.000Z",
								"state":           "operational",
								"availabilityPct": float64(100),
								"ttfbMs":          float64(110),
								"tps":             float64(28),
								"sampleCount":     float64(12),
								"errorMessages":   []any{"boom"},
							},
						},
					},
				},
			},
			map[string]any{"displayName": "no slug", "models": []any{}},
			map[string]any{"publicGroupSlug": "group-b", "displayName": "Group B", "models": "not-an-array"},
		},
	}
}

const (
	goldenFreshUntil = "2099-01-01T00:00:00.000Z"
	goldenStaleUntil = "2026-09-13T00:05:00.000Z"
)

// goldenConfigSnapshotBody 与 TS 侧的 configSnapshotBody 逐字同源。
func goldenConfigSnapshotBody() map[string]any {
	return map[string]any{
		"configVersion":          fixtureVersion,
		"generatedAt":            fixtureNowISO,
		"siteTitle":              "  CC Hub  ",
		"siteDescription":        "  desc  ",
		"timeZone":               "Asia/Shanghai",
		"defaultIntervalMinutes": float64(5),
		"defaultRangeHours":      float64(24),
		"groups": []any{
			map[string]any{"slug": "group-a", "displayName": "Group A", "sortOrder": float64(1), "description": nil, "models": []any{}},
		},
	}
}

func seedGoldenConfig(store *fakeStatusStore) {
	rawConfig, _ := json.Marshal(goldenConfigSnapshotBody())
	store.values[BuildConfigVersionPointerKey()] = fixtureVersion
	store.values[BuildConfigSnapshotKey(fixtureVersion)] = string(rawConfig)
	store.values[BuildInternalConfigSnapshotKey(fixtureVersion)] = string(rawConfig)
}

func seedGoldenProjection(store *fakeStatusStore, prefix string, coverageComplete bool, innerVersion string, freshUntil string, lastComplete any) {
	if innerVersion == "" {
		innerVersion = fixtureVersion
	}
	if freshUntil == "" {
		freshUntil = goldenFreshUntil
	}
	manifest := map[string]any{
		"configVersion":          innerVersion,
		"generation":             "gen-0",
		"lastCompleteGeneration": lastComplete,
		"generatedAt":            fixtureNowISO,
		"freshUntil":             freshUntil,
		"rebuildState":           "idle",
		"rollupCoverageComplete": coverageComplete,
	}
	rawManifest, _ := json.Marshal(manifest)
	manifestKey, _ := BuildManifestKey(fixtureVersion, 5, 24, prefix)
	store.values[manifestKey] = string(rawManifest)

	rawSnapshot, _ := json.Marshal(goldenSnapshotBody())
	snapshotKey, _ := BuildCurrentSnapshotKey(5, 24, fixtureGeneration, prefix)
	store.values[snapshotKey] = string(rawSnapshot)
}

// seedGoldenScenario 与 TS 脚本的 `scenarios[].seed` 逐场景同源。
func seedGoldenScenario(store *fakeStatusStore, name string) {
	generation := fixtureGeneration
	switch name {
	case "fresh":
		seedGoldenConfig(store)
		seedGoldenProjection(store, "", true, fixtureVersion, goldenFreshUntil, generation)
	case "stale":
		seedGoldenConfig(store)
		seedGoldenProjection(store, "", true, fixtureVersion, goldenStaleUntil, generation)
	case "rebuilding-no-complete-generation":
		seedGoldenConfig(store)
		manifest := map[string]any{
			"configVersion":          fixtureVersion,
			"lastCompleteGeneration": nil,
			"generatedAt":            fixtureNowISO,
			"freshUntil":             goldenFreshUntil,
			"rebuildState":           "idle",
		}
		raw, _ := json.Marshal(manifest)
		key, _ := BuildManifestKey(fixtureVersion, 5, 24, "")
		store.values[key] = string(raw)
	case "snapshot-missing":
		seedGoldenConfig(store)
		manifest := map[string]any{
			"configVersion":          fixtureVersion,
			"lastCompleteGeneration": generation,
			"generatedAt":            fixtureNowISO,
			"freshUntil":             goldenFreshUntil,
			"rebuildState":           "idle",
			"rollupCoverageComplete": true,
		}
		raw, _ := json.Marshal(manifest)
		key, _ := BuildManifestKey(fixtureVersion, 5, 24, "")
		store.values[key] = string(raw)
	case "manifest-missing":
		seedGoldenConfig(store)
	case "legacy-only":
		seedGoldenConfig(store)
		seedGoldenProjection(store, LegacyPublicStatusRedisPrefix, true, fixtureVersion, goldenFreshUntil, generation)
	case "rollup-coverage-incomplete-with-legacy":
		seedGoldenConfig(store)
		seedGoldenProjection(store, "", false, fixtureVersion, goldenFreshUntil, generation)
		seedGoldenProjection(store, LegacyPublicStatusRedisPrefix, true, fixtureVersion, goldenFreshUntil, generation)
	case "config-version-mismatch":
		seedGoldenConfig(store)
		seedGoldenProjection(store, "", true, "cfg-0", goldenFreshUntil, generation)
	case "redis-unavailable":
		seedGoldenConfig(store)
		seedGoldenProjection(store, "", true, fixtureVersion, goldenFreshUntil, generation)
	case "no-configured-groups":
		// 空库：hasConfiguredGroups=false 时压根不读 Redis。
	}
}

func TestReadPublicStatusGoldenMatchesNodeScenarios(t *testing.T) {
	golden := loadReadGolden(t)

	for name, want := range golden.Scenarios {
		t.Run(name, func(t *testing.T) {
			store := newFakeStatusStore()
			seedGoldenScenario(store, name)
			if name == "redis-unavailable" {
				store.notReady = true
			}

			hasGroups := true
			if name == "no-configured-groups" {
				hasGroups = false
			}
			version := fixtureVersion

			reasons := make([]string, 0)
			payload := ReadPublicStatusPayload(context.Background(), store, ReadPublicStatusPayloadInput{
				IntervalMinutes:     5,
				RangeHours:          24,
				NowISO:              golden.NowISO,
				ConfigVersion:       &version,
				HasConfiguredGroups: &hasGroups,
				TriggerRebuildHint: func(reason string) {
					reasons = append(reasons, reason)
					SchedulePublicStatusRebuild(context.Background(), store, ScheduleRebuildInput{
						IntervalMinutes: 5,
						RangeHours:      24,
						Reason:          reason,
						RequestedAt:     golden.NowISO,
					})
				},
			})

			// 1) payload 逐字段
			if got, wantValue := marshalNormalized(t, payload), normalizeJSONBytes(t, want.Payload); !reflect.DeepEqual(got, wantValue) {
				t.Fatalf("payload 与 Node 不一致\nGo  : %s\nNode: %s", mustJSON(t, got), string(want.Payload))
			}

			// 2) 路由响应逐字段（同一查询：include 全开；meta/defaults 同值）
			values := parseQueryValues(t, "include=meta,defaults,groups,timeline")
			query, validationErr := ParsePublicStatusQuery(values, PublicStatusQueryDefaults{IntervalMinutes: 5, RangeHours: 24})
			if validationErr != nil {
				t.Fatalf("夹具查询不该校验失败：%+v", validationErr.Issues)
			}
			var rebuildReason *string
			if len(reasons) > 0 {
				last := reasons[len(reasons)-1]
				rebuildReason = &last
			}
			siteTitle := "CC Hub"
			siteDescription := "desc"
			timeZone := "Asia/Shanghai"
			response := BuildPublicStatusRouteResponse(PublicStatusRouteResponseInput{
				Payload:       payload,
				Query:         query,
				Defaults:      PublicStatusQueryDefaults{IntervalMinutes: 5, RangeHours: 24},
				Meta:          &PublicStatusRouteMeta{SiteTitle: &siteTitle, SiteDescription: &siteDescription, TimeZone: &timeZone},
				RebuildReason: rebuildReason,
			})
			if got, wantValue := marshalNormalized(t, response), normalizeJSONBytes(t, want.Response); !reflect.DeepEqual(got, wantValue) {
				t.Fatalf("路由响应与 Node 不一致\nGo  : %s\nNode: %s", mustJSON(t, got), string(want.Response))
			}

			// 3) 重建提示：顺序与内容
			wantReasons := make([]string, 0, len(want.Hints))
			for _, hint := range want.Hints {
				wantReasons = append(wantReasons, hint.Reason)
			}
			if !reflect.DeepEqual(reasons, wantReasons) {
				t.Fatalf("重建提示与 Node 不一致：Go %v vs Node %v", reasons, wantReasons)
			}

			// 4) 提示键：存在性 + TTL（秒）+ 正文 reason
			hintKey, _ := BuildRebuildHintKey(5, 24, "")
			_, present := store.values[hintKey]
			if present != want.HintKeyPresent {
				t.Fatalf("提示键存在性与 Node 不一致：Go %v vs Node %v（key=%s）", present, want.HintKeyPresent, hintKey)
			}
			if want.HintKeyPresent {
				ttl := store.ttls[hintKey]
				if want.HintTTLSeconds == nil || ttl.Seconds() != *want.HintTTLSeconds {
					t.Fatalf("提示键 TTL 与 Node 不一致：Go %v vs Node %v", ttl.Seconds(), want.HintTTLSeconds)
				}
				var hintBody struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal([]byte(store.values[hintKey]), &hintBody); err != nil {
					t.Fatalf("提示正文不是 JSON: %v", err)
				}
				var wantBody struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(want.HintBody, &wantBody); err != nil {
					t.Fatalf("golden 提示正文不是 JSON: %v", err)
				}
				if hintBody.Reason != wantBody.Reason {
					t.Fatalf("提示正文 reason 不一致：Go %q vs Node %q", hintBody.Reason, wantBody.Reason)
				}
			}

			// 5) 读写后 manifest 的 rebuildState（提示路径会把它改成 rebuilding）。
			// golden 里的 `null` 意为「**该键不存在**」（Node 查的是默认前缀 v2 下的 manifest），
			// 不是「存在但为空」——legacy-only / manifest-missing / no-configured-groups 三种场景均如此。
			manifestKey, _ := BuildManifestKey(fixtureVersion, 5, 24, "")
			gotManifestRaw := store.values[manifestKey]
			var wantManifest map[string]any
			if len(want.ManifestAfter) != 0 {
				if err := json.Unmarshal(want.ManifestAfter, &wantManifest); err != nil {
					t.Fatalf("golden manifest 不是 JSON: %v", err)
				}
			}
			if wantManifest == nil {
				if gotManifestRaw != "" {
					t.Fatalf("Node 侧 v2 manifest 为 null 而 Go 侧写了：%s", gotManifestRaw)
				}
				return
			}
			if gotManifestRaw == "" {
				t.Fatalf("Go 侧 manifest 缺失，Node 侧为 %s", string(want.ManifestAfter))
			}
			var gotManifest struct {
				RebuildState string `json:"rebuildState"`
			}
			if err := json.Unmarshal([]byte(gotManifestRaw), &gotManifest); err != nil {
				t.Fatalf("Go 侧 manifest 不是 JSON: %v", err)
			}
			if gotManifest.RebuildState != wantManifest["rebuildState"] {
				t.Fatalf("manifest.rebuildState 不一致：Go %q vs Node %v", gotManifest.RebuildState, wantManifest["rebuildState"])
			}
		})
	}
}

func TestParsePublicStatusQueryGoldenMatchesNode(t *testing.T) {
	golden := loadReadGolden(t)
	if len(golden.Queries) == 0 {
		t.Fatal("golden 没有查询用例")
	}

	for _, testCase := range golden.Queries {
		t.Run(testCase.RawQuery, func(t *testing.T) {
			values := parseQueryValues(t, testCase.RawQuery)
			query, validationErr := ParsePublicStatusQuery(values, PublicStatusQueryDefaults{IntervalMinutes: 15, RangeHours: 72})

			if testCase.OK {
				if validationErr != nil {
					t.Fatalf("Node 判合法而 Go 报错：%+v", validationErr.Issues)
				}
				if got, want := marshalNormalized(t, parsedQueryToNodeShape(query)), normalizeJSONBytes(t, testCase.Parsed); !reflect.DeepEqual(got, want) {
					t.Fatalf("解析结果与 Node 不一致\nGo  : %s\nNode: %s", mustJSON(t, got), string(testCase.Parsed))
				}
				return
			}

			if validationErr == nil {
				t.Fatalf("Node 判非法而 Go 通过：%s", testCase.RawQuery)
			}
			if len(validationErr.Issues) != len(testCase.Issues) {
				t.Fatalf("issue 条数不一致：Go %d vs Node %d\nGo  : %+v\nNode: %+v",
					len(validationErr.Issues), len(testCase.Issues), validationErr.Issues, testCase.Issues)
			}
			for index, wantIssue := range testCase.Issues {
				gotIssue := validationErr.Issues[index]
				if gotIssue.Field != wantIssue.Field || gotIssue.Code != wantIssue.Code || gotIssue.Message != wantIssue.Message {
					t.Fatalf("第 %d 条 issue 不一致：Go %+v vs Node %+v", index, gotIssue, wantIssue)
				}
			}
		})
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(raw)
}

// parseQueryValues 把查询串转成 url.Values。
//
// 用 `url.ParseQuery` 而不是手写切分：它对百分号与 `+`（空格）的处置与 Node 的
// `URLSearchParams` 一致，而手写一份就等于引入第二套解码语义（夹具的 `%20` / `%01`
// 全靠它才能与 Node 对上）。
func parseQueryValues(t *testing.T, rawQuery string) map[string][]string {
	t.Helper()
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("解析查询串 %q 失败: %v", rawQuery, err)
	}
	return values
}

// parsedQueryToNodeShape 把 Go 的解析结果映射成 Node 的字段名，供 golden 深比。
//
// 为什么在测试里映射而不是给生产类型加 JSON 标签：`PublicStatusParsedQuery` 是进程内类型，
// 线上从不序列化（响应里的 `resolvedQuery` 另有带标签的类型）。为测试方便给它加标签，
// 会让人误以为它是一份对外契约。
func parsedQueryToNodeShape(query PublicStatusParsedQuery) map[string]any {
	return map[string]any{
		"intervalMinutes": query.IntervalMinutes,
		"rangeHours":      query.RangeHours,
		"filters": map[string]any{
			"groupSlugs": nonNilStrings(query.Filters.GroupSlugs),
			"models":     nonNilStrings(query.Filters.Models),
			"statuses":   nonNilStrings(query.Filters.Statuses),
			"q":          query.Filters.Q,
		},
		"include": nonNilStrings(query.Include),
		"defaults": map[string]any{
			"intervalMinutes": query.Defaults.IntervalMinutes,
			"rangeHours":      query.Defaults.RangeHours,
		},
		"resolvedQuery": map[string]any{
			"intervalMinutes": query.ResolvedQuery.IntervalMinutes,
			"rangeHours":      query.ResolvedQuery.RangeHours,
			"groupSlugs":      nonNilStrings(query.ResolvedQuery.GroupSlugs),
			"models":          nonNilStrings(query.ResolvedQuery.Models),
			"statuses":        nonNilStrings(query.ResolvedQuery.Statuses),
			"q":               query.ResolvedQuery.Q,
			"include":         nonNilStrings(query.ResolvedQuery.Include),
		},
	}
}

// nonNilStrings 保证 JSON 里是数组而不是 null（Node 恒为数组）。
func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
