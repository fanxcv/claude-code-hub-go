package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件是「写路径失败的三件观测」的钉子：失败审计 + 含成因的服务端日志 + 不变的 400 响应体。
//
// 为什么钉在 providerWriteFailure 而不是处理器上：写路径处理器要真库才能走到失败分支
// （providers_write_test.go 是集成用例，无 CCH_TEST_DSN 即跳过），而 2026-09-15 那次事故的
// 回归必须在无 DB 的 CI 里也红。处理器里所有「失败且带成因」的出口都收敛到这个函数，
// 故钉住它等价于钉住那些出口的三件观测。
//
// 真库上的端到端对照（撤销快照写失败 → 400 + 失败审计行）见
// providers_write_test.go 的 TestProviderWriteUpdateUndoFailureOnRealDeps。

// providerWriteFailureRecorder 执行一次 providerWriteFailure 并返回三件观测。
func providerWriteFailureRecorder(
	t *testing.T,
	deps Deps,
	action string,
	targetID int64,
	targetName, errorMessage string,
	details map[string]any,
	cause error,
) (*httptest.ResponseRecorder, *recordingProblemsLogger) {
	t.Helper()
	logger := &recordingProblemsLogger{}
	deps.Problems = NewProblems(logger)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/providers/149", nil)
	providerWriteFailure(deps, recorder, request, action, targetID, targetName,
		errorMessage, details, cause)
	return recorder, logger
}

// TestProviderWriteFailureEmitsAuditLogAndOpaqueProblem 是本次事故的直接回归：
// 撤销快照写失败时，必须同时留下「失败审计」「含成因的日志」，并且**只回公开文案的 400**。
func TestProviderWriteFailureEmitsAuditLogAndOpaqueProblem(t *testing.T) {
	audit := &recordingAudit{}
	cause := errors.New("redis://default:secrettoken@192.0.2.10:6379: i/o timeout")
	recorder, logger := providerWriteFailureRecorder(t, Deps{Audit: audit},
		"provider.update", 149, "", "UPDATE_FAILED", map[string]any{
			"changedFields":       []string{"isEnabled"},
			"dbUpdateApplied":     true,
			"undoSnapshotWritten": false,
		}, cause)

	// ① 失败审计：一条、success=false、Node 的常量 errorMessage、目标 id。
	if len(audit.events) != 1 {
		t.Fatalf("必须落一条失败审计，实得 %d 条", len(audit.events))
	}
	event := audit.events[0]
	if event.Success {
		t.Fatalf("失败审计的 success 必须为 false：%+v", event)
	}
	if event.Action != "provider.update" || event.ErrorMessage != "UPDATE_FAILED" {
		t.Fatalf("动作名/错误码不符 Node：action=%q errorMessage=%q", event.Action, event.ErrorMessage)
	}
	if event.TargetType != "provider" || event.TargetID != "149" {
		t.Fatalf("目标不符：targetType=%q targetID=%q", event.TargetType, event.TargetID)
	}
	// 「DB 已生效、只是撤销快照没写成」这一事实必须能从审计读出。
	if event.Details["dbUpdateApplied"] != true || event.Details["undoSnapshotWritten"] != false {
		t.Fatalf("审计 details 未区分 DB 已生效与快照未写入：%v", event.Details)
	}
	// 审计行不得夹带原始成因（对管理面多角色可见）。
	if text := auditText(event.Details); strings.Contains(text, "secrettoken") ||
		strings.Contains(text, "i/o timeout") {
		t.Fatalf("审计 details 不得夹带原始成因：%s", text)
	}

	// ② 服务端日志：一条 warn，含成因原文，凭据已遮蔽。
	if len(logger.events) != 1 || logger.events[0] != "admin_action_error" {
		t.Fatalf("必须记一条 warn admin_action_error，实得 %v", logger.events)
	}
	if text, _ := logger.warns[0]["error"].(string); !strings.Contains(text, "i/o timeout") {
		t.Fatalf("日志必须含成因原文，实得 %q", text)
	} else if strings.Contains(text, "secrettoken") {
		t.Fatalf("日志里 URL 内嵌凭据必须被遮蔽，实得 %q", text)
	}

	// ③ 响应体：400 + detail 为公开常量，且不含成因片段（客户端形状不因观测增强而变）。
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("应 400，实得 %d", recorder.Code)
	}
	body := recorder.Body.String()
	if detail := problemDetailOf(t, body); detail != "Bad request" {
		t.Fatalf("detail 必须是公开常量，实得 %q", detail)
	}
	for _, leak := range []string{"i/o timeout", "secrettoken", "redis://"} {
		if strings.Contains(body, leak) {
			t.Fatalf("响应体不得夹带 %q：%s", leak, body)
		}
	}
}

// TestProviderWriteFailureWithoutTargetIDKeepsNullTarget 钉住创建失败这一支：
// 插入没成功时没有 id，审计的 target_id 必须落空（Node 的 `targetId: undefined`），
// 而不是写一个假的 0。
func TestProviderWriteFailureWithoutTargetIDKeepsNullTarget(t *testing.T) {
	audit := &recordingAudit{}
	recorder, _ := providerWriteFailureRecorder(t, Deps{Audit: audit},
		"provider.create", 0, "新建甲", "CREATE_FAILED", nil, errors.New("pg: 约束冲突"))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("应 400，实得 %d", recorder.Code)
	}
	if len(audit.events) != 1 {
		t.Fatalf("创建失败也必须落一条失败审计，实得 %d 条", len(audit.events))
	}
	event := audit.events[0]
	if event.TargetID != "" {
		t.Fatalf("无 id 时 target_id 必须落空，实得 %q", event.TargetID)
	}
	if event.TargetName != "新建甲" {
		t.Fatalf("创建失败的审计必须带 targetName，实得 %q", event.TargetName)
	}
}

// TestProviderWriteFailureSurvivesNilAuditSink 钉住审计缺席（未装配）时行为不退化：
// 审计是 fire-and-forget，缺了它也必须照常回 400，不得变成 panic 或 500。
func TestProviderWriteFailureSurvivesNilAuditSink(t *testing.T) {
	recorder, logger := providerWriteFailureRecorder(t, Deps{},
		"provider.update", 149, "", "UPDATE_FAILED", nil, errors.New("redis: nil"))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("审计缺席时应照常 400，实得 %d", recorder.Code)
	}
	if len(logger.warns) != 1 {
		t.Fatalf("审计缺席不应影响日志，实得 %v", logger.events)
	}
}

// problemDetailOf 从响应正文里取 detail。
func problemDetailOf(t *testing.T, body string) string {
	t.Helper()
	var payload struct {
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("响应正文不是 JSON：%v（原文 %s）", err, body)
	}
	return payload.Detail
}

// auditText 把审计 details 拼成一段可搜索的文本（只为断言不夹带成因）。
func auditText(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// failingProviderUndoKV 让撤销快照的每一次写入都失败，模拟生产里 Redis 的 i/o timeout。
//
// 用替身而不是真 Redis：要么去改 Redis 让它超时（不可能），要么等真实超时（坏测试）。
type failingProviderUndoKV struct{}

func (failingProviderUndoKV) SetEx(context.Context, string, []byte, time.Duration) error {
	return errors.New("redis: i/o timeout")
}

func (failingProviderUndoKV) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (failingProviderUndoKV) GetDel(context.Context, string) ([]byte, bool, error) {
	return nil, false, nil
}

func (failingProviderUndoKV) Del(context.Context, ...string) error { return nil }
