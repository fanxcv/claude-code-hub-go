package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// 本文件是「按 id 取一条通知绑定」的读面，对应 Node 的 getBindingById
// （src/repository/notification-bindings.ts:125）。
//
// 投递期为什么还要再读一次绑定：绑定级的 templateOverride 只在**投递那一刻**参与模板渲染
// （Node notification-queue.ts:573-581 在处理作业时现取），而调度任务里只带着 bindingId。
//
// 与 AdminListNotificationBindings 的差别：Node 的 getBindingById 只 select 绑定行本身、
// 不 join 目标（目标由投递层按 targetId 单独取），本函数照此——就不必造一份带目标快照的
// 重结构，也避免多一次无用的 join。

// AdminNotificationBindingRef 是一条绑定的引用（无目标快照）。
type AdminNotificationBindingRef struct {
	ID               int64           `json:"id"`
	NotificationType string          `json:"notificationType"`
	TargetID         int64           `json:"targetId"`
	IsEnabled        bool            `json:"isEnabled"`
	TemplateOverride json.RawMessage `json:"templateOverride"`
}

// AdminNotificationBindingByID 取一条绑定；不存在时回 (nil, nil)——与 Node 的 null 同语义。
//
// 找不到绑定不是错误：Node 侧 `binding?.templateOverride ?? null` 会让投递继续（只是不带覆盖）。
func (p *Pools) AdminNotificationBindingByID(
	ctx context.Context,
	id int64,
) (*AdminNotificationBindingRef, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT b.id,
			b.notification_type AS "notificationType",
			b.target_id AS "targetId",
			COALESCE(b.is_enabled, true) AS "isEnabled",
			b.template_override AS "templateOverride"
		FROM notification_target_bindings b
		WHERE b.id = $1
		LIMIT 1) t`
	var payload string
	if err := pool.QueryRow(ctx, query, id).Scan(&payload); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 查询通知绑定失败: %w", err)
	}
	var binding AdminNotificationBindingRef
	if err := json.Unmarshal([]byte(payload), &binding); err != nil {
		return nil, fmt.Errorf("store: 通知绑定行反序列化失败: %w", err)
	}
	return &binding, nil
}
