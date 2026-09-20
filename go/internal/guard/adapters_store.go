package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件是数据库侧的适配器实现：密钥/用户、三份配置快照、拦截与预热日志、请求日志开行。
//
// SQL 落点说明：store 目前只暴露了 system_settings、providers、provider_endpoints、keys、
// model_prices 五个只读视图，users / sensitive_words / request_filters / 密钥归并视图都还没有
// 对应的读函数。本文件因此自带最小的 SELECT，并沿用 store 的两条既有约定：
// 只读走 `row_to_json`（列名即 JSON 键名，加列不会漏读）、只读走 Data 分道。
// 这些 SELECT 的归属是 store 的管理面视图；一旦 store 补上对应视图，本文件应改成调用它，
// 删除自带 SQL（与 route 包删掉自带 SELECT 的做法一致）。

// ---- 密钥与用户 ----

// authRow 是密钥归并查询的一行（keys ⨝ users）。
type authRow struct {
	KeyID             int64           `json:"key_id"`
	KeyName           string          `json:"key_name"`
	KeyIsEnabled      *bool           `json:"key_is_enabled"`
	KeyExpiresAt      *string         `json:"key_expires_at"`
	KeyProviderGroup  *string         `json:"key_provider_group"`
	UserID            int64           `json:"user_id"`
	UserName          string          `json:"user_name"`
	UserIsEnabled     *bool           `json:"user_is_enabled"`
	UserExpiresAt     *string         `json:"user_expires_at"`
	UserProviderGroup *string         `json:"user_provider_group"`
	AllowedClients    json.RawMessage `json:"allowed_clients"`
	BlockedClients    json.RawMessage `json:"blocked_clients"`
	AllowedModels     json.RawMessage `json:"allowed_models"`
}

// userRow 是 users 表按 id 读取的一行。
type userRow struct {
	ID             int64           `json:"id"`
	Name           string          `json:"name"`
	IsEnabled      *bool           `json:"is_enabled"`
	ExpiresAt      *string         `json:"expires_at"`
	AllowedClients json.RawMessage `json:"allowed_clients"`
	BlockedClients json.RawMessage `json:"blocked_clients"`
	AllowedModels  json.RawMessage `json:"allowed_models"`
}

// authEntry 是密钥缓存里的一条：解析结果 + 有效分组所需的两级 provider_group。
//
// 分组列随密钥查询一并取回（同一次 SELECT），因此选路拿有效分组不需要第二次查询。
type authEntry struct {
	resolution        AuthResolution
	keyProviderGroup  string
	userProviderGroup string
}

// authStoreOptions 是 AuthStore 的构造参数。
type authStoreOptions struct {
	pools    *store.Pools
	keyTTL   time.Duration
	userTTL  time.Duration
	logger   *logx.Logger
	registry *cfgsync.Registry
}

// AuthStore 用真实库实现 AuthResolver、UserDirectory 与 UserExpiryMarker。
//
// 缓存形制与 Node 的差异（有意）：Node 把活跃密钥与用户缓存进 Redis（api-key-auth-cache.ts，
// 默认 TTL 60s），供全部实例共享；本波是进程内缓存（同样的 TTL、同样的失效通道），
// 跨实例一致性与真空过滤器（apiKeyVacuumFilter）尚未移植。语义上的差别只有一点：
// 某实例删掉密钥后，其它实例最迟在一个 TTL 或一次 api_keys 失效消息后跟上。
type AuthStore struct {
	pools    *store.Pools
	keys     *cfgsync.KeyedCache[authEntry]
	users    *cfgsync.TTLMap[int64, User]
	logger   *logx.Logger
	registry *cfgsync.Registry

	keyLoads  counter
	userLoads counter
}

func newAuthStore(opts authStoreOptions) *AuthStore {
	return &AuthStore{
		pools:    opts.pools,
		keys:     cfgsync.NewKeyedCache[authEntry](opts.keyTTL, DefaultAPIKeyCacheSize),
		users:    cfgsync.NewTTLMap[int64, User](opts.userTTL, DefaultUserCacheSize),
		logger:   opts.logger,
		registry: opts.registry,
	}
}

