package main

import (
	"context"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/patrol"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把「未落终态行」的自愈巡检接进进程启动。
//
// 归属与生命周期：巡检只读写共享连接池（走 control 分道），因此它必须**先于**池关闭而停，
// 并且在池打开之后才装配。停止点是 boot 的 closeAll（在关池之前），有界等待由 patrol.Stop
// 自己保证（5 s）。
//
// 装配失败只降级不 fail fast：巡检是兜底手段，装不上只意味着退回「没有自愈」的旧行为，
// 而 fail fast 会让一个自愈任务把整个数据面拖垮，代价完全不对等。
func startPatrol(
	ctx context.Context,
	cfg config.Config,
	logger *logx.Logger,
	pools *store.Pools,
) (func(), error) {
	if !cfg.Patrol.Enabled || pools == nil {
		logger.Info("patrol_absent", map[string]any{
			"reason": patrolAbsentReason(cfg, pools),
		})
		return nil, nil
	}
	patrolRunner, err := patrol.New(patrol.Options{
		Store:           patrol.NewDBStore(pools),
		Logger:          logger,
		UnsettledAfter:  cfg.Patrol.UnsettledAfter,
		Interval:        cfg.Patrol.Interval,
		BatchSize:       cfg.Patrol.BatchSize,
		MaxRowsPerRound: cfg.Patrol.MaxRowsPerRound,
	})
	if err != nil {
		return nil, err
	}
	logger.Info("patrol_started", map[string]any{
		"unsettledAfterMs": cfg.Patrol.UnsettledAfter.Milliseconds(),
		"intervalMs":       cfg.Patrol.Interval.Milliseconds(),
		"batchSize":        cfg.Patrol.BatchSize,
		"maxRowsPerRound":  cfg.Patrol.MaxRowsPerRound,
		"statusCode":       patrol.RepairStatusCode,
		"errorMessage":     patrol.RepairErrorMessage,
	})
	patrolRunner.Start(ctx)
	return patrolRunner.Stop, nil
}

// patrolAbsentReason 说明巡检为何不装配（可观测优先：静默关闭最难发现）。
func patrolAbsentReason(cfg config.Config, pools *store.Pools) string {
	if !cfg.Patrol.Enabled {
		return "CCH_PATROL_ENABLED=false"
	}
	if pools == nil {
		return "DSN not configured"
	}
	return "unknown"
}
