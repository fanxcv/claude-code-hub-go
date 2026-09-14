package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// 本文件是 sessions 详情面**最后两条账本查询**：相邻请求导航与请求审计行。
//
// 唯一真源：src/repository/message.ts 的 findAdjacentSessionRequests（:2461-2545）与
// findMessageRequestAuditById（:1282-1316）。
//
// 为什么它们与定位器分开：定位器回答「这次要看哪一行」，这两条回答「这一行的前/后邻居是谁」
// 与「这一行的审计字段是什么」。三个问题三种条件（前/后邻居要时间序、审计要 by-id 且带所有者），
// 合在一起会让「用哪套条件」在一次查询里失去可读性。

// AdminSessionAdjacentRequest 是一条相邻请求的导航目标。
type AdminSessionAdjacentRequest struct {
	RequestID       int64  `json:"requestId"`
	SourceSessionID string `json:"sourceSessionId"`
	RequestSequence int64  `json:"requestSequence"`
}

// AdminSessionAdjacent 是详情页的前/后邻居。
//
// 两个字段都可能为 nil（首条没有前一条）；Node 用 null，Go 侧用指针让 JSON 同样是 null。
type AdminSessionAdjacent struct {
	Prev *AdminSessionAdjacentRequest `json:"prevRequest"`
	Next *AdminSessionAdjacentRequest `json:"nextRequest"`
}

// FindAdminAdjacentSessionRequests 复刻 findAdjacentSessionRequests。
//
// 三处照抄 Node：
//
//  1. **先按 (identity, owner, id) 取当前行**，拿它的 created_at 作为时间基准；取不到就
//     直接返回两个 nil（不猜）。is_replay = false 与 deleted_at IS NULL 是硬条件。
//  2. **前后各一条**，判据是 (created_at, id) 的字典序，不是单纯的 id（同一毫秒内的多条
//     请求靠 id 定序）。前一条的排序用 `created_at DESC NULLS LAST, id DESC`，后一条用
//     正序——与 Node 的 orderBy 逐字一致。
//  3. **时间基准为 NULL 时退化成纯 id 比较**：Node 的 `current.createdAt` 判空后直接返回
//     两个 null（不是退化），故这里同样直接返回空（历史行不编邻居）。
func (p *Pools) FindAdminAdjacentSessionRequests(
	ctx context.Context,
	identity string,
	requestID int64,
	ownerUserID int64,
) (AdminSessionAdjacent, error) {
	result := AdminSessionAdjacent{}
	if identity == "" || requestID <= 0 {
		return result, nil
	}
	pool, err := p.Control()
	if err != nil {
		return result, err
	}

	lookup, args := adminSessionCanonicalCondition(identity, ownerUserID, "mr.")
	args = append(args, requestID)
	currentQuery := `SELECT mr.created_at FROM message_request mr WHERE ` + lookup +
		` AND mr.id = $` + strconv.Itoa(len(args)) +
		` AND mr.is_replay = false AND mr.deleted_at IS NULL LIMIT 1`

	// 用 time.Time 而不是字符串：pgx 的 binary 格式不接受把 timestamptz 扫进 *string
	// （真库上直接报 "cannot scan timestamptz (OID 1184) in binary format into **string"）。
	var createdAt *time.Time
	if scanErr := pool.QueryRow(ctx, currentQuery, args...).Scan(&createdAt); scanErr != nil {
		if isNoRows(scanErr) {
			return result, nil
		}
		return result, fmt.Errorf("store: 查相邻请求的时间基准失败: %w", scanErr)
	}
	if createdAt == nil {
		return result, nil
	}

	timeline, timelineArgs := adminSessionCanonicalCondition(identity, ownerUserID, "mr.")
	// 参数直接用 time.Time：pgx 会把两侧都编成 timestamptz，比较语义交给库。
	timelineArgs = append(timelineArgs, *createdAt, requestID)
	prevPlaceholder := strconv.Itoa(len(timelineArgs) - 1)
	nextPlaceholder := strconv.Itoa(len(timelineArgs))
	base := ` FROM message_request mr WHERE ` + timeline +
		` AND mr.session_id IS NOT NULL AND mr.request_sequence IS NOT NULL
		  AND mr.is_replay = false AND mr.deleted_at IS NULL`

	prevQuery := `SELECT mr.id, mr.session_id, mr.request_sequence` + base +
		` AND (mr.created_at < $` + prevPlaceholder +
		` OR (mr.created_at = $` + prevPlaceholder + ` AND mr.id < $` + nextPlaceholder + `))` +
		` ORDER BY mr.created_at DESC NULLS LAST, mr.id DESC LIMIT 1`
	prev, prevErr := scanAdjacent(pool, ctx, prevQuery, timelineArgs)
	if prevErr != nil {
		return result, prevErr
	}
	result.Prev = prev

	nextQuery := `SELECT mr.id, mr.session_id, mr.request_sequence` + base +
		` AND (mr.created_at > $` + prevPlaceholder +
		` OR (mr.created_at = $` + prevPlaceholder + ` AND mr.id > $` + nextPlaceholder + `))` +
		` ORDER BY mr.created_at ASC, mr.id ASC LIMIT 1`
	next, nextErr := scanAdjacent(pool, ctx, nextQuery, timelineArgs)
	if nextErr != nil {
		return result, nextErr
	}
	result.Next = next
	return result, nil
}

