// Package deps 建立并探测 cchd 的外部依赖：PostgreSQL（pgx/v5）与 Redis（go-redis/v9）。
//
// 连接预算按 config.SplitPoolBudget 分道，物理连接数等于总预算，不随 lane 数相乘。
// 错误信息在离开本包前一律经 sanitize 处理，确保 DSN / REDIS_URL 原文不泄漏到日志。
package deps

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/httpapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// Bundle 持有本进程的全部外部依赖句柄。
type Bundle struct {
	// pools 是与数据面、管理面共用的连接池；PG 探针走它的 control 道，
	// 本包不再自建第二条池（否则物理连接数是配置预算的两倍）。
	pools *store.Pools
	// dsnConfigured 记录 DSN 是否配置：探针在池装配前也要能区分
	// 「没配 DSN」与「配了但还没接上池」。
	dsnConfigured bool
	redis         *redis.Client
	rules         *cfgsync.Snapshot
	// secrets 里的字符串在对外报错前会被替换掉，防止凭据回显。
	secrets []string
	// closeOnce 让 Close 幂等：调用方可能同时从 defer 与显式路径关闭。
	closeOnce sync.Once
	closeErr  error
}

// New 按配置建立连接。DSN 或 REDIS_URL 缺省时对应句柄为 nil，不算错误——
// 骨架必须能在没有依赖的情况下启动并如实报告 not_configured。
func New(ctx context.Context, cfg config.Config, rules *cfgsync.Snapshot) (*Bundle, error) {
	bundle := &Bundle{rules: rules}
	if cfg.DSN != "" {
		bundle.secrets = append(bundle.secrets, cfg.DSN)
	}
	if cfg.RedisURL != "" {
		bundle.secrets = append(bundle.secrets, cfg.RedisURL)
	}

	bundle.dsnConfigured = cfg.DSN != ""

	if cfg.RedisURL != "" {
		options, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			return nil, fmt.Errorf("解析 REDIS_URL 失败（值已隐去）: %w", bundle.sanitize(err))
		}
		options.MaxRetries = 2
		options.DialTimeout = 5 * time.Second
		options.ReadTimeout = 10 * time.Second
		options.WriteTimeout = 10 * time.Second
		bundle.redis = redis.NewClient(options)
	}

	return bundle, nil
}

// AttachStorePools 把共享连接池接到 PG 探针上。
//
// 装配顺序约束：deps.New 早于 store.Open（后者是惰性的，不建物理连接），
// 故池只能后接。池接上之前，即使 DSN 已配置，探针也如实报 not_configured。
func (b *Bundle) AttachStorePools(pools *store.Pools) { b.pools = pools }

// PingPG 探测 PostgreSQL：走共享池的 control 道，不做准入（探活不该被负载挤掉）。
func (b *Bundle) PingPG(ctx context.Context) (httpapi.DependencyStatus, string) {
	if !b.dsnConfigured {
		return httpapi.StatusNotSet, "DSN not configured"
	}
	if b.pools == nil {
		return httpapi.StatusNotSet, "store pools not attached"
	}
	pool, err := b.pools.Control()
	if err != nil {
		return httpapi.StatusError, b.sanitize(err).Error()
	}
	if err := pool.Ping(ctx); err != nil {
		return httpapi.StatusError, b.sanitize(err).Error()
	}
	return httpapi.StatusOK, ""
}

// PingRedis 探测 Redis。
func (b *Bundle) PingRedis(ctx context.Context) (httpapi.DependencyStatus, string) {
	if b.redis == nil {
		return httpapi.StatusNotSet, "REDIS_URL not configured"
	}
	if err := b.redis.Ping(ctx).Err(); err != nil {
		return httpapi.StatusError, b.sanitize(err).Error()
	}
	return httpapi.StatusOK, ""
}

// RulesStatus 报告规则快照状态。
func (b *Bundle) RulesStatus() (httpapi.DependencyStatus, string) {
	if b.rules == nil || !b.rules.Loaded() {
		return httpapi.StatusNotLoaded, "rules snapshot not loaded yet"
	}
	return httpapi.StatusOK, ""
}

// Close 释放全部连接，可重复调用。返回的错误已脱敏。
func (b *Bundle) Close() error {
	b.closeOnce.Do(func() {
		// PG 池是与数据面/管理面共用的 store.Pools，由 boot 统一收口，本包不关。
		if b.redis != nil {
			if err := b.redis.Close(); err != nil {
				b.closeErr = b.sanitize(err)
			}
		}
	})
	return b.closeErr
}

// sanitize 把已知凭据原文从错误信息里抹掉。
func (b *Bundle) sanitize(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range b.secrets {
		if secret == "" {
			continue
		}
		message = strings.ReplaceAll(message, secret, "[redacted]")
	}
	return fmt.Errorf("%s", message)
}
