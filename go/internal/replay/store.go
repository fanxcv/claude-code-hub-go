package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// Redis 热层键形制与 Lua 脚本逐字节复刻 src/app/v1/_lib/proxy/replay/replay-store.ts。
// 键前缀/TTL/脚本语义都是跨语言契约：切换期间 Node 与 Go 必须能互相命中同一份条目，
// 因此这里不引入任何「更优」的命名或脚本改写。
const (
	// OwnerLeaseTTLSeconds 是 owner 租约 TTL：owner 崩溃后新 claim 至多等这么久即可接管。
	ownerLeaseTTLSeconds = 45
	// DefaultTTLSeconds 对齐 env.schema REPLAY_TTL_SECONDS 默认 600。
	DefaultTTLSeconds = 600
)

// Lua 脚本正文（与 replay-store.ts 逐字节一致）。
const (
	luaCompareDelete = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`

	luaCompareExpire = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0`

	luaHeartbeatOwned = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
local rawMeta = redis.call('GET', KEYS[2])
if not rawMeta then
  return 0
end
local ok, meta = pcall(cjson.decode, rawMeta)
if not ok then
  return 0
end
meta.heartbeatAt = tonumber(ARGV[4])
redis.call('SETEX', KEYS[2], ARGV[2], cjson.encode(meta))
if redis.call('EXISTS', KEYS[3]) == 1 then
  redis.call('EXPIRE', KEYS[3], ARGV[2])
end
redis.call('EXPIRE', KEYS[1], ARGV[3])
return 1`

	luaPrepareOwned = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('DEL', KEYS[2])
redis.call('DEL', KEYS[3])
redis.call('EXPIRE', KEYS[1], ARGV[2])
return 1`

	luaWriteOwned = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return -1
end
local len = redis.call('LLEN', KEYS[3])
if #ARGV > 4 then
  len = redis.call('RPUSH', KEYS[3], unpack(ARGV, 5))
  if tonumber(ARGV[2]) > 0 then
    redis.call('EXPIRE', KEYS[3], ARGV[2])
  end
end
redis.call('SETEX', KEYS[2], ARGV[2], ARGV[4])
redis.call('EXPIRE', KEYS[1], ARGV[3])
return len`

	luaAbortOwned = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('SETEX', KEYS[2], ARGV[2], ARGV[3])
redis.call('DEL', KEYS[3])
redis.call('DEL', KEYS[1])
return 1`

	luaDiscardOwned = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('DEL', KEYS[2])
redis.call('DEL', KEYS[3])
redis.call('DEL', KEYS[1])
return 1`

	luaCompleteOwned = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return 0
end
redis.call('SETEX', KEYS[2], ARGV[2], ARGV[3])
redis.call('DEL', KEYS[1])
return 1`

	luaReadGeneration = `
local rawMeta = redis.call('GET', KEYS[1])
if not rawMeta then
  return {0}
end
local ok, meta = pcall(cjson.decode, rawMeta)
if not ok or tostring(meta.messageRequestId or '') ~= ARGV[1] then
  return {-1}
end
local values = redis.call('LRANGE', KEYS[2], ARGV[2], ARGV[3])
if tonumber(ARGV[4]) > 0 then
  redis.call('EXPIRE', KEYS[2], ARGV[4])
end
return {1, values}`

	luaReadOwned = `
if redis.call('GET', KEYS[1]) ~= ARGV[1] then
  return {0}
end
return {1, redis.call('LRANGE', KEYS[2], ARGV[2], ARGV[3])}`
)

// MetaStatus 是回放条目的状态机。
type MetaStatus string

const (
	MetaOwning    MetaStatus = "owning"
	MetaCompleted MetaStatus = "completed"
	MetaAborted   MetaStatus = "aborted"
)

// Delivery 对应 ReplayDelivery。
type Delivery string

const (
	DeliveryStream   Delivery = "stream"
	DeliveryBuffered Delivery = "buffered"
)

