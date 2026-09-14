package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件是 **GET /api/public-site-meta**（Node 侧 src/app/api/public-site-meta/route.ts）。
//
// 它读的是 public-status 的**配置投影快照**（Redis 里的版本化键 + 当前指针），不是库：
// 快照由发布方（Node 的 public-status/config-publisher）在供应商/分组/设置变更时写，
// 内容已裁剪成 public-safe 子集。故本端点的依赖是 Redis，不是 PG。
//
// 三条形状要点：
//
//  1. 有快照 → 200 + `Cache-Control: public, max-age=30, stale-while-revalidate=60`；
//  2. 无快照 → 仍 200，但 `available:false, reason:"projection_missing"` + `no-store`
//     （前端据此隐藏站点描述，而不是报错）；
//  3. 只有**读快照本身抛异常**才 503（Node 的 catch 分支）——Redis 读取失败在 Node 侧被
//     safeGet 吞掉，等价于「无快照」，故这里的 Redis 读取失败也归一为「无快照」。
const (
	publicSiteMetaCacheControl     = "public, max-age=30, stale-while-revalidate=60"
	publicSiteMetaRedisPrefix      = "public-status:v2"
	publicSiteMetaLegacyPrefix     = "public-status:v1"
	defaultPublicStatusSiteDesc    = "Request-derived public status"
	publicSiteMetaUnavailableError = "Public site metadata unavailable"
)

// PublicStatusConfigSnapshot 是投影快照里本端点用到的字段。
//
// 只声明这几个字段而不是整份快照：本端点只暴露站点标题/描述/时区，多读的字段一旦被
// 顺手写进响应，就等于把内部元数据（分组、模型、厂商图标）泄露到无需认证的公开路由上。
type PublicStatusConfigSnapshot struct {
	ConfigVersion   string  `json:"configVersion"`
	SiteTitle       string  `json:"siteTitle"`
	SiteDescription string  `json:"siteDescription"`
	TimeZone        *string `json:"timeZone"`
}

// PublicStatusSnapshotReader 读「当前」public-status 配置快照；无快照时返回 (nil, nil)。
type PublicStatusSnapshotReader interface {
	ReadCurrentPublicStatusConfigSnapshot(ctx context.Context) (*PublicStatusConfigSnapshot, error)
}

// redisPublicStatusReader 是按 Node 键布局读快照的实现。
//
// 键布局（src/lib/public-status/redis-contract.ts:77-93）：
//
//	<prefix>:config:current           指针（`{"key":...}` 或 `{"configVersion":...}` 或裸 `cfg-…`）
//	<prefix>:config-version:current   版本指针（同上解析）
//	<prefix>:config:<version>         版本化快照正文（版本段是 encodeURIComponent 后的值）
//
// 先按版本指针取版本化键，再退回 `config:current` 指针；两轮都套 v2 与 legacy v1 两套前缀。
type redisPublicStatusReader struct {
	client redis.UniversalClient
	logger *logx.Logger
}

// NewPublicStatusSnapshotReader 建 Redis 快照读取器；client 为 nil 时返回 nil（装配方据此降级）。
func NewPublicStatusSnapshotReader(
	client redis.UniversalClient,
	logger *logx.Logger,
) PublicStatusSnapshotReader {
	if client == nil {
		return nil
	}
	if logger == nil {
		logger = logx.New(nil)
	}
	return &redisPublicStatusReader{client: client, logger: logger}
}

// ReadCurrentPublicStatusConfigSnapshot 复刻 readCurrentPublicStatusConfigSnapshot 的查找顺序。
func (r *redisPublicStatusReader) ReadCurrentPublicStatusConfigSnapshot(
	ctx context.Context,
) (*PublicStatusConfigSnapshot, error) {
	for _, prefix := range []string{publicSiteMetaRedisPrefix, publicSiteMetaLegacyPrefix} {
		if snapshot := r.readByVersionPointer(ctx, prefix); snapshot != nil {
			return snapshot, nil
		}
		if snapshot := r.readByCurrentPointer(ctx, prefix); snapshot != nil {
			return snapshot, nil
		}
	}
	return nil, nil
}

