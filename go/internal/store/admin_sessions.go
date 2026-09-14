package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 本文件是 sessions 资源族（/api/v1/sessions*）的只读面。
//
// 唯一真源：src/repository/message.ts 的 messageCanonicalSessionLookup（:78-93）与
// findRequestsBySessionIdentity（:2339-2415），以及 actions/active-sessions.ts 的所有权判定
// （用 aggregateMultipleSessionStats 的 user_id 列）。
//
// 三处照抄 Node 的细节：
//
//  1. **规范 identity 是 COALESCE(session_identity, session_id)**，且保留前缀（pfx:/sid:）走
//     另一套条件（messageCanonicalSessionLookup）。非保留前缀**不加**物理 session_id 的 OR
//     支——这一点与 usage-log 的筛选语义（admin_usage_logs.go 的 addSessionCondition 会加 OR）
//     **不同**，两者不是同一个条件，不可互相替换。
//  2. **owner 过滤是可选的一层**：admin 查会话时不带 user_id 条件，普通用户带上。Node 靠它
//     把「别人的会话」与「不存在的会话」合并成同一个回答（带 owner 条件时根本查不到）。
//  3. **displaySequence 是窗口函数**：prefix_affinity 会话按 (created_at, id) 升序重编号，
//     其余取 request_sequence（为空时同样落到窗口序号）。窗口的 ORDER BY 必须与 Node 一致，
//     否则「显示第几轮」会对不上。
//
// 一处刻意不改的差异：Node 的 count 与分页是**两条**独立查询（无事务），两者之间新写入的
// 请求会让 `hasMore` 略有偏差。这里同样两条查询——加事务会把管理面只读路径绑上快照隔离，
// 换来的只是消除一个瞬时偏差，不值得。

// adminSessionPhysicalIdentityColumn 是 Node 第二个分支引用的**物理**列
// （message.ts:84-85 的 `messageRequest.sessionIdentity`，不是 COALESCE 后的表达式）。
const adminSessionPhysicalIdentityColumn = "session_identity"

// AdminSessionOwner 是一次「规范 identity + 所有者」查询的结果。
type AdminSessionOwner struct {
	// CanonicalIdentity 是 COALESCE(session_identity, session_id)，后续查询用这个 identity。
	CanonicalIdentity string
	// UserID 是会话归属用户（权限判定的依据）。
	UserID int64
}

// adminSessionCanonicalCondition 复刻 messageCanonicalSessionLookup（message.ts:78-93）。
//
// ownerUserID <= 0 表示不加 owner 条件（Node 的 ownerUserId === undefined）。返回的条件里
// 占位符按 args 顺序编号，调用方可直接追加自己的参数。
//
// prefix 是列限定前缀（单表查询传空串，带 JOIN 的查询传 "mr."）：keys 表也有 user_id 列，
// 不限定会撞名。
func adminSessionCanonicalCondition(identity string, ownerUserID int64, prefix string) (string, []any) {
	canonical := "COALESCE(" + prefix + "session_identity, " + prefix + "session_id)"
	physical := prefix + adminSessionPhysicalIdentityColumn
	var conditions []string
	var args []any

	if IsReservedSessionIdentity(identity) {
		// 保留前缀：canonical = identity；带 owner 时物理 session_identity 必须是 identity
		// 或为 NULL——后者把「物理 session_id 恰好等于这个保留串」的同名行排除掉。
		args = append(args, identity)
		canonicalEquals := canonical + " = $" + strconv.Itoa(len(args))
		if ownerUserID > 0 {
			args = append(args, identity)
			conditions = append(conditions, canonicalEquals+" AND ("+
				physical+" = $"+strconv.Itoa(len(args))+" OR "+
				physical+" IS NULL)")
		} else {
			args = append(args, identity)
			conditions = append(conditions, canonicalEquals+" AND "+
				physical+" = $"+strconv.Itoa(len(args)))
		}
	} else {
		args = append(args, identity)
		conditions = append(conditions, canonical+" = $"+strconv.Itoa(len(args)))
	}

	if ownerUserID > 0 {
		args = append(args, ownerUserID)
		conditions = append(conditions, prefix+"user_id = $"+strconv.Itoa(len(args)))
	}
	return strings.Join(conditions, " AND "), args
}

