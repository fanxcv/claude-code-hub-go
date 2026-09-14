package usersreset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件复刻 src/lib/user-statistics-reset/reset-status-store.ts：状态读写、用户维度认领与
// 5h 固定窗口准备。
//
// 与 Node 的差别只有一处，且是刻意的：Node 的 getReadyRedis 会**等**连接就绪（最多
// REDIS_COMMAND_TIMEOUT_MS），Go 侧直接以命令超时报错。理由：那套等待是 ioredis 的 client 状态机
// 产物（status !== "ready" 时挂监听器）；go-redis 的连接自带重试与超时，再叠一层等待只是多一条
// 会自己超时的路径。对调用方的可见语义不变：拿不到 Redis 就是 REDIS_UNAVAILABLE。

// ErrRedisUnavailable 表示命令连接不可用（reset-status-store.ts:33-35）。
var ErrRedisUnavailable = &Error{Code: ErrCodeRedisUnavailable}

// StatusStore 是状态键与认领键的读写门面。
type StatusStore struct {
	client redis.UniversalClient
}

// NewStatusStore 构造状态存储；client 为 nil 时所有方法返回 REDIS_UNAVAILABLE。
func NewStatusStore(client redis.UniversalClient) *StatusStore {
	return &StatusStore{client: client}
}

// Available 表示命令连接已装配。
func (s *StatusStore) Available() bool { return s != nil && s.client != nil }

// Set 写入状态记录（reset-status-store.ts:70-80）。
//
// 写完不校验返回值：SETEX 没有「写失败但无错误」的情形（RedisKVStore.set 在 Node 侧要判
// 假值是因为它容忍 undefined），故这里只把错误当作失败。
func (s *StatusStore) Set(ctx context.Context, record Record) error {
	if !s.Available() {
		return ErrRedisUnavailable
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("usersreset: 序列化作业状态失败: %w", err)
	}
	if err := s.client.Set(ctx, statusKey(record.ResetID), payload, StatusTTLSeconds*time.Second).Err(); err != nil {
		return fmt.Errorf("usersreset: 写入作业状态失败: %w", err)
	}
	return nil
}

// Get 读状态记录；不存在返回 (nil, nil)（reset-status-store.ts:82-101）。
//
// 缺字段的容错照抄：Node 用 `?? []` 与 `=== 1 ? 1 : null` 兜住早期版本写下的记录，
// Go 侧同样把 nil 切片归一成空切片、把非 1 的版本号归一成 nil——否则一条旧记录会以
// 「版本号是 0」的形态进入 ensurePrepared 的判断，被当成已准备而跳过 5h 准备。
func (s *StatusStore) Get(ctx context.Context, resetID string) (*Record, error) {
	if !s.Available() {
		return nil, ErrRedisUnavailable
	}
	raw, err := s.client.Get(ctx, statusKey(resetID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("usersreset: 读取作业状态失败: %w", err)
	}
	var record Record
	if err := decodeRecord(raw, &record); err != nil {
		// 内容坏掉时报错而不是当作不存在：当作不存在会让调用方重新排一个作业，
		// 而真正的坏记录会一直留在键里（reset-status-store.ts:96-98 同）。
		return nil, err
	}
	return &record, nil
}

// decodeRecord 解析状态键的内容并归一缺字段（Node 的 `?? []` 与 `=== 1 ? 1 : null`）。
func decodeRecord(raw []byte, target *Record) error {
	if err := json.Unmarshal(raw, target); err != nil {
		return newError(ErrCodeStatusInvalid)
	}
	if target.Fixed5hKeyIDs == nil {
		target.Fixed5hKeyIDs = []int64{}
	}
	if target.Fixed5hPreparationVersion == nil || *target.Fixed5hPreparationVersion != 1 {
		target.Fixed5hPreparationVersion = nil
	}
	return nil
}

// Delete 删除状态记录（reset-status-store.ts:103-106）。
func (s *StatusStore) Delete(ctx context.Context, resetID string) error {
	if !s.Available() {
		return ErrRedisUnavailable
	}
	if err := s.client.Del(ctx, statusKey(resetID)).Err(); err != nil {
		return fmt.Errorf("usersreset: 删除作业状态失败: %w", err)
	}
	return nil
}

// ClaimActive 抢占用户维度的认领（reset-status-store.ts:108-127）。
//
// 返回已有的持有者 id 时 acquired=false：同一用户同一时刻只能有一个在途重置，
// 后来的请求要么返回已有作业，要么在对账后重排（见 queue.go）。
func (s *StatusStore) ClaimActive(ctx context.Context, userID int64, resetID string) (bool, string, error) {
	if !s.Available() {
		return false, "", ErrRedisUnavailable
	}
	key := activeKey(userID)
	acquired, err := s.client.SetNX(ctx, key, resetID, StatusTTLSeconds*time.Second).Result()
	if err != nil {
		return false, "", fmt.Errorf("usersreset: 抢占用户重置认领失败: %w", err)
	}
	if acquired {
		return true, resetID, nil
	}
	existing, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		// 抢不到却又读不到：只可能是在 SETNX 与 GET 之间过期了。报错让调用方重试整条路径，
		// 而不是当成「已认领」返回一个空 id（reset-status-store.ts:69-72 同）。
		return false, "", newError(ErrCodeActiveClaimFailed)
	}
	if err != nil {
		return false, "", fmt.Errorf("usersreset: 读取用户重置认领失败: %w", err)
	}
	return false, existing, nil
}

