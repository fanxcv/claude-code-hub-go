// Package clientver 复刻 Node 的客户端版本解析与检查（src/lib/ua-parser.ts、
// src/lib/client-version-checker.ts），实现 guard.VersionChecker。
//
// 语义是 fail-open：任何一步出错都放行——版本检查只用于提示升级，不该成为可用性单点。
package clientver

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

const (
	// userVersionKeyPrefix 与 Node 一致：client_version:{clientType}:{userId}。
	userVersionKeyPrefix = "client_version:"
	// gaVersionKeyPrefix 与 Node 一致：ga_version:{clientType}。
	gaVersionKeyPrefix = "ga_version:"

	// userVersionTTLSeconds 与 Node 的 TTL.USER_VERSION 一致（7 天，匹配活跃窗口）。
	userVersionTTLSeconds = 7 * 24 * 60 * 60
	// gaVersionTTLSeconds 与 Node 的 TTL.GA_VERSION 一致（5 分钟）。
	gaVersionTTLSeconds = 5 * 60

	// activeWindowDays 是活跃用户窗口（Node getActiveUserVersions(7)）。
	activeWindowDays = 7
	// gaThreshold 是「某版本用户数达到多少才算 GA」的阈值（Node CLIENT_VERSION_GA_THRESHOLD，默认 2）。
	gaThreshold = 2
)

// uaPattern 与 Node ua-parser.ts:46 的正则同形：{clientType}/{semver}。
var uaPattern = regexp.MustCompile(`^([a-zA-Z0-9_-]+)/([0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.]+)?)`)

// displayNames 与 Node getClientTypeDisplayName 的映射一致。
var displayNames = map[string]string{
	"claude-vscode":            "Claude VSCode Extension",
	"claude-cli":               "Claude CLI",
	"claude-cli-unknown":       "Claude CLI (Unknown Version)",
	"anthropic-sdk-typescript": "Anthropic SDK (TypeScript)",
}

// ParseUserAgent 解析 UA，失败返回 false（Node 的 parseUserAgent 返回 null）。
func ParseUserAgent(userAgent string) (guard.ClientVersion, bool) {
	match := uaPattern.FindStringSubmatch(userAgent)
	if match == nil {
		return guard.ClientVersion{}, false
	}
	base, version := match[1], match[2]
	if base != "claude-cli" {
		return guard.ClientVersion{ClientType: base, Version: version}, true
	}
	return guard.ClientVersion{ClientType: determineClaudeClientType(userAgent), Version: version}, true
}

// determineClaudeClientType 复刻 ua-parser.ts:88：按括号内标记区分 VSCode 插件与纯 CLI。
func determineClaudeClientType(userAgent string) string {
	if strings.Contains(userAgent, "claude-vscode") {
		return "claude-vscode"
	}
	if strings.Contains(userAgent, "cli") {
		return "claude-cli"
	}
	return "claude-cli-unknown"
}

// DisplayName 返回客户端类型的展示名（未知类型原样返回）。
func DisplayName(clientType string) string {
	if name, ok := displayNames[clientType]; ok {
		return name
	}
	return clientType
}

// ActiveUserSource 提供活跃用户的 UA 分布（Node getActiveUserVersions）。
type ActiveUserSource interface {
	ActiveUserAgents(ctx context.Context, days int) ([]store.ActiveUserAgent, error)
}

// Options 是 Checker 的构造参数。
type Options struct {
	// Redis 为 nil 时不做版本记录与 GA 缓存，ShouldUpgrade 退化为放行。
	Redis redis.UniversalClient
	// Users 为 nil 时 GA 只能来自 Redis 缓存（缓存未命中即放行）。
	Users ActiveUserSource
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Now 可注入时钟。
	Now func() time.Time
}

// Checker 实现 guard.VersionChecker。
type Checker struct {
	redis  redis.UniversalClient
	users  ActiveUserSource
	logger *logx.Logger
	now    func() time.Time
}

var _ guard.VersionChecker = (*Checker)(nil)

// NewChecker 构造版本检查器。
func NewChecker(options Options) *Checker {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Checker{redis: options.Redis, users: options.Users, logger: logger, now: now}
}

// ParseUserAgent 实现 guard.VersionChecker。
func (c *Checker) ParseUserAgent(userAgent string) (guard.ClientVersion, bool) {
	return ParseUserAgent(userAgent)
}

