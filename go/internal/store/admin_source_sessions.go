package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// 本文件是 sessions 资源族的**物理来源枚举**，终止端点（DELETE /sessions/{id} 与
// POST /sessions:batchTerminate）用它先拿到「物理 sessionId + keyId + providerIds 集合」。
//
// 唯一真源：src/repository/message.ts 的 listPhysicalSessionSourcesForIdentity（:1544-1620）。
//
// 三处照抄 Node 的细节，改动即语义漂移：
//
//  1. **「只认当前绑定」的关联子查询**（:1571-1581）：一行算数，当且仅当它的规范 identity 等于
//     同一 (session_id, user_id, key) 上**最近一条未删非回放行**的规范 identity。前缀亲和换绑后，
//     旧绑定的行不会再被枚举出来——终止时也就不会误摘新绑定的索引。
//  2. **最终供应商取自 provider_chain 的末元素**（:1556-1570）：末元素是对象、有 'id' 键、
//     且 id 是全数字串时才用它，否则回退到 provider_id。非数组/空数组/坏形状一律回退。
//  3. **providerIds 同时收 provider_id 与最终供应商**（:1600-1612）：两者都收，>0 才算；
//     同一 (session, key) 下去重。
//
// 一处**登记过的差异**：Node 的查询**无 ORDER BY**，返回顺序与 providerIds 顺序由计划决定；
// 这里定序为 (session_id, key_id, created_at, id)，结果可复现。

// PhysicalSessionSource 是一个物理 Session 在某个 Key 上的来源。
type PhysicalSessionSource struct {
	SessionID string
	UserID    int64
	KeyID     int64
	// ProviderIDs 是按「首次出现」定序、已去重的供应商 id（含尝试过的与最终生效的）。
	ProviderIDs []int64
}

// physicalSessionFinalProviderExpr 复刻 Node 的最终供应商表达式（:1556-1570）。
//
// 用 jsonb_exists(chain -> -1, 'id') 而不是 `?` 运算符：两者同义（`?` 就是 jsonb_exists），
// 但 `?` 在部分驱动/协议下会被当作占位符看待，函数写法没有这层歧义。
const physicalSessionFinalProviderExpr = `COALESCE(
	CASE
		WHEN mr.provider_chain IS NOT NULL
			AND jsonb_typeof(mr.provider_chain) = 'array'
			AND jsonb_array_length(mr.provider_chain) > 0
			AND jsonb_typeof(mr.provider_chain -> -1) = 'object'
			AND jsonb_exists(mr.provider_chain -> -1, 'id')
			AND (mr.provider_chain -> -1 ->> 'id') ~ '^[0-9]+$'
			THEN (mr.provider_chain -> -1 ->> 'id')::integer
			ELSE NULL
	END,
	mr.provider_id
)`

// ListPhysicalSessionSourcesForIdentity 复刻 listPhysicalSessionSourcesForIdentity
// （message.ts:1544-1620）。
//
// ownerUserID <= 0 表示不加所有者条件（Node 的 ownerUserId === undefined）。
// 没有任何来源时返回空切片（不是 nil），与 Node 返回 `[]` 一致。
func (p *Pools) ListPhysicalSessionSourcesForIdentity(
	ctx context.Context,
	identity string,
	ownerUserID int64,
) ([]PhysicalSessionSource, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}

	// 列名前缀 mr.：本查询 JOIN 了 keys，而 keys 也有 user_id 列，不限定会撞名。
	condition, args := adminSessionCanonicalCondition(identity, ownerUserID, "mr.")

	// 关联子查询与 Node 同：只比其他条件里的 (session_id, user_id, key)，不带规范 identity 条件；
	// 排序 (created_at DESC, id DESC) 与 Node 的 orderBy 逐字一致。
	query := `SELECT
		mr.session_id, mr.user_id, k.id, mr.provider_id,
		` + physicalSessionFinalProviderExpr + ` AS final_provider_id
		FROM message_request mr
		INNER JOIN keys k ON mr.key = k.key
		WHERE ` + condition + `
			AND mr.session_id IS NOT NULL
			AND mr.is_replay = false
			AND mr.deleted_at IS NULL
			AND COALESCE(mr.session_identity, mr.session_id) = (
				SELECT COALESCE(latest.session_identity, latest.session_id)
				FROM message_request latest
				WHERE latest.session_id = mr.session_id
					AND latest.user_id = mr.user_id
					AND latest.key = mr.key
					AND latest.deleted_at IS NULL
					AND latest.is_replay = false
				ORDER BY latest.created_at DESC, latest.id DESC
				LIMIT 1
			)
		ORDER BY mr.session_id, k.id, mr.created_at, mr.id`

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 枚举会话物理来源失败: %w", err)
	}
	defer rows.Close()

	// 按 (sessionId, keyId) 归并；Node 用 Map 保持「首次出现」序，这里用索引表复现同一语义。
	sources := make([]PhysicalSessionSource, 0, 4)
	indexBySource := map[string]int{}
	providerSeen := map[string]map[int64]struct{}{}

	appendProvider := func(sourceIndex int, providerID int64) {
		if providerID <= 0 {
			return
		}
		source := sources[sourceIndex]
		sourceKey := physicalSessionSourceKey(source.SessionID, source.KeyID)
		seen, ok := providerSeen[sourceKey]
		if !ok {
			seen = map[int64]struct{}{}
			providerSeen[sourceKey] = seen
		}
		if _, duplicate := seen[providerID]; duplicate {
			return
		}
		seen[providerID] = struct{}{}
		source.ProviderIDs = append(source.ProviderIDs, providerID)
		sources[sourceIndex] = source
	}

	for rows.Next() {
		var sessionID string
		var userID int64
		var keyID int64
		var providerID int64
		var finalProviderID *int64
		if err := rows.Scan(&sessionID, &userID, &keyID, &providerID, &finalProviderID); err != nil {
			return nil, fmt.Errorf("store: 读取会话物理来源行失败: %w", err)
		}
		sourceKey := physicalSessionSourceKey(sessionID, keyID)
		index, exists := indexBySource[sourceKey]
		if !exists {
			index = len(sources)
			indexBySource[sourceKey] = index
			sources = append(sources, PhysicalSessionSource{
				SessionID:   sessionID,
				UserID:      userID,
				KeyID:       keyID,
				ProviderIDs: []int64{},
			})
		}
		// Node 先收 provider_id 再收最终供应商（:1600-1612）。
		appendProvider(index, providerID)
		if finalProviderID != nil {
			appendProvider(index, *finalProviderID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历会话物理来源行失败: %w", err)
	}
	return sources, nil
}

// physicalSessionSourceKey 拼 (sessionId, keyId) 的归并键（Node 用 JSON.stringify([sid, keyId])）。
func physicalSessionSourceKey(sessionID string, keyID int64) string {
	return strings.Join([]string{sessionID, strconv.FormatInt(keyID, 10)}, ":")
}
