package adminapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件实现 Invalidator：把失效广播到 Node 侧的 Redis 缓存与本进程的配置快照。
//
// 唯一真源：src/lib/security/api-key-auth-cache.ts（20-23 的键布局与 307-386 的失效）、
// src/lib/redis/cost-cache-cleanup.ts（103-190 的 user 清理、319-360 的单个 key 清理）、
// src/lib/rate-limit/service.ts:495-498（total_cost 键的写法）、src/lib/redis/pubsub.ts:7-14
// （通道名）。
//
// 两条必须守住的纪律：
//  1. **不得新建通道**：通道名是跨进程契约，改名即静默失去失效（Go 与 Node 共用同一套）。
//     本文件只从 cfgsync.Spec 取通道，自己一个都不拼。
//  2. **不得把密钥原文写进日志**：total_cost 的键名里嵌着 API Key 原文（Node 侧的既有布局就是
//     这样），所以下面的日志只记条数、键 ID 与脱敏后的模式，绝不回显扫描到的键名。

// apiKeyAuthCachePrefix 与 api-key-auth-cache.ts:20-23 的键布局逐字一致（CACHE_VERSION = 1）。
const (
	apiKeyAuthKeyPrefix  = "api_key_auth:v1:key:"
	apiKeyAuthUserPrefix = "api_key_auth:v1:user:"
)

// costCacheScanBatch 是 SCAN 的批量大小。
//
// 用一个小的批量而不是全量 KEYS：KEYS 在大键空间下会阻塞整个 Redis 实例（Node 侧也是用
// scanPattern 而不是 KEYS）。
const costCacheScanBatch = 256

// CacheInvalidator 是 Invalidator 接口的默认实现。
type CacheInvalidator struct {
	client redis.UniversalClient
	bus    *cfgsync.Bus
	pools  *store.Pools
	logger *logx.Logger
}

// InvalidatorOptions 是失效广播器的构造参数。
type InvalidatorOptions struct {
	// Redis 是命令连接；nil 时所有 Redis 失效是空操作（与 Node 未配 Redis 时一致）。
	Redis redis.UniversalClient
	// Bus 是配置域失效总线；nil 时 PublishDomain 只清本进程缓存（由 cfgsync 的 TTL 兜底）。
	Bus *cfgsync.Bus
	// Pools 用于「按键取值的总消费缓存」清理时需要的一把密钥串；nil 时跳过该部分。
	Pools *store.Pools
	// Logger 为 nil 时静默。
	Logger *logx.Logger
}

// NewCacheInvalidator 建失效广播器。
func NewCacheInvalidator(deps Deps, options InvalidatorOptions) *CacheInvalidator {
	// Logger 一律补成非 nil：logx 的方法在 nil 接收者上会 panic，而失效广播里有多处
	// 「删除条数」之类的 info 日志不该因为没接日志器就把请求打挂。
	logger := options.Logger
	if logger == nil {
		logger = deps.Logger
	}
	if logger == nil {
		logger = logx.New(nil)
	}
	pools := options.Pools
	if pools == nil {
		pools = deps.Store
	}
	return &CacheInvalidator{
		client: options.Redis,
		bus:    options.Bus,
		pools:  pools,
		logger: logger,
	}
}

// InvalidateKeyAuth 复刻 invalidateCachedKey（api-key-auth-cache.ts:307-319）。
func (i *CacheInvalidator) InvalidateKeyAuth(ctx context.Context, apiKey string) {
	if i == nil || i.client == nil || apiKey == "" {
		return
	}
	digest := sha256.Sum256([]byte(apiKey))
	key := apiKeyAuthKeyPrefix + hex.EncodeToString(digest[:])
	if err := i.client.Del(ctx, key).Err(); err != nil {
		i.warn("admin_invalidate_key_auth_failed", map[string]any{"error": err.Error()})
	}
}

