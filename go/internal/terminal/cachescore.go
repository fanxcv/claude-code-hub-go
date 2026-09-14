package terminal

// F3b 缓存模拟列。逐字对齐 Node 的 src/lib/cache-effectiveness/gate.ts
// （它是 CCHP finalize/cache_score.go 的移植），纯函数、无 IO。
//
// 为什么放在 terminal 包：这五个字段是**终态落库列**（message_request 的
// cache_compatibility_key / cache_score_eligible / cache_score_excluded_reason /
// theoretical_cache_tokens / cache_ttl_bucket），而终态列只允许在终态写里落一次。
// 派生规则的唯一真源是上面那份 TS；本文件是它的等价实现，两者不一致即为缺陷。
// 对比证据见。

// 排除原因的取值域，逐字对齐 gate.ts 的 CACHE_SCORE_EXCLUDED。
const (
	// CacheScoreExcludedNoAffinityKey 表示无法指纹化（无亲和或指纹缺失）。
	CacheScoreExcludedNoAffinityKey = "no_affinity_key"
	// CacheScoreExcludedAttemptFailed 表示终态不是成功交付。
	CacheScoreExcludedAttemptFailed = "attempt_failed"
	// CacheScoreExcludedNotObservable 表示上游没报可观测 usage（input tokens 缺失）。
	CacheScoreExcludedNotObservable = "not_observable"
	// CacheScoreExcludedStreamTruncated 表示流未自然结束。
	CacheScoreExcludedStreamTruncated = "stream_truncated"
)

// bytesPerToken 是「规范化前缀字节 → token」的粗估系数（gate.ts 的 BYTES_PER_TOKEN）。
const bytesPerToken = 4

// CacheScoreInput 是一次派生所需的请求级事实。
//
// 亲和部分刻意与选路包解耦：本包不依赖 route，调用方（数据面）把选路固化下来的
// 两个候选指纹与 tip 前缀字节传进来，优先级解析（Matched > Tip）由**本函数**做——
// 与 gate.ts 的 `matchedFp ?? tip?.fp ?? chain.sys.fp` 同序，避免同一条规则散落两处。
type CacheScoreInput struct {
	// ScopeTag 是亲和键作用域（密钥 × 格式 × 模型）。为空表示本次无亲和。
	ScopeTag string
	// MatchedFingerprint 是本次命中的绑定指纹；未命中或未提名时为空。
	MatchedFingerprint string
	// TipFingerprint 是指纹链最深边界；TipDepth 为 0 时它就是系统段指纹
	// （gate.ts 的 `chain.sys.fp` 兜底正是这一情形）。
	TipFingerprint string
	// TipPrefixBytes 是 tip 边界的前缀字节数。
	TipPrefixBytes int
	// HasTip 为 false 表示没有 tip（无链）。Node 用 `tip ? ... : null` 区分，
	// 此时 theoreticalCacheTokens 必须为 NULL 而不是 0。
	HasTip bool
	// Succeeded 是终态是否 2xx 成功交付（Node 的 finalized.isSuccessfulCompletion）。
	Succeeded bool
	// UsageObservable 是上游是否报告了可观测 usage（Node 判据：input_tokens 非 null）。
	UsageObservable bool
	// StreamTruncated 是流是否被截断（未自然结束）。
	StreamTruncated bool
	// CacheTTL 是实际应用的缓存 TTL（"5m"/"1h"/"mixed"）；空串归入 "5m" 桶。
	CacheTTL string
}

// CacheScoreFields 是要落库的五个字段。指针为 nil 表示该列留 NULL。
type CacheScoreFields struct {
	CacheCompatibilityKey    *string
	CacheScoreEligible       bool
	CacheScoreExcludedReason *string
	TheoreticalCacheTokens   *int64
	CacheTTLBucket           *string
}

// ComputeCacheScoreFields 复刻 gate.ts 的 computeCacheScoreFields。
//
// 门控顺序（短路）：no_affinity_key -> attempt_failed -> not_observable -> stream_truncated -> eligible。
// 顺序不可调换：它决定 excluded_reason 取哪一个值，而下游按该值决定是否纳入窗口聚合。
func ComputeCacheScoreFields(in CacheScoreInput) CacheScoreFields {
	fingerprint := in.MatchedFingerprint
	if fingerprint == "" {
		fingerprint = in.TipFingerprint
	}
	if in.ScopeTag == "" || fingerprint == "" {
		reason := CacheScoreExcludedNoAffinityKey
		return CacheScoreFields{
			CacheScoreEligible:       false,
			CacheScoreExcludedReason: &reason,
		}
	}

	key := in.ScopeTag + ":" + fingerprint
	var theoretical *int64
	if in.HasTip {
		value := int64(in.TipPrefixBytes / bytesPerToken)
		theoretical = &value
	}
	bucket := in.CacheTTL
	if bucket == "" {
		bucket = "5m"
	}

	fields := CacheScoreFields{
		CacheCompatibilityKey:  &key,
		TheoreticalCacheTokens: theoretical,
		CacheTTLBucket:         &bucket,
	}
	switch {
	case !in.Succeeded:
		reason := CacheScoreExcludedAttemptFailed
		fields.CacheScoreEligible = false
		fields.CacheScoreExcludedReason = &reason
	case !in.UsageObservable:
		reason := CacheScoreExcludedNotObservable
		fields.CacheScoreEligible = false
		fields.CacheScoreExcludedReason = &reason
	case in.StreamTruncated:
		reason := CacheScoreExcludedStreamTruncated
		fields.CacheScoreEligible = false
		fields.CacheScoreExcludedReason = &reason
	default:
		fields.CacheScoreEligible = true
	}
	return fields
}
