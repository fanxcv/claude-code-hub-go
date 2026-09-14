package store

import "context"

// 本文件是 ip-geo 可见性判据的只读查询：复刻 Node `getMyIpGeoDetails`（actions/my-usage.ts:893-931）
// 的两段查询。
//
// 为什么要有它而不是把两段 SQL 写在 adminapi：可见性判据的**两段是同一件事的两半**——
// 第一段查活跃请求行，第二段查「请求行已被清掉但账本仍在」的残留（Node 的 not exists 子查询）。
// 拆开写在调用方会让「只查了第一段」看起来像实现了一半，而症状是**残留 IP 查不到归属地**。
//
// 两个谓词都复用本包既有的移植常量（`ExcludeWarmupCondition` / `BillingCondition`），
// 不在这里另写一份：谓词一旦抄成第二份，将来改口径就会只改一处。

// IPGeoVisibleForKey 判 `clientIP` 是否出现在该密钥的可见日志里。
//
// 逐条对齐 Node：
//
//  1. `message_request`：`key = $1 and client_ip = $2 and deleted_at is null` 且**排除预热**
//     （EXCLUDE_WARMUP_CONDITION）；Node 只 `limit(1)`，这里用 EXISTS 同义且不取行；
//  2. 若第 1 段无行，再看 `usage_ledger`：同一 key + 同一 client_ip，且对应的 `message_request`
//     行**已删除**（Node 的 `not exists (… deleted_at is null …)` 子查询），并把账本行限定在
//     计费条件内（LEDGER_BILLING_CONDITION）。
//
// 两段都查不到 → 不可见。查询故障如实返回 error（调用方按依赖故障作答，不静默当不可见——
// 「查库坏了」与「确实没见过这个 IP」是两件事，混起来会让运维白找半天）。
func (p *Pools) IPGeoVisibleForKey(ctx context.Context, keyValue, clientIP string) (bool, error) {
	if keyValue == "" || clientIP == "" {
		return false, nil
	}
	var row struct {
		Visible bool `json:"visible"`
	}
	// 与 Node 的差异只有一处（登记在报告里）：Node 分两段查并 `limit(1)`，这里合成一条 EXISTS；
	// 判据（两段任一命中即可见）完全一致，少一次往返。
	err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT EXISTS (
				SELECT 1 FROM message_request
				WHERE key = $1 AND client_ip = $2 AND deleted_at IS NULL
				  AND `+ExcludeWarmupCondition+`
				UNION ALL
				SELECT 1 FROM usage_ledger
				WHERE key = $1 AND client_ip = $2
				  AND `+BillingCondition+`
				  AND NOT EXISTS (
					SELECT 1 FROM message_request AS mr_active
					WHERE mr_active.id = usage_ledger.request_id
					  AND mr_active.deleted_at IS NULL
					  AND mr_active.key = usage_ledger.key
				  )
			) AS visible
		) t`,
		&row,
		[]any{keyValue, clientIP},
	)
	if err != nil {
		if err == ErrNotFound {
			return false, nil
		}
		return false, err
	}
	return row.Visible, nil
}
