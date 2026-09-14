package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 sessions 资源族的**请求定位器与来源链**（详情面与 origin-chain 的共同底座）。
//
// 唯一真源：src/repository/message.ts 的 messageSessionLookup（:66-77）、
// findSessionRequestLocator（:1650-1729）与 findSessionOriginChain（:1219-1280），
// 以及 src/lib/session-request-locator.ts 的 resolveSessionRequestLocator（选择器语义）。
//
// 为什么定位器是详情面的地基：Node 的六条会话端点（detail / messages / messages-exists /
// response / origin-chain / requests）都先经它把「公开 identity + 可选序号/物理来源」解析成
// 一条**具体的账本行**（requestId + 物理 session_id + 序号 + keyId + userId）。这一步同时完成
// 三件事：规范 identity 的归并、物理来源的校验、所有者过滤。后续所有读取都必须复用它——
// 各自再查一次会把「定位到了 A、读了 B」的窗口打开。
//
// 两处照抄 Node 的细节，改动即语义漂移：
//
//  1. **两套 lookup 条件不通用**（message.ts:66-93）。带 requestId 时用 messageSessionLookup
//     （保留前缀走**裸物理列** session_identity，其余走 COALESCE；且非保留前缀或带 owner 时
//     额外 OR 物理 session_id）；不带 requestId 时用 messageCanonicalSessionLookup
//     admin_sessions.go 的同名实现）。两者对「物理 id 能不能归并到规范 identity」的判法不同。
//  2. **序号缺失与序号非法是两件事**。Node 的 normalizeRequestSequence 把非正数判成 null，
//     于是「没传序号」与「传了 0」走同一条路；而 resolveSessionRequestLocator 的前缀亲和
//     完整性判据（见下）也按同一个 null 判定。
//
// 一处**登记过的差异**：Node 的 resolveSessionRequestLocator 在 `requestId !== undefined`
// 时**跳过**前缀亲和的完整性校验（:27-41 直接 return），本包的 FindAdminSessionRequestLocator
// 只做查询、由调用方 adminapi 决定是否做该校验——把判据留在 handler 层是为了让 store 只回答
// 「查到了什么」，而不是把一条业务规则埋进 SQL 包。

// AdminSessionRequestLocatorSelector 是定位器的选择器（Node 的 selector 参数）。
//
// 三个字段都是「零值即不参与条件」：RequestID<=0、SourceSessionID==""、RequestSequence<=0
// 分别表示未指定。这与 Node 的 `!== undefined` 判定等价，因为 handler 层已把非法序号归一成 0
// （normalizeRequestSequence 的语义：非正数 → 未指定）。
type AdminSessionRequestLocatorSelector struct {
	RequestID       int64
	SourceSessionID string
	RequestSequence int64
}

// AdminSessionRequestLocator 是一条被定位到的账本行。
type AdminSessionRequestLocator struct {
	// RequestID 是账本行 id（响应体工件与来源链都以它为准）。
	RequestID int64
	// CanonicalSessionID 是 COALESCE(session_identity, session_id)。
	CanonicalSessionID string
	// SourceSessionID 是**物理** session_id（Redis 工件的键用它，不是规范 identity）。
	SourceSessionID string
	RequestSequence int64
	KeyID           int64
	UserID          int64
	// IdentityKind 取值 "session_id" | "prefix_affinity"（Node 对非 prefix_affinity 一律归成
	// session_id，包括 NULL）。
	IdentityKind string
	ScopeTag     *string
	Fingerprint  *string
}

// FindAdminSessionRequestLocator 复刻 findSessionRequestLocator（message.ts:1650-1729）。
//
// ownerUserID <= 0 表示不加所有者条件（admin）。查不到返回 (nil, nil)——与 Node 返回 null 同判；
// 「查不到」与「查询失败」必须分开，否则 handler 会把一次数据库故障答成 404。
func (p *Pools) FindAdminSessionRequestLocator(
	ctx context.Context,
	identity string,
	selector AdminSessionRequestLocatorSelector,
	ownerUserID int64,
) (*AdminSessionRequestLocator, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}

	// 条件的选择照抄 Node 的三元：带 requestId 用 messageSessionLookup，否则用 canonical。
	var condition string
	var args []any
	if selector.RequestID > 0 {
		condition, args = adminSessionPhysicalLookupCondition(identity, ownerUserID, "mr.")
	} else {
		condition, args = adminSessionCanonicalCondition(identity, ownerUserID, "mr.")
	}

	// Node 的两个常量前提 + 三个可选选择器，顺序与 selector 的对象展开一致。
	where := condition + ` AND mr.session_id IS NOT NULL AND mr.request_sequence IS NOT NULL`
	if selector.RequestID > 0 {
		args = append(args, selector.RequestID)
		where += " AND mr.id = $" + strconv.Itoa(len(args))
	}
	if selector.SourceSessionID != "" {
		args = append(args, selector.SourceSessionID)
		where += " AND mr.session_id = $" + strconv.Itoa(len(args))
	}
	if selector.RequestSequence > 0 {
		args = append(args, selector.RequestSequence)
		where += " AND mr.request_sequence = $" + strconv.Itoa(len(args))
	}
	where += " AND mr.deleted_at IS NULL"

	query := `SELECT
		mr.id,
		COALESCE(mr.session_identity, mr.session_id),
		mr.session_id,
		mr.request_sequence,
		k.id,
		mr.user_id,
		mr.session_identity_kind,
		mr.affinity_scope_tag,
		mr.affinity_fingerprint
		FROM message_request mr
		INNER JOIN keys k ON mr.key = k.key
		WHERE ` + where + `
		ORDER BY mr.created_at DESC NULLS LAST, mr.id DESC
		LIMIT 1`

	var (
		locator      AdminSessionRequestLocator
		identityKind *string
	)
	if err := pool.QueryRow(ctx, query, args...).Scan(
		&locator.RequestID,
		&locator.CanonicalSessionID,
		&locator.SourceSessionID,
		&locator.RequestSequence,
		&locator.KeyID,
		&locator.UserID,
		&identityKind,
		&locator.ScopeTag,
		&locator.Fingerprint,
	); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 定位会话请求失败: %w", err)
	}

	// Node 的收尾判空：任一必需字段缺失都当作「没定位到」（理由是历史行的部分列为 NULL）。
	// id / keyId / userId 在库里都是非空列，request_sequence 已由 WHERE 保证，故实际只可能
	// 因数据异常触发；照抄是为了让两侧对同一条坏数据给同一个答案。
	if locator.RequestID == 0 || locator.CanonicalSessionID == "" ||
		locator.SourceSessionID == "" || locator.KeyID == 0 || locator.UserID == 0 {
		return nil, nil
	}
	locator.IdentityKind = normalizeAdminSessionIdentityKind(identityKind)
	return &locator, nil
}

