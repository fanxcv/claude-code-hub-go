package route

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// TestIntegrationModelCoverageMatchesDatabase 用**真库**断言「模型覆盖计数」与库里的事实一致。
//
// 为何必须有真库这一环：计数本身是纯函数累加（桩数据单测已覆盖），但它的**输入**来自
// store 的选路视图（`allowed_models` 那一列）。若视图少读或读错这一列，桩数据单测永远是绿的，
// 而生产上「模型没人支持」会被误判成「供应商不可用」——正是本任务要消灭的误诊。
//
// 口径：用一个不可能出现在任何显式规则里的随机模型名，于是**只有「空允许集 = 全放行」的供应商**
// 会被算作支持；这同时可以用一条 SQL 独立数出来，形成交叉验证。
func TestIntegrationModelCoverageMatchesDatabase(t *testing.T) {
	pools := integrationPools(t)
	source := NewStoreSource(pools)
	ctx := context.Background()

	unknown := fmt.Sprintf("zz-unknown-%d", time.Now().UnixNano())
	selector := NewSelector(Options{Source: source})

	// 为何要重试：本用例做两遍独立读取（本地取一份供应商列表 + 选路器自己再读一次 + SQL 数一次），
	// 而**同一次 `go test ./...` 里别的包也在增删供应商**（本仓已知的共享夹具干扰类）。
	// 三包并行（-race 下更慢）时窗口拉开，两次读到的家数会不一样——那是环境漂移，不是逻辑错。
	// 逻辑错会**每次都**对不上，故重试耗尽仍报错（带全部观测数字）。
	const attempts = 3
	var lastMismatch string
	for attempt := 1; attempt <= attempts; attempt++ {
		providers, err := source.Providers(ctx)
		if err != nil {
			t.Fatalf("读取供应商失败: %v", err)
		}
		if len(providers) == 0 {
			t.Skip("库中没有启用态供应商，跳过该断言")
		}

		// 不带 Group：不做分组过滤，故 totalProviders 应等于源里读到的家数（口径可直接对账）。
		result, err := selector.Select(ctx, Request{Model: unknown})
		if err != nil {
			t.Fatalf("选路失败: %v", err)
		}
		allowAll, wildcardish := countAllowAllProviders(t, pools)

		wantFromView := 0
		for _, p := range providers {
			if providerSupportsModel(p, unknown) {
				wantFromView++
			}
		}

		consistent := result.Context.TotalProviders == len(providers) &&
			result.Context.ModelSupportedProviders == wantFromView &&
			(wildcardish != 0 || result.Context.ModelSupportedProviders == allowAll)
		if consistent {
			// 对账三：（独立计算）上一条件在 wildcardish==0 时已把 SQL 家数一并比过。
			return
		}
		lastMismatch = fmt.Sprintf(
			"第 %d 次：本地读到 %d 家 / 选路器读到 %d 家 / 视图判定支持 %d / 选路器覆盖 %d / SQL 全放行 %d / 通配规则 %d",
			attempt, len(providers), result.Context.TotalProviders, wantFromView,
			result.Context.ModelSupportedProviders, allowAll, wildcardish)
		t.Logf("共享库在两次读取之间发生漂移（环境现象，重试）：%s", lastMismatch)
		time.Sleep(200 * time.Millisecond)
	}

	// 重试耗尽：要么是逻辑错（每次都对不上），要么环境持续在写；两者都该让人看见。
	t.Fatalf("模型覆盖计数与库中事实对不上（若伴随其它包的供应商夹具写入，属共享库漂移）：%s",
		lastMismatch)
}

// countAllowAllProviders 数「空/NULL 允许集（= 全放行）」的启用供应商，
// 并返回「带通配/前缀/包含类规则」的家数（后者会破坏随机名不被命中的前提）。
func countAllowAllProviders(t *testing.T, pools *store.Pools) (int, int) {
	t.Helper()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取数据分道失败: %v", err)
	}
	var allowAll, wildcardish int
	// 与选路视图同源的筛选条件（见 store.FindEnabledProviders）。
	//
	// 「全放行」的判定必须防住库里的**非数组**值：本库实测有行的 allowed_models 是标量/
	// JSON null（jsonb_array_length 会直接报错）。而 Go 的 normalizeAllowedModelRules 对
	// 解不成数组的值一律返回 ok=false（= 放行），故这里把「非数组」也算进全放行口径，
	// 两侧语义才不会分叉。
	if err := pool.QueryRow(context.Background(), `
		SELECT
		  count(*) FILTER (
		    WHERE allowed_models IS NULL
		       OR jsonb_typeof(allowed_models) = 'null'
		       OR jsonb_typeof(allowed_models) <> 'array'
		       OR jsonb_array_length(allowed_models) = 0
		  )::int,
		  count(*) FILTER (
		    WHERE jsonb_typeof(allowed_models) = 'array'
		      AND allowed_models::text ~ '(prefix|suffix|contains|glob|regex)'
		  )::int
		FROM providers
		WHERE is_enabled = true AND deleted_at IS NULL`,
	).Scan(&allowAll, &wildcardish); err != nil {
		t.Fatalf("统计允许集失败: %v", err)
	}
	return allowAll, wildcardish
}

// TestIntegrationUnknownModelIsAcceptedWhenSomeoneAllowsAll 记录**环境事实**：
// 只要库里还有「空允许集 = 全放行」的供应商，任何模型名都会被它们接住，
// 于是「模型无人支持」这种 503 在**本环境不可达**（除非全库都显式配了允许集）。
//
// 这不是产品行为，而是共享测试库的现状；把它写成用例是为了让后来者一眼看到
// 「为什么端到端的覆盖=0 只能靠桩夹具验」，而不是怀疑代码没生效。
func TestIntegrationUnknownModelIsAcceptedWhenSomeoneAllowsAll(t *testing.T) {
	pools := integrationPools(t)
	allowAll, _ := countAllowAllProviders(t, pools)
	if allowAll == 0 {
		t.Skip("库里没有空允许集的供应商，该环境事实不适用")
	}
	t.Logf("库中有 %d 家启用供应商为空允许集（全放行）→ 任意模型名都有候选，"+
		"端到端的 model_matches_no_provider 在本环境不可达", allowAll)
}
