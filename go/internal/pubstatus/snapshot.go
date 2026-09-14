package pubstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是 Node src/lib/public-status/{redis-contract,config-snapshot}.ts 的转写：键布局、
// 快照形状与写入语义。
//
// 三个必须逐字对齐的点：
//  1. **键名**：`public-status:v2:config:<version>` / `:config-internal:<version>` /
//     `:config-version:current`。键名是跨进程契约（Node 的读路径与 Go 的 /api/public-site-meta
//     都按它取数），改名即静默读不到。
//  2. **TTL 只给版本化键**（30 天），三个 current 指针键**不给 TTL**：版本化键每次发布都铸新
//     版本会无限堆积；指针键长期不发布就丢的话，公开状态页会整体变暗。
//  3. **指针推进用 Lua 比较并置**（`current <= ARGV[1]`）：并发发布时只有更新的版本能推进指针，
//     否则老版本会把指针推回去。
const (
	// PublicStatusRedisPrefix 是当前前缀（redis-contract.ts:3）。
	PublicStatusRedisPrefix = "public-status:v2"
	// LegacyPublicStatusRedisPrefix 是 v1 前缀（读路径的兼容回退用；本包的写路径不写它）。
	LegacyPublicStatusRedisPrefix = "public-status:v1"
	// PublicStatusConfigTTLSeconds 见文件头第 2 条（config-snapshot.ts:35）。
	PublicStatusConfigTTLSeconds = 60 * 60 * 24 * 30
	// DefaultPublicStatusSiteDescription 是描述兜底（config-snapshot.ts:12）。
	DefaultPublicStatusSiteDescription = "Request-derived public status"
	// DefaultSiteTitle 是站点标题兜底（src/lib/site-title.ts:1）。
	DefaultSiteTitle = "CC Hub Go"
)

// PublicStatusModelSnapshot 是快照里的单个模型（config-snapshot.ts:37-42）。
type PublicStatusModelSnapshot struct {
	PublicModelKey   string `json:"publicModelKey"`
	Label            string `json:"label"`
	VendorIconKey    string `json:"vendorIconKey"`
	RequestTypeBadge string `json:"requestTypeBadge"`
}

// PublicStatusGroupSnapshot 是快照里的单个分组（config-snapshot.ts:44-50）。
type PublicStatusGroupSnapshot struct {
	Slug        string                      `json:"slug"`
	DisplayName string                      `json:"displayName"`
	SortOrder   float64                     `json:"sortOrder"`
	Description *string                     `json:"description"`
	Models      []PublicStatusModelSnapshot `json:"models"`
}

// PublicStatusConfigSnapshot 是公开快照。字段顺序照 Node 的对象字面量顺序
// （config-snapshot.ts:152-169）：generatedAt 紧跟 configVersion，便于与 Node 产物比对。
type PublicStatusConfigSnapshot struct {
	ConfigVersion          string                      `json:"configVersion"`
	GeneratedAt            string                      `json:"generatedAt"`
	SiteTitle              string                      `json:"siteTitle"`
	SiteDescription        string                      `json:"siteDescription"`
	TimeZone               *string                     `json:"timeZone"`
	DefaultIntervalMinutes int                         `json:"defaultIntervalMinutes"`
	DefaultRangeHours      int                         `json:"defaultRangeHours"`
	Groups                 []PublicStatusGroupSnapshot `json:"groups"`
}

// InternalPublicStatusGroupSnapshot 是内部快照的分组（多带源分组 id/名）。
type InternalPublicStatusGroupSnapshot struct {
	SourceGroupID   *int64                      `json:"sourceGroupId"`
	SourceGroupName string                      `json:"sourceGroupName"`
	Slug            string                      `json:"slug"`
	DisplayName     string                      `json:"displayName"`
	SortOrder       float64                     `json:"sortOrder"`
	Description     *string                     `json:"description"`
	Models          []PublicStatusModelSnapshot `json:"models"`
}