// Meta 是回放元数据，JSON 字段名与 Node 的 ReplayMeta 一致（Lua 按这些名字 cjson 解）。
type Meta struct {
	Status           MetaStatus        `json:"status"`
	Verifier         string            `json:"verifier"`
	ScopeTag         string            `json:"scopeTag"`
	StatusCode       int               `json:"statusCode"`
	Headers          map[string]string `json:"headers"`
	Delivery         Delivery          `json:"delivery"`
	Format           string            `json:"format"`
	Model            *string           `json:"model"`
	ChunkCount       int64             `json:"chunkCount"`
	ByteSize         int64             `json:"byteSize"`
	HeartbeatAt      int64             `json:"heartbeatAt"`
	MessageRequestID *int64            `json:"messageRequestId"`
	AbortReason      string            `json:"abortReason,omitempty"`
}

// PersistedRow 是 replay_payloads 的持久行（完成屏障之后的唯一正文副本）。
type PersistedRow struct {
	ReplayID               string
	Verifier               string
	ScopeTag               string
	KeyID                  int64
	UserID                 int64
	Format                 string
	Model                  *string
	StatusCode             int
	Headers                map[string]string
	Payload                string
	ByteSize               int64
	SourceMessageRequestID *int64
	ExpiresAt              time.Time
}

// PersistResult 是持久化写入的结果。
type PersistResult string

const (
	PersistWritten  PersistResult = "persisted"
	PersistExisting PersistResult = "existing"
)

// DurableConflictError 表示同 replayId 已有一个未过期的、内容不一致的持久 winner
// （同一身份推导出不同 verifier/payload —— 理论上只有哈希碰撞或实现差异会走到这里）。
type DurableConflictError struct {
	ReplayID string
}

func (e *DurableConflictError) Error() string {
	return fmt.Sprintf("replay: durable replay 冲突 %s", e.ReplayID)
}

// WriteOutcome 是 WriteOwned 的三态结果。
type WriteOutcome int

const (
	// WriteOK 写入成功，count 为当前 chunk 总数。
	WriteOK WriteOutcome = iota
	// WriteLeaseLost owner 租约已失（token 不再归本请求）。
	WriteLeaseLost
	// WriteUnavailable Redis 不可用或协议异常。
	WriteUnavailable
)

// ReadOutcome 是分页读的三态。
type ReadOutcome int

const (
	ReadOK ReadOutcome = iota
	// ReadGenerationChanged 条目已换代或消失，绝不能把返回块接到当前订阅者已有前缀后。
	ReadGenerationChanged
	// ReadUnavailable Redis 不可用或协议异常。
	ReadUnavailable
)

// StoreOptions 是 NewStore 的入参。
type StoreOptions struct {
	// Redis 是热层客户端（go-redis，可直连/哨兵/集群）。
	Redis redis.UniversalClient
	// Pools 是 PG 分道池，完成持久层用它。
	Pools *store.Pools
	// TTL 是热层条目 TTL；0 时用 DefaultTTLSeconds。
	TTL time.Duration
	// Now 是可注入时钟；nil 用 time.Now。
	Now func() time.Time
}

// Store 是回放双层存储。
type Store struct {
	rdb   redis.UniversalClient
	pools *store.Pools
	ttl   time.Duration
	now   func() time.Time

	scripts struct {
		compareDelete  *redis.Script
		compareExpire  *redis.Script
		heartbeatOwned *redis.Script
		prepareOwned   *redis.Script
		writeOwned     *redis.Script
		abortOwned     *redis.Script
		discardOwned   *redis.Script
		completeOwned  *redis.Script
		readGeneration *redis.Script
		readOwned      *redis.Script
	}
}

