package adminapi

import (
	"context"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是供应商写路径的**熔断配置同步**：把三个熔断阈值写进 Redis 哈希
// `circuit_breaker:config:<id>`（Node 的 saveProviderCircuitConfig，
// src/lib/redis/circuit-breaker-config.ts:105-135）。
//
// 为什么这条不能省：**数据面的熔断读只看 Redis**（Go 的 route.ReadProviderCircuitConfig 键缺失即取
// 出厂默认 5/1800000/2，不回落库；Node 的 loadProviderCircuitConfig 才回落库）。若写路径只改库不写
// 这个哈希，管理员把阈值调成 9 之后，数据面仍按 5 闸门工作——不报错、不日志，只是行为不是他要的那个。
// 更坏的一档是**陈旧哈希**：Node 早年缓存过 threshold=5，Go 改成 9 而不覆盖哈希，两个后端会得到
// 两个不同的闸门。
//
// 与 Node 的差异（登记为白名单）：Node 写失败只记 warn 不阻断请求（:128-134），这里同；但 Go 侧在
// 未装配（无 Redis）时额外记一条 warn，因为「没有 Redis」在 Go 数据面等于「熔断阈值无处生效」。

// ProviderCircuitConfigWriter 写供应商熔断配置到 Redis 哈希。
//
// 比 redis.UniversalClient 窄：本模块只用得上「写一个哈希」这一件事，测试也就不必造一个完整客户端替身。
type ProviderCircuitConfigWriter interface {
	// WriteProviderCircuitConfig 覆盖该供应商的熔断三字段（字段名与 Node 逐字一致），
	// 外加**等待阶梯**的两项（releaseIncrement / maxOpenCount，Go 侧增强）。
	WriteProviderCircuitConfig(
		ctx context.Context,
		providerID int64,
		failureThreshold, openDurationMS, halfOpenSuccessThreshold,
		releaseIncrementMS, maxOpenCount int64,
	) error
}

// redisCircuitConfigWriter 是 ProviderCircuitConfigWriter 的 Redis 实现。
type redisCircuitConfigWriter struct {
	client redis.UniversalClient
}

// NewRedisProviderCircuitConfig 用命令连接装配熔断配置写入器；client 为 nil 时返回 nil。
func NewRedisProviderCircuitConfig(client redis.UniversalClient) ProviderCircuitConfigWriter {
	if client == nil {
		return nil
	}
	return &redisCircuitConfigWriter{client: client}
}

func (w *redisCircuitConfigWriter) WriteProviderCircuitConfig(
	ctx context.Context,
	providerID int64,
	failureThreshold, openDurationMS, halfOpenSuccessThreshold,
	releaseIncrementMS, maxOpenCount int64,
) error {
	key := route.ProviderConfigKeyPrefix + strconv.FormatInt(providerID, 10)
	// HSET 而非覆盖整个键：Node 侧同（hset，不是 set），键上可能还有别的读取方写入的字段。
	if err := w.client.HSet(ctx, key, map[string]any{
		"failureThreshold":         strconv.FormatInt(failureThreshold, 10),
		"openDuration":             strconv.FormatInt(openDurationMS, 10),
		"halfOpenSuccessThreshold": strconv.FormatInt(halfOpenSuccessThreshold, 10),
		"releaseIncrement":         strconv.FormatInt(releaseIncrementMS, 10),
		"maxOpenCount":             strconv.FormatInt(maxOpenCount, 10),
	}).Err(); err != nil {
		return fmt.Errorf("adminapi: 写熔断配置失败: %w", err)
	}
	return nil
}

// providerSyncCircuitConfig 把库里的三个阈值同步进 Redis（创建与更新后都调）。
//
// 失败只记 warn：与 Node 一致（熔断配置同步失败不该让「改名字」这个请求失败）。
func providerSyncCircuitConfig(deps Deps, providerID int64, thresholds providerCircuitThresholds) {
	if deps.ProviderCircuitConfig == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_providers_circuit_config_unwired", map[string]any{
				"providerId": providerID,
				"effect":     "数据面的熔断阈值读 Redis，未装配时该供应商按出厂默认工作",
			})
		}
		return
	}
	if err := deps.ProviderCircuitConfig.WriteProviderCircuitConfig(
		context.Background(), providerID,
		thresholds.FailureThreshold, thresholds.OpenDurationMS, thresholds.HalfOpenSuccessThreshold,
		thresholds.ReleaseIncrementMS, thresholds.MaxOpenCount,
	); err != nil && deps.Logger != nil {
		deps.Logger.Warn("admin_providers_circuit_config_sync_failed", map[string]any{
			"providerId": providerID,
			"error":      err.Error(),
		})
	}
}

// providerCircuitThresholds 是同步进 Redis 的熔断阈值（含等待阶梯两项）。
type providerCircuitThresholds struct {
	FailureThreshold         int64
	OpenDurationMS           int64
	HalfOpenSuccessThreshold int64
	// ReleaseIncrementMS 是等待阶梯的递增时长（0 = 不启用）。
	ReleaseIncrementMS int64
	// MaxOpenCount 是等待阶梯的最大次数（0 = 不启用）。
	MaxOpenCount int64
}

// providerThresholdsFromRow 从库里的一行取三阈值；列为空时取与 route 包一致的出厂默认
// （Node 同：loadProviderCircuitConfig 在供应商不存在时用 DEFAULT_CIRCUIT_BREAKER_CONFIG）。
func providerThresholdsFromRow(provider *store.AdminProvider) providerCircuitThresholds {
	thresholds := providerCircuitThresholds{
		FailureThreshold:         route.DefaultFailureThreshold,
		OpenDurationMS:           route.DefaultOpenDurationMS,
		HalfOpenSuccessThreshold: route.DefaultHalfOpenSuccessThreshold,
		// 等待阶梯的出厂默认是 0 = 不启用（窗口恒为 openDuration），与加阶梯之前一致。
		ReleaseIncrementMS: route.DefaultOpenDurationIncrementMS,
		MaxOpenCount:       route.DefaultMaxOpenCount,
	}
	if provider == nil {
		return thresholds
	}
	if provider.CircuitFailureThreshold != nil {
		thresholds.FailureThreshold = int64(*provider.CircuitFailureThreshold)
	}
	if provider.CircuitOpenDuration != nil {
		thresholds.OpenDurationMS = int64(*provider.CircuitOpenDuration)
	}
	if provider.CircuitHalfOpenThreshold != nil {
		thresholds.HalfOpenSuccessThreshold = int64(*provider.CircuitHalfOpenThreshold)
	}
	// 可空列：未填（null）就是不启用阶梯。负值一律当 0——数据面也会把负数夹到 0
	// （route.nonNegativeConfigInt），两处同口径以免「库里 -1、哈希里夹到 0」看起来像丢配置。
	if provider.CircuitReleaseIncrement != nil && *provider.CircuitReleaseIncrement > 0 {
		thresholds.ReleaseIncrementMS = int64(*provider.CircuitReleaseIncrement)
	}
	if provider.CircuitMaxOpenCount != nil && *provider.CircuitMaxOpenCount > 0 {
		thresholds.MaxOpenCount = int64(*provider.CircuitMaxOpenCount)
	}
	return thresholds
}