// InternalPublicStatusConfigSnapshot 是内部快照。
//
// 键序照 Node 的 `{...input, generatedAt}`（config-snapshot.ts:265-272）：spread 先占位、
// generatedAt 落在末尾——与公开快照的键序**不同**，两边各自照抄。
type InternalPublicStatusConfigSnapshot struct {
	ConfigVersion          string                              `json:"configVersion"`
	SiteTitle              string                              `json:"siteTitle"`
	SiteDescription        string                              `json:"siteDescription"`
	TimeZone               *string                             `json:"timeZone"`
	DefaultIntervalMinutes int                                 `json:"defaultIntervalMinutes"`
	DefaultRangeHours      int                                 `json:"defaultRangeHours"`
	Groups                 []InternalPublicStatusGroupSnapshot `json:"groups"`
	GeneratedAt            string                              `json:"generatedAt"`
}

// encodeKeyPart 复刻 redis-contract.ts:37-39 的 encodeURIComponent。
//
// 不用 url.QueryEscape：它把空格编成 `+`，且保留集与 encodeURIComponent 不同。版本段当前
// 只含 `cfg-<毫秒>`，但键名是契约，逃逸规则也必须一致。
func encodeKeyPart(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	const hexDigits = "0123456789ABCDEF"

	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if strings.IndexByte(unreserved, character) >= 0 {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hexDigits[character>>4])
		builder.WriteByte(hexDigits[character&0x0f])
	}
	return builder.String()
}

// BuildConfigSnapshotKey 复刻 buildPublicStatusConfigSnapshotKey（redis-contract.ts:77-82）。
func BuildConfigSnapshotKey(configVersion string) string {
	return PublicStatusRedisPrefix + ":config:" + encodeKeyPart(configVersion)
}

// BuildInternalConfigSnapshotKey 复刻 buildPublicStatusInternalConfigSnapshotKey。
func BuildInternalConfigSnapshotKey(configVersion string) string {
	return PublicStatusRedisPrefix + ":config-internal:" + encodeKeyPart(configVersion)
}

// BuildConfigVersionPointerKey 复刻 buildPublicStatusConfigVersionPointerKey。
func BuildConfigVersionPointerKey() string {
	return PublicStatusRedisPrefix + ":config-version:current"
}

