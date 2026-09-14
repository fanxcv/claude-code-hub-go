package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是供应商写路径的**撤销快照层**：把 Node 的 RedisKVStore 三个实例搬到 Go 侧。
//
// 唯一真源：src/actions/providers.ts:1469-1480（三个 store 的 prefix 与 TTL）、
// :2225-2514（preview / apply）、:2515-2700（undoPatch）、:3016-3070（undoDelete）、
// src/lib/redis/redis-kv-store.ts:59-110（setex 写、get、getAndDelete 语义）。
//
// 为什么必须先有这一层：Node 的删除是软删（`deleted_at = now()`）+ 一张 60 秒的撤销快照；
// Go 若只做软删而不写快照，UI 上的「撤销」按钮会永久报「撤销窗口已过期」——那是**静默的错行为**，
// 比不接管这条路由更坏（见 providers.go 顶部未注册清单里 DELETE 那条理由）。

const (
	// providerDeleteUndoPrefix 是删除撤销快照的键前缀（actions/providers.ts:1478）。
	providerDeleteUndoPrefix = "cch:prov:undo-del:"
	// providerPatchUndoPrefix 是更新撤销快照的键前缀（actions/providers.ts:1474）。
	providerPatchUndoPrefix = "cch:prov:undo-patch:"
	// providerPreviewPrefix 是批量补丁预览快照的键前缀（actions/providers.ts:1470）。
	providerPreviewPrefix = "cch:prov:preview:"

	// ProviderDeleteUndoTTLSeconds 是删除撤销窗口（actions/providers.ts:1344）。
	ProviderDeleteUndoTTLSeconds = 60
	// ProviderPatchUndoTTLSeconds 是单条更新的撤销窗口（actions/providers.ts:1343）。
	//
	// 比删除的 60 秒短得多是 Node 的既定取值，不是笔误：单条更新的撤销靠内存快照，
	// 窗口必须小到「用户来不及用旧值覆盖别人的新改动」。
	ProviderPatchUndoTTLSeconds = 10
	// ProviderPreviewTTLSeconds 是批量补丁预览的窗口（actions/providers.ts:1342）。
	ProviderPreviewTTLSeconds = 60
)

// ProviderUndoKV 是撤销快照的键值存储面。
//
// 比 `redis.UniversalClient` 窄的理由：本模块只用得上 setex / get / getAndDelete / del 四个动作，
// 而测试需要能替身（撤销闭环的失败分支——「token 过期」「operationId 不匹配」——用真 Redis
// 只能靠睡够 TTL 来触发，那是等真实长超时的坏测试）。
type ProviderUndoKV interface {
	// SetEx 写入并附 TTL。
	SetEx(ctx context.Context, key string, payload []byte, ttl time.Duration) error
	// Get 读取；不存在返回 found=false（不是错误）。
	Get(ctx context.Context, key string) (payload []byte, found bool, err error)
	// GetDel 原子读取并删除（Node 的 getAndDelete）。
	GetDel(ctx context.Context, key string) (payload []byte, found bool, err error)
	// Del 删除一个或多个键。
	Del(ctx context.Context, keys ...string) error
}

// redisProviderUndoKV 是 ProviderUndoKV 的 Redis 实现。
type redisProviderUndoKV struct {
	client redis.UniversalClient
}

// NewRedisProviderUndoKV 用命令连接装配撤销快照存储；client 为 nil 时返回 nil
// （装配处据此不注册依赖它的路由，而不是注册出「撤销必失败」的假实现）。
func NewRedisProviderUndoKV(client redis.UniversalClient) ProviderUndoKV {
	if client == nil {
		return nil
	}
	return &redisProviderUndoKV{client: client}
}

func (s *redisProviderUndoKV) SetEx(
	ctx context.Context,
	key string,
	payload []byte,
	ttl time.Duration,
) error {
	return s.client.SetEx(ctx, key, payload, ttl).Err()
}

