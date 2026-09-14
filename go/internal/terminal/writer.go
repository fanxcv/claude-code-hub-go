package terminal

import (
	"context"
	"errors"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// ErrNotSettled 表示终态写没有赢得该行：行已是终态，或不存在。
// 调用方据此判定「本次调用不拥有该行」，不得发放后续对外可见副作用。
var ErrNotSettled = errors.New("terminal: 终态写未赢得该行（已终态或行不存在）")

// Writer 是本包需要的 store 面。定义成接口只为一件实事：让测试注入可控失败的假实现，
// 从而在没有数据库时覆盖重试、幂等与失败分支。生产装配用 StoreWriter。
type Writer interface {
	CreateMessageRequest(
		ctx context.Context,
		data store.CreateMessageRequestData,
	) (store.MessageRequest, error)
	UpdateDetailsIfUnfinalized(
		ctx context.Context,
		id int64,
		patch store.DetailsPatch,
	) (bool, error)
	UpdateWinnerCost(ctx context.Context, id int64, winnerCost string, costBreakdown []byte) error
	FindModelPrice(ctx context.Context, modelName string) (*store.ModelPrice, error)
}

// StoreWriter 把 *store.Pools 适配成 Writer。
type StoreWriter struct {
	Pools *store.Pools
}

func (w StoreWriter) CreateMessageRequest(
	ctx context.Context,
	data store.CreateMessageRequestData,
) (store.MessageRequest, error) {
	return w.Pools.CreateMessageRequest(ctx, data)
}

func (w StoreWriter) UpdateDetailsIfUnfinalized(
	ctx context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	return w.Pools.UpdateDetailsIfUnfinalized(ctx, id, patch)
}

func (w StoreWriter) UpdateWinnerCost(
	ctx context.Context,
	id int64,
	winnerCost string,
	costBreakdown []byte,
) error {
	return w.Pools.UpdateWinnerCost(ctx, id, winnerCost, costBreakdown)
}

func (w StoreWriter) FindModelPrice(ctx context.Context, modelName string) (*store.ModelPrice, error) {
	price, err := w.Pools.FindModelPrice(ctx, modelName)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrPriceNotFound
	}
	return price, err
}

// FindRollupFacts 把行事实读取面接到 store 上（见 store/rollup_facts.go）。
//
// 它**不在 Writer 接口里**：只有装配了 Rollup 旁路时才需要这一读，把它塞进 Writer 会让所有
// 既有测试替身（只关心终态与成本的那些）被迫实现一个用不到的读取面。旁路侧用可选接口断言。
func (w StoreWriter) FindRollupFacts(ctx context.Context, id int64) (*store.RollupFacts, error) {
	return w.Pools.FindRollupFacts(ctx, id)
}
