package usersreset

import (
	"errors"
	"fmt"
	"time"
)

// 本文件是作业记录与错误码的唯一出处，逐字对齐 src/lib/user-statistics-reset/types.ts 与
// reset-service.ts 的错误码。
//
// JSON 键名必须与 Node 一致：Node 直接 JSON.parse 状态键的原文当 TS 对象用，字段名不一致即
// 「两侧互见」失效——不只是读不到，而是读到一个字段全空的记录（那比读不到更坏，UI 会显示
// 一个 queued 且计数为 0 的作业）。

// Status 是作业状态（types.ts:1）。
type Status string

const (
	// StatusQueued 已入队（未开始，或一次失败后等待重试）。
	StatusQueued Status = "queued"
	// StatusRunning 执行中。
	StatusRunning Status = "running"
	// StatusCompleted 已完成。
	StatusCompleted Status = "completed"
	// StatusFailed 终态失败（重试已用尽）。
	StatusFailed Status = "failed"
)

// activeStatuses 是「认领仍有效」的状态集（reset-queue.ts:388 的 ["queued","running"]）。
func isActiveStatus(status Status) bool {
	return status == StatusQueued || status == StatusRunning
}

// Record 是状态键里存的对象（types.ts:26-29 的 UserStatisticsResetStoredRecord）。
type Record struct {
	ResetID                string  `json:"resetId"`
	UserID                 int64   `json:"userId"`
	Status                 Status  `json:"status"`
	RequestedAt            string  `json:"requestedAt"`
	StartedAt              *string `json:"startedAt"`
	CompletedAt            *string `json:"completedAt"`
	DeletedMessageRequests int64   `json:"deletedMessageRequests"`
	DeletedUsageLedger     int64   `json:"deletedUsageLedger"`
	ErrorCode              *string `json:"errorCode"`
	// Fixed5hKeyIDs 与 Fixed5hPreparationVersion 是**对内**字段：Node 的公开响应
	// （UserStatisticsResetResponseSchema）不含它们，故 Public() 会剥掉。
	Fixed5hKeyIDs             []int64 `json:"fixed5hKeyIds"`
	Fixed5hPreparationVersion *int    `json:"fixed5hPreparationVersion"`
}

// PublicRecord 是两条路由对外作答的形状（src/lib/api/v1/schemas/users.ts:73-83）。
type PublicRecord struct {
	ResetID                string  `json:"resetId"`
	UserID                 int64   `json:"userId"`
	Status                 Status  `json:"status"`
	RequestedAt            string  `json:"requestedAt"`
	StartedAt              *string `json:"startedAt"`
	CompletedAt            *string `json:"completedAt"`
	DeletedMessageRequests int64   `json:"deletedMessageRequests"`
	DeletedUsageLedger     int64   `json:"deletedUsageLedger"`
	ErrorCode              *string `json:"errorCode"`
}

// Public 剥掉对内字段（reset-queue.ts:52-59 的 toPublicRecord）。
func (r Record) Public() PublicRecord {
	return PublicRecord{
		ResetID:                r.ResetID,
		UserID:                 r.UserID,
		Status:                 r.Status,
		RequestedAt:            r.RequestedAt,
		StartedAt:              r.StartedAt,
		CompletedAt:            r.CompletedAt,
		DeletedMessageRequests: r.DeletedMessageRequests,
		DeletedUsageLedger:     r.DeletedUsageLedger,
		ErrorCode:              r.ErrorCode,
	}
}

// Progress 是一次重置已删的条数（reset-service.ts:34-41 的 progress）。
type Progress struct {
	DeletedMessageRequests int64
	DeletedUsageLedger     int64
}