func (s *redisProviderUndoKV) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := s.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (s *redisProviderUndoKV) GetDel(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := s.client.GetDel(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (s *redisProviderUndoKV) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return s.client.Del(ctx, keys...).Err()
}

// ProviderDeleteUndo 是删除撤销快照（actions/providers.ts:1462-1466 的 ProviderDeleteUndoSnapshot）。
//
// 字段名逐字对齐 Node 的 JSON，因为**快照可能由 Node 写、由 Go 读**（切换期两个后端并存）：
// 少一个字段或改一个键名，读出来就是 operationId 不匹配 → 撤销恒返回 UNDO_CONFLICT。
type ProviderDeleteUndo struct {
	UndoToken   string  `json:"undoToken"`
	OperationID string  `json:"operationId"`
	ProviderIDs []int64 `json:"providerIds"`
}

// ProviderPatchUndo 是单条/批量更新的撤销快照（actions/providers.ts:1456-1461）。
//
// Preimage 的键是**字符串化的 provider id**、值是 Node 的 camelCase Provider 字段
// actions/providers.ts:1496 的 SINGLE_EDIT_PREIMAGE_FIELD_TO_PROVIDER_KEY），故用 map[string]any：
// 快照要能原样回写 Node，改形状就等于让 Node 读不懂。
type ProviderPatchUndo struct {
	UndoToken   string                    `json:"undoToken"`
	OperationID string                    `json:"operationId"`
	ProviderIDs []int64                   `json:"providerIds"`
	Preimage    map[string]map[string]any `json:"preimage"`
	// Durable 为 true 表示快照来自持久账本（批量补丁的幂等记录），Go 侧尚未移植该账本，
	// 故读到的 durable 快照按「无账本可回退」处理（见 providers_write.go 的 undoPatch）。
	Durable bool `json:"durable,omitempty"`
}

// providerDeleteUndoKey 与 providerPatchUndoKey 产出快照键。
func providerDeleteUndoKey(token string) string { return providerDeleteUndoPrefix + token }

func providerPatchUndoKey(token string) string { return providerPatchUndoPrefix + token }

// putProviderDeleteUndo 写入删除撤销快照（TTL 固定 60 秒，与 Node 一致）。
func putProviderDeleteUndo(
	ctx context.Context,
	kv ProviderUndoKV,
	snapshot ProviderDeleteUndo,
) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("adminapi: 序列化删除撤销快照失败: %w", err)
	}
	return kv.SetEx(
		ctx,
		providerDeleteUndoKey(snapshot.UndoToken),
		payload,
		ProviderDeleteUndoTTLSeconds*time.Second,
	)
}

// putProviderPatchUndo 写入更新撤销快照（TTL 固定 10 秒，与 Node 一致）。
func putProviderPatchUndo(
	ctx context.Context,
	kv ProviderUndoKV,
	snapshot ProviderPatchUndo,
) error {
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("adminapi: 序列化更新撤销快照失败: %w", err)
	}
	return kv.SetEx(
		ctx,
		providerPatchUndoKey(snapshot.UndoToken),
		payload,
		ProviderPatchUndoTTLSeconds*time.Second,
	)
}

// takeProviderPatchUndo 原子读并删除更新撤销快照（Node 的 getAndDelete，:2573-2580）。
//
// 为什么更新用原子的而删除不用：删除的软删是可重复执行的（恢复一个已恢复的行等于无操作），
// 所以 Node 先校验 operationId 再删 token；更新的 preimage 回写不是幂等的（它会把字段倒回旧值），
// 两个并发撤销必须只有一个能拿到快照。
func takeProviderPatchUndo(
	ctx context.Context,
	kv ProviderUndoKV,
	token string,
) (*ProviderPatchUndo, error) {
	payload, found, err := kv.GetDel(ctx, providerPatchUndoKey(token))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return decodeProviderPatchUndo(payload)
}

// loadProviderDeleteUndo 读删除撤销快照；不存在返回 nil。
func loadProviderDeleteUndo(
	ctx context.Context,
	kv ProviderUndoKV,
	token string,
) (*ProviderDeleteUndo, error) {
	payload, found, err := kv.Get(ctx, providerDeleteUndoKey(token))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var snapshot ProviderDeleteUndo
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, fmt.Errorf("adminapi: 解析删除撤销快照失败: %w", err)
	}
	return &snapshot, nil
}

func decodeProviderPatchUndo(payload []byte) (*ProviderPatchUndo, error) {
	var snapshot ProviderPatchUndo
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, fmt.Errorf("adminapi: 解析更新撤销快照失败: %w", err)
	}
	return &snapshot, nil
}

// newProviderUndoToken 复刻 Node 的 createProviderPatchUndoToken（actions/providers.ts:1637-1639）。
//
// 注意名字：删除用的也是这个前缀（removeProvider 调的就是它），不是笔误。
func newProviderUndoToken() string {
	return "provider_patch_undo_" + randomUUIDString()
}

// newProviderOperationID 复刻 Node 的 createProviderPatchOperationId（:1641-1643）。
func newProviderOperationID() string {
	return "provider_patch_apply_" + randomUUIDString()
}
