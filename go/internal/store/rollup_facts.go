package store

import (
	"context"
	"time"
)

// RollupFacts 是 public-status rollup 事件需要的行内事实。
//
// 为什么要回读行（而不是让结算方把模型传进来）：数据面的结算载荷（`terminal.Settlement`）
// 刻意**不含模型**——模型是「开行时」写进 `message_request` 的，而结算只写终态列。Node 侧的
// 对应处理正是「行内事实」：
//
//   - 主路径：开行时把 `createdAt/model/originalModel/durationMs` 存进进程内 seed 注册表
//     （`src/repository/message.ts:95-100`、`:119-135`）；
//   - 回退：seed 不在时**回读那一行**（`message.ts:184-206` 的
//     `readPublicStatusRequestSeedFallback`，查询列 `created_at/model/original_model/duration_ms`）。
//
// Go 的结算是一次调用就提交终态，进程内没有「插入」与「收尾」两个模块的裂缝，故直接取
// **回退路径**即可：一次按主键的只读查询，语义与 Node 的回退逐列一致。
type RollupFacts struct {
	// CreatedAt 是行创建时刻，决定 rollup 落在哪个 5 分钟桶。
	CreatedAt time.Time `json:"created_at"`
	// Model 是行上的模型名（开行时写入的原始请求模型）。
	Model *string `json:"model"`
	// OriginalModel 是原始模型列（Node 的 rollup 以它为准，见 resolveSuccessRateModelKey）。
	OriginalModel *string `json:"original_model"`
	// DurationMS 是行上的总耗时（结算载荷未带时用它兜底）。
	DurationMS *int `json:"duration_ms"`
}

// FindRollupFacts 读一行请求日志的 rollup 事实。
//
// 过滤条件与 Node 的回退查询逐条对齐（`message.ts:187-198`）：
//   - `deleted_at IS NULL`：软删行不计入公开统计；
//   - `ExcludeWarmupCondition`：预热抢答不是真实用户请求，Node 从公开统计里排除。
//
// 找不到行时返回 `ErrNotFound`（调用方按「本次不写 rollup」处理）。
func (p *Pools) FindRollupFacts(ctx context.Context, id int64) (*RollupFacts, error) {
	var facts RollupFacts
	err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT created_at, model, original_model, duration_ms
			FROM message_request
			WHERE id = $1
			  AND deleted_at IS NULL
			  AND `+ExcludeWarmupCondition+`
		) t`,
		&facts,
		[]any{id},
	)
	if err != nil {
		return nil, err
	}
	return &facts, nil
}
