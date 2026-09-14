package store

import (
	"context"
	"time"
)

// ActiveUserAgent 是活跃用户版本分布的一行（对应 Node RawUserVersion 的 userId + userAgent）。
type ActiveUserAgent struct {
	UserID    int64  `json:"userId"`
	UserAgent string `json:"userAgent"`
}

// ActiveUserAgents 查询过去 days 天内活跃用户的 UA 分布。
//
// 与 Node src/repository/client-versions.ts 同一口径：按 (user_id, user_agent) 去重、
// 过滤软删用户、只看有过请求的窗口内活跃用户。版本检查据此算 GA 版本。
func (p *Pools) ActiveUserAgents(ctx context.Context, days int) ([]ActiveUserAgent, error) {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days)
	return readRowsAs[ActiveUserAgent](
		ctx,
		p,
		`SELECT row_to_json(t)::text FROM (
			SELECT DISTINCT message_request.user_id AS "userId", message_request.user_agent AS "userAgent"
			FROM message_request
			LEFT JOIN users ON message_request.user_id = users.id AND users.deleted_at IS NULL
			WHERE message_request.created_at >= $1 AND message_request.user_agent IS NOT NULL
		) t`,
		cutoff,
	)
}