// authKeysQuery 复刻 resolveApiKeyAuthOutcome 的 DB 路径。
//
// 刻意放宽 WHERE：不过滤 is_enabled / expires_at，失败原因由调用方分类；用户软删直接折叠成
// 「不存在」（与密钥不存在的语义等价）。keys.key 不是唯一列，故返回全部匹配行再由调用方
// 取最有利状态。
const authKeysQuery = `SELECT row_to_json(t)::text FROM (
	SELECT k.id AS key_id, k.name AS key_name, k.is_enabled AS key_is_enabled,
	       k.expires_at AS key_expires_at, k.provider_group AS key_provider_group,
	       u.id AS user_id, u.name AS user_name, u.is_enabled AS user_is_enabled,
	       u.expires_at AS user_expires_at, u.provider_group AS user_provider_group,
	       u.allowed_clients, u.blocked_clients, u.allowed_models
	FROM keys k
	JOIN users u ON u.id = k.user_id
	WHERE k.key = $1 AND k.deleted_at IS NULL AND u.deleted_at IS NULL
) t`

const authUserQuery = `SELECT row_to_json(t)::text FROM (
	SELECT id, name, is_enabled, expires_at, allowed_clients, blocked_clients, allowed_models
	FROM users WHERE id = $1 AND deleted_at IS NULL
) t`

// ResolveAPIKey 解析密钥。
func (a *AuthStore) ResolveAPIKey(ctx context.Context, key string) (AuthResolution, error) {
	trimmed := strings.TrimSpace(key)
	if trimmed == "" {
		return AuthResolution{}, ErrKeyNotFound
	}
	entries, err := a.entries(ctx, trimmed)
	if err != nil {
		return AuthResolution{}, err
	}
	if len(entries) == 0 {
		return AuthResolution{}, ErrKeyNotFound
	}
	entry := entries[0]
	// 顺带填用户缓存：Node 在密钥命中后同样只补一次 user（getCachedUser）。
	a.users.Set(entry.resolution.User.ID, entry.resolution.User)
	return entry.resolution, nil
}

// entries 走密钥缓存取归并结果。
//
// 负结果（不存在/禁用/过期）不进缓存：它们随时可能因为管理面新建或启用密钥而翻转，
// 缓存负结果会让「刚建的密钥不可用」变成不定长窗口。
func (a *AuthStore) entries(ctx context.Context, key string) ([]authEntry, error) {
	return a.keys.Get(ctx, key, func(ctx context.Context) ([]authEntry, error) {
		// 计数在 loadKey 内部、查询之后自增：负结果同样打库，同样要计入（否则
		// 「负结果不缓存」这条断言看不见代价）。
		return a.loadKey(ctx, key)
	})
}

// loadKey 读库并分类失败原因。
func (a *AuthStore) loadKey(ctx context.Context, key string) ([]authEntry, error) {
	rows, err := queryJSONRows(ctx, a.pools, authKeysQuery, key)
	a.keyLoads.add()
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrKeyNotFound
	}

	now := time.Now()
	parsed := make([]authRow, 0, len(rows))
	for _, raw := range rows {
		var row authRow
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("guard: 密钥行反序列化失败: %w", err)
		}
		parsed = append(parsed, row)
	}

	// 同一条密钥串可能匹配多行（keys.key 非唯一）。取最有利状态：ok > 过期 > 禁用，
	// 避免「一个禁用行排前面」把一个仍在用的密钥判成 key_disabled。
	var active *authRow
	for index := range parsed {
		row := &parsed[index]
		if row.KeyIsEnabled != nil && *row.KeyIsEnabled && !expiredAt(row.KeyExpiresAt, now) {
			active = row
			break
		}
	}
	if active == nil {
		for index := range parsed {
			if row := &parsed[index]; row.KeyIsEnabled != nil && *row.KeyIsEnabled {
				return nil, ErrKeyExpired
			}
		}
		return nil, ErrKeyDisabled
	}

	user, err := userFromRow(active.UserID, active.UserName, active.UserIsEnabled, active.UserExpiresAt,
		active.AllowedClients, active.BlockedClients, active.AllowedModels)
	if err != nil {
		return nil, err
	}
	entry := authEntry{
		resolution: AuthResolution{
			User: user,
			Key:  Key{ID: active.KeyID, Name: active.KeyName, UserID: active.UserID},
		},
		keyProviderGroup:  derefString(active.KeyProviderGroup),
		userProviderGroup: derefString(active.UserProviderGroup),
	}
	markLoaded(a.registry, cfgsync.DomainAPIKeys)
	return []authEntry{entry}, nil
}

