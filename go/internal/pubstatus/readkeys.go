package pubstatus

import (
	"fmt"
	"strings"
	"time"
)

// 本文件是 Node `src/lib/public-status/redis-contract.ts` 的**读侧**转写：桶对齐、manifest /
// snapshot / rebuild-hint 三组键，以及 manifest 的服务态判定。
//
// 与既有 `snapshot.go` 的分工：那里是**配置投影**（config snapshot）的键与形状，这里是
// **时序投影**（manifest + snapshot + rollup）的键与形状。两者同属一份跨语言契约，改名即静默
// 读不到，故键名逐字照抄、逐字有测试。
//
// **写侧缺失（如实登记）**：manifest 与 `<prefix>:snapshot:<generation>:…` 由 Node 的
// `rebuild-worker.ts` 写入（Go 侧没有等价物，见 readstore.go 文件头）。本文件的键构造器是为
// **读**服务的；将来移植 worker 时可直接复用它们，无需再抄一遍键名。

// PublicStatusRollupBucketMinutes 是 rollup 桶宽（redis-contract.ts:5）。
const PublicStatusRollupBucketMinutes = 5

// ISO 毫秒精度 UTC 格式，逐字对齐 Node 的 `Date.prototype.toISOString()`：
// 固定三位毫秒 + `Z`。缺一位都会让键名与 Node 分叉（键名里嵌着对齐后的时刻）。
const isoMilliLayout = "2006-01-02T15:04:05.000Z"

// PublicStatusManifest 是 manifest 记录。
//
// 只声明**读路径真正会读**的字段：JSON 里多出来的字段本来就会被忽略，而声明了没人读的字段只会
// 让人误以为有消费者（`rollupCoverageStartedAt`/`rollupSampleCount` 属写侧观测，故不在此）。
type PublicStatusManifest struct {
	ConfigVersion string `json:"configVersion"`
	Generation    string `json:"generation"`
	// LastCompleteGeneration 为 null 表示从未有过完整代（此时服务态是 rebuilding，不是 no-data）。
	LastCompleteGeneration *string `json:"lastCompleteGeneration"`
	GeneratedAt            string  `json:"generatedAt"`
	FreshUntil             string  `json:"freshUntil"`
	// RebuildState 取 "idle" | "rebuilding"。新鲜期内它若为 rebuilding，服务态降为 stale：
	// 「有完整代可用」优先于「正在重建」——公开页不因为后台在干活就变灰。
	RebuildState string `json:"rebuildState"`
	// RollupCoverageComplete 用指针：读路径判的是 `=== false`（缺席不等于 false）。
	RollupCoverageComplete *bool `json:"rollupCoverageComplete"`
}

// ServeState 是公开服务态（redis-contract.ts:7）。
type ServeState string

const (
	ServeStateFresh      ServeState = "fresh"
	ServeStateStale      ServeState = "stale"
	ServeStateRebuilding ServeState = "rebuilding"
	ServeStateNoData     ServeState = "no-data"
)

// PublicStatusManifestResolution 是 manifest 的解析结果（redis-contract.ts:26-30）。
type PublicStatusManifestResolution struct {
	RebuildState           ServeState
	SourceGeneration       string
	LastCompleteGeneration string
}

// AlignBucketStartUTC 复刻 alignBucketStartUtc（redis-contract.ts:41-53）：按 UTC 向下对齐到
// intervalMinutes 的整数倍边界。
func AlignBucketStartUTC(isoTimestamp string, intervalMinutes int) (string, error) {
	if err := assertPositiveInt(intervalMinutes, "intervalMinutes"); err != nil {
		return "", err
	}
	parsed, err := ParseISOMilli(isoTimestamp)
	if err != nil {
		return "", fmt.Errorf("pubstatus: 无效的 ISO 时间 %q: %w", isoTimestamp, err)
	}
	bucketMs := int64(intervalMinutes) * 60 * 1000
	// Node 用 Math.floor（向下取整），Go 的整数除法向零截断：负毫秒时两者不同，故显式做
	// 地板除。当前时刻恒为正，但键名是契约，边界语义也照抄。
	alignedMs := floorDiv(parsed.UnixMilli(), bucketMs) * bucketMs
	return time.UnixMilli(alignedMs).UTC().Format(isoMilliLayout), nil
}

// ParseISOMilli 解析 ISO 时间串（接受 RFC3339 任意精度小数秒）。
//
// Node 侧走 `Date.parse`，它接受的形态比 RFC3339 宽（例如空格分隔、缺时区）。这里只接受
// RFC3339 一族：manifest 是**我们自己与 Node 共同写入**的记录，格式固定；放宽解析等于给
// 「格式已经跑偏」的情况兜底，反而掩盖写入侧的问题。
func ParseISOMilli(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

// floorDiv 是数学意义上的地板除（向负无穷取整）。
func floorDiv(a, b int64) int64 {
	quotient := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		quotient--
	}
	return quotient
}

