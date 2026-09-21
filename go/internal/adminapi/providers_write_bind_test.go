package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**跨层**用例：adminapi 的解码产物要能直接喂给 store 的列绑定。
//
// 为什么必须有这一层：两侧各有一套「字段 → 类型」的知识（解码 spec 决定产出什么 Go 值，
// store 的 kind 表决定接受什么 Go 值），任何一侧漂移都不会被各自的单测发现——2026-09-15 的
// 生产事故正是如此：providerNullableEnumFieldSpec / providerNullablePreferenceSpec 返回裸
// `string`，而可空文本列的 kind 只认 `*string`，于是「单编辑表单保存任何供应商」全部 400，
// 而两侧单测全绿（各自都自洽）。这条用例把两套知识对起来。

// providerBindProbeDSN 是 RFC 5737 文档段地址，探针**绝不连库**，只为让池构造通过。
const providerBindProbeDSN = "postgres://probe:probe@192.0.2.10:5432/probe"

// providerBindProbe 把字段袋喂给存储层绑定，返回其错误（不连库）。
//
// 可行的理由：store.AdminPatchProvider 先 adminProviderBind（纯 Go 的类型检查）再取池，
// 故用「已关闭的池」当探针——绑定通过时得到池生命周期错误，绑定失败时得到「值类型不符」。
// 这样本用例在无 CCH_TEST_DSN 的 CI 上也能守住这条缝。
func providerBindProbe(t *testing.T, fields map[string]any) error {
	t.Helper()
	pools, err := store.Open(context.Background(), store.Options{DSN: providerBindProbeDSN})
	if err != nil {
		t.Fatalf("构造探针池失败: %v", err)
	}
	if err := pools.Close(); err != nil {
		t.Fatalf("关闭探针池失败: %v", err)
	}
	_, err = pools.AdminPatchProvider(context.Background(), 1, fields)
	return err
}

// providerBindSwallowedByClosedPool 报告绑定是否**通过**（探针只撞到池的生命周期检查）。
func providerBindSwallowedByClosedPool(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Database pools are closed")
}

