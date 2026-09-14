package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 本文件是 audit_log 表的管理面**读面**（资源族 /api/v1/audit-logs）。
//
// 唯一真源：src/repository/audit-log.ts 的 listAuditLogs / getAuditLog / toRow。SQL 形状、
// 排序、游标（keyset）语义与 NULL 语义逐条对齐该文件；写面在 adminapi/audit.go（Go 侧的
// 审计落库），与本文件无关。
//
// 分道：读走 control（与 usage-logs / me 的读面一致，不占数据面连接）。

// AdminAuditLogRow 是 audit_log 的一行，形状与 Node 的 AuditLogRow 逐字一致
// （src/lib/api/v1/schemas/audit-logs.ts:44-63 的 AuditLogSchema 是它的对外投影）。
//
// 字段名用 Node 的 camelCase：本类型同时充当响应体的形状来源，故不能改成 Go 风格命名再映射。
// 可空列一律用指针（Node 侧 Drizzle 的 $inferSelect 允许 null，序列化成 JSON 的 null）；
// beforeValue / afterValue 是 jsonb，原样透传（RawMessage 保留 null 与嵌套结构）。
type AdminAuditLogRow struct {
	ID               int64           `json:"id"`
	ActionCategory   string          `json:"actionCategory"`
	ActionType       string          `json:"actionType"`
	TargetType       *string         `json:"targetType"`
	TargetID         *string         `json:"targetId"`
	TargetName       *string         `json:"targetName"`
	BeforeValue      json.RawMessage `json:"beforeValue"`
	AfterValue       json.RawMessage `json:"afterValue"`
	OperatorUserID   *int64          `json:"operatorUserId"`
	OperatorUserName *string         `json:"operatorUserName"`
	OperatorKeyID    *int64          `json:"operatorKeyId"`
	OperatorKeyName  *string         `json:"operatorKeyName"`
	OperatorIP       *string         `json:"operatorIp"`
	UserAgent        *string         `json:"userAgent"`
	Success          bool            `json:"success"`
	ErrorMessage     *string         `json:"errorMessage"`
	// CreatedAt 是 ISO 8601 串（Node 侧是 Date 对象，JSON 序列化成 toISOString() 的同形文本）。
	// 列可空，故用 COALESCE 兜到 Unix 纪元——Node 的 toRow/getAuditLogCreatedAt 用
	// `row.createdAt ?? new Date(0)` 兜同一件事。
	CreatedAt string `json:"createdAt"`
}

// AdminAuditLogFilter 是 listAuditLogs 的筛选条件（Node 的 AuditLogFilter）。
//
// 只保留 API 这一层能传进来的四项：category / success / from / to。Node 的 repository 还认
// actionType / operatorUserId / operatorIp / targetType / targetId，但那几个只有内部调用方用，
// 管理面路由不暴露（写在这里会让「实现了却没接线」看起来像支持）。
type AdminAuditLogFilter struct {
	Category string
	Success  *bool
	From     *time.Time
	To       *time.Time
}

// AdminAuditLogCursor 是 keyset 分页游标（Node 的 AuditLogCursor：createdAt 是 ISO 串）。
type AdminAuditLogCursor struct {
	CreatedAt string
	ID        int64
}

// auditLogColumns 与 toRow 的投影一致；时间列转成 Node toISOString() 的同形文本。
//
// MS 是三位毫秒：Node 的 Date.toISOString() 只保留毫秒，故不能写 US（微秒）——那会多出三位
// 数字，前端解析后与 Node 的串不等。
const auditLogColumns = `id,
	action_category AS "actionCategory",
	action_type AS "actionType",
	target_type AS "targetType",
	target_id AS "targetId",
	target_name AS "targetName",
	before_value AS "beforeValue",
	after_value AS "afterValue",
	operator_user_id AS "operatorUserId",
	operator_user_name AS "operatorUserName",
	operator_key_id AS "operatorKeyId",
	operator_key_name AS "operatorKeyName",
	operator_ip AS "operatorIp",
	user_agent AS "userAgent",
	success,
	error_message AS "errorMessage",
	COALESCE(to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
		'1970-01-01T00:00:00.000Z') AS "createdAt"`

