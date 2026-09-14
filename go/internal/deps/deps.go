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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/httpapi"
)

// Bundle 持有本进程的全部外部依赖句柄。
type Bundle struct {
	pgPool *pgxpool.Pool
	redis  *redis.Client
	rules  *cfgsync.Snapshot
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

	if cfg.DSN != "" {
		poolConfig, err := pgxpool.ParseConfig(cfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("解析 DSN 失败（值已隐去）: %w", bundle.sanitize(err))
		}
		// M1 没有请求路径，只建 control 道；data / writer 两道随 M2 的请求与写入路径落地。
		controlLane := config.LaneControl
		poolConfig.MaxConns = int32(cfg.Pool.Size(cfg.Pool.PhysicalLane(controlLane)))
		if poolConfig.MaxConns < 1 {
			poolConfig.MaxConns = 1
		}
		poolConfig.MinConns = 0
		poolConfig.MaxConnLifetime = time.Hour
		poolConfig.MaxConnIdleTime = cfg.DB.IdleTimeout
		poolConfig.ConnConfig.ConnectTimeout = cfg.DB.ConnectTimeout
		if poolConfig.ConnConfig.RuntimeParams == nil {
			poolConfig.ConnConfig.RuntimeParams = map[string]string{}
		}
		poolConfig.ConnConfig.RuntimeParams["application_name"] = config.LaneApplicationName(controlLane)
		poolConfig.ConnConfig.RuntimeParams["statement_timeout"] =
			fmt.Sprintf("%d", cfg.DB.StatementExpiry.Milliseconds())
		poolConfig.ConnConfig.RuntimeParams["lock_timeout"] =
			fmt.Sprintf("%d", cfg.DB.LockExpiry.Milliseconds())

		pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
		if err != nil {
			return nil, fmt.Errorf("建立 PostgreSQL 连接池失败（凭据已隐去）: %w", bundle.sanitize(err))
		}
		bundle.pgPool = pool
	}

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

// PingPG 探测 PostgreSQL。
func (b *Bundle) PingPG(ctx context.Context) (httpapi.DependencyStatus, string) {
	if b.pgPool == nil {
		return httpapi.StatusNotSet, "DSN not configured"
	}
	if err := b.pgPool.Ping(ctx); err != nil {
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
		if b.pgPool != nil {
			b.pgPool.Close()
		}
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