// providerWriteSamplePayload 是「单编辑表单保存」形状的载荷：字段全集 + 每字段一个合法值。
//
// 值取 UI 真实会发出的形态（偏好列一律 "inherit"、多选列空数组、可空列给真实值而不是 null，
// 以保证非空分支被走到——事故恰恰出在非空分支）。
var providerWriteSamplePayload = map[string]string{
	"name":            `"供应商甲"`,
	"url":             `"https://api.example.com"`,
	"key":             `"sk-placeholder"`,
	"description":     `"说明"`,
	"provider_type":   `"openai-compatible"`,
	"is_enabled":      `true`,
	"weight":          `1`,
	"priority":        `3`,
	"cost_multiplier": `1`,

	"group_tag":             `"chat,codex"`,
	"group_priorities":      `{"chat":1}`,
	"preserve_client_ip":    `true`,
	"disable_session_reuse": `false`,
	"model_redirects":       `[{"source":"deepseek-v4.1-flash","target":"deepseek-v4-flash","matchType":"exact"}]`,
	"allowed_models":        `[{"matchType":"exact","pattern":"deepseek-v4-flash"}]`,
	"allowed_clients":       `["claude_code"]`,
	"blocked_clients":       `["cursor"]`,

	"active_time_start":           `"00:00"`,
	"active_time_end":             `"23:59"`,
	"mcp_passthrough_type":        `"none"`,
	"mcp_passthrough_url":         `"https://mcp.example.com"`,
	"protocol_conversion_enabled": `true`,

	"limit_5h_usd":              `1`,
	"limit_5h_reset_mode":       `"rolling"`,
	"limit_daily_usd":           `1`,
	"daily_reset_mode":          `"fixed"`,
	"daily_reset_time":          `"00:00"`,
	"limit_weekly_usd":          `1`,
	"limit_monthly_usd":         `1`,
	"limit_total_usd":           `1`,
	"limit_concurrent_sessions": `1`,
	"max_retry_attempts":        `3`,

	"circuit_breaker_failure_threshold":           `5`,
	"circuit_breaker_open_duration":               `60`,
	"circuit_breaker_half_open_success_threshold": `2`,
	"circuit_breaker_release_increment":           `1`,
	"circuit_breaker_max_open_count":              `10`,

	"proxy_url":                        `"http://proxy.example.com:3128"`,
	"proxy_fallback_to_direct":         `true`,
	"custom_headers":                   `{"X-Tenant":"placeholder"}`,
	"first_byte_timeout_streaming_ms":  `1000`,
	"streaming_idle_timeout_ms":        `1000`,
	"request_timeout_non_streaming_ms": `1000`,
	"website_url":                      `"https://example.com"`,
	"favicon_url":                      `"https://example.com/favicon.ico"`,

	"cache_ttl_preference":                 `"inherit"`,
	"swap_cache_ttl_billing":               `false`,
	"context_1m_preference":                `"inherit"`,
	"codex_reasoning_effort_preference":    `"inherit"`,
	"codex_reasoning_summary_preference":   `"inherit"`,
	"codex_text_verbosity_preference":      `"inherit"`,
	"codex_parallel_tool_calls_preference": `"inherit"`,
	"codex_image_generation_preference":    `"inherit"`,
	"codex_service_tier_preference":        `"inherit"`,
	"codex_max_tokens_preference":          `"inherit"`,
	"anthropic_max_tokens_preference":      `"inherit"`,
	"anthropic_thinking_budget_preference": `"inherit"`,
	"anthropic_adaptive_thinking":          `{"effort":"high","modelMatchMode":"all","models":["claude-opus-5"]}`,
	"openai_max_tokens_preference":         `"inherit"`,
	"gemini_google_search_preference":      `"inherit"`,

	"tpm": `100`,
	"rpm": `100`,
	"rpd": `100`,
	"cc":  `100`,

	// 低速降级：开关给 true（非空分支），各参数给真实值而不是 null（可空列的 null 分支
	// 另有覆盖，这里走非空分支——事故恰恰出在非空分支）。判定窗与基线窗是两列。
	"slow_rate_monitor_enabled":         `true`,
	"slow_rate_window_seconds":          `1800`,
	"slow_rate_baseline_window_seconds": `259200`,
	"slow_rate_min_samples":             `100`,
	"slow_rate_trigger_count":           `3`,
	"slow_rate_ratio_per_mille":         `300`,
	"slow_rate_penalty_step":            `10`,
	"slow_rate_penalty_max":             `30`,
	// 首字后停滞探测阈值列（0130），同样走非空分支。
	"slow_rate_probe_after_first_byte_seconds": `30`,
}

// TestProviderCreateFieldsBindToStoreTypes 覆盖创建入口：同一份表里除了 description
// （仅更新路径有）之外的字段，走创建字段表解码后也必须能绑定。
func TestProviderCreateFieldsBindToStoreTypes(t *testing.T) {
	specs := providerCreateWriteSpecs()
	names := make([]string, 0, len(specs))
	payload := make(map[string]string, len(specs))
	for name := range specs {
		sample, ok := providerWriteSamplePayload[name]
		if !ok {
			t.Fatalf("创建字段 %q 没有样值可用", name)
		}
		names = append(names, name)
		payload[name] = sample
	}
	decoded, issues := providerDecodeWriteFields(providerSampleObject(t, payload), names, specs)
	if len(issues) > 0 {
		t.Fatalf("创建样例应当解码通过: %+v", issues)
	}
	if err := providerBindProbe(t, decoded); !providerBindSwallowedByClosedPool(err) {
		t.Errorf("创建路径的字段袋无法被存储层绑定: %v", err)
	}
}

