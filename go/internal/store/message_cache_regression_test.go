package store

import (
	"context"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

// 不写 cache_read 就不该为观测两列多查一次库。用 nil 池调推导即可证明这一点：
// 真去查库会在 pool.QueryRow 处 panic，能原样返回才说明提前返回生效了。
func TestDeriveCacheRegressionSkipsWithoutCacheRead(t *testing.T) {
	token := int64(10)
	flag := true
	cases := []struct {
		name  string
		patch DetailsPatch
	}{
		{name: "本次不写 cache_read", patch: DetailsPatch{InputTokens: &token}},
		{name: "调用方已给出回退事实", patch: DetailsPatch{CacheReadInputTokens: &token, CacheRegressed: &flag}},
		{name: "调用方已给出上一行读数", patch: DetailsPatch{CacheReadInputTokens: &token, PrevCacheReadTokens: &token}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := deriveCacheRegression(context.Background(), nil, 1, testCase.patch)
			if got.CacheRegressed != testCase.patch.CacheRegressed {
				t.Fatalf("推导不该改写调用方给定的 cache_regressed: %v -> %v",
					testCase.patch.CacheRegressed, got.CacheRegressed)
			}
			if got.PrevCacheReadTokens != testCase.patch.PrevCacheReadTokens {
				t.Fatal("推导不该改写调用方给定的 prev_cache_read_tokens")
			}
		})
	}
}

// 推导失败不得让终态写失败。用「准入被拒」的池触发一次真实读失败
// （maxOutstanding=0 时 QueryRow 返回错误行），确认推导静默让位、patch 其余部分原样交给终态写。
func TestDeriveCacheRegressionSwallowsLookupFailure(t *testing.T) {
	rejected := &Pool{lane: config.LaneData, maxOutstanding: 0}
	cacheRead := int64(120)
	got := deriveCacheRegression(context.Background(), rejected, 1, DetailsPatch{CacheReadInputTokens: &cacheRead})
	if got.CacheRegressed != nil || got.PrevCacheReadTokens != nil {
		t.Fatalf("读失败时两列应留 NULL，实际 regressed=%v prev=%v",
			got.CacheRegressed, got.PrevCacheReadTokens)
	}
	if got.CacheReadInputTokens == nil || *got.CacheReadInputTokens != cacheRead {
		t.Fatal("读失败不得影响 cache_read 本身落库")
	}
}

// 观测两列必须能被普通 patch 表达（各自占一个占位符），否则终态列集审计
// （terminal.setColumnsOf 解析 `"列" = $n`）看不见它们，写没写、写对没写都无从断言。
func TestBuildDetailsPatchQueryCarriesRegressionColumns(t *testing.T) {
	cacheRead := int64(120)
	previous := int64(300)
	regressed := true
	query, args := BuildDetailsPatchQuery(7, DetailsPatch{
		CacheReadInputTokens: &cacheRead,
		PrevCacheReadTokens:  &previous,
		CacheRegressed:       &regressed,
	})

	for _, fragment := range []string{
		`"cache_read_input_tokens" = $`,
		`"prev_cache_read_tokens" = $`,
		`"cache_regressed" = $`,
	} {
		if !strings.Contains(query, fragment) {
			t.Fatalf("SET 里缺少 %s：%s", fragment, query)
		}
	}
	// 一列一个参数，最后一个参数是 id——与终态 patch 的既有约定一致。
	if len(args) != 4 {
		t.Fatalf("参数个数 = %d，want 4（三列 + id）", len(args))
	}
	if got, ok := args[2].(bool); !ok || !got {
		t.Fatalf("第三个参数应为 cache_regressed 的 bool 值，实际 %#v", args[2])
	}

	// 反向：不置位时两列不得出现在语句里（既有形状不变）。
	plain, _ := BuildDetailsPatchQuery(7, DetailsPatch{CacheReadInputTokens: &cacheRead})
	if strings.Contains(plain, "prev_cache_read_tokens") || strings.Contains(plain, "cache_regressed") {
		t.Fatalf("未置位的观测列不应出现在 SET 里：%s", plain)
	}
}
