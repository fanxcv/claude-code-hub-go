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

// 本文件钉住 provider 写路径的**可空性**与 Node schema 的一致性。
//
// 事故（生产，2026-09-13）：用户在界面编辑供应商保存时报
// `{"path":["max_retry_attempts"],"code":"invalid_type","message":"Expected number, received null"}`，
// 新增供应商时报同一句 `One or more fields are invalid`。
//
// 根因：Node 的 `ProviderCreateSchema` 里 `max_retry_attempts` 是
// `z.number().int().min(1).max(10).nullable().optional()`（见 src/lib/api/v1/schemas/providers.ts），
// 且 `providers.max_retry_attempts` 是可空列（无默认值）；而 Go 的字段表误用了
// `providerIntFieldSpec(false, …)`（非空 spec）→ null 被 `object.Number` 判成 invalid_type。
//
// 为什么不能只改这一处就收工：同表里三个超时字段 Node 也是 `.nullable()`，但它们的**列是 NOT NULL DEFAULT 0**，
// 所以 null 的正确落库语义是「用默认 0」，而不是 NULL——照搬可空 spec 会把 400 换成 500（撞 NOT NULL）。
// 本文件同时钉住这两类语义，以及「范围没被放宽」。
func TestProviderWriteNullabilityMatchesNodeSchema(t *testing.T) {
	pools := testPools(t)
	client := providerWriteRedis(t)
	deps := &Deps{ProviderUndoKV: NewRedisProviderUndoKV(client)}
	router := providerWriteRouter(t, pools, deps)
	prefix := fmt.Sprintf("go-pnull-%d", time.Now().UnixNano())
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

	// ① 新增：可空字段送 null 必须通过（改前：400 invalid_type）。
	createBody := fmt.Sprintf(`{
		"name": "%s 空值",
		"url": "https://%s.example.com/anthropic",
		"key": "sk-%s",
		"max_retry_attempts": null,
		"first_byte_timeout_streaming_ms": null,
		"streaming_idle_timeout_ms": null,
		"request_timeout_non_streaming_ms": null
	}`, prefix, prefix, prefix)
	recorder := providerRequest(t, router, http.MethodPost, "/api/v1/providers", createBody,
		"application/json", false)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("新增送 null 应 201，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败: %v（原文 %s）", err, recorder.Body.String())
	}
	createdID := int64(created["id"].(float64))

	// 落库语义：可空列 → NULL；NOT NULL 列 → 默认 0（Node 的 `?? PROVIDER_TIMEOUT_DEFAULTS.*`）。
	if retryNull, timeouts := providerNullabilityColumns(t, pools, createdID); !retryNull {
		t.Fatalf("max_retry_attempts 送 null 应落 NULL（Node 的 `?? null`）")
	} else if timeouts != [3]int64{0, 0, 0} {
		t.Fatalf("三个超时送 null 应落默认 0（列 NOT NULL DEFAULT 0），实得 %v", timeouts)
	}

	// ② 更新（用户实际场景）：PATCH 送 null 必须通过。
	updateBody := `{"max_retry_attempts": null, "first_byte_timeout_streaming_ms": null, "streaming_idle_timeout_ms": null}`
	recorder = providerRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", createdID), updateBody, "application/json", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("更新送 null 应 200（用户报的就是这条），实得 %d：%s",
			recorder.Code, recorder.Body.String())
	}
	if retryNull, timeouts := providerNullabilityColumns(t, pools, createdID); !retryNull || timeouts != [3]int64{0, 0, 0} {
		t.Fatalf("更新后落库应仍为 NULL / 0，实得 retryNull=%v timeouts=%v", retryNull, timeouts)
	}

	// ③ 正常值仍然工作，且真的落库（防「放宽成谁都收、却写不进去」）。
	recorder = providerRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", createdID),
		`{"max_retry_attempts": 7, "streaming_idle_timeout_ms": 1234}`, "application/json", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("更新送正常值应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	if retryNull, timeouts := providerNullabilityColumns(t, pools, createdID); retryNull || timeouts[1] != 1234 {
		t.Fatalf("正常值应落库（retry 非空、idle=1234），实得 retryNull=%v timeouts=%v", retryNull, timeouts)
	}
}

// 本用例钉住**范围没被放宽**：改可空性时如果用了一个「不校验 min/max」的 spec，
// Node 的 `.min(1).max(10)` 就会被静默取消——那是与本次修复反向的另一类回归。
func TestProviderWriteNullabilityKeepsNodeRanges(t *testing.T) {
	pools := testPools(t)
	client := providerWriteRedis(t)
	deps := &Deps{ProviderUndoKV: NewRedisProviderUndoKV(client)}
	router := providerWriteRouter(t, pools, deps)

	cases := []struct {
		name   string
		field  string
		value  string
		code   string
		expect int
	}{
		{"max_retry_attempts 低于 min(1)", "max_retry_attempts", "0", "too_small", http.StatusBadRequest},
		{"max_retry_attempts 高于 max(10)", "max_retry_attempts", "11", "too_big", http.StatusBadRequest},
		{"超时为负（Node min(0)）", "streaming_idle_timeout_ms", "-1", "too_small", http.StatusBadRequest},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"name": "range-%d", "url": "https://range.example.com/anthropic",
				"key": "sk-range", %q: %s}`, time.Now().UnixNano(), item.field, item.value)
			recorder := providerRequest(t, router, http.MethodPost, "/api/v1/providers", body,
				"application/json", false)
			if recorder.Code != item.expect {
				t.Fatalf("应 %d，实得 %d：%s", item.expect, recorder.Code, recorder.Body.String())
			}
			// 错误码也要对：前端按 code 决定提示（too_small / too_big）。
			if !strings.Contains(recorder.Body.String(), item.code) {
				t.Fatalf("响应应含 %q：%s", item.code, recorder.Body.String())
			}
		})
	}
}

// providerNullabilityColumns 读回四列的落库值：retry 是否为 NULL，以及三个超时列的值。
func providerNullabilityColumns(t *testing.T, pools *store.Pools, providerID int64) (bool, [3]int64) {
	t.Helper()
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var retry *int64
	var first, idle, nonStream int64
	if err := writer.QueryRow(context.Background(),
		`SELECT max_retry_attempts, first_byte_timeout_streaming_ms,
		        streaming_idle_timeout_ms, request_timeout_non_streaming_ms
		 FROM providers WHERE id = $1`, providerID,
	).Scan(&retry, &first, &idle, &nonStream); err != nil {
		t.Fatalf("读回可空性列失败: %v", err)
	}
	return retry == nil, [3]int64{first, idle, nonStream}
}