// FindAdminSessionOriginChain 复刻 findSessionOriginChain（message.ts:1219-1280）。
//
// 语义：在**选中请求所属 key epoch 内**，找「不晚于该请求」的最近一条带初始选链的账本行，
// 取其 provider_chain。用三条判据把它钉死：
//
//  1. key epoch：`sel.key = k.key` 且 `k.id = keyId` —— 同一把 key 的字符串在换 epoch 后可能
//     指向另一个 key 行，故必须用 keyId 对齐全链。
//  2. 会话与所有者：`mr.session_id = sel.session_id` 且 `mr.user_id = sel.user_id`。
//  3. 时间边界 requestBoundary：按 (created_at, id) 字典序不晚于选中行；选中行 created_at 为
//     NULL 时退化成纯 id 比较（历史行）。
//
// 只看带 `reason: "initial_selection"` 的链：会话中途的换供应商链不该被当作「来源」。
// 返回 nil 表示没有这样的行（Node 返回 null）。
func (p *Pools) FindAdminSessionOriginChain(
	ctx context.Context,
	requestID int64,
	keyID int64,
	ownerUserID int64,
) (json.RawMessage, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	if requestID <= 0 || keyID <= 0 || ownerUserID <= 0 {
		return nil, nil
	}

	query := `SELECT mr.provider_chain::text
		FROM message_request mr
		INNER JOIN keys k ON mr.key = k.key
		INNER JOIN message_request sel
			ON sel.id = $1
			AND sel.key = k.key
			AND sel.user_id = $2
			AND sel.session_id IS NOT NULL
			AND sel.deleted_at IS NULL
			AND mr.session_id = sel.session_id
			AND mr.user_id = sel.user_id
			AND (
				(sel.created_at IS NOT NULL AND (
					mr.created_at < sel.created_at
					OR (mr.created_at = sel.created_at AND mr.id <= sel.id)))
				OR (sel.created_at IS NULL AND mr.id <= sel.id)
			)
		WHERE k.id = $3
			AND mr.user_id = $2
			AND mr.deleted_at IS NULL
			AND (mr.blocked_by IS NULL OR mr.blocked_by <> 'warmup')
			AND mr.provider_chain IS NOT NULL
			AND mr.provider_chain @> '[{"reason": "initial_selection"}]'::jsonb
		ORDER BY
			CASE WHEN sel.created_at IS NULL THEN mr.id END DESC,
			mr.created_at DESC NULLS LAST,
			mr.id DESC
		LIMIT 1`

	var chain *string
	if err := pool.QueryRow(ctx, query, requestID, ownerUserID, keyID).Scan(&chain); err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 查询会话来源链失败: %w", err)
	}
	if chain == nil || *chain == "" {
		return nil, nil
	}
	return json.RawMessage(*chain), nil
}

// adminSessionPhysicalLookupCondition 复刻 messageSessionLookup（message.ts:66-77）。
//
// 与 adminSessionCanonicalCondition 的两处**实质差别**（不可互相替换）：
//
//  1. 保留前缀时比的是**裸物理列** session_identity（canonical 版本比 COALESCE 结果），
//     且不加 canonical 版本那条「物理列必须等于 identity 或为 NULL」的排除支。
//  2. 非保留前缀或带 owner 时，canonical 条件之外**再 OR 一次物理 session_id**——这正是
//     「UI 拿物理 id 打开会话也能定位到」的入口。
func adminSessionPhysicalLookupCondition(
	identity string, ownerUserID int64, prefix string,
) (string, []any) {
	canonical := "COALESCE(" + prefix + "session_identity, " + prefix + "session_id)"
	physical := prefix + adminSessionPhysicalIdentityColumn
	var canonicalCondition string
	var args []any

	if IsReservedSessionIdentity(identity) {
		args = append(args, identity)
		canonicalCondition = physical + " = $" + strconv.Itoa(len(args))
	} else {
		args = append(args, identity)
		canonicalCondition = canonical + " = $" + strconv.Itoa(len(args))
	}
	if ownerUserID > 0 || !IsReservedSessionIdentity(identity) {
		args = append(args, identity)
		canonicalCondition = "(" + canonicalCondition + " OR " + prefix + "session_id = $" +
			strconv.Itoa(len(args)) + ")"
	}

	conditions := []string{canonicalCondition}
	if ownerUserID > 0 {
		args = append(args, ownerUserID)
		conditions = append(conditions, prefix+"user_id = $"+strconv.Itoa(len(args)))
	}
	return strings.Join(conditions, " AND "), args
}