// scanAdjacent 扫一条导航目标；查不到返回 nil（不是空结构）。
func scanAdjacent(
	pool *Pool, ctx context.Context, query string, args []any,
) (*AdminSessionAdjacentRequest, error) {
	var (
		requestID  int64
		sessionID  *string
		requestSeq *int64
	)
	if err := pool.QueryRow(ctx, query, args...).Scan(&requestID, &sessionID, &requestSeq); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 查相邻会话请求失败: %w", err)
	}
	// Node 的 toTarget：三个字段任一缺失都不算一个可用目标。
	if requestID == 0 || sessionID == nil || *sessionID == "" || requestSeq == nil {
		return nil, nil
	}
	return &AdminSessionAdjacentRequest{
		RequestID:       requestID,
		SourceSessionID: *sessionID,
		RequestSequence: *requestSeq,
	}, nil
}

// AdminMessageRequestAudit 是一条请求的审计字段（详情页 specialSettings 的账本来源）。
type AdminMessageRequestAudit struct {
	StatusCode       *int
	BlockedBy        *string
	BlockedReason    *string
	CacheTTLApplied  *string
	Context1mApplied *bool
	// SpecialSettings 是 jsonb 原文；nil 表示该列为 NULL（Node 侧同样按「非数组」处理）。
	SpecialSettings []byte
}

// FindAdminMessageRequestAudit 复刻 findMessageRequestAuditById（message.ts:1282-1316）。
//
// ownerUserID <= 0 表示不加所有者条件（admin）。查不到返回 (nil, nil)。
//
// **`context1mApplied` 只读不派生**：Node 在 storeSessionInfo 时代会自动派生一条
// anthropic_context_1m_header_override 展示项，后来改成「保留参数但不再派生」
// special-settings.ts:25 的注释），故这里同样只把它带出来、不据此造设置项。
func (p *Pools) FindAdminMessageRequestAudit(
	ctx context.Context,
	requestID int64,
	ownerUserID int64,
) (*AdminMessageRequestAudit, error) {
	if requestID <= 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	args := []any{requestID}
	where := "mr.id = $1 AND mr.deleted_at IS NULL"
	if ownerUserID > 0 {
		args = append(args, ownerUserID)
		where += " AND mr.user_id = $" + strconv.Itoa(len(args))
	}
	query := `SELECT mr.status_code, mr.blocked_by, mr.blocked_reason, mr.cache_ttl_applied,
		mr.context_1m_applied, mr.special_settings
		FROM message_request mr WHERE ` + where + ` LIMIT 1`

	var audit AdminMessageRequestAudit
	if scanErr := pool.QueryRow(ctx, query, args...).Scan(
		&audit.StatusCode, &audit.BlockedBy, &audit.BlockedReason, &audit.CacheTTLApplied,
		&audit.Context1mApplied, &audit.SpecialSettings,
	); scanErr != nil {
		if isNoRows(scanErr) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 查请求审计行失败: %w", scanErr)
	}
	return &audit, nil
}

// DecodeSpecialSettings 把 jsonb 原文解成设置项切片。
//
// 解不动（非数组、坏 JSON）一律返回 nil：Node 的 `Array.isArray(...)` 判定同判。
func (a *AdminMessageRequestAudit) DecodeSpecialSettings() []map[string]any {
	if a == nil || len(a.SpecialSettings) == 0 {
		return nil
	}
	var decoded []map[string]any
	if err := json.Unmarshal(a.SpecialSettings, &decoded); err != nil {
		return nil
	}
	return decoded
}