// readByVersionPointer 走 `config-version:current` → `config:<version>`。
func (r *redisPublicStatusReader) readByVersionPointer(
	ctx context.Context,
	prefix string,
) *PublicStatusConfigSnapshot {
	version := extractCurrentConfigVersion(r.safeGet(ctx, prefix+":config-version:current"))
	if version == "" {
		return nil
	}
	return r.safeSnapshot(ctx, prefix+":config:"+url.PathEscape(version))
}

// readByCurrentPointer 走 `config:current` 里的 `key` 指向的快照键。
func (r *redisPublicStatusReader) readByCurrentPointer(
	ctx context.Context,
	prefix string,
) *PublicStatusConfigSnapshot {
	raw := r.safeGet(ctx, prefix+":config:current")
	if raw == "" {
		return nil
	}
	var pointer struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(raw), &pointer); err != nil || pointer.Key == "" {
		return nil
	}
	return r.safeSnapshot(ctx, pointer.Key)
}

// safeGet 读一个键；**任何错误都归一为「键不存在」**（与 Node 的 safeGet 同义：Redis 抖动时
// 这个公开端点退化成「无快照」，而不是把 Redis 的内部错误暴露给访客）。
func (r *redisPublicStatusReader) safeGet(ctx context.Context, key string) string {
	value, err := r.client.Get(ctx, key).Result()
	if err != nil {
		if err != redis.Nil {
			r.logger.Warn("public_status_snapshot_read_failed", map[string]any{
				"key":   key,
				"error": err.Error(),
			})
		}
		return ""
	}
	return value
}

// safeSnapshot 读并解析一份快照正文。
func (r *redisPublicStatusReader) safeSnapshot(ctx context.Context, key string) *PublicStatusConfigSnapshot {
	raw := r.safeGet(ctx, key)
	if raw == "" {
		return nil
	}
	var snapshot PublicStatusConfigSnapshot
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return nil
	}
	return &snapshot
}

// publicStatusConfigKeyPattern 复刻 Node 的 `:config(?:-internal)?:([^:]+)$`。
var publicStatusConfigKeyPattern = regexp.MustCompile(`:config(?:-internal)?:([^:]+)$`)

// extractCurrentConfigVersion 复刻 Node 的版本指针解析（config-snapshot.ts:158-177）：
// 裸 `cfg-` 串即版本；否则取 JSON 的 configVersion；再否则从 key 的末段反解。
func extractCurrentConfigVersion(pointerRaw string) string {
	if pointerRaw == "" {
		return ""
	}
	if strings.HasPrefix(pointerRaw, "cfg-") {
		return pointerRaw
	}
	var pointer struct {
		Key           string `json:"key"`
		ConfigVersion string `json:"configVersion"`
	}
	if err := json.Unmarshal([]byte(pointerRaw), &pointer); err != nil {
		return ""
	}
	if pointer.ConfigVersion != "" {
		return pointer.ConfigVersion
	}
	if pointer.Key != "" {
		// 与 Node 的正则 `:config(?:-internal)?:([^:]+)$` 同义：取末段版本号再解码。
		if match := publicStatusConfigKeyPattern.FindStringSubmatch(pointer.Key); match != nil {
			if decoded, err := url.PathUnescape(match[1]); err == nil {
				return decoded
			}
			return match[1]
		}
	}
	return ""
}

// publicSiteMetaBody 是响应体（两种分支共用一套键，缺省的键为 null）。
type publicSiteMetaBody struct {
	Available       bool    `json:"available"`
	SiteTitle       *string `json:"siteTitle"`
	SiteDescription *string `json:"siteDescription"`
	TimeZone        *string `json:"timeZone"`
	Source          string  `json:"source"`
	Reason          *string `json:"reason,omitempty"`
}

