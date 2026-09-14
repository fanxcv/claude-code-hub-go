package route

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住熔断在**选路**里的三档语义（用户问：「熔断过程中的供应商不是不应该被决策链选取么？」）：
//
//	open 且窗口未过期  → **排除**（ReasonCircuitOpen，details=circuit_open）
//	open 但窗口已过期  → **放行试探**（Node 语义：过期即视作 half-open）
//	half-open          → **放行试探**
//
// 为什么值得单独钉：排除过严会让「熔断到期」永远恢复不了（请求全被拒 → 没有成功去推进半开计数）；
// 排除过松会让真熔断形同虚设。两边的错法在界面上的表现几乎一样（都显示「熔断」），
// 只能靠选路结果的 reason/details 与链上 circuitState 区分。
//
// 为什么另有一份真 Redis 变体（本文件末尾）：本变体用假 Redis 把**判定矩阵**钉死（快、无需环境），
// 真 Redis 变体证明「同一条判定在真键形制上成立」——键名/字段名写错时只有后者会红。
func TestCircuitSelectionSemantics(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		state        map[string]string
		wantFiltered bool
		wantDetails  string
		// wantChainState 是落链的 circuitState（Node 的 getCircuitState）。
		// 注意与「是否被排除」是两个判据：half-open 是**放行**的，但它必须如实记成 half-open，
		// 折成 closed 会让链上看不出「这家正在试探恢复」。
		wantChainState CircuitState
		why            string
	}{
		{
			name: "open 且窗口未过期 → 排除",
			state: map[string]string{
				"circuitState":     "open",
				"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
				"failureCount":     "9",
			},
			wantFiltered:   true,
			wantDetails:    "circuit_open",
			wantChainState: StateClosed, // 被排除者不进链首（无选中结果）
			why:            "真熔断：窗口还在，必须不选它，否则熔断形同虚设",
		},
		{
			name: "open 但窗口已过期 → 放行试探，且链上记 half-open",
			state: map[string]string{
				"circuitState":     "open",
				"circuitOpenUntil": strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
				"failureCount":     "11",
			},
			wantFiltered:   false,
			wantChainState: StateHalfOpen,
			why:            "生产 156/145 的同款形态：过期即 half-open，必须放行试探，否则熔断永远回不到 closed",
		},
		{
			name: "half-open → 放行试探，且链上记 half-open",
			state: map[string]string{
				"circuitState":         "half-open",
				"circuitOpenUntil":     strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
				"failureCount":         "11",
				"halfOpenSuccessCount": "1",
			},
			wantFiltered:   false,
			wantChainState: StateHalfOpen,
			why:            "半开期就是靠放行试探来判定能否归闭；raw 已是 half-open 时必须如实落链（Node 的 getCircuitState 读内存态即如此）",
		},
		{
			name:           "closed → 放行",
			state:          map[string]string{"circuitState": "closed"},
			wantFiltered:   false,
			wantChainState: StateClosed,
			why:            "常态",
		},
		{
			name:           "键缺失 → 视为 closed 放行",
			state:          nil,
			wantFiltered:   false,
			wantChainState: StateClosed,
			why:            "状态缺失一律 closed（与 Node 同判）",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := baseProvider(1, convert.ProviderClaude)
			client := &fakeRedis{hashes: map[string]map[string]string{}}
			if tc.state != nil {
				client.hashes[providerStateKey(provider.ID)] = tc.state
			}
			selector := NewSelector(Options{
				Source: &stubSource{providers: []Provider{provider}},
				Health: newTestHealth(t, client, true),
			})

			result, err := selector.Select(context.Background(), Request{})
			if err != nil {
				t.Fatalf("选路失败: %v", err)
			}

			reason := filteredReasonOrEmpty(result.Context, provider.ID)
			if tc.wantFiltered {
				if reason != ReasonCircuitOpen {
					t.Fatalf("%s：期望被熔断排除（reason=%q），实际 reason=%q（%s）",
						tc.name, ReasonCircuitOpen, reason, tc.why)
				}
				if result.Provider != nil {
					t.Fatalf("%s：唯一候选被排除时不应选出供应商，实际 %+v", tc.name, result.Provider)
				}
				if details := filteredDetailsOrEmpty(result.Context, provider.ID); details != tc.wantDetails {
					t.Fatalf("%s：details 期望 %q，实际 %q", tc.name, tc.wantDetails, details)
				}
				return
			}

			if reason == ReasonCircuitOpen {
				t.Fatalf("%s：不应被熔断排除，实际 reason=%q（%s）", tc.name, reason, tc.why)
			}
			if result.Provider == nil {
				t.Fatalf("%s：应被放行并选中，实际未选出供应商", tc.name)
			}
			if result.Provider.ID != provider.ID {
				t.Fatalf("%s：选中的应是唯一候选 %d，实际 %d", tc.name, provider.ID, result.Provider.ID)
			}
			if result.CircuitState != tc.wantChainState {
				t.Fatalf("%s：链上 circuitState 期望 %q，实际 %q（%s）",
					tc.name, tc.wantChainState, result.CircuitState, tc.why)
			}
		})
	}
}