// 错误码逐字取自 reset-service.ts 与 reset-queue.ts 的抛出点：这些码会原样落进作业状态并由
// UI 展示，故不能改写文案。
const (
	// ErrCodeInvalidCutoff 表示 requestedAt 不是合法时间（reset-service.ts:206）。
	ErrCodeInvalidCutoff = "USER_STATISTICS_RESET_INVALID_CUTOFF"
	// ErrCodeRowsLocked 表示分批删除后仍有残留（有并发事务持锁），可重试（reset-service.ts:186）。
	ErrCodeRowsLocked = "USER_STATISTICS_RESET_ROWS_LOCKED"
	// ErrCodeOperationFailed 是兜底失败码（reset-service.ts:196、:220）。
	ErrCodeOperationFailed = "USER_STATISTICS_RESET_OPERATION_FAILED"
	// ErrCodeCacheCleanupFailed 表示 Redis 缓存清理失败（reset-service.ts:236）。
	ErrCodeCacheCleanupFailed = "USER_STATISTICS_RESET_CACHE_CLEANUP_FAILED"
	// ErrCodeFixed5hPrepareFailed 表示 5h 固定窗口准备失败（reset-queue.ts:322）。
	ErrCodeFixed5hPrepareFailed = "USER_STATISTICS_RESET_FIXED_5H_PREPARE_FAILED"
	// ErrCodeActiveClaimFailed 表示认领查不到持有者（reset-status-store.ts:71）。
	ErrCodeActiveClaimFailed = "USER_STATISTICS_RESET_ACTIVE_CLAIM_FAILED"
	// ErrCodeActiveStatusMissing：认领指向的作业没有状态记录（reset-queue.ts:396）。
	ErrCodeActiveStatusMissing = "USER_STATISTICS_RESET_ACTIVE_STATUS_MISSING"
	// ErrCodeActiveJobTerminal：认领指向的作业已终态（reset-queue.ts:406）。
	// **Go 侧不抛出这个码**：Node 靠 Bull 的 job state 判定，而 Go 的作业是否已终态完全由状态记录
	// 表达，那种情形已归到 ACTIVE_STATUS_MISSING 那一支（见 queue.go 的 reconcileActive）。
	// 保留常量是为了让逐行对比两端实现的人能一眼看到这个已知差异，而不是找不到它。
	ErrCodeActiveJobTerminal = "USER_STATISTICS_RESET_ACTIVE_JOB_TERMINAL"
	// ErrCodeRedisUnavailable 表示命令连接不可用（reset-status-store.ts:33）。
	ErrCodeRedisUnavailable = "USER_STATISTICS_RESET_REDIS_UNAVAILABLE"
	// ErrCodeStatusInvalid 表示状态键内容不是合法 JSON（reset-status-store.ts:96）。
	ErrCodeStatusInvalid = "USER_STATISTICS_RESET_STATUS_INVALID"
	// ErrCodeStatusWriteFailed 表示状态写入失败（reset-status-store.ts:77）。
	ErrCodeStatusWriteFailed = "USER_STATISTICS_RESET_STATUS_WRITE_FAILED"
)

// Error 是带进度的重置错误（reset-service.ts:34-51 的 UserStatisticsResetError）。
//
// 进度必须随错误一起带出来：重试时要把上一轮的已删条数累加，否则 UI 会看到计数回退。
type Error struct {
	Code     string
	Progress Progress
}

// Error 实现 error。
func (e *Error) Error() string { return e.Code }

// newError 构造只带错误码的错误。
func newError(code string) *Error { return &Error{Code: code} }

// errorCode 取出错误的码，非本包错误一律归为兜底码（reset-queue.ts:38-43）。
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if resetErr := asResetError(err); resetErr != nil {
		return resetErr.Code
	}
	return ErrCodeOperationFailed
}

// errorProgress 取出错误带的进度，非本包错误返回零进度（reset-queue.ts:544-548）。
func errorProgress(err error) Progress {
	if resetErr := asResetError(err); resetErr != nil {
		return resetErr.Progress
	}
	return Progress{}
}

// asResetError 把错误链里的本包错误取出来（错误在 worker 里会被多层包装）。
func asResetError(err error) *Error {
	var resetErr *Error
	if errors.As(err, &resetErr) {
		return resetErr
	}
	return nil
}

// isoMillis 复刻 JS 的 Date#toISOString（毫秒精度、UTC、固定 24 字符）。
//
// Node 侧写进状态键的 requestedAt/startedAt 就是这个形状，Go 必须逐字对齐：
// Node 的 schema 用 z.string().datetime() 校验，多一位小数即 400。
func isoMillis(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseIsoMillis 解析 Node 写下的 ISO 时间串。
func parseIsoMillis(raw string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("usersreset: requestedAt 不是合法时间: %w", err)
	}
	return parsed, nil
}