// User 按 id 读回用户属性，命中缓存即不查库。
func (a *AuthStore) User(ctx context.Context, userID int64) (User, error) {
	if userID == 0 {
		return User{}, fmt.Errorf("guard: 用户 id 为空: %w", store.ErrNotFound)
	}
	if cached, ok := a.users.Get(userID); ok {
		return cached, nil
	}
	raw, err := queryJSONRow(ctx, a.pools, authUserQuery, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return User{}, fmt.Errorf("guard: 用户 %d 不存在: %w", userID, store.ErrNotFound)
		}
		return User{}, err
	}
	var row userRow
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		return User{}, fmt.Errorf("guard: 用户行反序列化失败: %w", err)
	}
	user, err := userFromRow(row.ID, row.Name, row.IsEnabled, row.ExpiresAt,
		row.AllowedClients, row.BlockedClients, row.AllowedModels)
	if err != nil {
		return User{}, err
	}
	a.userLoads.add()
	a.users.Set(row.ID, user)
	return user, nil
}

// MarkUserExpired 复刻 markUserExpired：只把仍在启用态的用户标记为禁用，幂等。
func (a *AuthStore) MarkUserExpired(ctx context.Context, userID int64) error {
	pool, err := a.pools.Control()
	if err != nil {
		return err
	}
	_, err = pool.Exec(ctx,
		`UPDATE users SET is_enabled = false, updated_at = now()
		 WHERE id = $1 AND is_enabled = true AND deleted_at IS NULL`,
		userID)
	if err != nil {
		return fmt.Errorf("guard: 标记用户过期失败: %w", err)
	}
	a.users.Delete(userID)
	return nil
}

// ProviderGroup 返回请求的有效分组，复刻 getEffectiveProviderGroup：
// key.provider_group > user.provider_group > default。
//
// 走的是同一条密钥缓存，因此正常情况下零额外查询；缓存已被失效时最多补一次读。
func (a *AuthStore) ProviderGroup(ctx context.Context, req *pctx.Context) string {
	if req == nil {
		return route.GroupDefault
	}
	auth, ok := req.Auth()
	if !ok || auth.APIKey == "" {
		return route.GroupDefault
	}
	entries, err := a.entries(ctx, auth.APIKey)
	if err != nil {
		a.logger.Debug("guard.adapters.group_lookup_failed", map[string]any{"error": err.Error()})
		return route.GroupDefault
	}
	if len(entries) == 0 {
		return route.GroupDefault
	}
	entry := entries[0]
	if entry.keyProviderGroup != "" {
		return entry.keyProviderGroup
	}
	if entry.userProviderGroup != "" {
		return entry.userProviderGroup
	}
	return route.GroupDefault
}

// Invalidate 清空密钥缓存（绑定 api_keys 失效通道）。
//
// 不清用户缓存：用户缓存的失效由管理面在用户变更时单独发布（Node 侧同样分两条路径）。
func (a *AuthStore) Invalidate() { a.keys.Invalidate() }

// KeyLoads 返回密钥装载次数（观测与「不每请求查库」断言用）。
func (a *AuthStore) KeyLoads() int64 { return a.keyLoads.get() }

// UserLoads 返回用户装载次数。
func (a *AuthStore) UserLoads() int64 { return a.userLoads.get() }

// userFromRow 把一行映射成 User 属性切面。
func userFromRow(
	id int64,
	name string,
	isEnabled *bool,
	expiresAt *string,
	allowedClients json.RawMessage,
	blockedClients json.RawMessage,
	allowedModels json.RawMessage,
) (User, error) {
	expiry, err := parseTimestamp(derefString(expiresAt))
	if err != nil {
		return User{}, err
	}
	user := User{
		ID:             id,
		Name:           name,
		IsEnabled:      isEnabled != nil && *isEnabled,
		AllowedClients: decodeStringArray(allowedClients),
		BlockedClients: decodeStringArray(blockedClients),
		AllowedModels:  decodeStringArray(allowedModels),
	}
	if !expiry.IsZero() {
		user.ExpiresAt = &expiry
	}
	return user, nil
}