// TestCircuitSelectionOpenWindowBlocksOnlyThatProvider 钉住「排除是逐个供应商判定的」：
// 同一次选路里，处于 open 窗口内的被排除、其余照常参与——不是「有一个熔断就整体失败」。
func TestCircuitSelectionOpenWindowBlocksOnlyThatProvider(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	blocked := baseProvider(1, convert.ProviderClaude)
	healthy := baseProvider(2, convert.ProviderClaude)
	client := &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(blocked.ID): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
	}}
	selector := NewSelector(Options{
		Source: &stubSource{providers: []Provider{blocked, healthy}},
		Health: newTestHealth(t, client, true),
	})

	result, err := selector.Select(context.Background(), Request{})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != healthy.ID {
		t.Fatalf("应选中健康的那家，实际 %+v", result.Provider)
	}
	if got := filteredReasonOrEmpty(result.Context, blocked.ID); got != ReasonCircuitOpen {
		t.Fatalf("被熔断那家的理由应为 %q，实际 %q", ReasonCircuitOpen, got)
	}
	if got := filteredReasonOrEmpty(result.Context, healthy.ID); got != "" {
		t.Fatalf("健康那家不应有过滤理由，实际 %q", got)
	}
}

// TestProviderStateReturnsHalfOpenForProbingProvider 单钉读取侧的三态：raw=half-open 必须回 half-open。
//
// 这条曾错过一次：ProviderState 把 raw=half-open 折成 closed，于是**正在试探恢复**的供应商
// 在链记录里显示成 closed（Node 的 getCircuitState 读内存态、会回 half-open）。
// 折成 closed 不影响选路（放行判定在 ProviderOpen，看的是窗口是否过期），但会让
// 「这家是不是在试探恢复」这一列失真——而它正是排查熔断问题的唯一入口。
func TestProviderStateReturnsHalfOpenForProbingProvider(t *testing.T) {
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	reader := newTestHealth(t, &fakeRedis{hashes: map[string]map[string]string{
		providerStateKey(1): {
			"circuitState":         "half-open",
			"halfOpenSuccessCount": "1",
		},
		providerStateKey(2): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
		},
		providerStateKey(3): {
			"circuitState":     "open",
			"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
		},
		providerStateKey(4): {"circuitState": "closed"},
	}}, true)

	cases := map[int64]CircuitState{1: StateHalfOpen, 2: StateHalfOpen, 3: StateOpen, 4: StateClosed}
	for id, want := range cases {
		if got := reader.ProviderState(context.Background(), id); got != want {
			t.Fatalf("供应商 %d 的链上状态期望 %q，实际 %q", id, want, got)
		}
	}
	// 键缺失（未熔断过的供应商）一律 closed。
	if got := reader.ProviderState(context.Background(), 99); got != StateClosed {
		t.Fatalf("键缺失应视为 closed，实际 %q", got)
	}
}