// ReleaseActive 释放认领，且只删「值还是自己」的键（reset-status-store.ts:129-135）。
//
// 为什么必须比对：认领键有 7 天 TTL，一个早已结束的作业若在此时无条件 DEL，会把后来者
// 正在跑的作业的认领删掉——那个作业随后就无法被对账路径找到（认领是它对外的唯一凭据）。
func (s *StatusStore) ReleaseActive(ctx context.Context, userID int64, resetID string) error {
	if !s.Available() {
		return ErrRedisUnavailable
	}
	if err := s.client.Eval(ctx, luaCompareDelete, []string{activeKey(userID)}, resetID).Err(); err != nil {
		return fmt.Errorf("usersreset: 释放用户重置认领失败: %w", err)
	}
	return nil
}

// PrepareFixed5h 准备 5h 固定窗口并返回切点时刻
// （cost-cache-cleanup.ts:62-99 的 prepareUserStatisticsResetFixed5h）。
//
// 幂等：标记键存在即回它已存的切点，不再删键、不再改切点。重试与新实例接手都靠这一点，
// 否则第二次执行会用一个**更晚**的切点去删行，把第一次之后新产生的统计一并清掉。
func (s *StatusStore) PrepareFixed5h(
	ctx context.Context,
	resetID string,
	userID int64,
	keyIDs []int64,
) (time.Time, error) {
	if !s.Available() {
		return time.Time{}, ErrRedisUnavailable
	}
	keys := fixed5hWindowKeys(resetID, userID, keyIDs)
	raw, err := s.client.Eval(ctx, luaPrepareFixed5h, keys, StatusTTLSeconds).Result()
	if err != nil {
		return time.Time{}, fmt.Errorf("usersreset: 准备 5h 固定窗口失败: %w", err)
	}
	millis, ok := asInt64(raw)
	if !ok || millis <= 0 {
		// 脚本回空或非数值＝准备失败（Node 侧返回 null 后由调用方抛 FIXED_5H_PREPARE_FAILED）。
		return time.Time{}, newError(ErrCodeFixed5hPrepareFailed)
	}
	return time.UnixMilli(millis).UTC(), nil
}

// asInt64 把 Redis 的脚本返回值（int64 或字符串）解析成毫秒数。
func asInt64(raw any) (int64, bool) {
	switch typed := raw.(type) {
	case int64:
		return typed, true
	case string:
		var parsed int64
		if _, err := fmt.Sscan(typed, &parsed); err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