// NormalizeSiteTitle 复刻 src/lib/site-title.ts:3-11。
func NormalizeSiteTitle(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// ResolveSiteDescription 复刻 resolvePublicStatusSiteDescription（config-snapshot.ts:118-131）。
func ResolveSiteDescription(siteTitle string, siteDescription *string) string {
	if siteDescription != nil {
		if trimmed := strings.TrimSpace(*siteDescription); trimmed != "" {
			return trimmed
		}
	}
	if title, ok := NormalizeSiteTitle(siteTitle); ok {
		return title + " public status"
	}
	return DefaultPublicStatusSiteDescription
}

// ResolveRequestTypeBadge 复刻 config-publisher.ts:25-51。
func ResolveRequestTypeBadge(modelName string, providerTypeOverride string) string {
	switch providerTypeOverride {
	case "claude", "claude-auth":
		return "anthropic"
	case "codex":
		return "codex"
	case "gemini", "gemini-cli":
		return "gemini"
	case "openai-compatible":
		return "openaiCompatible"
	}

	normalized := strings.ToLower(modelName)
	switch {
	case strings.Contains(normalized, "codex"):
		return "codex"
	case strings.Contains(normalized, "claude"):
		return "anthropic"
	case strings.Contains(normalized, "gemini"):
		return "gemini"
	default:
		return "openaiCompatible"
	}
}

// NormalizePublicInterval 复刻 config-publisher.ts:53-55：不在选项集里回退 5 分钟。
func NormalizePublicInterval(minutes int) int {
	switch minutes {
	case 5, 15, 30, 60:
		return minutes
	default:
		return 5
	}
}

// NormalizePublicRange 复刻 config-publisher.ts:57-59：越界回退 24 小时。
func NormalizePublicRange(hours int) int {
	if hours >= 1 && hours <= maxPublicStatusRangeHours {
		return hours
	}
	return 24
}

// maxPublicStatusRangeHours 照 constants.ts:3。
const maxPublicStatusRangeHours = 168

// BuildConfigSnapshot 复刻 buildPublicStatusConfigSnapshot（config-snapshot.ts:152-176）。
func BuildConfigSnapshot(input ConfigSnapshotInput, generatedAt time.Time) PublicStatusConfigSnapshot {
	groups := make([]PublicStatusGroupSnapshot, 0, len(input.Groups))
	for _, group := range input.Groups {
		models := make([]PublicStatusModelSnapshot, 0, len(group.Models))
		for _, model := range group.Models {
			models = append(models, PublicStatusModelSnapshot{
				PublicModelKey:   model.PublicModelKey,
				Label:            model.Label,
				VendorIconKey:    model.VendorIconKey,
				RequestTypeBadge: model.RequestTypeBadge,
			})
		}
		groups = append(groups, PublicStatusGroupSnapshot{
			Slug:        group.Slug,
			DisplayName: group.DisplayName,
			SortOrder:   group.SortOrder,
			Description: group.Description,
			Models:      models,
		})
	}
	sort.SliceStable(groups, func(left, right int) bool {
		if groups[left].SortOrder != groups[right].SortOrder {
			return groups[left].SortOrder < groups[right].SortOrder
		}
		return groups[left].Slug < groups[right].Slug
	})

	return PublicStatusConfigSnapshot{
		ConfigVersion:          input.ConfigVersion,
		GeneratedAt:            isoMilliUTC(generatedAt),
		SiteTitle:              strings.TrimSpace(input.SiteTitle),
		SiteDescription:        ResolveSiteDescription(input.SiteTitle, input.SiteDescription),
		TimeZone:               input.TimeZone,
		DefaultIntervalMinutes: input.DefaultIntervalMinutes,
		DefaultRangeHours:      input.DefaultRangeHours,
		Groups:                 groups,
	}
}

// BuildInternalConfigSnapshot 复刻 buildInternalPublicStatusConfigSnapshot。
func BuildInternalConfigSnapshot(input ConfigSnapshotInput, generatedAt time.Time) InternalPublicStatusConfigSnapshot {
	groups := make([]InternalPublicStatusGroupSnapshot, 0, len(input.Groups))
	for _, group := range input.Groups {
		models := make([]PublicStatusModelSnapshot, 0, len(group.Models))
		for _, model := range group.Models {
			models = append(models, PublicStatusModelSnapshot{
				PublicModelKey:   model.PublicModelKey,
				Label:            model.Label,
				VendorIconKey:    model.VendorIconKey,
				RequestTypeBadge: model.RequestTypeBadge,
			})
		}
		groups = append(groups, InternalPublicStatusGroupSnapshot{
			SourceGroupID:   group.SourceGroupID,
			SourceGroupName: group.SourceGroupName,
			Slug:            group.Slug,
			DisplayName:     group.DisplayName,
			SortOrder:       group.SortOrder,
			Description:     group.Description,
			Models:          models,
		})
	}
	sort.SliceStable(groups, func(left, right int) bool {
		if groups[left].SortOrder != groups[right].SortOrder {
			return groups[left].SortOrder < groups[right].SortOrder
		}
		return groups[left].Slug < groups[right].Slug
	})

	return InternalPublicStatusConfigSnapshot{
		ConfigVersion:          input.ConfigVersion,
		SiteTitle:              input.SiteTitle,
		SiteDescription:        ResolveSiteDescription(input.SiteTitle, input.SiteDescription),
		TimeZone:               input.TimeZone,
		DefaultIntervalMinutes: input.DefaultIntervalMinutes,
		DefaultRangeHours:      input.DefaultRangeHours,
		Groups:                 groups,
		GeneratedAt:            isoMilliUTC(generatedAt),
	}
}

// ConfigSnapshotInput 是两侧快照共用的构造入参。
type ConfigSnapshotInput struct {
	ConfigVersion          string
	SiteTitle              string
	SiteDescription        *string
	TimeZone               *string
	DefaultIntervalMinutes int
	DefaultRangeHours      int
	Groups                 []ConfigSnapshotGroup
}

// ConfigSnapshotGroup 是入参里的分组。
type ConfigSnapshotGroup struct {
	SourceGroupID   *int64
	SourceGroupName string
	Slug            string
	DisplayName     string
	SortOrder       float64
	Description     *string
	Models          []PublicStatusModelSnapshot
}

// isoMilliUTC 输出 JS Date#toISOString 的形状（毫秒、UTC、固定 24 字符）。
func isoMilliUTC(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// advancePointerScript 与 config-snapshot.ts:316-324 的 Lua 逐字一致。
//
// 语义：只有「当前不存在」或「当前 <= 新版本」时才写。版本串是 `cfg-<毫秒>`，字典序与时间序
// 一致，故可以直接比字符串。
const advancePointerScript = `
      local current = redis.call('GET', KEYS[1])
      if (not current) or current <= ARGV[1] then
        redis.call('SET', KEYS[1], ARGV[1])
        return 1
      end
      return 0
    `

// PublishResult 是发布结果（config-publisher.ts:58-64 的返回形状）。
type PublishResult struct {
	ConfigVersion string
	Key           string
	Written       bool
	GroupCount    int
}

// PublishConfigProjection 是写入面：版本化键（带 TTL）+ `config-version:current` 指针（CAS）。
//
// 与 Node 的 config-publisher.ts:155-175 逐条对齐：
//   - 先写内部快照，再写公开快照（Node 的顺序）；
//   - 两次写入都把 `setCurrentPointer` 关掉——即**不写** `config:current` /
//     `config-internal:current` 两个键，Node 这条路径也不写（只有指针 CAS 那条走）；
//   - 两次都成功才推进 `config-version:current`；
//   - 三者全成功才算 written。
func PublishConfigProjection(
	ctx context.Context,
	client redis.UniversalClient,
	public PublicStatusConfigSnapshot,
	internal InternalPublicStatusConfigSnapshot,
) (PublishResult, error) {
	result := PublishResult{ConfigVersion: public.ConfigVersion, Key: BuildConfigSnapshotKey(public.ConfigVersion)}
	if client == nil {
		return result, nil
	}

	internalKey := BuildInternalConfigSnapshotKey(internal.ConfigVersion)
	internalPayload, marshalErr := json.Marshal(internal)
	if marshalErr != nil {
		return result, fmt.Errorf("pubstatus: 序列化内部快照失败: %w", marshalErr)
	}
	publicPayload, marshalErr := json.Marshal(public)
	if marshalErr != nil {
		return result, fmt.Errorf("pubstatus: 序列化公开快照失败: %w", marshalErr)
	}

	ttl := time.Duration(PublicStatusConfigTTLSeconds) * time.Second
	if err := client.Set(ctx, internalKey, internalPayload, ttl).Err(); err != nil {
		return result, fmt.Errorf("pubstatus: 写内部配置快照失败: %w", err)
	}
	if err := client.Set(ctx, result.Key, publicPayload, ttl).Err(); err != nil {
		return result, fmt.Errorf("pubstatus: 写公开配置快照失败: %w", err)
	}

	advanced, err := AdvanceConfigVersionPointer(ctx, client, public.ConfigVersion)
	if err != nil {
		return result, err
	}
	result.Written = advanced
	return result, nil
}

// AdvanceConfigVersionPointer 是 `config-version:current` 的比较并置。
func AdvanceConfigVersionPointer(ctx context.Context, client redis.UniversalClient, configVersion string) (bool, error) {
	if client == nil {
		return false, nil
	}
	outcome, err := client.Eval(ctx, advancePointerScript, []string{BuildConfigVersionPointerKey()}, configVersion).Int64()
	if err != nil {
		return false, fmt.Errorf("pubstatus: 推进配置版本指针失败: %w", err)
	}
	return outcome == 1, nil
}