// NewStore 构造存储层。Pools 可空：PG 完成持久层未接线时对应方法返回错误
// （局部装配/纯 Redis 测试可用）；生产者装配必须两者齐备。
func NewStore(opts StoreOptions) (*Store, error) {
	if opts.Redis == nil {
		return nil, errors.New("replay: Redis 客户端不能为空")
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultTTLSeconds * time.Second
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	store := &Store{rdb: opts.Redis, pools: opts.Pools, ttl: opts.TTL, now: now}
	store.scripts.compareDelete = redis.NewScript(luaCompareDelete)
	store.scripts.compareExpire = redis.NewScript(luaCompareExpire)
	store.scripts.heartbeatOwned = redis.NewScript(luaHeartbeatOwned)
	store.scripts.prepareOwned = redis.NewScript(luaPrepareOwned)
	store.scripts.writeOwned = redis.NewScript(luaWriteOwned)
	store.scripts.abortOwned = redis.NewScript(luaAbortOwned)
	store.scripts.discardOwned = redis.NewScript(luaDiscardOwned)
	store.scripts.completeOwned = redis.NewScript(luaCompleteOwned)
	store.scripts.readGeneration = redis.NewScript(luaReadGeneration)
	store.scripts.readOwned = redis.NewScript(luaReadOwned)
	return store, nil
}

// TTLSeconds 返回热层 TTL（秒）。
func (s *Store) TTLSeconds() int64 {
	return int64(s.ttl / time.Second)
}

// 键构造（与 Node 逐字节一致）。
func ownerKey(replayID string) string  { return "cch:replay:owner:" + replayID }
func metaKey(replayID string) string   { return "cch:replay:meta:" + replayID }
func chunksKey(replayID string) string { return "cch:replay:chunks:" + replayID }

// GetMeta 读回热层 meta；不存在或 Redis 不可用返回 nil（读取端视为 miss）。
func (s *Store) GetMeta(ctx context.Context, replayID string) (*Meta, error) {
	raw, err := s.rdb.Get(ctx, metaKey(replayID)).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta Meta
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// WriteOwned 是 owner 热层写入：token 校验、chunk 追加、owning meta 更新与租约续期在
// 同一 Lua 内完成，避免旧 owner 在租约交接窗口污染新 owner 的 chunks/meta。
// 失败时返回 WriteUnavailable（Redis 不可用），token 失效返回 WriteLeaseLost。
func (s *Store) WriteOwned(
	ctx context.Context,
	replayID, ownerToken string,
	meta *Meta,
	values []string,
) (int64, WriteOutcome) {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return 0, WriteUnavailable
	}
	keys := []string{ownerKey(replayID), metaKey(replayID), chunksKey(replayID)}
	args := []any{ownerToken, s.TTLSeconds(), ownerLeaseTTLSeconds, string(metaJSON)}
	for _, value := range values {
		args = append(args, value)
	}
	raw, err := s.scripts.writeOwned.Run(ctx, s.rdb, keys, args...).Result()
	if err != nil {
		return 0, WriteUnavailable
	}
	length, ok := asInt64(raw)
	if !ok {
		return 0, WriteUnavailable
	}
	if length == -1 {
		return 0, WriteLeaseLost
	}
	return length, WriteOK
}

// ReadChunksForGeneration 按 owner 请求 ID 原子校验 meta 并读取 LIST 分页。
// 条目不存在返回 ([]string{}, ReadOK)；换代或消失返回 ReadGenerationChanged；
// 协议异常返回 ReadUnavailable。调用方不得把后两种结果接到既有前缀后。
func (s *Store) ReadChunksForGeneration(
	ctx context.Context,
	replayID string,
	messageRequestID int64,
	from, max int64,
	refreshTTL time.Duration,
) ([]string, ReadOutcome) {
	if max <= 0 {
		return []string{}, ReadOK
	}
	keys := []string{metaKey(replayID), chunksKey(replayID)}
	args := []any{messageRequestID, from, from + max - 1, int64(refreshTTL / time.Second)}
	raw, err := s.scripts.readGeneration.Run(ctx, s.rdb, keys, args...).Result()
	if err != nil {
		return nil, ReadUnavailable
	}
	outer, ok := raw.([]interface{})
	if !ok || len(outer) < 1 {
		return nil, ReadUnavailable
	}
	state, stateOK := asInt64(outer[0])
	if !stateOK {
		return nil, ReadUnavailable
	}
	switch {
	case state < 0:
		return nil, ReadGenerationChanged
	case state == 0:
		return []string{}, ReadOK
	case state != 1:
		return nil, ReadUnavailable
	}
	return asStringSlice(outer[1]), ReadOK
}

// ReadOwnedChunks 在 owner 完成前按 token fencing 分页读取自身 LIST，租约换代后停止。
func (s *Store) ReadOwnedChunks(
	ctx context.Context,
	replayID, ownerToken string,
	from, max int64,
) ([]string, ReadOutcome) {
	if max <= 0 {
		return []string{}, ReadOK
	}
	keys := []string{ownerKey(replayID), chunksKey(replayID)}
	args := []any{ownerToken, from, from + max - 1}
	raw, err := s.scripts.readOwned.Run(ctx, s.rdb, keys, args...).Result()
	if err != nil {
		return nil, ReadUnavailable
	}
	outer, ok := raw.([]interface{})
	if !ok || len(outer) < 1 {
		return nil, ReadUnavailable
	}
	state, stateOK := asInt64(outer[0])
	if !stateOK || state != 1 {
		return nil, ReadGenerationChanged
	}
	return asStringSlice(outer[1]), ReadOK
}

// TryClaimOwner 用 SET NX EX 抢占 owner 租约；Redis 不可用视为失败（不做 replay）。
func (s *Store) TryClaimOwner(ctx context.Context, replayID, ownerToken string) bool {
	ok, err := s.rdb.SetNX(
		ctx, ownerKey(replayID), ownerToken, ownerLeaseTTLSeconds*time.Second,
	).Result()
	return err == nil && ok
}

// RenewOwnerLease compare-and-expire：仅 token 仍归自己时续期。
// Redis 不可用按 Node 语义返回 true（状态未知，不惩罚仍在正常冲刷的 owner）。
func (s *Store) RenewOwnerLease(ctx context.Context, replayID, ownerToken string) bool {
	raw, err := s.scripts.compareExpire.Run(
		ctx, s.rdb, []string{ownerKey(replayID)},
		ownerToken, ownerLeaseTTLSeconds,
	).Result()
	if err != nil {
		return true
	}
	value, ok := asInt64(raw)
	return ok && value == 1
}

// HeartbeatOwned 原子刷新 token、owning meta 心跳与现存 LIST TTL。
func (s *Store) HeartbeatOwned(ctx context.Context, replayID, ownerToken string, heartbeatAt int64) bool {
	raw, err := s.scripts.heartbeatOwned.Run(
		ctx, s.rdb,
		[]string{ownerKey(replayID), metaKey(replayID), chunksKey(replayID)},
		ownerToken, s.TTLSeconds(), ownerLeaseTTLSeconds, heartbeatAt,
	).Result()
	if err != nil {
		return false
	}
	value, ok := asInt64(raw)
	return ok && value == 1
}

// PrepareOwned 在 PG miss 后原子确认租约仍归当前请求、清理旧热层并续租。
func (s *Store) PrepareOwned(ctx context.Context, replayID, ownerToken string) bool {
	raw, err := s.scripts.prepareOwned.Run(
		ctx, s.rdb,
		[]string{ownerKey(replayID), metaKey(replayID), chunksKey(replayID)},
		ownerToken, s.TTLSeconds(),
	).Result()
	if err != nil {
		return false
	}
	value, ok := asInt64(raw)
	return ok && value == 1
}

// ReleaseOwner 释放租约（compare-delete，只删自己的）。
func (s *Store) ReleaseOwner(ctx context.Context, replayID, ownerToken string) {
	_, _ = s.scripts.compareDelete.Run(
		ctx, s.rdb, []string{ownerKey(replayID)}, ownerToken,
	).Result()
}

// AbortOwned 仅当前 token 仍持租约时原子终止条目并清理热层响应块。返回 true 表示动作
// 已执行；返回 false 表示租约已失（条目归他人）或 Redis 不可用。
func (s *Store) AbortOwned(ctx context.Context, replayID, ownerToken string, meta *Meta) bool {
	return s.fencedMetaWrite(ctx, s.scripts.abortOwned, replayID, ownerToken, meta)
}

// DiscardOwned 放弃热层候选但不写 aborted（不遮蔽已存在的 PG winner）。
func (s *Store) DiscardOwned(ctx context.Context, replayID, ownerToken string) bool {
	keys := []string{ownerKey(replayID), metaKey(replayID), chunksKey(replayID)}
	raw, err := s.scripts.discardOwned.Run(ctx, s.rdb, keys, ownerToken).Result()
	if err != nil {
		return false
	}
	value, ok := asInt64(raw)
	return ok && value == 1
}

// CompleteOwned 仅当前 token 仍持租约时原子翻转 completed meta 并释放租约。
func (s *Store) CompleteOwned(ctx context.Context, replayID, ownerToken string, meta *Meta) bool {
	return s.fencedMetaWrite(ctx, s.scripts.completeOwned, replayID, ownerToken, meta)
}

// fencedMetaWrite 执行 3 键脚本（abort/completed），meta 序列化失败视为未执行。
func (s *Store) fencedMetaWrite(
	ctx context.Context,
	script *redis.Script,
	replayID, ownerToken string,
	meta *Meta,
) bool {
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return false
	}
	keys := []string{ownerKey(replayID), metaKey(replayID), chunksKey(replayID)}
	raw, err := script.Run(ctx, s.rdb, keys, ownerToken, s.TTLSeconds(), string(metaJSON)).Result()
	if err != nil {
		return false
	}
	value, ok := asInt64(raw)
	return ok && value == 1
}