func assertPositiveInt(value int, label string) error {
	if value <= 0 {
		return fmt.Errorf("pubstatus: %s 必须为正整数，收到 %d", label, value)
	}
	return nil
}

// prefixOr 复刻 redis-contract.ts:74-76 的 `prefix ?? PUBLIC_STATUS_REDIS_PREFIX`。
func prefixOr(prefix string) string {
	if prefix == "" {
		return PublicStatusRedisPrefix
	}
	return prefix
}

// BuildManifestKey 复刻 buildPublicStatusManifestKey（redis-contract.ts:103-117）。
//
// 形如 `<prefix>:manifest:<version>:<interval>m:<range>h`。prefix 传空串即当前前缀 v2；
// 读路径对 legacy v1 前缀复用同一构造器（Node 的 `options.prefix`）。
func BuildManifestKey(configVersion string, intervalMinutes, rangeHours int, prefix string) (string, error) {
	if err := assertPositiveInt(intervalMinutes, "intervalMinutes"); err != nil {
		return "", err
	}
	if err := assertPositiveInt(rangeHours, "rangeHours"); err != nil {
		return "", err
	}
	return strings.Join([]string{
		prefixOr(prefix),
		"manifest",
		encodeKeyPart(configVersion),
		fmt.Sprintf("%dm", intervalMinutes),
		fmt.Sprintf("%dh", rangeHours),
	}, ":"), nil
}

// BuildCurrentSnapshotKey 复刻 buildPublicStatusCurrentSnapshotKey（redis-contract.ts:119-133）。
func BuildCurrentSnapshotKey(intervalMinutes, rangeHours int, generation, prefix string) (string, error) {
	if err := assertPositiveInt(intervalMinutes, "intervalMinutes"); err != nil {
		return "", err
	}
	if err := assertPositiveInt(rangeHours, "rangeHours"); err != nil {
		return "", err
	}
	return strings.Join([]string{
		prefixOr(prefix),
		"snapshot",
		encodeKeyPart(generation),
		fmt.Sprintf("%dm", intervalMinutes),
		fmt.Sprintf("%dh", rangeHours),
	}, ":"), nil
}

// BuildRebuildHintKey 复刻 buildPublicStatusRebuildHintKey（redis-contract.ts:167-177）。
func BuildRebuildHintKey(intervalMinutes, rangeHours int, prefix string) (string, error) {
	if err := assertPositiveInt(intervalMinutes, "intervalMinutes"); err != nil {
		return "", err
	}
	if err := assertPositiveInt(rangeHours, "rangeHours"); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:rebuild-hint:%dm:%dh", prefixOr(prefix), intervalMinutes, rangeHours), nil
}

// ResolveManifestState 复刻 resolvePublicStatusManifestState（redis-contract.ts:191-229）。
//
// 核心公开语义：**有完整代时优先服务历史快照**，没有完整代时才诚实返回 rebuilding/no-data。
// 三种输入各有其服务态：无 manifest → no-data；有 manifest 但从未有成代 → rebuilding；
// 有完整代 → 新鲜期内 fresh（若后台正在重建则降 stale，避免公开页因为后台干活而变灰）。
func ResolveManifestState(
	manifest *PublicStatusManifest,
	nowISO string,
) PublicStatusManifestResolution {
	if manifest == nil {
		return PublicStatusManifestResolution{RebuildState: ServeStateNoData}
	}

	lastComplete := ""
	if manifest.LastCompleteGeneration != nil {
		lastComplete = *manifest.LastCompleteGeneration
	}
	if lastComplete == "" {
		return PublicStatusManifestResolution{RebuildState: ServeStateRebuilding}
	}

	nowParsed, nowErr := ParseISOMilli(nowISO)
	freshParsed, freshErr := ParseISOMilli(manifest.FreshUntil)
	if nowErr == nil && freshErr == nil && !nowParsed.After(freshParsed) {
		state := ServeStateFresh
		if manifest.RebuildState == "rebuilding" {
			state = ServeStateStale
		}
		return PublicStatusManifestResolution{
			RebuildState:           state,
			SourceGeneration:       lastComplete,
			LastCompleteGeneration: lastComplete,
		}
	}

	return PublicStatusManifestResolution{
		RebuildState:           ServeStateStale,
		SourceGeneration:       lastComplete,
		LastCompleteGeneration: lastComplete,
	}
}