// expiredAt 判定时间戳是否已过期；nil 表示永不过期。
func expiredAt(raw *string, now time.Time) bool {
	expiry, err := parseTimestamp(derefString(raw))
	if err != nil || expiry.IsZero() {
		return false
	}
	return !expiry.After(now)
}

// derefString 解引用字符串指针，nil 得空串。
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ---- 配置快照 ----

// SettingsCache 是 system_settings 的进程内快照（对应 system-settings-cache.ts）。
//
// TTL 取 cfgsync 的域规格（SystemSettingsTTL）：失效消息之外还有时间自愈上界，
// 因为管理面改设置时若失效广播丢失，也不能让错误配置活到进程重启。
type SettingsCache struct {
	pools    *store.Pools
	cache    *cfgsync.ValueCache[store.SystemSettings]
	logger   *logx.Logger
	registry *cfgsync.Registry
	loads    counter
}

func newSettingsCache(pools *store.Pools, registry *cfgsync.Registry, logger *logx.Logger) *SettingsCache {
	ttl := cfgsync.Spec(cfgsync.DomainSystemSettings).TTL
	return &SettingsCache{
		pools:    pools,
		cache:    cfgsync.NewValueCache[store.SystemSettings](ttl),
		logger:   logger,
		registry: registry,
	}
}

