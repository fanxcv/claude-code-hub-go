package dataplane

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// billGateSettings 是「非成功请求是否计费」的设置桩：同时回答取价基准与开关。
type billGateSettings struct {
	source            string
	billNonSuccessful bool
}

func (f billGateSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{
		BillingModelSource:        f.source,
		BillNonSuccessfulRequests: f.billNonSuccessful,
	}, nil
}

// TestCostResolverNonSuccessBillingGate 钉住非 2xx 的计费闸门两态。
//
// 为什么必须有这条：闸门是「钱」的分支，且两态的差别只在状态码与一个设置位上；
// 生产开着该开关（true），关掉时若不生效就会对失败请求扣费，打开时若不生效就会漏收。
func TestCostResolverNonSuccessBillingGate(t *testing.T) {
	prices := map[string]string{"model-a": priceJSON}
	ctx := context.Background()

	newResolver := func(billNonSuccessful bool) *costResolver {
		return newCostResolverWith(
			billGateSettings{source: "redirected", billNonSuccessful: billNonSuccessful},
			&fakeCostPrices{byModel: prices},
			&fakeCostGroups{},
			nil,
		)
	}
	input := func(status int) costInput {
		return costInput{
			RequestedModel:  "model-a",
			RedirectedModel: "model-a",
			StatusCode:      status,
			Usage:           usageAt(1000, 500),
		}
	}

	t.Run("非 2xx 且开关关闭：不计费", func(t *testing.T) {
		if cost := newResolver(false).resolve(ctx, input(499)); cost != nil {
			t.Fatalf("开关关闭时非 2xx 不该计费，得到 %+v", cost)
		}
	})

	t.Run("非 2xx 且开关打开：按用量计费", func(t *testing.T) {
		cost := newResolver(true).resolve(ctx, input(499))
		if cost == nil {
			t.Fatal("开关打开时非 2xx（上游已回报用量）应计费，得到 nil")
		}
	})

	t.Run("非 2xx 且开关打开但用量为零：不计费", func(t *testing.T) {
		zero := int64(0)
		in := input(499)
		in.Usage = usageAt(zero, zero)
		if cost := newResolver(true).resolve(ctx, in); cost != nil {
			t.Fatalf("零用量不该计费，得到 %+v", cost)
		}
	})

	t.Run("2xx 时开关不参与判定", func(t *testing.T) {
		if cost := newResolver(false).resolve(ctx, input(200)); cost == nil {
			t.Fatal("2xx 应计费，得到 nil")
		}
	})

	t.Run("状态码未知（0）按成功处理", func(t *testing.T) {
		// 改造前没有 StatusCode 这一列，0 是「调用方没给」的零值；它必须沿用旧行为，
		// 否则所有未接线状态码的调用方会在开关关闭时静默停止计费。
		if cost := newResolver(false).resolve(ctx, input(0)); cost == nil {
			t.Fatal("状态未知应按成功处理，得到 nil")
		}
	})
}