// TestProviderNullableTextSpecsReturnPointers 是一条窄钉子：事故那两个 spec 的产物必须是
// `*string`。它们曾被写成裸 string，而各自单测只看「值对不对」，于是漂移三周无人发现。
func TestProviderNullableTextSpecsReturnPointers(t *testing.T) {
	cases := map[string]providerDecodeSpec{
		"providerNullableEnumFieldSpec":  providerNullableEnumFieldSpec(providerCacheTTLPreferences),
		"providerNullablePreferenceSpec": providerNullablePreferenceSpec(validateMaxTokensPreference, "inherit or a positive integer string"),
		"providerNullableFieldSpec":      providerNullableFieldSpec(0),
	}
	samples := map[string]string{
		"providerNullableEnumFieldSpec":  `"inherit"`,
		"providerNullablePreferenceSpec": `"inherit"`,
		"providerNullableFieldSpec":      `"x"`,
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			object := providerSampleObject(t, map[string]string{"field": samples[name]})
			value, present := spec.Decode(object, "field")
			if !present {
				t.Fatal("样值应当被解码")
			}
			text, ok := value.(*string)
			if !ok {
				t.Fatalf("可空文本列必须产出 *string（store 的绑定形状），实际 %T", value)
			}
			if text == nil {
				t.Fatal("非 null 样值不应产出 nil 指针")
			}
		})
	}
}

// TestProviderWriteSamplePayloadCoversSpecTable 保证上表与字段表同步：新增字段而没给样值
// 会让下面那条贯通用例静默漏测，故这里先红。
func TestProviderWriteSamplePayloadCoversSpecTable(t *testing.T) {
	specs := providerUpdateWriteSpecs()
	for name := range specs {
		if _, ok := providerWriteSamplePayload[name]; !ok {
			t.Errorf("字段 %q 在 providerUpdateWriteSpecs 里，但 providerWriteSamplePayload 没有样值", name)
		}
	}
	for name := range providerWriteSamplePayload {
		if _, ok := specs[name]; !ok {
			t.Errorf("样值表里的 %q 不在 providerUpdateWriteSpecs 里（字段已删或改名？）", name)
		}
	}
}

// TestProviderUpdateFieldsBindToStoreTypes 是本次事故的贯通回归：
// 单编辑表单形状的载荷经解码后，每个字段都必须能被存储层绑定接受。
func TestProviderUpdateFieldsBindToStoreTypes(t *testing.T) {
	object := providerSampleObject(t, providerWriteSamplePayload)
	fields, issues := providerDecodeWriteFields(object, providerSortedKeys(providerWriteSamplePayload),
		providerUpdateWriteSpecs())
	if len(issues) > 0 {
		t.Fatalf("样例载荷应当解码通过，却有 %d 条校验问题: %+v", len(issues), issues)
	}

	if err := providerBindProbe(t, fields); !providerBindSwallowedByClosedPool(err) {
		t.Errorf("整份样例载荷绑定失败（绑定应当通过、只撞到关闭的池）: %v", err)
	}

	// 再逐字段定位：哪一条漂移都能指名报出，而不是只看到「整份失败」。
	for _, name := range providerSortedKeys(providerWriteSamplePayload) {
		t.Run(name, func(t *testing.T) {
			single := providerSampleObject(t, map[string]string{name: providerWriteSamplePayload[name]})
			decoded, issues := providerDecodeWriteFields(single, []string{name}, providerUpdateWriteSpecs())
			if len(issues) > 0 {
				t.Fatalf("字段 %q 的样值应当解码通过: %+v", name, issues)
			}
			if err := providerBindProbe(t, decoded); !providerBindSwallowedByClosedPool(err) {
				t.Errorf("字段 %q 解码后的值无法被存储层绑定（解码层与 kind 表漂移）: %v", name, err)
			}
		})
	}
}

// TestProviderBindProbeRejectsWrongType 是上面那条用例的**空跑守卫**：
// 若探针本身失去了判别力（例如 store 改成宽容任何类型），这里必须红。
func TestProviderBindProbeRejectsWrongType(t *testing.T) {
	err := providerBindProbe(t, map[string]any{"cache_ttl_preference": int64(5)})
	if err == nil || !strings.Contains(err.Error(), "值类型不符") {
		t.Fatalf("探针应当拒绝错误类型（期望「值类型不符」），实际: %v", err)
	}
}

