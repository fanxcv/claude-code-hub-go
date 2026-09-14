package pubstatus

import (
	"strings"
	"testing"
	"time"
)

// 本文件钉住 redis-contract.ts 的**读侧**键名与状态判定。键名是跨语言契约：Node 的读路径与
// 本包的读路径必须命中同一批键，改名即静默读不到（表现是公开页永远「正在重建」，而不是报错）。

func TestBuildManifestKeyMatchesNodeShape(t *testing.T) {
	key, err := BuildManifestKey("cfg-1", 5, 24, "")
	if err != nil {
		t.Fatalf("建键失败: %v", err)
	}
	want := "public-status:v2:manifest:cfg-1:5m:24h"
	if key != want {
		t.Fatalf("manifest 键不符：want %q got %q", want, key)
	}

	legacy, err := BuildManifestKey("cfg-1", 15, 168, LegacyPublicStatusRedisPrefix)
	if err != nil {
		t.Fatalf("建 legacy 键失败: %v", err)
	}
	if wantLegacy := "public-status:v1:manifest:cfg-1:15m:168h"; legacy != wantLegacy {
		t.Fatalf("legacy manifest 键不符：want %q got %q", wantLegacy, legacy)
	}
}

func TestBuildKeyEscapesVersionLikeEncodeURIComponent(t *testing.T) {
	// encodeURIComponent 的保留集与 url.PathEscape / QueryEscape 都不同：
	// 这里钉住 `(` `)` `!` `~` 不转义、`/` `:` 转义——版本段现在是 `cfg-<毫秒>`，
	// 但键名是契约，逃逸规则必须与 Node 逐字一致。
	version, err := BuildManifestKey("cfg-1/two three(x)~!", 5, 24, "")
	if err != nil {
		t.Fatalf("建键失败: %v", err)
	}
	want := "public-status:v2:manifest:cfg-1%2Ftwo%20three(x)~!:5m:24h"
	if version != want {
		t.Fatalf("版本段逃逸不符：want %q got %q", want, version)
	}
}

func TestBuildCurrentSnapshotKeyMatchesNodeShape(t *testing.T) {
	key, err := BuildCurrentSnapshotKey(5, 24, "gen-1", "")
	if err != nil {
		t.Fatalf("建键失败: %v", err)
	}
	want := "public-status:v2:snapshot:gen-1:5m:24h"
	if key != want {
		t.Fatalf("snapshot 键不符：want %q got %q", want, key)
	}
}

func TestBuildRebuildHintKeyMatchesNodeShape(t *testing.T) {
	key, err := BuildRebuildHintKey(30, 72, "")
	if err != nil {
		t.Fatalf("建键失败: %v", err)
	}
	want := "public-status:v2:rebuild-hint:30m:72h"
	if key != want {
		t.Fatalf("rebuild-hint 键不符：want %q got %q", want, key)
	}
}

func TestBuildKeyRejectsNonPositiveWindows(t *testing.T) {
	if _, err := BuildManifestKey("cfg-1", 0, 24, ""); err == nil {
		t.Fatal("intervalMinutes=0 应报错（Node 的 assertPositiveInteger）")
	}
	if _, err := BuildCurrentSnapshotKey(5, -1, "gen-1", ""); err == nil {
		t.Fatal("rangeHours=-1 应报错")
	}
	if _, err := BuildRebuildHintKey(5, 0, ""); err == nil {
		t.Fatal("rangeHours=0 应报错")
	}
}

func TestAlignBucketStartUTCFloorsToBucket(t *testing.T) {
	// 5 分钟桶：00:07:30.500Z → 00:05:00.000Z（向下对齐，不是四舍五入）。
	aligned, err := AlignBucketStartUTC("2026-09-13T00:07:30.500Z", 5)
	if err != nil {
		t.Fatalf("对齐失败: %v", err)
	}
	if want := "2026-09-13T00:05:00.000Z"; aligned != want {
		t.Fatalf("桶对齐不符：want %q got %q", want, aligned)
	}

	// 边界正上方：00:05:00.001Z 仍在 00:05 桶里。
	aligned, err = AlignBucketStartUTC("2026-09-13T00:05:00.001Z", 5)
	if err != nil {
		t.Fatalf("对齐失败: %v", err)
	}
	if want := "2026-09-13T00:05:00.000Z"; aligned != want {
		t.Fatalf("桶边界不符：want %q got %q", want, aligned)
	}

	// 60 分钟桶：跨到上一个整点。
	aligned, err = AlignBucketStartUTC("2026-09-13T00:59:59.999Z", 60)
	if err != nil {
		t.Fatalf("对齐失败: %v", err)
	}
	if want := "2026-09-13T00:00:00.000Z"; aligned != want {
		t.Fatalf("60 分钟桶不符：want %q got %q", want, aligned)
	}
}