// InvalidateUserAuth 复刻 invalidateCachedUser（api-key-auth-cache.ts:378-386）。
func (i *CacheInvalidator) InvalidateUserAuth(ctx context.Context, userID int64) {
	if i == nil || i.client == nil {
		return
	}
	if err := i.client.Del(ctx, fmt.Sprintf("%s%d", apiKeyAuthUserPrefix, userID)).Err(); err != nil {
		i.warn("admin_invalidate_user_auth_failed", map[string]any{
			"userId": userID,
			"error":  err.Error(),
		})
	}
}

// InvalidateKeyCost 复刻 clearSingleKeyCostCache（cost-cache-cleanup.ts:319-360）。
//
// 需要两段键：`key:<id>:cost_*` 只用到 id；`total_cost:key:<key 原文>` 用的是密钥串本身。
// 接口只给了 id，故这里补一次按键读值（只读 Data 分道、一次主键查询）。读不到时只清第一段
// 并记 warn：宁可少清一个 5 分钟 TTL 的缓存，也不要因为一次失败让整个失效调用报错。
func (i *CacheInvalidator) InvalidateKeyCost(ctx context.Context, keyID int64) {
	if i == nil || i.client == nil {
		return
	}
	i.scanAndDelete(ctx, fmt.Sprintf("key:%d:cost_*", keyID), "key_cost")

	keyValue, err := i.keyValueByID(ctx, keyID)
	if err != nil {
		i.warn("admin_invalidate_key_cost_lookup_failed", map[string]any{
			"keyId": keyID,
			"error": err.Error(),
		})
		return
	}
	if keyValue == "" {
		return
	}
	i.scanAndDelete(ctx, "total_cost:key:"+keyValue, "key_total_cost")
	i.scanAndDelete(ctx, "total_cost:key:"+keyValue+":*", "key_total_cost_suffixed")
}

// InvalidateUserCost 复刻 clearUserCostCache（cost-cache-cleanup.ts:103-190）。
//
// 该函数在 Node 侧要调用方传入 keyIds，这里自己查一次用户名下的键 id：调用方（A1 的资源模块）
// 未必持有完整清单，而漏清会让「重置用户限额」看起来没生效。userID 为 -1（ADMIN_TOKEN 虚拟
// 用户）时无键可清，直接返回。
func (i *CacheInvalidator) InvalidateUserCost(ctx context.Context, userID int64) {
	if i == nil || i.client == nil {
		return
	}
	i.scanAndDelete(ctx, fmt.Sprintf("user:%d:cost_*", userID), "user_cost")
	i.scanAndDelete(ctx, fmt.Sprintf("total_cost:user:%d", userID), "user_total_cost")
	i.scanAndDelete(ctx, fmt.Sprintf("total_cost:user:%d:*", userID), "user_total_cost_suffixed")

	if userID <= 0 {
		return
	}
	keyIDs, err := i.keyIDsByUser(ctx, userID)
	if err != nil {
		i.warn("admin_invalidate_user_cost_lookup_failed", map[string]any{
			"userId": userID,
			"error":  err.Error(),
		})
		return
	}
	for _, keyID := range keyIDs {
		i.scanAndDelete(ctx, fmt.Sprintf("key:%d:cost_*", keyID), "user_key_cost")
	}
}

// PublishDomain 广播配置域失效。
//
// 通道取 cfgsync.Spec(domain).Channel（空 Channel 表示该域不参与广播，此时只记 warn）：
// 通道名不许在本文件里拼，拼错不会报错，只会让另一侧的缓存静静过期。
func (i *CacheInvalidator) PublishDomain(ctx context.Context, domain cfgsync.Domain) {
	if i == nil {
		return
	}
	if i.bus == nil {
		i.warn("admin_invalidate_domain_unwired", map[string]any{"domain": string(domain)})
		return
	}
	spec := cfgsync.Spec(domain)
	if spec.Channel == "" {
		i.warn("admin_invalidate_domain_unknown", map[string]any{"domain": string(domain)})
		return
	}
	i.bus.Publish(ctx, spec.Channel, "")
}