// TestProviderBatchPatchValuesBindToStoreTypes 覆盖第二条生产者：批量 patch 的 {set} 与 {clear}。
//
// clear 尤其要靠这条守住：它取的是契约表里写死的常量（"inherit"、空数组），
// 常量与列类型漂移同样会让整批操作 400。
func TestProviderBatchPatchValuesBindToStoreTypes(t *testing.T) {
	for _, spec := range providerPatchFieldSpecs {
		sample, ok := providerWriteSamplePayload[spec.Field]
		if !ok {
			t.Errorf("批量 patch 字段 %q 没有样值可用（请在 providerWriteSamplePayload 补上）", spec.Field)
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(sample), &value); err != nil {
			t.Fatalf("样值 %q 不是合法 JSON: %v", sample, err)
		}

		drafts := map[string]map[string]any{
			"set": {spec.Field: map[string]any{"set": value}},
		}
		if spec.Clearable {
			drafts["clear"] = map[string]any{spec.Field: map[string]any{"clear": true}}
		}
		for mode, draft := range drafts {
			t.Run(spec.Field+"/"+mode, func(t *testing.T) {
				patch, contractErr := normalizeProviderBatchPatchDraft(draft)
				if contractErr != nil {
					t.Fatalf("草稿应当通过契约校验: %s", contractErr.Message)
				}
				updates := buildProviderBatchApplyUpdates(patch)
				if len(updates) != 1 {
					t.Fatalf("期望产出一条更新，实际 %d 条: %+v", len(updates), updates)
				}
				if err := providerBindProbe(t, map[string]any(updates)); !providerBindSwallowedByClosedPool(err) {
					t.Errorf("批量 patch 的 %s 值无法被存储层绑定（契约常量与列类型漂移）: %v", mode, err)
				}
			})
		}
	}
}

// TestProviderPreimageFieldsBindToStoreTypes 覆盖第三条生产者：撤销回写用的前像。
//
// 前像走的是另一套 kind 名（providerWriteKindOf），与解码 spec 是**第三份**同类知识，
// 因此同样需要与 store 的绑定对一次。
//
// 遍历 providerPreimageFieldNames（快照实际会装进去的字段）而不是样值表：key 故意不入快照
// （凭据不进撤销前像），description 走的是反向查表的单例分支，故两者不在该表里。
func TestProviderPreimageFieldsBindToStoreTypes(t *testing.T) {
	for payload, camel := range providerPreimageFieldNames {
		sample, ok := providerWriteSamplePayload[payload]
		if !ok {
			t.Errorf("前像字段 %q 没有样值可用（请在 providerWriteSamplePayload 补上）", payload)
			continue
		}
		t.Run(payload, func(t *testing.T) { providerAssertPreimageBindable(t, camel, sample) })
	}
	t.Run("description", func(t *testing.T) {
		providerAssertPreimageBindable(t, "description", providerWriteSamplePayload["description"])
	})
}

func providerAssertPreimageBindable(t *testing.T, camel, sample string) {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(sample), &value); err != nil {
		t.Fatalf("样值不是合法 JSON: %v", err)
	}
	payload, issues := providerPreimageToWriteFields(map[string]any{camel: value})
	if len(issues) > 0 {
		t.Fatalf("前像值应当可写回: %+v", issues)
	}
	if len(payload) != 1 {
		t.Fatalf("期望转出一个字段，实际 %d 个", len(payload))
	}
	if err := providerBindProbe(t, payload); !providerBindSwallowedByClosedPool(err) {
		t.Errorf("前像转出的字段袋无法被存储层绑定: %v", err)
	}
}

// providerSampleObject 把「payload 名 → 原始 JSON」的表变成解码器要的 adminObject。
func providerSampleObject(t *testing.T, payload map[string]string) *adminObject {
	t.Helper()
	names := providerSortedKeys(payload)
	fields := make(map[string]json.RawMessage, len(payload))
	for _, name := range names {
		fields[name] = json.RawMessage(payload[name])
	}
	return adminNewObject(fields, names...)
}