func TestAlignBucketStartUTCRejectsBadInput(t *testing.T) {
	if _, err := AlignBucketStartUTC("not-a-time", 5); err == nil {
		t.Fatal("非法时间应报错（Node 的 Date.parse NaN 分支）")
	}
	if _, err := AlignBucketStartUTC("2026-09-13T00:07:30Z", 0); err == nil {
		t.Fatal("intervalMinutes=0 应报错")
	}
}

// TestResolveManifestStateFiveStates 覆盖 redis-contract.ts:191-229 的五条分支。
func TestResolveManifestStateFiveStates(t *testing.T) {
	nowISO := "2026-09-13T00:10:00.000Z"

	lastComplete := "gen-1"
	cases := []struct {
		name     string
		manifest *PublicStatusManifest
		want     PublicStatusManifestResolution
	}{
		{
			name:     "无 manifest → no-data",
			manifest: nil,
			want:     PublicStatusManifestResolution{RebuildState: ServeStateNoData},
		},
		{
			name:     "有 manifest 但没有完整代 → rebuilding",
			manifest: &PublicStatusManifest{ConfigVersion: "cfg-1", FreshUntil: "2026-09-13T00:20:00.000Z"},
			want:     PublicStatusManifestResolution{RebuildState: ServeStateRebuilding},
		},
		{
			name: "新鲜期内且后台空闲 → fresh，源代取完整代",
			manifest: &PublicStatusManifest{
				ConfigVersion:          "cfg-1",
				LastCompleteGeneration: &lastComplete,
				FreshUntil:             "2026-09-13T00:20:00.000Z",
				RebuildState:           "idle",
			},
			want: PublicStatusManifestResolution{
				RebuildState: ServeStateFresh, SourceGeneration: "gen-1", LastCompleteGeneration: "gen-1",
			},
		},
		{
			name: "新鲜期内但后台正在重建 → stale（有完整代优先于正在重建）",
			manifest: &PublicStatusManifest{
				ConfigVersion:          "cfg-1",
				LastCompleteGeneration: &lastComplete,
				FreshUntil:             "2026-09-13T00:20:00.000Z",
				RebuildState:           "rebuilding",
			},
			want: PublicStatusManifestResolution{
				RebuildState: ServeStateStale, SourceGeneration: "gen-1", LastCompleteGeneration: "gen-1",
			},
		},
		{
			name: "过了新鲜期 → stale，仍服务最后一个完整代",
			manifest: &PublicStatusManifest{
				ConfigVersion:          "cfg-1",
				LastCompleteGeneration: &lastComplete,
				FreshUntil:             "2026-09-13T00:05:00.000Z",
				RebuildState:           "idle",
			},
			want: PublicStatusManifestResolution{
				RebuildState: ServeStateStale, SourceGeneration: "gen-1", LastCompleteGeneration: "gen-1",
			},
		},
		{
			name: "freshUntil 不可解析 → stale（Node 的 NaN 分支）",
			manifest: &PublicStatusManifest{
				ConfigVersion:          "cfg-1",
				LastCompleteGeneration: &lastComplete,
				FreshUntil:             "not-a-time",
				RebuildState:           "idle",
			},
			want: PublicStatusManifestResolution{
				RebuildState: ServeStateStale, SourceGeneration: "gen-1", LastCompleteGeneration: "gen-1",
			},
		},
		{
			name: "lastCompleteGeneration 是空串 → rebuilding（Node 的裸 ! 判定）",
			manifest: &PublicStatusManifest{
				ConfigVersion: "cfg-1", LastCompleteGeneration: pointerTo(""),
				FreshUntil: "2026-09-13T00:20:00.000Z",
			},
			want: PublicStatusManifestResolution{RebuildState: ServeStateRebuilding},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ResolveManifestState(testCase.manifest, nowISO)
			if got != testCase.want {
				t.Fatalf("解析结果不符：want %+v got %+v", testCase.want, got)
			}
		})
	}
}

func TestAlignBucketStartUTCIsStableAcrossCalls(t *testing.T) {
	// 键名里嵌着对齐后的时刻：同一输入必须恒得同一键（否则读侧会漏命中自己刚写的键）。
	first, err := AlignBucketStartUTC("2026-09-13T00:07:30.500Z", 5)
	if err != nil {
		t.Fatalf("对齐失败: %v", err)
	}
	second, err := AlignBucketStartUTC("2026-09-13T00:07:30.500Z", 5)
	if err != nil {
		t.Fatalf("对齐失败: %v", err)
	}
	if first != second || !strings.HasSuffix(first, ".000Z") {
		t.Fatalf("对齐不稳定或格式不对：%q / %q", first, second)
	}
	if _, err := time.Parse(time.RFC3339Nano, first); err != nil {
		t.Fatalf("对齐结果不是合法 RFC3339：%v", err)
	}
}

// pointerTo 是测试用的取址助手（本包测试里多处需要 *string / *bool）。
func pointerTo[T any](value T) *T {
	return &value
}