// TestIntegrationCircuitSelectionSemanticsAgainstRealRedis 是真 Redis 变体：
// 同一条判定矩阵必须在**真键形制**上成立（键名/字段名写错时只有本用例会红）。
//
// 环境门控：未设 CCH_TEST_REDIS_URL 时整例跳过（与 route 包其它集成用例同一约定）。
func TestIntegrationCircuitSelectionSemanticsAgainstRealRedis(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name         string
		state        map[string]any
		wantFiltered bool
		wantChain    CircuitState
	}{
		{
			name: "open 窗口内 → 排除",
			state: map[string]any{
				"circuitState":     "open",
				"circuitOpenUntil": strconv.FormatInt(now.Add(time.Hour).UnixMilli(), 10),
				"failureCount":     "9",
			},
			wantFiltered: true,
			wantChain:    StateClosed, // 被排除者不进链首
		},
		{
			name: "open 窗口已过期 → 放行试探（half-open）",
			state: map[string]any{
				"circuitState":     "open",
				"circuitOpenUntil": strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
				"failureCount":     "11",
			},
			wantFiltered: false,
			wantChain:    StateHalfOpen,
		},
		{
			name: "raw half-open → 放行试探（half-open）",
			state: map[string]any{
				"circuitState":         "half-open",
				"circuitOpenUntil":     strconv.FormatInt(now.Add(-time.Minute).UnixMilli(), 10),
				"failureCount":         "11",
				"halfOpenSuccessCount": "1",
			},
			wantFiltered: false,
			wantChain:    StateHalfOpen,
		},
		{
			name:         "closed → 放行",
			state:        map[string]any{"circuitState": "closed"},
			wantFiltered: false,
			wantChain:    StateClosed,
		},
	}

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := baseProvider(int64(9000+index), convert.ProviderClaude)
			key := providerStateKey(provider.ID)
			t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
			if err := client.HSet(ctx, key, tc.state).Err(); err != nil {
				t.Fatalf("写熔断状态失败: %v", err)
			}

			selector := NewSelector(Options{
				Source: &stubSource{providers: []Provider{provider}},
				Health: newTestHealth(t, client, true),
			})
			result, err := selector.Select(ctx, Request{})
			if err != nil {
				t.Fatalf("选路失败: %v", err)
			}

			reason := filteredReasonOrEmpty(result.Context, provider.ID)
			if tc.wantFiltered {
				if reason != ReasonCircuitOpen || result.Provider != nil {
					t.Fatalf("期望被排除，实际 reason=%q provider=%+v", reason, result.Provider)
				}
				return
			}
			if reason == ReasonCircuitOpen {
				t.Fatalf("不应被排除，实际 reason=%q", reason)
			}
			if result.Provider == nil {
				t.Fatal("应被放行并选中，实际未选出供应商")
			}
			if result.CircuitState != tc.wantChain {
				t.Fatalf("链上 circuitState 期望 %q，实际 %q", tc.wantChain, result.CircuitState)
			}
		})
	}
}

// filteredReasonOrEmpty 取「被过滤时记下的理由」，无记录返回空。
//
// 不用 recordOf/reasonOf：那两个助手把「无记录」当断言失败（它们服务于「该供应商已被过滤」的断言），
// 而**放行的供应商本来就没有记录**。早先一版就是这么误用，把「语义正确」误报成两条红。
func filteredReasonOrEmpty(dc DecisionContext, id int64) Reason {
	for _, record := range dc.FilteredProviders {
		if record.ID == id {
			return record.Reason
		}
	}
	return ""
}

// filteredDetailsOrEmpty 同上，取 details。
func filteredDetailsOrEmpty(dc DecisionContext, id int64) string {
	for _, record := range dc.FilteredProviders {
		if record.ID == id {
			return record.Details
		}
	}
	return ""
}