// AdminListAuditLogs 复刻 listAuditLogs：按 (created_at DESC, id DESC) 取 pageSize+1 行。
//
// 返回的行已裁到 pageSize，第二返回值是**下一页游标**（没有下一页时为 nil）。上限与 Node 的
// `Math.min(Math.max(pageSize ?? 50, 1), 500)` 一致：调用方（管理面）传的是 zod 校验过的
// 1..100，但仓库层自己也要钳，否则内部调用方能传进来一个 10 万行的页。
//
// 游标的比较式与 Node 完全一致：`created_at < cursorCreatedAt OR (created_at = cursorCreatedAt
// AND id < cursorId)`——只按 id 比较会在同一毫秒内跨页漏行，只按时间比较会在时间相同时死循环。
func (p *Pools) AdminListAuditLogs(
	ctx context.Context,
	filter AdminAuditLogFilter,
	cursor *AdminAuditLogCursor,
	pageSize int,
) ([]AdminAuditLogRow, *AdminAuditLogCursor, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 500 {
		pageSize = 500
	}

	conditions := make([]string, 0, 6)
	args := make([]any, 0, 6)
	add := func(expression string, value any) {
		args = append(args, value)
		conditions = append(conditions, fmt.Sprintf(expression, len(args)))
	}

	if filter.Category != "" {
		add("action_category = $%d", filter.Category)
	}
	if filter.Success != nil {
		add("success = $%d", *filter.Success)
	}
	if filter.From != nil {
		add("created_at >= $%d::timestamptz", *filter.From)
	}
	if filter.To != nil {
		add("created_at <= $%d::timestamptz", *filter.To)
	}
	if cursor != nil {
		args = append(args, cursor.CreatedAt, cursor.ID)
		conditions = append(conditions, fmt.Sprintf(
			"(created_at < $%d::timestamptz OR (created_at = $%d::timestamptz AND id < $%d))",
			len(args)-1, len(args)-1, len(args),
		))
	}

	query := `SELECT row_to_json(t)::text FROM (SELECT ` + auditLogColumns + ` FROM audit_log`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	args = append(args, pageSize+1)
	query += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d) t`, len(args))

	rows, err := p.auditLogRows(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) <= pageSize {
		return rows, nil, nil
	}
	trimmed := rows[:pageSize]
	last := trimmed[len(trimmed)-1]
	return trimmed, &AdminAuditLogCursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
}

// AdminGetAuditLog 复刻 getAuditLog：按 id 取一行；不存在时返回 ErrNotFound。
func (p *Pools) AdminGetAuditLog(ctx context.Context, id int64) (AdminAuditLogRow, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + auditLogColumns +
		` FROM audit_log WHERE id = $1) t`
	rows, err := p.auditLogRows(ctx, query, id)
	if err != nil {
		return AdminAuditLogRow{}, err
	}
	if len(rows) == 0 {
		return AdminAuditLogRow{}, ErrNotFound
	}
	return rows[0], nil
}

// auditLogRows 用 control 分道读多行（row_to_json 文本行）。
//
// 名字带 auditLog 前缀是有意的：本包有多个并行 lane 各自加文件，通用名（rowsAs 之类）会撞符号。
func (p *Pools) auditLogRows(ctx context.Context, query string, args ...any) ([]AdminAuditLogRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询审计日志失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminAuditLogRow, 0, 8)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取审计日志行失败: %w", err)
		}
		var row AdminAuditLogRow
		if err := json.Unmarshal([]byte(payload), &row); err != nil {
			return nil, fmt.Errorf("store: 审计日志行反序列化失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历审计日志行失败: %w", err)
	}
	return results, nil
}