// scanAndDelete 按模式扫描并删除；单次扫描失败只告警。
func (i *CacheInvalidator) scanAndDelete(ctx context.Context, pattern, label string) {
	deleted := 0
	iterator := i.client.Scan(ctx, 0, pattern, costCacheScanBatch).Iterator()
	for iterator.Next(ctx) {
		key := iterator.Val()
		if err := i.client.Del(ctx, key).Err(); err != nil {
			i.warn("admin_invalidate_delete_failed", map[string]any{
				"label": label,
				"error": err.Error(),
			})
			continue
		}
		deleted++
	}
	if err := iterator.Err(); err != nil {
		i.warn("admin_invalidate_scan_failed", map[string]any{
			"label": label,
			// 模式也不回显：total_cost 的模式里嵌着密钥原文。
			"pattern": redactCostPattern(pattern),
			"error":   err.Error(),
		})
		if deleted == 0 {
			return
		}
	}
	if deleted > 0 {
		i.info("admin_invalidate_cost_deleted", map[string]any{
			"label":   label,
			"deleted": deleted,
		})
	}
}

// redactCostPattern 把模式里的密钥原文抹掉，只留下形状。
//
// 只对嵌了密钥原文的那类模式生效（`total_cost:key:` 之后的部分就是密钥）。
func redactCostPattern(pattern string) string {
	const marker = "total_cost:key:"
	if !strings.HasPrefix(pattern, marker) {
		return pattern
	}
	return marker + "<key>"
}

// keyValueByID 读一把密钥的原文。
//
// 返回值是凭据，只用于拼 Redis 键，**不得**写进日志、错误信息或审计。
func (i *CacheInvalidator) keyValueByID(ctx context.Context, keyID int64) (string, error) {
	if i.pools == nil {
		return "", nil
	}
	const query = `SELECT row_to_json(t)::text FROM (
		SELECT key FROM keys WHERE id = $1 AND deleted_at IS NULL
	) t`
	rows, err := queryJSONRows(ctx, i.pools, query, keyID)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	var row struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(rows[0]), &row); err != nil {
		return "", fmt.Errorf("adminapi: 密钥行反序列化失败: %w", err)
	}
	return row.Key, nil
}

// keyIDsByUser 读某个用户名下的键 id（含禁用键：禁用键的计数键也可能存在）。
func (i *CacheInvalidator) keyIDsByUser(ctx context.Context, userID int64) ([]int64, error) {
	if i.pools == nil {
		return nil, nil
	}
	const query = `SELECT row_to_json(t)::text FROM (
		SELECT id FROM keys WHERE user_id = $1 AND deleted_at IS NULL
	) t`
	rows, err := queryJSONRows(ctx, i.pools, query, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, raw := range rows {
		var row struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("adminapi: 键 id 反序列化失败: %w", err)
		}
		ids = append(ids, row.ID)
	}
	return ids, nil
}

// warn 记一条告警；Logger 缺席时静默。
func (i *CacheInvalidator) warn(event string, fields map[string]any) {
	if i.logger == nil {
		return
	}
	i.logger.Warn(event, fields)
}

// info 记一条信息；Logger 缺席时静默。
func (i *CacheInvalidator) info(event string, fields map[string]any) {
	if i.logger == nil {
		return
	}
	i.logger.Info(event, fields)
}

// Wired 报告失效广播是否接上了任意一路。
//
// 保留成显式查询而不是「静默空操作」：装配期可据此把「切了 Go 但失效广播没接」这条静默风险
// 写进启动日志（与亲和静默失效是同一类问题）。
func (i *CacheInvalidator) Wired() bool {
	if i == nil {
		return false
	}
	return i.client != nil || i.bus != nil
}