// DisplayName 实现 guard.VersionChecker。
func (c *Checker) DisplayName(clientType string) string { return DisplayName(clientType) }

// UpdateUserVersion 记录用户当前版本；尽力而为（失败只记日志，不影响请求）。
func (c *Checker) UpdateUserVersion(ctx context.Context, userID int64, client guard.ClientVersion) error {
	if c.redis == nil || userID <= 0 || client.ClientType == "" {
		return nil
	}
	key := userVersionKeyPrefix + client.ClientType + ":" + strconv.FormatInt(userID, 10)
	if err := c.redis.Set(ctx, key, client.Version, time.Duration(userVersionTTLSeconds)*time.Second).Err(); err != nil {
		c.logger.Warn("client_version_update_failed", map[string]any{"error": err.Error()})
		return err
	}
	return nil
}

// ShouldUpgrade 判断是否需要升级（Node ClientVersionChecker.shouldUpgrade）。
//
// 放行（needsUpgrade=false）的两种情形与 Node 同：取不到 GA 版本、或任何一步出错。
func (c *Checker) ShouldUpgrade(
	ctx context.Context,
	client guard.ClientVersion,
) (bool, string, error) {
	gaVersion, err := c.detectGAVersion(ctx, client.ClientType)
	if err != nil {
		// fail-open：检查失败即放行（版本检查不是可用性单点）。
		c.logger.Warn("client_version_check_failed", map[string]any{"error": err.Error()})
		return false, "", nil
	}
	if gaVersion == "" {
		return false, "", nil
	}
	return IsVersionLess(client.Version, gaVersion), gaVersion, nil
}

// gaCacheValue 是 Redis 里 ga_version:* 的 JSON 形状（Node 存 {version,userCount}）。
type gaCacheValue struct {
	Version   string `json:"version"`
	UserCount int    `json:"userCount"`
}

// detectGAVersion 先读缓存再按活跃用户分布计算，与 Node 同序（client-version-checker.ts:156）。
func (c *Checker) detectGAVersion(ctx context.Context, clientType string) (string, error) {
	if c.redis != nil {
		cached, err := c.redis.Get(ctx, gaVersionKeyPrefix+clientType).Result()
		if err == nil && cached != "" {
			var value gaCacheValue
			if decodeErr := json.Unmarshal([]byte(cached), &value); decodeErr == nil && value.Version != "" {
				return value.Version, nil
			}
		} else if err != nil && err != redis.Nil {
			return "", err
		}
	}
	if c.users == nil {
		// 没有活跃用户来源：相当于「无 GA 版本」，放行。
		return "", nil
	}
	active, err := c.users.ActiveUserAgents(ctx, activeWindowDays)
	if err != nil {
		return "", err
	}
	gaVersion, userCount := computeGAVersion(active, clientType)
	if gaVersion == "" {
		return "", nil
	}
	if c.redis != nil {
		payload, marshalErr := json.Marshal(gaCacheValue{Version: gaVersion, UserCount: userCount})
		if marshalErr == nil {
			// 缓存写失败不影响判定（Node 同样把这一步当尽力而为）。
			if setErr := c.redis.Set(
				ctx,
				gaVersionKeyPrefix+clientType,
				payload,
				time.Duration(gaVersionTTLSeconds)*time.Second,
			).Err(); setErr != nil {
				c.logger.Warn("client_version_ga_cache_failed", map[string]any{"error": setErr.Error()})
			}
		}
	}
	return gaVersion, nil
}

// computeGAVersion 复刻 computeGAVersionFromUsers：同版本去重计数，达到阈值者取最高版本。
//
// 返回 GA 版本与该版本的去重用户数；无版本达标时返回空串。
func computeGAVersion(active []store.ActiveUserAgent, clientType string) (string, int) {
	usersByVersion := map[string]map[int64]struct{}{}
	for _, user := range active {
		client, ok := ParseUserAgent(user.UserAgent)
		if !ok || client.ClientType != clientType || client.Version == "" {
			continue
		}
		if usersByVersion[client.Version] == nil {
			usersByVersion[client.Version] = map[int64]struct{}{}
		}
		usersByVersion[client.Version][user.UserID] = struct{}{}
	}
	gaVersion := ""
	userCount := 0
	for version, users := range usersByVersion {
		if len(users) < gaThreshold {
			continue
		}
		if gaVersion == "" || IsVersionGreater(version, gaVersion) {
			gaVersion = version
			userCount = len(users)
		}
	}
	return gaVersion, userCount
}
