package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 PUT /api/v1/public/status/settings（Node：actions/public-status.ts:62）。
//
// 共享的 system_settings 行**会被本端点写**（那是它的职责），故测试先记原值、结束时还原——
// 否则并发跑的其它用例会读到被改过的窗口/间隔。

func newPublicStatusSettingsRouter(t *testing.T, pools *store.Pools, publisher PublicStatusPublisher) *Router {
	t.Helper()
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	api := &publicStatusSettingsAPI{
		pools:     pools,
		publisher: publisher,
		problems:  NewProblems(nil),
		logger:    logx.New(nil),
		now:       time.Now,
	}
	router.Add(Route{
		Method: http.MethodPut, Path: "/public/status/settings", Access: AccessAdmin,
		Module: "public", OperationID: "updatePublicStatusSettings",
		Handler: http.HandlerFunc(api.handlePut),
	})
	return router
}

func putPublicStatusSettings(t *testing.T, router *Router, body string) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/v1/public/status/settings", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func TestPublicStatusSettingsRejectsUnknownKeysAndMissingFields(t *testing.T) {
	router := newPublicStatusSettingsRouter(t, &store.Pools{}, nil)

	// 未知顶层键（zod 的 .strict()）。
	status, body := putPublicStatusSettings(t, router,
		`{"publicStatusWindowHours":24,"publicStatusAggregationIntervalMinutes":5,"groups":[],"extra":1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("未知键应 400，实得 %d（%s）", status, body)
	}
	if !strings.Contains(body, "unrecognized_keys") {
		t.Fatalf("应报 unrecognized_keys，实得 %s", body)
	}

	// 缺必填键。
	status, body = putPublicStatusSettings(t, router, `{"publicStatusWindowHours":24}`)
	if status != http.StatusBadRequest {
		t.Fatalf("缺必填应 400，实得 %d（%s）", status, body)
	}

	// 越界（windowHours > 168）。
	status, body = putPublicStatusSettings(t, router,
		`{"publicStatusWindowHours":169,"publicStatusAggregationIntervalMinutes":5,"groups":[]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("越界应 400，实得 %d（%s）", status, body)
	}

	// 分组元素里的未知键同样拒。
	status, body = putPublicStatusSettings(t, router,
		`{"publicStatusWindowHours":24,"publicStatusAggregationIntervalMinutes":5,`+
			`"groups":[{"groupName":"g","publicModels":[],"nope":true}]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("分组未知键应 400，实得 %d（%s）", status, body)
	}
}

func TestPublicStatusSettingsRejectsIntervalOutsideOptions(t *testing.T) {
	router := newPublicStatusSettingsRouter(t, &store.Pools{}, nil)

	// interval 不在 [5,15,30,60] → Node 的业务错（无 errorCode ⇒ public_status.action_failed）。
	status, body := putPublicStatusSettings(t, router,
		`{"publicStatusWindowHours":24,"publicStatusAggregationIntervalMinutes":7,"groups":[]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("非法间隔应 400，实得 %d（%s）", status, body)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("应是问题信封: %s", body)
	}
	if decoded["errorCode"] != "public_status.action_failed" {
		t.Fatalf("错误码应为 public_status.action_failed，实得 %v", decoded["errorCode"])
	}
}

// TestPublicStatusSettingsWritesGroupsAndIsIdempotent 是本文件的核心：
// 真库上跑一遍「启用一个分组 → 描述被写回 → 再提交同一份表单不再产生更新」。
func TestPublicStatusSettingsWritesGroupsAndIsIdempotent(t *testing.T) {
	pools := meOpenPools(t)
	ctx := context.Background()

	// 记录并还原系统设置两列（本端点会写它们）。
	original, err := pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读原始设置失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pools.UpdateAdminSystemSettings(context.Background(), original.ID, store.AdminSystemSettingsPatch{
			Updates: map[store.AdminSystemSettingsColumn]any{
				store.ColPublicStatusWindowHours:     original.PublicStatusWindowHours,
				store.ColPublicStatusAggregationMins: original.PublicStatusAggregationMins,
			},
		})
	})

	groupName := fmt.Sprintf("go-pubstatus-it-%d", time.Now().UnixNano())
	created, err := pools.AdminCreateProviderGroup(ctx, groupName, nil, nil)
	if err != nil {
		t.Fatalf("建分组夹具失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pools.AdminDeleteProviderGroup(context.Background(), created.ID)
	})

	router := newPublicStatusSettingsRouter(t, pools, nil)
	payload := fmt.Sprintf(`{"publicStatusWindowHours":48,"publicStatusAggregationIntervalMinutes":15,
		"groups":[{"groupName":%q,"displayName":"对外名","publicGroupSlug":"public-slug",
		"publicModels":[{"modelKey":"claude-sonnet-4-5"}]}]}`, groupName)
	payload = strings.ReplaceAll(payload, "\n", "")
	payload = strings.ReplaceAll(payload, "\t", "")

	status, body := putPublicStatusSettings(t, router, payload)
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d（%.600s）", status, body)
	}
	var response struct {
		UpdatedGroupCount                 int     `json:"updatedGroupCount"`
		ConfigVersion                     string  `json:"configVersion"`
		PublicStatusProjectionWarningCode *string `json:"publicStatusProjectionWarningCode"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("响应不是合法 JSON: %v（%s）", err, body)
	}
	if response.UpdatedGroupCount != 1 {
		t.Fatalf("应恰好更新 1 个分组（只有本夹具改了），实得 %d", response.UpdatedGroupCount)
	}
	if !strings.HasPrefix(response.ConfigVersion, "cfg-") {
		t.Fatalf("configVersion 应与 Node 同形（cfg-<ms>），实得 %q", response.ConfigVersion)
	}
	// 未装配发布器 ⇒ 与 Node 的发布失败同判。
	if response.PublicStatusProjectionWarningCode == nil ||
		*response.PublicStatusProjectionWarningCode != "PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED" {
		t.Fatalf("未装配发布器时应回 PUBLISH_FAILED，实得 %v", response.PublicStatusProjectionWarningCode)
	}

	// 描述确实按 Node 的格式写回，且能被读侧解析回同一份配置。
	fresh, err := pools.AdminGetProviderGroupByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("读回分组失败: %v", err)
	}
	parsed := pubstatus.ParsePublicStatusDescription(fresh.Description)
	if parsed.PublicStatus == nil {
		t.Fatalf("描述里应有 publicStatus 段，实得 %v", fresh.Description)
	}
	if parsed.PublicStatus.DisplayName != "对外名" || parsed.PublicStatus.PublicGroupSlug != "public-slug" {
		t.Fatalf("publicStatus 写回不符: %+v", parsed.PublicStatus)
	}
	if len(parsed.PublicStatus.PublicModels) != 1 || parsed.PublicStatus.PublicModels[0].ModelKey != "claude-sonnet-4-5" {
		t.Fatalf("publicModels 写回不符: %+v", parsed.PublicStatus.PublicModels)
	}
	if !strings.Contains(*fresh.Description, `"version":2`) {
		t.Fatalf("描述串应带版本号，实得 %s", *fresh.Description)
	}

	// 系统设置两列被写入。
	updated, err := pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读回设置失败: %v", err)
	}
	if updated.PublicStatusWindowHours != 48 || updated.PublicStatusAggregationMins != 15 {
		t.Fatalf("两列应被写入（48/15），实得 %d/%d",
			updated.PublicStatusWindowHours, updated.PublicStatusAggregationMins)
	}

	// 幂等：同一份表单再提交，不应再产生分组更新（描述已相同）。
	status, body = putPublicStatusSettings(t, router, payload)
	if status != http.StatusOK {
		t.Fatalf("第二次应 200，实得 %d（%.300s）", status, body)
	}
	var second struct {
		UpdatedGroupCount int `json:"updatedGroupCount"`
	}
	if err := json.Unmarshal([]byte(body), &second); err != nil {
		t.Fatalf("第二次响应不是合法 JSON: %v", err)
	}
	if second.UpdatedGroupCount != 0 {
		t.Fatalf("重复提交应无变化（updatedGroupCount=0），实得 %d", second.UpdatedGroupCount)
	}

	// 停用该分组（不在 groups 里出现）→ 描述被清成 nil（Node 写 publicStatus: null）。
	disablePayload := `{"publicStatusWindowHours":48,"publicStatusAggregationIntervalMinutes":15,"groups":[]}`
	status, body = putPublicStatusSettings(t, router, disablePayload)
	if status != http.StatusOK {
		t.Fatalf("停用提交应 200，实得 %d（%.300s）", status, body)
	}
	afterDisable, err := pools.AdminGetProviderGroupByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("停用后读回失败: %v", err)
	}
	disabled := pubstatus.ParsePublicStatusDescription(afterDisable.Description)
	if disabled.PublicStatus != nil {
		t.Fatalf("停用后不该再有 publicStatus 段，实得 %v", afterDisable.Description)
	}
	// 但第二次提交会连带更新**所有**曾被启用的分组；这里只断言本夹具已停用。
	_ = body
}