// ResolveAdminSessionOwner 解析一次规范 identity 与所有者。
//
// 复刻 aggregateMultipleSessionStats 里的**身份归并**（message.ts:1755-1795 的 userInfoRows
// 查询）：Node 的所有权判定不是拿入参 identity 直接查规范列，而是先走一遍 LATERAL 的
// UNION ALL——**物理** session_id 也能被归并到它的规范 identity 上（第二支要求
// `session_identity IS NOT NULL AND session_identity <> sid AND session_id = sid`，且该支对
// pfx:/sid: 前缀的入参不生效）。这一步不能省：省掉后，UI 用物理 id 打开会话会得到 404，
// 而 Node 会正常答出该会话。
//
// 取候选的方式与 Node 同序：identity_priority 优先，其次 created_at DESC NULLS LAST、id DESC。
// 查不到（或不属于该 owner）时 ok=false，调用方作答 404——与 Node 把「不存在」与「无权」
// 合并成同一个回答的做法一致。
func (p *Pools) ResolveAdminSessionOwner(
	ctx context.Context,
	identity string,
	ownerUserID int64,
) (AdminSessionOwner, bool, error) {
	pool, err := p.Control()
	if err != nil {
		return AdminSessionOwner{}, false, err
	}
	// 占位符顺序固定为 $1=identity、$2=reserved；owner 条件（$3）两支都要加，且只在需要时
	// 出现——Node 的 ownerCondition 同样同时出现在两个 WHERE 中。参数一律按此顺序传入，
	// 否则「管理员不带 owner」时会因为引用了一个没传的占位符而整条查询报错。
	args := []any{identity, IsReservedSessionIdentity(identity)}
	ownerCondition := ""
	if ownerUserID > 0 {
		args = append(args, ownerUserID)
		ownerCondition = " AND user_id = $3"
	}

	query := `SELECT COALESCE(mr.session_identity, mr.session_id), mr.user_id
		FROM (SELECT $1::text AS sid) AS requested
		CROSS JOIN LATERAL (
			SELECT session_id, session_identity, user_id
			FROM (
				SELECT session_id, session_identity, user_id, created_at, id,
					CASE WHEN session_identity = sid THEN 0 ELSE 1 END AS identity_priority
				FROM message_request
				WHERE COALESCE(session_identity, session_id) = sid
					AND deleted_at IS NULL` + ownerCondition + `

				UNION ALL

				SELECT session_id, session_identity, user_id, created_at, id,
					1 AS identity_priority
				FROM message_request
				WHERE NOT $2::boolean
					AND session_identity IS NOT NULL
					AND session_identity <> sid
					AND session_id = sid
					AND deleted_at IS NULL` + ownerCondition + `
			) candidates
			ORDER BY identity_priority, created_at DESC NULLS LAST, id DESC
			LIMIT 1
		) mr`

	var owner AdminSessionOwner
	if err := pool.QueryRow(ctx, query, args...).Scan(&owner.CanonicalIdentity, &owner.UserID); err != nil {
		if isNoRows(err) {
			return AdminSessionOwner{}, false, nil
		}
		return AdminSessionOwner{}, false, fmt.Errorf("store: 解析会话所有者失败: %w", err)
	}
	return owner, true, nil
}

// AdminSessionRequestRow 是会话时间线上的一行请求。
type AdminSessionRequestRow struct {
	ID              int64
	SourceSessionID string
	Sequence        int64
	DisplaySequence int64
	Model           *string
	StatusCode      *int
	CostUSD         *string
	CreatedAt       *time.Time
	InputTokens     *int64
	OutputTokens    *int64
	ErrorMessage    *string
}

// AdminSessionRequestsPage 是一页会话请求与总数。
type AdminSessionRequestsPage struct {
	Requests []AdminSessionRequestRow
	Total    int64
}

// FindAdminSessionRequests 复刻 findRequestsBySessionIdentity（message.ts:2339-2415）。
//
// ownerUserID <= 0 表示不加 owner 条件（admin）。order 只认 "asc"，其余按 desc 处理——调用方
// 已按枚举校验过，这里只是兜底。
func (p *Pools) FindAdminSessionRequests(
	ctx context.Context,
	identity string,
	ownerUserID int64,
	limit int,
	offset int,
	order string,
) (AdminSessionRequestsPage, error) {
	pool, err := p.Control()
	if err != nil {
		return AdminSessionRequestsPage{}, err
	}
	condition, args := adminSessionCanonicalCondition(identity, ownerUserID, "")
	// Node 的 where 另有三个常量条件：物理 session_id 非空、request_sequence 非空、非回放。
	where := condition + ` AND session_id IS NOT NULL AND request_sequence IS NOT NULL
		AND is_replay = false AND deleted_at IS NULL`

	var total int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::bigint FROM message_request WHERE `+where, args...,
	).Scan(&total); err != nil {
		return AdminSessionRequestsPage{}, fmt.Errorf("store: 统计会话请求数失败: %w", err)
	}

	// 排序与 Node 的 orderBy 逐字一致：desc 走 NULLS LAST，asc 不带。
	orderClause := "created_at DESC NULLS LAST, id DESC"
	if order == "asc" {
		orderClause = "created_at ASC, id ASC"
	}

	selectQuery := `SELECT
		id, session_id, request_sequence,
		CASE
			WHEN session_identity_kind = 'prefix_affinity'
				THEN row_number() OVER (ORDER BY created_at ASC, id ASC)::bigint
			ELSE COALESCE(
				request_sequence,
				row_number() OVER (ORDER BY created_at ASC, id ASC)::bigint
			)
		END AS display_sequence,
		model, status_code, cost_usd::text, created_at,
		input_tokens, output_tokens, error_message
		FROM message_request
		WHERE ` + where + `
		ORDER BY ` + orderClause + `
		LIMIT $` + strconv.Itoa(len(args)+1) + ` OFFSET $` + strconv.Itoa(len(args)+2)

	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := pool.Query(ctx, selectQuery, pageArgs...)
	if err != nil {
		return AdminSessionRequestsPage{}, fmt.Errorf("store: 查询会话请求列表失败: %w", err)
	}
	defer rows.Close()

	page := AdminSessionRequestsPage{Total: total}
	for rows.Next() {
		var row AdminSessionRequestRow
		if err := rows.Scan(
			&row.ID, &row.SourceSessionID, &row.Sequence, &row.DisplaySequence,
			&row.Model, &row.StatusCode, &row.CostUSD, &row.CreatedAt,
			&row.InputTokens, &row.OutputTokens, &row.ErrorMessage,
		); err != nil {
			return AdminSessionRequestsPage{}, fmt.Errorf("store: 读取会话请求行失败: %w", err)
		}
		page.Requests = append(page.Requests, row)
	}
	if err := rows.Err(); err != nil {
		return AdminSessionRequestsPage{}, fmt.Errorf("store: 遍历会话请求行失败: %w", err)
	}
	return page, nil
}
