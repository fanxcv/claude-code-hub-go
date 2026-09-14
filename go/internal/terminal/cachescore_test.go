package terminal

import "testing"

// 用例逐条对着 Node 的 src/lib/cache-effectiveness/gate.ts 写：
// 每个分支（无亲和 / 失败 / 不可观测 / 截断 / 合格）与两个边界（prefixBytes 取整、
// 空 CacheTTL 归 5m）都必须有自己的用例，且断言到字段级。
func TestComputeCacheScoreFieldsMatchesGateSpec(t *testing.T) {
	str := func(v string) *string { return &v }
	i64 := func(v int64) *int64 { return &v }

	cases := []struct {
		name string
		in   CacheScoreInput
		want CacheScoreFields
	}{
		{
			name: "无作用域（无法指纹化）→ 只有排除原因，其余全 NULL",
			in:   CacheScoreInput{Succeeded: true, UsageObservable: true, HasTip: true, TipPrefixBytes: 400},
			want: CacheScoreFields{
				CacheScoreEligible:       false,
				CacheScoreExcludedReason: str(CacheScoreExcludedNoAffinityKey),
			},
		},
		{
			name: "有作用域但两个指纹都空 → 同样是 no_affinity_key",
			in:   CacheScoreInput{ScopeTag: "abc", Succeeded: true, UsageObservable: true},
			want: CacheScoreFields{
				CacheScoreEligible:       false,
				CacheScoreExcludedReason: str(CacheScoreExcludedNoAffinityKey),
			},
		},
		{
			name: "命中指纹优先于 tip 指纹（Matched > Tip）",
			in: CacheScoreInput{
				ScopeTag: "abc", MatchedFingerprint: "m1", TipFingerprint: "t1",
				HasTip: true, TipPrefixBytes: 10, Succeeded: true, UsageObservable: true,
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:    str("abc:m1"),
				CacheScoreEligible:       true,
				CacheScoreExcludedReason: nil,
				TheoreticalCacheTokens:   i64(2), // 10 / 4 取整
				CacheTTLBucket:           str("5m"),
			},
		},
		{
			name: "未命中时回落到 tip 指纹；空 TTL 归入 5m 桶",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1",
				HasTip: true, TipPrefixBytes: 7, Succeeded: true, UsageObservable: true,
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:  str("abc:t1"),
				CacheScoreEligible:     true,
				TheoreticalCacheTokens: i64(1), // 7 / 4 取整
				CacheTTLBucket:         str("5m"),
			},
		},
		{
			name: "显式 TTL 原样进桶（1h）",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1", HasTip: true, TipPrefixBytes: 40,
				Succeeded: true, UsageObservable: true, CacheTTL: "1h",
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:  str("abc:t1"),
				CacheScoreEligible:     true,
				TheoreticalCacheTokens: i64(10),
				CacheTTLBucket:         str("1h"),
			},
		},
		{
			name: "无 tip → theoretical 必须 NULL 而不是 0（Node 的 `tip ? ... : null`）",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1", HasTip: false,
				Succeeded: true, UsageObservable: true, CacheTTL: "mixed",
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:  str("abc:t1"),
				CacheScoreEligible:     true,
				TheoreticalCacheTokens: nil,
				CacheTTLBucket:         str("mixed"),
			},
		},
		{
			name: "非成功终态 → attempt_failed（但兼容键与理论量仍落）",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1", HasTip: true, TipPrefixBytes: 4000,
				Succeeded: false, UsageObservable: true,
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:    str("abc:t1"),
				CacheScoreEligible:       false,
				CacheScoreExcludedReason: str(CacheScoreExcludedAttemptFailed),
				TheoreticalCacheTokens:   i64(1000),
				CacheTTLBucket:           str("5m"),
			},
		},
		{
			name: "成功但上游未报 usage → not_observable",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1", HasTip: true, TipPrefixBytes: 40,
				Succeeded: true, UsageObservable: false,
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:    str("abc:t1"),
				CacheScoreEligible:       false,
				CacheScoreExcludedReason: str(CacheScoreExcludedNotObservable),
				TheoreticalCacheTokens:   i64(10),
				CacheTTLBucket:           str("5m"),
			},
		},
		{
			name: "失败优先于不可观测（门控短路顺序）",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1", HasTip: true, TipPrefixBytes: 40,
				Succeeded: false, UsageObservable: false, StreamTruncated: true,
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:    str("abc:t1"),
				CacheScoreEligible:       false,
				CacheScoreExcludedReason: str(CacheScoreExcludedAttemptFailed),
				TheoreticalCacheTokens:   i64(10),
				CacheTTLBucket:           str("5m"),
			},
		},
		{
			name: "不可观测优先于截断",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1", HasTip: true, TipPrefixBytes: 40,
				Succeeded: true, UsageObservable: false, StreamTruncated: true,
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:    str("abc:t1"),
				CacheScoreEligible:       false,
				CacheScoreExcludedReason: str(CacheScoreExcludedNotObservable),
				TheoreticalCacheTokens:   i64(10),
				CacheTTLBucket:           str("5m"),
			},
		},
		{
			name: "流被截断 → stream_truncated",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1", HasTip: true, TipPrefixBytes: 40,
				Succeeded: true, UsageObservable: true, StreamTruncated: true,
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:    str("abc:t1"),
				CacheScoreEligible:       false,
				CacheScoreExcludedReason: str(CacheScoreExcludedStreamTruncated),
				TheoreticalCacheTokens:   i64(10),
				CacheTTLBucket:           str("5m"),
			},
		},
		{
			name: "前缀不足一个 token → 取整为 0（不是 NULL）",
			in: CacheScoreInput{
				ScopeTag: "abc", TipFingerprint: "t1", HasTip: true, TipPrefixBytes: 3,
				Succeeded: true, UsageObservable: true,
			},
			want: CacheScoreFields{
				CacheCompatibilityKey:  str("abc:t1"),
				CacheScoreEligible:     true,
				TheoreticalCacheTokens: i64(0),
				CacheTTLBucket:         str("5m"),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeCacheScoreFields(tc.in)
			assertStringPointer(t, "CacheCompatibilityKey", got.CacheCompatibilityKey, tc.want.CacheCompatibilityKey)
			assertStringPointer(t, "CacheScoreExcludedReason", got.CacheScoreExcludedReason, tc.want.CacheScoreExcludedReason)
			assertStringPointer(t, "CacheTTLBucket", got.CacheTTLBucket, tc.want.CacheTTLBucket)
			if got.CacheScoreEligible != tc.want.CacheScoreEligible {
				t.Fatalf("CacheScoreEligible = %v，期望 %v", got.CacheScoreEligible, tc.want.CacheScoreEligible)
			}
			switch {
			case tc.want.TheoreticalCacheTokens == nil:
				if got.TheoreticalCacheTokens != nil {
					t.Fatalf("TheoreticalCacheTokens = %d，期望 NULL", *got.TheoreticalCacheTokens)
				}
			case got.TheoreticalCacheTokens == nil:
				t.Fatalf("TheoreticalCacheTokens = NULL，期望 %d", *tc.want.TheoreticalCacheTokens)
			case *got.TheoreticalCacheTokens != *tc.want.TheoreticalCacheTokens:
				t.Fatalf("TheoreticalCacheTokens = %d，期望 %d", *got.TheoreticalCacheTokens, *tc.want.TheoreticalCacheTokens)
			}
		})
	}
}

// 合格分支的排除原因必须是 NULL（不是空串）：落库时 NULL 与 ” 在下游聚合里
// 走不同分支（normalizeExcludedReason 把 ” 当无原因）。
func TestComputeCacheScoreFieldsEligibleHasNoReason(t *testing.T) {
	fields := ComputeCacheScoreFields(CacheScoreInput{
		ScopeTag: "s", TipFingerprint: "f", HasTip: true, TipPrefixBytes: 4,
		Succeeded: true, UsageObservable: true,
	})
	if !fields.CacheScoreEligible {
		t.Fatal("合格请求应标记为 eligible")
	}
	if fields.CacheScoreExcludedReason != nil {
		t.Fatalf("合格请求的排除原因必须为 NULL，实际 %q", *fields.CacheScoreExcludedReason)
	}
}

func assertStringPointer(t *testing.T, name string, got, want *string) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("%s = %q，期望 NULL", name, *got)
	case want != nil && got == nil:
		t.Fatalf("%s = NULL，期望 %q", name, *want)
	case want != nil && got != nil && *got != *want:
		t.Fatalf("%s = %q，期望 %q", name, *got, *want)
	}
}