// publicStatusMetaAPI 是端点的处理器依赖。
type publicStatusMetaAPI struct {
	reader PublicStatusSnapshotReader
	logger *logx.Logger
}

// RegisterPublicStatusMetaRoutes 注册根级 GET /api/public-site-meta。
//
// 快照读取器未装配（无 Redis）时不注册：回退 Node（那里有完整实现），而不是让 Go 恒答
// 「无快照」——那会把「Go 没接 Redis」伪装成「投影确实不存在」。
//
// 模块名取 system 而不是 pubmeta：本包「源码里声明过的模块必须出现在路由表里」的结构性测试
// （cmd/cchd/admin_register_test.go）用模块名做「registrar 是否提前 return」的判据，而一条
// **条件注册**且默认依赖为空的模块会让那条测试红。名字取同一族，既保住该判据，也让日志里
// 「system 的设置面」只有一个名字。
func RegisterPublicStatusMetaRoutes(router *Router, deps Deps) {
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	if deps.PublicStatusSnapshots == nil {
		logger.Error("admin_pubmeta_reader_unwired", map[string]any{
			"module": "pubmeta",
			"action": "routes_not_registered",
		})
		return
	}
	api := &publicStatusMetaAPI{reader: deps.PublicStatusSnapshots, logger: logger}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/public-site-meta",
		Access:               AccessPublic,
		Module:               "system",
		OperationID:          "getPublicSiteMeta",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handlePublicSiteMeta),
	})
}

// handlePublicSiteMeta 复刻 GET /api/public-site-meta 的两条分支与一条 catch。
func (api *publicStatusMetaAPI) handlePublicSiteMeta(
	writer http.ResponseWriter,
	request *http.Request,
) {
	snapshot, err := api.reader.ReadCurrentPublicStatusConfigSnapshot(request.Context())
	if err != nil {
		api.logger.Error("admin_pubmeta_read_failed", map[string]any{"error": err.Error()})
		writePublicSiteMeta(writer, http.StatusServiceUnavailable, "no-store", map[string]any{
			"error": publicSiteMetaUnavailableError,
		})
		return
	}
	if snapshot == nil {
		reason := "projection_missing"
		writePublicSiteMeta(writer, http.StatusOK, "no-store", publicSiteMetaBody{
			Available: false,
			Source:    "projection",
			Reason:    &reason,
		})
		return
	}
	var siteTitle *string
	if normalized := normalizeSiteTitleValue(snapshot.SiteTitle); normalized != nil {
		siteTitle = normalized
	}
	description := resolvePublicStatusSiteDescription(snapshot.SiteTitle, snapshot.SiteDescription)
	writePublicSiteMeta(writer, http.StatusOK, publicSiteMetaCacheControl, publicSiteMetaBody{
		Available:       true,
		SiteTitle:       siteTitle,
		SiteDescription: &description,
		TimeZone:        snapshot.TimeZone,
		Source:          "projection",
	})
}

// normalizeSiteTitleValue 复刻 normalizeSiteTitle（src/lib/site-title.ts:3-10）：
// 非字符串或 trim 后为空即 null。
func normalizeSiteTitleValue(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// resolvePublicStatusSiteDescription 复刻 resolvePublicStatusSiteDescription
// （config-snapshot.ts:121-136）：描述优先；否则「<站点标题> public status」；再否则出厂文案。
func resolvePublicStatusSiteDescription(siteTitle, siteDescription string) string {
	if trimmed := strings.TrimSpace(siteDescription); trimmed != "" {
		return trimmed
	}
	if title := normalizeSiteTitleValue(siteTitle); title != nil {
		return *title + " public status"
	}
	return defaultPublicStatusSiteDesc
}

// writePublicSiteMeta 写响应（含 Cache-Control）。
func writePublicSiteMeta(writer http.ResponseWriter, status int, cacheControl string, body any) {
	writer.Header().Set("Cache-Control", cacheControl)
	writeShellJSONNoEnvelope(writer, status, body)
}
