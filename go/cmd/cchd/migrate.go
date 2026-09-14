package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/migrate"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 等数据库的重试参数取 Node 的 `checkDatabaseConnection(retries = 30, delay = 2000)`
// （src/lib/migrate.ts:252）默认值：容器与 PG 同时拉起时，Node 最多等 60s 才放弃。
// Go 侧同样等：迁移动不了就等于 schema 未就位，早退只会换来 crash loop。
const (
	migrationConnectAttempts = 30
	migrationConnectDelay    = 2 * time.Second
)

// migrationRetry 是「等待数据库」的参数，可被测试覆盖。
type migrationRetry struct {
	Attempts int
	Delay    time.Duration
}

func defaultMigrationRetry() migrationRetry {
	return migrationRetry{Attempts: migrationConnectAttempts, Delay: migrationConnectDelay}
}

// autoMigrateDisabled 复刻 src/instrumentation.ts:585-590 的判定。
//
// 刻意**不用** cfg.AutoMigrate：config 的 kindBool 走 `text != "false" && text != "0"`
// （zod 的 booleanTransform），而 instrumentation 自己的判定是
// `trim().toLowerCase()` 后属于 {false, 0, no, off}。故 `AUTO_MIGRATE=off` 在 Node 下
// **关闭**迁移、在 config 口径下却是 true——迁移是写 schema 的动作，判定必须照 instrumentation。
//
// 未设置与空串都视为开启（Node 的 `?.trim()` 让 undefined 与 "" 都不落在关闭集合里）。
func autoMigrateDisabled(lookup config.LookupEnvFunc) (disabled bool, raw string) {
	if lookup == nil {
		return false, ""
	}
	value, _ := lookup("AUTO_MIGRATE")
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "false", "0", "no", "off":
		return true, value
	}
	return false, value
}

// runStartupMigrations 是 Node `runMigrations()` 在 Go 进程里的对应步骤。
//
// 顺序与 Node 的 instrumentation 一致：先等连得上（带重试），再按 AUTO_MIGRATE 决定跑不跑；
// 任一步失败即返回错误，由 main 以退出码 1 终止（Node 侧是 `process.exit(1)`）。
//
// 一处**有意的不等同**：DSN 为空时 Node 直接退出 1，而本进程保留既有的「无 DSN 降级骨架模式」
// （装配阶段报 dataplane_absent / admin_plane_degraded，见 boot.go）。此时不迁移、只记
// migration_skipped，避免把既有降级语义改成崩溃。
func runStartupMigrations(
	ctx context.Context,
	dsn string,
	lookup config.LookupEnvFunc,
	logger *logx.Logger,
	retry migrationRetry,
) error {
	if strings.TrimSpace(dsn) == "" {
		logger.Warn("migration_skipped", map[string]any{"reason": "dsn_not_configured"})
		return nil
	}
	if disabled, raw := autoMigrateDisabled(lookup); disabled {
		logger.Info("migration_skipped", map[string]any{
			"reason": "auto_migrate_disabled",
			"value":  raw,
		})
		return nil
	}

	logger.Info("migration_started", map[string]any{"advisoryLock": migrate.AdvisoryLockName})

	pool, err := openMigrationPool(ctx, dsn, logger, retry)
	if err != nil {
		logger.Error("migration_failed", map[string]any{"stage": "connect", "error": err.Error()})
		return err
	}
	defer pool.Close()

	result, err := migrate.Up(ctx, pool)
	if err != nil {
		logger.Error("migration_failed", map[string]any{"stage": "apply", "error": err.Error()})
		return err
	}

	fields := map[string]any{
		"applied":        result.Applied,
		"statements":     result.Statements,
		"repairedLedger": result.RepairedRows,
		"preflightRuns":  result.PreflightRuns,
		"latestBefore":   int64Text(result.LatestBefore),
		"latestAfter":    int64Text(result.LatestAfter),
	}
	if result.UpToDate() {
		logger.Info("migration_up_to_date", fields)
		return nil
	}
	fields["tags"] = result.AppliedTags
	logger.Info("migration_applied", fields)
	return nil
}

// openMigrationPool 建专用迁移连接（MaxConns=1，与 Node 的 `postgres(DSN, {max: 1})` 同义），
// 并按 Node 的重试预算等待数据库可达。只重试**连接**：一旦连上，迁移本身的失败立即上报
// （与 Node 的「先 checkDatabaseConnection 再 runMigrations，迁移阶段不重试」一致）。
func openMigrationPool(
	ctx context.Context,
	dsn string,
	logger *logx.Logger,
	retry migrationRetry,
) (*pgxpool.Pool, error) {
	attempts := retry.Attempts
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		pool, err := migrate.Open(ctx, dsn)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool, nil
			} else {
				lastErr = pingErr
			}
			pool.Close()
		} else {
			lastErr = err
		}
		if attempt == attempts {
			break
		}
		if lastErr != nil {
			logger.Warn("migration_waiting_database", map[string]any{
				"attempt": attempt,
				"of":      attempts,
				"error":   lastErr.Error(),
			})
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("migrate: 等待数据库时上下文结束: %w", ctx.Err())
		case <-time.After(retry.Delay):
		}
	}
	if lastErr == nil {
		lastErr = errors.New("migrate: 数据库不可达")
	}
	return nil, fmt.Errorf("migrate: 等待数据库 %d 次后仍失败: %w", attempts, lastErr)
}

// int64Text 把可空水位渲染成日志友好值（nil 表示账本为空）。
func int64Text(value *int64) string {
	if value == nil {
		return "none"
	}
	return fmt.Sprintf("%d", *value)
}
