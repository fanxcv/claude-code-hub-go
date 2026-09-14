// jobsrun 手动驱动后台任务一轮：云价格同步与可用性投影回填。
//
// 用途：验收与排障。cchd 内部的 Scheduler 有错峰与单例约束，手工排查时需要「立刻跑一轮
// 并看到原始计数」的入口。参数与 cchd 的开关一致（同读 CCH_* 环境变量）。
//
// 用法（在 go/ 目录下）：
//
//	CCH_TEST_DSN=... go run ./cmd/jobsrun -job=price-sync
//	CCH_TEST_DSN=... go run ./cmd/jobsrun -job=avail-backfill
//	CCH_TEST_DSN=... go run ./cmd/jobsrun -job=all
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

func main() {
	job := flag.String("job", "all", "price-sync | avail-backfill | all")
	dsnEnv := flag.String("dsn-env", "CCH_TEST_DSN", "读取 DSN 的环境变量名")
	timeout := flag.Duration("timeout", 15*time.Minute, "整体超时")
	// force 用一个不存在的模型名填 overwriteManual：按 Node 口径，非空列表会跳过版本短路，
	// 因此这会真的重放整表（用于验证写入路径，而不是只验证短路）。
	force := flag.Bool("force", false, "禁用版本短路，强制重放整表")
	flag.Parse()

	dsn := os.Getenv(*dsnEnv)
	if dsn == "" {
		fmt.Fprintf(os.Stderr, "未设置 %s（或 -dsn-env 指定的变量）\n", *dsnEnv)
		os.Exit(2)
	}

	logger := logx.New(os.Stderr)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	pools, err := store.Open(ctx, store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-jobsrun",
	})
	if err != nil {
		logger.Error("pools_open_failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
	defer func() { _ = pools.Close() }()

	failed := false
	if *job == "all" || *job == "price-sync" {
		options := jobs.PriceSyncOptions{Pools: pools, Logger: logger}
		if *force {
			options.OverwriteManual = []string{"__jobsrun_force_replay__"}
		}
		syncer, err := jobs.NewPriceSyncer(options)
		if err != nil {
			logger.Error("price_sync_init_failed", map[string]any{"error": err.Error()})
			os.Exit(1)
		}
		started := time.Now()
		result, err := syncer.RunOnce(ctx)
		if err != nil {
			logger.Error("price_sync_failed", map[string]any{"error": err.Error()})
			failed = true
		} else if result != nil {
			logger.Info("price_sync_done", map[string]any{
				"added":     len(result.Added),
				"updated":   len(result.Updated),
				"unchanged": len(result.Unchanged),
				"failed":    len(result.Failed),
				"total":     result.Total,
				"elapsedMs": time.Since(started).Milliseconds(),
			})
		}
	}

	if *job == "all" || *job == "avail-backfill" {
		backfill, err := jobs.NewAvailBackfill(jobs.AvailBackfillOptions{Pools: pools, Logger: logger})
		if err != nil {
			logger.Error("backfill_init_failed", map[string]any{"error": err.Error()})
			os.Exit(1)
		}
		result, err := backfill.RunOnce(ctx)
		if err != nil {
			logger.Error("backfill_failed", map[string]any{"error": err.Error()})
			failed = true
		} else {
			logger.Info("backfill_done", map[string]any{
				"skipped":     result.Skipped,
				"reason":      result.SkippedReason,
				"inserted":    result.Inserted,
				"chunks":      result.Chunks,
				"interrupted": result.Interrupted,
			})
		}
	}

	if failed {
		os.Exit(1)
	}
}