// asInt64 把 go-redis 的返回值归一成整型。
func asInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

// asStringSlice 把 Redis 数组归一成字符串切片。
func asStringSlice(value any) []string {
	array, ok := value.([]interface{})
	if !ok {
		return []string{}
	}
	values := make([]string, 0, len(array))
	for _, item := range array {
		values = append(values, fmt.Sprintf("%v", item))
	}
	return values
}

// ===== PG 完成持久层 =====

// persistColumns 是 replay_payloads 的 INSERT/UPDATE 列。
var persistColumns = []string{
	"replay_id", "verifier", "scope_tag", "key_id", "user_id", "format", "model",
	"status_code", "headers_json", "payload", "byte_size", "source_message_request_id",
	"expires_at",
}

// persistSelect 是读回持久行的列。
const persistSelect = `replay_id, verifier, scope_tag, key_id, user_id, format, model,
	status_code, headers_json, payload, byte_size, source_message_request_id, expires_at`

// PersistCompleted 写 PG 完成持久层。
//
// RETURNING 是冲突探测信号而非冗余：`ON CONFLICT DO UPDATE ... WHERE expires_at <= now`
// 在「已存在未过期行」时既不更新也不报错，只表现为 0 行返回——因此必须靠 RETURNING 存在与否
// 区分 persisted 与 existing/conflict 两路（与 Node 的 .returning() 同语义）。
//
// 失败必须向调用方抛错：调用方（Complete 流程）依赖该错误走 abort——payload 未 durable
// 时绝不能把 meta 翻成 completed。同 id 存在未过期的内容一致行时返回 PersistExisting；
// 内容不一致时返回 *DurableConflictError。
func (s *Store) PersistCompleted(ctx context.Context, row PersistedRow) (PersistResult, error) {
	if s.pools == nil {
		return "", errors.New("replay: PG 完成持久层未接线")
	}
	pool, err := s.pools.Data()
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	headersJSON, err := json.Marshal(row.Headers)
	if err != nil {
		return "", err
	}
	placeholders := "$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13"
	query := `INSERT INTO replay_payloads (` + joinColumns() + `)
VALUES (` + placeholders + `)
ON CONFLICT (replay_id) DO UPDATE
SET verifier = EXCLUDED.verifier, scope_tag = EXCLUDED.scope_tag, key_id = EXCLUDED.key_id,
    user_id = EXCLUDED.user_id, format = EXCLUDED.format, model = EXCLUDED.model,
    status_code = EXCLUDED.status_code, headers_json = EXCLUDED.headers_json,
    payload = EXCLUDED.payload, byte_size = EXCLUDED.byte_size,
    source_message_request_id = EXCLUDED.source_message_request_id,
    created_at = $14
WHERE replay_payloads.expires_at <= $14
RETURNING replay_id`
	args := []any{
		row.ReplayID, row.Verifier, row.ScopeTag, row.KeyID, row.UserID, row.Format,
		row.Model, row.StatusCode, string(headersJSON), row.Payload, row.ByteSize,
		row.SourceMessageRequestID, row.ExpiresAt.UTC(), now,
	}
	var writtenID string
	err = pool.QueryRow(ctx, query, args...).Scan(&writtenID)
	if err == nil {
		return PersistWritten, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	// 冲突但 setWhere 未命中：已存在未过期行，做内容一致性复核。
	existing, fetchErr := s.findCompletedRow(ctx, row.ReplayID, now, false)
	if fetchErr != nil {
		return "", fetchErr
	}
	if existing == nil || !samePersistedRow(&row, existing) {
		return "", &DurableConflictError{ReplayID: row.ReplayID}
	}
	return PersistExisting, nil
}

// CleanupExpired 删除单批过期行（FOR UPDATE SKIP LOCKED，返回删除数）。
func (s *Store) CleanupExpired(ctx context.Context, cutoff time.Time) (int, error) {
	if s.pools == nil {
		return 0, errors.New("replay: PG 完成持久层未接线")
	}
	pool, err := s.pools.Data()
	if err != nil {
		return 0, err
	}
	const batchSize = 100
	query := `WITH doomed AS (
		SELECT replay_id
		FROM replay_payloads
		WHERE expires_at < $1
		ORDER BY expires_at, replay_id
		LIMIT ` + fmt.Sprintf("%d", batchSize) + `
		FOR UPDATE SKIP LOCKED
	)
	DELETE FROM replay_payloads AS rp USING doomed
	WHERE rp.replay_id = doomed.replay_id
	RETURNING 1`
	rows, err := pool.Query(ctx, query, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	return count, rows.Err()
}

// FindCompleted 读回未过期的持久行；不存在或 PG 未接线返回 nil（miss，不视为故障）。
func (s *Store) FindCompleted(ctx context.Context, replayID string) (*PersistedRow, error) {
	if s.pools == nil {
		return nil, nil
	}
	row, err := s.findCompletedRow(ctx, replayID, s.now().UTC(), true)
	if err != nil {
		return nil, err
	}
	return row, nil
}

func (s *Store) findCompletedRow(
	ctx context.Context,
	replayID string,
	now time.Time,
	errorOnPool bool,
) (*PersistedRow, error) {
	if s.pools == nil {
		return nil, errors.New("replay: PG 完成持久层未接线")
	}
	pool, err := s.pools.Data()
	if err != nil {
		return nil, err
	}
	var (
		row     PersistedRow
		model   *string
		raw     []byte
		expires time.Time
	)
	query := `SELECT ` + persistSelect + `
		FROM replay_payloads WHERE replay_id = $1 AND expires_at > $2 LIMIT 1`
	err = pool.QueryRow(ctx, query, replayID, now).Scan(
		&row.ReplayID, &row.Verifier, &row.ScopeTag, &row.KeyID, &row.UserID, &row.Format,
		&model, &row.StatusCode, &raw, &row.Payload, &row.ByteSize,
		&row.SourceMessageRequestID, &expires,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	row.Model = model
	row.ExpiresAt = expires
	_ = json.Unmarshal(raw, &row.Headers)
	return &row, nil
}

// samePersistedRow 复刻 isMatchingPersistedReplay：全部内容维度一致才算同一持久行。
func samePersistedRow(expected, actual *PersistedRow) bool {
	return expected.Verifier == actual.Verifier &&
		expected.ScopeTag == actual.ScopeTag &&
		expected.KeyID == actual.KeyID &&
		expected.UserID == actual.UserID &&
		expected.Format == actual.Format &&
		sameNullableString(expected.Model, actual.Model) &&
		expected.StatusCode == actual.StatusCode &&
		equalHeaders(expected.Headers, actual.Headers) &&
		expected.Payload == actual.Payload &&
		expected.ByteSize == actual.ByteSize
}

func sameNullableString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalHeaders(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

func joinColumns() string {
	joined := ""
	for i, column := range persistColumns {
		if i > 0 {
			joined += ", "
		}
		joined += column
	}
	return joined
}