// FindSystemSettings 返回快照；加载失败时把错误交给调用方（各守卫按自己的 fail-open 语义处理）。
func (c *SettingsCache) FindSystemSettings(ctx context.Context) (*store.SystemSettings, error) {
	settings, err := c.cache.Get(ctx, func(ctx context.Context) (store.SystemSettings, error) {
		loaded, err := c.pools.FindSystemSettings(ctx)
		if err != nil {
			return store.SystemSettings{}, err
		}
		c.loads.add()
		markLoaded(c.registry, cfgsync.DomainSystemSettings)
		return *loaded, nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return &settings, nil
}

// Invalidate 清空快照（绑定 system_settings 失效通道）。
func (c *SettingsCache) Invalidate() { c.cache.Invalidate() }

// Loads 返回装载次数。
func (c *SettingsCache) Loads() int64 { return c.loads.get() }

// sensitiveWordsQuery 取启用态词表。
//
// 顺序按 id 升序：命中判定要取「第一条命中」，顺序因此是可见行为（错误信息里回显命中词）。
const sensitiveWordsQuery = `SELECT row_to_json(t)::text FROM (
	SELECT word, match_type FROM sensitive_words WHERE is_enabled = true ORDER BY id ASC
) t`

// SensitiveCache 是敏感词快照（对应 getSensitiveWords 的缓存面）。
//
// 无 TTL：域规格把敏感词列为事件驱动（NoExpiry），只靠失效消息与订阅恢复时的 resync 重载。
type SensitiveCache struct {
	pools    *store.Pools
	cache    *cfgsync.ValueCache[[]SensitiveWord]
	logger   *logx.Logger
	registry *cfgsync.Registry
	loads    counter
}

func newSensitiveCache(pools *store.Pools, registry *cfgsync.Registry, logger *logx.Logger) *SensitiveCache {
	return &SensitiveCache{
		pools:    pools,
		cache:    cfgsync.NewValueCache[[]SensitiveWord](0, cfgsync.WithValueCacheNoExpiry[[]SensitiveWord]()),
		logger:   logger,
		registry: registry,
	}
}

// SensitiveWords 返回词表快照。
func (c *SensitiveCache) SensitiveWords(ctx context.Context) ([]SensitiveWord, error) {
	words, err := c.cache.Get(ctx, func(ctx context.Context) ([]SensitiveWord, error) {
		rows, err := queryJSONRows(ctx, c.pools, sensitiveWordsQuery)
		if err != nil {
			return nil, err
		}
		loaded := make([]SensitiveWord, 0, len(rows))
		for _, raw := range rows {
			var row struct {
				Word      string `json:"word"`
				MatchType string `json:"match_type"`
			}
			if err := json.Unmarshal([]byte(raw), &row); err != nil {
				return nil, fmt.Errorf("guard: 敏感词行反序列化失败: %w", err)
			}
			loaded = append(loaded, SensitiveWord{Word: row.Word, MatchType: row.MatchType})
		}
		c.loads.add()
		markLoaded(c.registry, cfgsync.DomainSensitiveWords)
		return loaded, nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return words, nil
}

// Invalidate 清空词表快照。
func (c *SensitiveCache) Invalidate() { c.cache.Invalidate() }

// Loads 返回装载次数。
func (c *SensitiveCache) Loads() int64 { return c.loads.get() }

// requestFiltersQuery 取启用态过滤规则。
//
// 排序 (priority, id) 升序由 SQL 保证：守卫步骤不重排，因为顺序影响叠加结果。
const requestFiltersQuery = `SELECT row_to_json(t)::text FROM (
	SELECT id, scope, action, target, priority, replacement, match_type,
	       binding_type, provider_ids, group_tags, rule_mode, execution_phase, operations
	FROM request_filters WHERE is_enabled = true
	ORDER BY priority ASC, id ASC
) t`

// FilterCache 是请求过滤器快照（域规格同为事件驱动，无 TTL）。
type FilterCache struct {
	pools    *store.Pools
	cache    *cfgsync.ValueCache[[]RequestFilter]
	logger   *logx.Logger
	registry *cfgsync.Registry
	loads    counter
}

func newFilterCache(pools *store.Pools, registry *cfgsync.Registry, logger *logx.Logger) *FilterCache {
	return &FilterCache{
		pools:    pools,
		cache:    cfgsync.NewValueCache[[]RequestFilter](0, cfgsync.WithValueCacheNoExpiry[[]RequestFilter]()),
		logger:   logger,
		registry: registry,
	}
}

// RequestFilters 返回规则快照（已按 priority, id 升序）。
func (c *FilterCache) RequestFilters(ctx context.Context) ([]RequestFilter, error) {
	filters, err := c.cache.Get(ctx, func(ctx context.Context) ([]RequestFilter, error) {
		rows, err := queryJSONRows(ctx, c.pools, requestFiltersQuery)
		if err != nil {
			return nil, err
		}
		loaded := make([]RequestFilter, 0, len(rows))
		for _, raw := range rows {
			var row struct {
				ID             int64           `json:"id"`
				Scope          string          `json:"scope"`
				Action         string          `json:"action"`
				Target         string          `json:"target"`
				Priority       int             `json:"priority"`
				Replacement    json.RawMessage `json:"replacement"`
				MatchType      string          `json:"match_type"`
				BindingType    string          `json:"binding_type"`
				ProviderIDs    json.RawMessage `json:"provider_ids"`
				GroupTags      json.RawMessage `json:"group_tags"`
				RuleMode       string          `json:"rule_mode"`
				ExecutionPhase string          `json:"execution_phase"`
				Operations     json.RawMessage `json:"operations"`
			}
			if err := json.Unmarshal([]byte(raw), &row); err != nil {
				return nil, fmt.Errorf("guard: 过滤规则行反序列化失败: %w", err)
			}
			if row.MatchType == "" {
				// match_type 是 nullable 列，空值在 Node 侧与「无匹配类型」等价（simple 模式）。
				row.MatchType = ""
			}
			loaded = append(loaded, RequestFilter{
				ID:             row.ID,
				Scope:          row.Scope,
				Action:         row.Action,
				Target:         row.Target,
				Priority:       row.Priority,
				Replacement:    row.Replacement,
				MatchType:      row.MatchType,
				BindingType:    row.BindingType,
				ProviderIDs:    decodeInt64Array(row.ProviderIDs),
				GroupTags:      decodeStringArray(row.GroupTags),
				RuleMode:       row.RuleMode,
				ExecutionPhase: row.ExecutionPhase,
				Operations:     row.Operations,
			})
		}
		c.loads.add()
		markLoaded(c.registry, cfgsync.DomainRequestFilters)
		return loaded, nil
	}, nil)
	if err != nil {
		return nil, err
	}
	return filters, nil
}

// Invalidate 清空规则快照。
func (c *FilterCache) Invalidate() { c.cache.Invalidate() }

// Loads 返回装载次数。
func (c *FilterCache) Loads() int64 { return c.loads.get() }

// ---- 拦截与预热日志 ----

// BlockedRecorder 把被拦截的请求记成一行**已终态**的 message_request。
//
// 走 terminal.Settler.SettleBlocked 而不是自己 INSERT：拦截类终态要求「建行 + 写终态」两步
// 的最终行、账本行与 outbox 事件与 Node 的单条 INSERT 等价，这套语义已经由终态包实现并
// 测试过（store 的插入面不表达 status_code/blocked_by）。适配器只负责把载荷搬过去。
type BlockedRecorder struct {
	settler *terminal.Settler
	logger  *logx.Logger
	rows    counter
}

func newBlockedRecorder(settler *terminal.Settler, logger *logx.Logger) *BlockedRecorder {
	return &BlockedRecorder{settler: settler, logger: logger}
}

// RecordBlocked 复刻 logBlockedRequest：provider_id = 0（未选供应商）、成本记 0、状态码即拦截码。
//
// pc 用于终态上报：拦截行已在库里（行 id 由建行结果给出），故它同样该进观测面。
func (r *BlockedRecorder) RecordBlocked(ctx context.Context, pc *pctx.Context, record BlockedRecord) error {
	if r == nil || r.settler == nil {
		return errors.New("guard: 终态结算器未接线，拦截日志未落库")
	}
	statusCode := record.StatusCode
	if statusCode <= 0 {
		statusCode = 400
	}
	create := store.CreateMessageRequestData{
		// provider_id = 0 是「被拦截、未选供应商」的约定值。
		ProviderID: 0,
		UserID:     record.UserID,
		Key:        record.APIKey,
	}
	if record.Model != "" {
		model := record.Model
		create.Model = &model
	}
	if record.SessionID != "" {
		sessionID := record.SessionID
		create.SessionID = &sessionID
	}
	zeroCost := "0"
	create.CostUSD = &zeroCost

	blockedBy := record.BlockedBy
	reason := string(record.Reason)
	settlement := terminal.Settlement{
		StatusCode:    statusCode,
		BlockedBy:     &blockedBy,
		BlockedReason: &reason,
	}
	if record.ErrorMessage != "" {
		message := record.ErrorMessage
		settlement.ErrorMessage = &message
	}
	if create.Model != nil {
		settlement.Model = create.Model
	}
	if _, err := r.settler.SettleBlocked(ctx, pc, create, settlement); err != nil {
		return fmt.Errorf("guard: 拦截日志落库失败: %w", err)
	}
	r.rows.add()
	return nil
}

// Rows 返回已落库的拦截行数（观测用）。
func (r *BlockedRecorder) Rows() int64 {
	if r == nil {
		return 0
	}
	return r.rows.get()
}

// WarmupRecorder 把被抢答的 warmup 请求记成一行终态记录（provider_id = 0，不计费）。
type WarmupRecorder struct {
	settler *terminal.Settler
	logger  *logx.Logger
	rows    counter
}

func newWarmupRecorder(settler *terminal.Settler, logger *logx.Logger) *WarmupRecorder {
	return &WarmupRecorder{settler: settler, logger: logger}
}

// RecordWarmup 复刻 warmup-guard 的落库：状态码 200、成本 NULL（显式不写，避免前端显示 $0）、
// blocked_by = warmup。
func (r *WarmupRecorder) RecordWarmup(ctx context.Context, pc *pctx.Context, record WarmupRecord) error {
	if r == nil || r.settler == nil {
		return errors.New("guard: 终态结算器未接线，warmup 日志未落库")
	}
	create := store.CreateMessageRequestData{
		ProviderID: 0,
		UserID:     record.UserID,
		Key:        record.APIKey,
	}
	if record.Model != "" {
		model := record.Model
		create.Model = &model
	}
	if record.OriginalModel != "" {
		original := record.OriginalModel
		create.OriginalModel = &original
	}
	if record.SessionID != "" {
		sessionID := record.SessionID
		create.SessionID = &sessionID
	}
	if record.Sequence > 0 {
		sequence := record.Sequence
		create.RequestSequence = &sequence
	}
	if record.UserAgent != "" {
		agent := record.UserAgent
		create.UserAgent = &agent
	}
	if record.Endpoint != "" {
		endpoint := record.Endpoint
		create.Endpoint = &endpoint
	}
	if record.MessagesCount > 0 {
		count := record.MessagesCount
		create.MessagesCount = &count
	}

	duration := int(record.DurationMS)
	blockedBy := "warmup"
	reason := `{"reason":"anthropic_warmup_intercepted","note":"已由 CCH 抢答，未转发上游，不计费/不限流/不计入统计"}`
	settlement := terminal.Settlement{
		StatusCode:    200,
		DurationMS:    &duration,
		TTFTMS:        &duration,
		FirstByteMS:   &duration,
		BlockedBy:     &blockedBy,
		BlockedReason: &reason,
	}
	if create.Model != nil {
		settlement.Model = create.Model
	}
	if _, err := r.settler.SettleBlocked(ctx, pc, create, settlement); err != nil {
		return fmt.Errorf("guard: warmup 日志落库失败: %w", err)
	}
	r.rows.add()
	return nil
}

// Rows 返回已落库的 warmup 行数（观测用）。
func (r *WarmupRecorder) Rows() int64 {
	if r == nil {
		return 0
	}
	return r.rows.get()
}

// ---- 请求日志开行 ----

// MessageWriter 复刻 ProxyMessageService.ensureContext：在选路之后建立请求日志行。
//
// 只开行、不落终态（终态属终态包），开行成功后把行标识写进 pctx（SetMessageRequestID），
// 让终态结算不需要重开一行。三个「拿不到就不写」的条件与 Node 一致：无鉴权结果、
// 无 provider 时不建行。
//
// 已知缺口（需要上游供给才能补齐，见报告）：
//   - session_id / request_sequence / session_identity* 经 SessionLookup 钩子取值，
//     钩子为 nil 时这些列写 NULL。**session_identity 与 session_identity_kind 已补齐**：
//     形制由亲和身份事实（pctx.AffinityIdentity）+ 设置 affinityIgnoreClientSessionId
//     按 Node 的两条支路判定（见 session_identity.go），这是使用记录页「渠道复用 / 新会话
//     新渠道」一列的数据源。affinity_scope_tag / affinity_fingerprint /
//     affinity_fingerprint_chain 仍写 NULL（不影响界面；Node 只是把它们同步进账本，
//     而 Go 不写账本——见 go-data-plane-takeover-plan 的约束）。
//   - cost_multiplier / group_cost_multiplier 依赖选路结果里的供应商倍率，而
//     pctx.ProviderSelection 只带路由必需字段，故本波不写（终态计费由供应商快照自行取用）。
type MessageWriter struct {
	pools *store.Pools
	// Body 取本次请求正文的工厂（由接线方按请求注入）。
	Body BodyFactory
	// SessionLookup 取已绑定的会话身份；nil 表示会话包未接线。
	SessionLookup func(*pctx.Context) (SessionResult, bool)
	// IgnoreClientSessionID 对应系统设置 affinityIgnoreClientSessionId：为真时
	// 粘性交给最长前缀亲和，会话身份的形制记作 prefix_affinity（Node 的 skipSessionBinding
	// 分支）。它是进程级事实，随亲和装配一起从系统设置读到（同 openAffinity 的读法）。
	IgnoreClientSessionID bool
	logger                *logx.Logger
	rows                  counter
}

func newMessageWriter(pools *store.Pools, logger *logx.Logger, ignoreClientSessionID bool) *MessageWriter {
	return &MessageWriter{pools: pools, logger: logger, IgnoreClientSessionID: ignoreClientSessionID}
}

// WithBody 返回绑定了本次请求正文工厂与会话查询的开行器视图。
//
// 为什么需要视图：Body 与 SessionLookup 是每请求事实，而 MessageWriter 本身是进程级共享
// 实例——直接改共享字段会让并发请求互相看到对方的正文（数据竞争 + 错行）。视图共享 Pools
// 与日志器，只替换这两个按请求变化的字段。
func (m *MessageWriter) WithBody(
	body BodyFactory,
	lookup func(*pctx.Context) (SessionResult, bool),
) *MessageWriter {
	if m == nil {
		return nil
	}
	return &MessageWriter{pools: m.pools, logger: m.logger, Body: body, SessionLookup: lookup, IgnoreClientSessionID: m.IgnoreClientSessionID}
}

// EnsureContext 建立请求日志上下文。
func (m *MessageWriter) EnsureContext(ctx context.Context, req *pctx.Context) error {
	auth, ok := req.Auth()
	if !ok || auth.KeyID == 0 || auth.UserID == 0 || auth.APIKey == "" {
		return nil
	}
	selection, ok := req.Provider()
	if !ok || selection.ProviderID == 0 {
		// Node 在无 provider 时把 messageContext 置空，不建行。
		return nil
	}
	body := requestBody(m.Body, req)
	data := store.CreateMessageRequestData{
		ProviderID: selection.ProviderID,
		UserID:     auth.UserID,
		Key:        auth.APIKey,
	}
	if model := bodyModel(body); model != "" {
		// 此处尚未发生模型重定向，故 original_model 与当前模型相同（Node 在 ensureContext 里
		// 同样是「先把它当作原始模型」，重定向后再幂等地覆写）。
		data.Model = &model
		original := model
		data.OriginalModel = &original
	}
	if agent := req.Headers().Get("user-agent"); agent != "" {
		data.UserAgent = &agent
	}
	if ip := req.ClientIP(); ip != "" {
		data.ClientIP = &ip
	}
	if endpoint := req.Path(); endpoint != "" {
		data.Endpoint = &endpoint
	}
	if count := bodyMessageCount(body); count > 0 {
		data.MessagesCount = &count
	}
	// 请求级审计（思考强度等）：Node 在**建行前**写入，Go 此前从未写，导致使用记录页的
	// 思考强度列恒为空。提取器按**客户端入站协议**选（见 special_settings.go 的三条不变量）。
	data.SpecialSettings = buildRequestSpecialSettings(
		body,
		clientFormatOf(req.ProtocolFrom()),
		req.Path(),
	)
	if m.SessionLookup != nil {
		if session, ok := m.SessionLookup(req); ok {
			if session.SessionID != "" {
				sessionID := session.SessionID
				data.SessionID = &sessionID
				// 会话身份与它的形制（Node 的 getSessionIdentityMetadata 两条支路）：
				// 见 sessionIdentityColumns 的注释。
				identity, kind := sessionIdentityColumns(sessionID, auth.KeyID, req, m.IgnoreClientSessionID)
				if identity != "" {
					data.SessionIdentity = &identity
				}
				if kind != "" {
					data.SessionIdentityKind = &kind
				}
			}
			if session.Sequence > 0 {
				sequence := session.Sequence
				data.RequestSequence = &sequence
			}
		}
	}

	// 无会话身份时 Node 的默认元数据同样是 kind="session_id"（identity 为空）。
	// 不写会让该列在 Go 侧恒为 NULL，而使用记录页正是读它显示「渠道复用 / 新会话新渠道」。
	if data.SessionIdentityKind == nil {
		kind := sessionIdentityKindSessionID
		data.SessionIdentityKind = &kind
	}

	createdID, err := m.pools.CreateMessageRequestID(ctx, data)
	if err != nil {
		return fmt.Errorf("guard: 建立请求日志上下文失败: %w", err)
	}
	m.rows.add()
	// 行标识交给上下文：终态结算（终态包）靠它认出「guard 开的这一行」。重复开行会在这里
	// 直接报错，不静默覆盖——那会导致两行、两套账。
	if err := req.SetMessageRequestID(createdID); err != nil {
		return fmt.Errorf("guard: 请求日志行标识写入失败: %w", err)
	}
	m.logger.Debug("guard.message_context.created", map[string]any{"requestId": createdID})
	return nil
}

// Rows 返回已开行的请求日志数（观测用）。
func (m *MessageWriter) Rows() int64 {
	if m == nil {
		return 0
	}
	return m.rows.get()
}

// ---- 查询小工具 ----

// queryJSONRows 执行只读查询并返回每行的 row_to_json 文本。
func queryJSONRows(
	ctx context.Context,
	pools *store.Pools,
	query string,
	args ...any,
) ([]string, error) {
	pool, err := pools.Data()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(contextOrBackground(ctx), query, args...)
	if err != nil {
		return nil, fmt.Errorf("guard: 只读查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]string, 0, 4)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("guard: 读取只读行失败: %w", err)
		}
		results = append(results, payload)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("guard: 只读遍历失败: %w", err)
	}
	return results, nil
}

// queryJSONRow 执行只读查询并返回单行的 row_to_json 文本；无行时返回 store.ErrNotFound。
func queryJSONRow(
	ctx context.Context,
	pools *store.Pools,
	query string,
	args ...any,
) (string, error) {
	rows, err := queryJSONRows(ctx, pools, query, args...)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", store.ErrNotFound
	}
	return rows[0], nil
}