// providerSampleBody 把样值表拼成请求体（与 UI 提交的单编辑表单同形）。
func providerSampleBody(t *testing.T, payload map[string]string) string {
	t.Helper()
	names := providerSortedKeys(payload)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		encoded, err := json.Marshal(name)
		if err != nil {
			t.Fatalf("字段名序列化失败: %v", err)
		}
		parts = append(parts, string(encoded)+":"+payload[name])
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// TestIntegrationProviderSingleEditPayloadPersists 是真库端到端复验：
// 单编辑表单形状的全量载荷经 PATCH 处理器必须 200，且**每个字段都真的落到列上**。
//
// 为何不能只靠上面那条无库用例：那条证的是「绑定不报类型错」，这条证的是
// 「真 PG 接受这些值、列里读回来就是发出去的值」（NOT NULL / 列类型 / jsonb 形状都在这里才碰得到）。
//
// 需真依赖：CCH_TEST_DSN（PG）与 CCH_TEST_REDIS_URL（撕销快照）；未设置即跳过。
func TestIntegrationProviderSingleEditPayloadPersists(t *testing.T) {
	pools := testPools(t)
	router := providerWriteRouter(t, pools, &Deps{
		ProviderUndoKV: NewRedisProviderUndoKV(providerWriteRedis(t)),
	})
	prefix := fmt.Sprintf("go-pvb-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, err := storeOpenForCleanup(t)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		writer, err := cleanup.Writer()
		if err != nil {
			return
		}
		_, _ = writer.Exec(context.Background(), `DELETE FROM providers WHERE name LIKE $1`, prefix+"%")
	})

	// 创建：拿一个真行（名称带夹具前缀以便清理；键值用样值表里的占位凭据）。
	payload := make(map[string]string, len(providerWriteSamplePayload))
	for name, value := range providerWriteSamplePayload {
		payload[name] = value
	}
	payload["name"] = fmt.Sprintf(`"%s 主线路"`, prefix)
	payload["url"] = fmt.Sprintf(`"https://%s.example.com"`, prefix)
	payload["key"] = fmt.Sprintf(`"sk-it-%s"`, prefix)
	delete(payload, "description")

	recorder := providerRequest(t, router, http.MethodPost, "/api/v1/providers",
		providerSampleBody(t, payload), "application/json", false)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	id := int64(created["id"].(float64))

	// 单编辑保存：同一份全量载荷经 PATCH。
	recorder = providerRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", id), providerSampleBody(t, payload), "application/json", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("单编辑保存应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}

	// 直读列：每一列都必须拿回发出去的值（尤其是事故里那批可空文本列）。
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var (
		anthropicMaxTokens, thinkingBudget, cacheTTL, codexEffort, codexMaxTokens *string
		limitConcurrent                                                           *int64
		allowedClients                                                            []byte
	)
	if err := pool.QueryRow(context.Background(), `
		SELECT anthropic_max_tokens_preference, anthropic_thinking_budget_preference,
		       cache_ttl_preference, codex_reasoning_effort_preference, codex_max_tokens_preference,
		       limit_concurrent_sessions, allowed_clients
		FROM providers WHERE id = $1`, id,
	).Scan(&anthropicMaxTokens, &thinkingBudget, &cacheTTL, &codexEffort, &codexMaxTokens,
		&limitConcurrent, &allowedClients); err != nil {
		t.Fatalf("读回供应商行失败: %v", err)
	}
	for _, column := range []struct {
		name  string
		value *string
	}{
		{"anthropic_max_tokens_preference", anthropicMaxTokens},
		{"anthropic_thinking_budget_preference", thinkingBudget},
		{"cache_ttl_preference", cacheTTL},
		{"codex_reasoning_effort_preference", codexEffort},
		{"codex_max_tokens_preference", codexMaxTokens},
	} {
		if column.value == nil || *column.value != "inherit" {
			t.Errorf("列 %s 应为 inherit，实得 %v", column.name, column.value)
		}
	}
	if limitConcurrent == nil || *limitConcurrent != 100 {
		t.Errorf("列 limit_concurrent_sessions 应为 100，实得 %v", limitConcurrent)
	}
	var clients []string
	if err := json.Unmarshal(allowedClients, &clients); err != nil || len(clients) != 1 || clients[0] != "claude_code" {
		t.Errorf("列 allowed_clients（jsonb）应为 [claude_code]，实得 %s（%v）", allowedClients, err)
	}
}
