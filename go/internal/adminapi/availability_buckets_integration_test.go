package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 本文件是 GET /api/availability（按时间桶聚合）的端到端用例：真路由表 + 真库。
//
// 验的是三件只有真库真路由才看得出来、且最容易写错的事：
//  1. **桶边界**：date_bin 的 5 分钟桶是按纪元对齐的，桶宽换档不能改边界；
//  2. **maxBuckets 截断**：只留最新 N 个非空桶，且**汇总字段只反映截断后的子窗口**
//     （不是整段查询窗）——当前状态更是只看返回桶的最后 3 个；
//  3. **聚合校验**：范围/桶预算的 400 分支（与参数格式校验的 400 是两段不同的判定）。
//
// 共享库纪律同其它集成用例：自钉唯一供应商、按精确 id 清场。

const availabilityBucketsBase = "2026-01-02T10:00:00Z"

func TestAvailabilityBucketedIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	seed := seedAvailability(t, pools)
	router := availabilityRouter(t, pools)

	ctx := context.Background()
	pool := seed.pool
	base, err := time.Parse(time.RFC3339, availabilityBucketsBase)
	if err != nil {
		t.Fatalf("基准时刻不合法: %v", err)
	}

	// 桶夹具：四行 1 分钟桶，横跨四个 5 分钟展示桶（10:00 / 10:05 / 10:10 / 10:15）。
	// 第 2 行故意落在 10:06——它必须与 10:05 合进同一个展示桶，这是被聚合的证据。
	rows := []struct {
		offsetMinutes int
		success       int
		failure       int
		latencyCount  int
		latencySumMS  int
	}{
		{0, 3, 1, 3, 300},
		{6, 2, 0, 2, 400},
		{11, 0, 4, 0, 0},
		{16, 0, 6, 0, 0},
	}
	for _, row := range rows {
		bucketStart := base.Add(time.Duration(row.offsetMinutes) * time.Minute)
		if _, err := pool.Exec(ctx,
			`INSERT INTO avail_bucket_1m
			   (provider_id, bucket_start, success_cnt, failure_cnt, excluded_cnt,
			    latency_cnt, latency_sum_ms, last_request_at)
			 VALUES ($1, $2, $3, $4, 0, $5, $6, $2)
			 ON CONFLICT (provider_id, bucket_start) DO UPDATE SET
			   success_cnt = EXCLUDED.success_cnt,
			   failure_cnt = EXCLUDED.failure_cnt,
			   latency_cnt = EXCLUDED.latency_cnt,
			   latency_sum_ms = EXCLUDED.latency_sum_ms,
			   last_request_at = EXCLUDED.last_request_at`,
			seed.providerID, bucketStart, row.success, row.failure,
			row.latencyCount, row.latencySumMS); err != nil {
			t.Fatalf("写 1 分钟投影桶夹具失败: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM avail_bucket_1m WHERE provider_id = $1`, seed.providerID)
	})

	end := base.Add(20 * time.Minute)
	query := url.Values{}
	query.Set("startTime", base.Format(time.RFC3339))
	query.Set("endTime", end.Format(time.RFC3339))
	query.Set("providerIds", fmt.Sprintf("%d", seed.providerID))
	query.Set("bucketSizeMinutes", "5")

	status, body := call(router, http.MethodGet, "/api/availability?"+query.Encode(), "")
	if status != http.StatusOK {
		t.Fatalf("分桶聚合应 200，实得 %d：%s", status, body)
	}
	payload := decodeObject(t, body)
	if payload["bucketSizeMinutes"].(float64) != 5 {
		t.Fatalf("回显桶宽应为 5：%s", body)
	}
	if payload["startTime"] != "2026-01-02T10:00:00.000Z" || payload["endTime"] != "2026-01-02T10:20:00.000Z" {
		t.Fatalf("回显窗口应为请求窗：%s", body)
	}
	providers, _ := payload["providers"].([]any)
	if len(providers) != 1 {
		t.Fatalf("应只有一个夹具供应商：%s", body)
	}
	provider, _ := providers[0].(map[string]any)
	if provider["providerId"].(float64) != float64(seed.providerID) ||
		provider["providerName"] != seed.prefix+"-provider" ||
		provider["providerType"] != "claude" || provider["isEnabled"] != true {
		t.Fatalf("供应商回显字段不符：%s", body)
	}

	// 汇总口径：桶只留非空的（10:00/10:05/10:10/10:15 四桶），green 5 / red 11。
	if provider["totalRequests"].(float64) != 16 ||
		provider["currentAvailability"].(float64) != 5.0/16.0 ||
		provider["successRate"].(float64) != 5.0/16.0 {
		t.Fatalf("汇总计数/可用率不符（应 16 与 5/16）：%s", body)
	}
	// 均值延迟按桶内 latency_cnt 加权：(300 + 400) / (3 + 2) = 140。
	if provider["avgLatencyMs"].(float64) != 140 {
		t.Fatalf("均值延迟应为 140：%s", body)
	}
	if provider["lastRequestAt"] != "2026-01-02T10:16:00.000Z" {
		t.Fatalf("最后请求时刻应为最后一桶的起点：%s", body)
	}
	// 当前状态只看最后 3 桶（10:05 g2 / 10:10 r4 / 10:15 r6）= 2/12 < 0.5 → red。
	if provider["currentStatus"] != "red" {
		t.Fatalf("最后 3 桶应判红：%s", body)
	}

	buckets, _ := provider["timeBuckets"].([]any)
	if len(buckets) != 4 {
		t.Fatalf("应返回 4 个非空展示桶：%s", body)
	}
	first, _ := buckets[0].(map[string]any)
	if first["bucketStart"] != "2026-01-02T10:00:00.000Z" ||
		first["bucketEnd"] != "2026-01-02T10:05:00.000Z" {
		t.Fatalf("首桶边界不符：%v", first)
	}
	if first["greenCount"].(float64) != 3 || first["redCount"].(float64) != 1 ||
		first["totalRequests"].(float64) != 4 || first["availabilityScore"].(float64) != 0.75 {
		t.Fatalf("首桶计数不符：%v", first)
	}
	// p50/p95/p99 当前就是均值近似（1 分钟桶只有 sum/count），三者必须与 avg 同值。
	if first["avgLatencyMs"].(float64) != 100 || first["p50LatencyMs"].(float64) != 100 ||
		first["p95LatencyMs"].(float64) != 100 || first["p99LatencyMs"].(float64) != 100 {
		t.Fatalf("首桶延迟字段不符（三条分位应与均值同值）：%v", first)
	}
	second, _ := buckets[1].(map[string]any)
	if second["bucketStart"] != "2026-01-02T10:05:00.000Z" ||
		second["greenCount"].(float64) != 2 {
		t.Fatalf("10:06 的桶应与 10:05 合并（证明按桶宽聚合）：%v", second)
	}
	// 只有一个供应商时，全站可用性就等于它的可用率。
	if payload["systemAvailability"].(float64) != 5.0/16.0 {
		t.Fatalf("全站可用性应为该供应商的加权值：%s", body)
	}

	// maxBuckets 截断：只留最新的非空桶。
	//
	// 这一条只能直测 store：HTTP 面上显式桶宽会先过「窗口 <= 桶宽 × maxBuckets」的预算校验，
	// 该校验恰好使「桶数 > maxBuckets」不可达（桶数上界就是 maxBuckets）。截断本身仍是
	// 必须对的口径（auto 桶宽 + 非整倍窗口 下会真发生），故在 store 层钉死。
	storeRows, err := pools.AdminListAvailabilityBuckets(
		ctx, []int64{seed.providerID}, 5, base, base.Add(20*time.Minute), 2)
	if err != nil {
		t.Fatalf("store 分桶查询失败: %v", err)
	}
	if len(storeRows) != 2 {
		t.Fatalf("maxBuckets=2 应只留两个桶，实得 %d", len(storeRows))
	}
	if !storeRows[0].BucketStart.Equal(base.Add(10*time.Minute)) ||
		!storeRows[1].BucketStart.Equal(base.Add(15*time.Minute)) {
		t.Fatalf("截断应保留**最新**的桶并按起点升序：%v / %v",
			storeRows[0].BucketStart, storeRows[1].BucketStart)
	}
	if storeRows[0].GreenCount != 0 || storeRows[0].RedCount != 4 ||
		storeRows[1].GreenCount != 0 || storeRows[1].RedCount != 6 {
		t.Fatalf("截断后的桶计数不符：%+v", storeRows)
	}

	// 供应商过滤 + 停用供应商：默认不含停用的，includeDisabled=true 才含。
	status, body = call(router, http.MethodGet,
		"/api/availability?providerIds=999999999", "")
	if status != http.StatusOK {
		t.Fatalf("空供应商列表应 200，实得 %d：%s", status, body)
	}
	payload = decodeObject(t, body)
	if len(payload["providers"].([]any)) != 0 || payload["systemAvailability"].(float64) != 0 {
		t.Fatalf("无匹配供应商时应回空列表与 0：%s", body)
	}

	// 停用供应商：默认被排除（is_enabled = true 那道条件），includeDisabled=true 才出现且带 isEnabled:false。
	var disabledID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO providers (name, url, key, provider_vendor_id, provider_type, is_enabled)
		 VALUES ($1, $2, $3, $4, 'claude', false) RETURNING id`,
		seed.prefix+"-disabled", "https://"+seed.domain+"/claude", "sk-ignored",
		seed.vendorID).Scan(&disabledID); err != nil {
		t.Fatalf("建停用供应商夹具失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM providers WHERE id = $1`, disabledID)
	})

	idsOnly := url.Values{}
	idsOnly.Set("providerIds", fmt.Sprintf("%d", disabledID))
	status, body = call(router, http.MethodGet, "/api/availability?"+idsOnly.Encode(), "")
	if status != http.StatusOK || len(decodeObject(t, body)["providers"].([]any)) != 0 {
		t.Fatalf("默认应排除停用供应商：%s", body)
	}
	idsOnly.Set("includeDisabled", "true")
	status, body = call(router, http.MethodGet, "/api/availability?"+idsOnly.Encode(), "")
	if status != http.StatusOK {
		t.Fatalf("含停用供应商应 200，实得 %d：%s", status, body)
	}
	disabledProviders, _ := decodeObject(t, body)["providers"].([]any)
	if len(disabledProviders) != 1 {
		t.Fatalf("includeDisabled=true 应含那个停用供应商：%s", body)
	}
	disabled, _ := disabledProviders[0].(map[string]any)
	if disabled["isEnabled"] != false || disabled["currentStatus"] != "unknown" ||
		len(disabled["timeBuckets"].([]any)) != 0 {
		t.Fatalf("无数据的停用供应商应如实回 unknown 与空桶：%s", body)
	}
}

func TestAvailabilityBucketedValidationIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	router := availabilityRouter(t, pools)

	long := url.Values{}
	long.Set("startTime", "2025-01-01T00:00:00Z")
	long.Set("endTime", "2025-06-01T00:00:00Z")
	budget := url.Values{}
	budget.Set("startTime", availabilityBucketsBase)
	budget.Set("endTime", "2026-01-02T10:20:00Z")
	budget.Set("bucketSizeMinutes", "5")
	budget.Set("maxBuckets", "3")
	reversed := url.Values{}
	reversed.Set("startTime", "2026-01-02T10:00:00Z")
	reversed.Set("endTime", "2026-01-02T09:00:00Z")

	cases := []struct {
		name    string
		query   string
		message string
	}{
		{"起止时刻格式", "startTime=&endTime=2026-01-02T10:00:00Z",
			"Invalid startTime: expected a valid Date or ISO timestamp"},
		{"起止时刻不可解析", "startTime=not-a-date",
			"Invalid startTime: expected a valid Date or ISO timestamp"},
		{"桶宽非数", "bucketSizeMinutes=abc", "Invalid bucketSizeMinutes: expected a positive number"},
		{"桶宽过小", "bucketSizeMinutes=0.1",
			"Invalid bucketSizeMinutes: expected a positive number not less than 0.25"},
		{"桶宽过大", "bucketSizeMinutes=1441",
			"Invalid bucketSizeMinutes: expected a positive number not greater than 1440"},
		{"桶数上限", "maxBuckets=101",
			"Invalid maxBuckets: expected a positive integer not greater than 100"},
		{"桶数非整数", "maxBuckets=1.5", "Invalid maxBuckets: expected a positive integer"},
		{"含停用开关非法", "includeDisabled=maybe",
			"Invalid includeDisabled: expected true or false"},
		{"供应商列表有空项", "providerIds=1,,2",
			"Invalid providerIds: expected comma-separated positive integers"},
		{"供应商 id 非数", "providerIds=abc", "Invalid providerIds: expected a positive integer"},
	}
	for _, testCase := range cases {
		status, body := call(router, http.MethodGet, "/api/availability?"+testCase.query, "")
		if status != http.StatusBadRequest || !strings.Contains(body, `"error":"`+testCase.message+`"`) {
			t.Fatalf("[%s] 应 400 且正文为 %q，实得 %d：%s",
				testCase.name, testCase.message, status, body)
		}
	}

	for _, testCase := range []struct {
		name    string
		query   url.Values
		message string
	}{
		{"范围倒置", reversed, "Invalid time range: endTime must be greater than or equal to startTime"},
		{"范围超 100 天", long, "Invalid time range: requested range must not exceed 100 days"},
		{"桶预算不足", budget,
			"Invalid bucket configuration: requested range exceeds the bucket budget " +
				"implied by bucketSizeMinutes and maxBuckets"},
	} {
		status, body := call(router, http.MethodGet, "/api/availability?"+testCase.query.Encode(), "")
		if status != http.StatusBadRequest || !strings.Contains(body, `"error":"`+testCase.message+`"`) {
			t.Fatalf("[%s] 应 400 且正文为 %q，实得 %d：%s",
				testCase.name, testCase.message, status, body)
		}
	}

	// 缺省参数是可用的：默认窗口 24 小时、桶宽按 50 桶目标推导。
	status, body := call(router, http.MethodGet, "/api/availability", "")
	if status != http.StatusOK {
		t.Fatalf("缺省参数应 200，实得 %d：%s", status, body)
	}
	payload := decodeObject(t, body)
	// 24h = 1440min / 50 = 28.8 → 档位里第一个 >= 28.8 的是 60。
	if payload["bucketSizeMinutes"].(float64) != 60 {
		t.Fatalf("默认桶宽应为 60（24 小时按 50 桶目标推导）：%s", body)
	}
}
