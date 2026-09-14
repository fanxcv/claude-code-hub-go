package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**长时剖析夹具**，默认跳过：只有设了 CCH_PROFILE_IDLE_SECONDS 才跑。
//
// 它按生产同口径把固定节奏的后台任务真的跑起来（不含会出网的探活），在整段窗口里采样
// runtime.MemStats 与进程 RSS，用来回答两件事：
//
//  1. 空闲期内存锯齿的**幅度与周期**（生产实测 75→94 MiB 摆动）；
//  2. GOGC / GOMEMLIMIT 取值对「峰值 RSS、GC 次数、分配速率」的影响（扫参表）。
//
// 用法：
//
//	CCH_PROFILE_IDLE_SECONDS=60 CCH_PROFILE_OUT=/tmp/idle.json \
//	CCH_TEST_DSN=... CCH_TEST_REDIS_URL=... GOGC=100 GOMEMLIMIT=512MiB \
//	  go test -run ProfileIdle -count=1 -cpuprofile /tmp/idle-cpu.pprof ./internal/jobs/
//
// 之所以不放进默认门禁：这是分钟级观测，不是正确性检查。
func TestProfileIdleSampler(t *testing.T) {
	seconds, err := strconv.Atoi(os.Getenv("CCH_PROFILE_IDLE_SECONDS"))
	if err != nil || seconds <= 0 {
		t.Skip("未设置 CCH_PROFILE_IDLE_SECONDS，跳过长时剖析夹具")
	}
	deps := profileDeps(t)
	out := os.Getenv("CCH_PROFILE_OUT")
	if out == "" {
		out = filepath.Join(os.TempDir(), "cch-idle-profile.json")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 生产节奏（cmd/cchd/jobs.go 与 jobs_ops.go 的默认值）：
	//   可用性投影消费 200ms、outbox 回放 30s、公开状态重建 30s、端点探活 10s（空目标）。
	startTicker := func(name string, every time.Duration, run func(context.Context) error) {
		go func() {
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := run(ctx); err != nil {
						t.Logf("%s 失败: %v", name, err)
					}
				}
			}
		}()
	}

	consumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{
		Pools:  deps.Pools,
		Logger: deps.Logger,
	})
	if err != nil {
		t.Fatalf("构造消费器失败: %v", err)
	}
	startTicker("avail-projection-consume", 200*time.Millisecond, func(ctx context.Context) error {
		_, err := consumer.RunOnce(ctx)
		return err
	})

	replay := NewOutboxReplay(deps)
	startTicker("routing-trace-outbox-replay", 30*time.Second, func(ctx context.Context) error {
		_, err := replay.Run(ctx)
		return err
	})

	rebuild := NewPublicStatusRebuild(deps, pubstatus.NewRedisProjectionRedis(deps.Redis), nil, nil)
	startTicker("public-status-rebuild", 30*time.Second, func(ctx context.Context) error {
		_, err := rebuild.Run(ctx)
		return err
	})

	// 探活注入空目标：默认实现会扫全库并真的拨测上游（OpsDeps.ProbeTargets 的注释写明了
	// 这个缝就是为隔离而留），本轮只量空转成本。
	probeDeps := deps
	probeDeps.ProbeTargets = func(context.Context) ([]store.ProbeEndpoint, error) { return nil, nil }
	probe := NewEndpointProbe(probeDeps, ProbeConfig{})
	startTicker("endpoint-probe-idle", 10*time.Second, func(ctx context.Context) error {
		_, err := probe.Run(ctx)
		return err
	})

	type sample struct {
		TMs          int64  `json:"tMs"`
		RSSBytes     uint64 `json:"rssBytes"`
		HeapAlloc    uint64 `json:"heapAlloc"`
		HeapInuse    uint64 `json:"heapInuse"`
		HeapSys      uint64 `json:"heapSys"`
		NextGC       uint64 `json:"nextGC"`
		NumGC        uint32 `json:"numGC"`
		PauseTotal   uint64 `json:"pauseTotalNs"`
		Goroutines   int    `json:"goroutines"`
		TotalAlloc   uint64 `json:"totalAlloc"`
		TotalMallocs uint64 `json:"totalMallocs"`
	}
	samples := make([]sample, 0, seconds*4)
	started := time.Now()
	var startStats, endStats runtime.MemStats
	runtime.ReadMemStats(&startStats)
	endStats = startStats
	for time.Since(started) < time.Duration(seconds)*time.Second {
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		endStats = stats
		samples = append(samples, sample{
			TMs:          time.Since(started).Milliseconds(),
			RSSBytes:     readRSSBytes(),
			HeapAlloc:    stats.HeapAlloc,
			HeapInuse:    stats.HeapInuse,
			HeapSys:      stats.HeapSys,
			NextGC:       stats.NextGC,
			NumGC:        stats.NumGC,
			PauseTotal:   stats.PauseTotalNs,
			Goroutines:   runtime.NumGoroutine(),
			TotalAlloc:   stats.TotalAlloc,
			TotalMallocs: stats.Mallocs,
		})
		time.Sleep(250 * time.Millisecond)
	}
	cancel()
	time.Sleep(200 * time.Millisecond)

	payload, err := json.Marshal(map[string]any{"samples": samples})
	if err != nil {
		t.Fatalf("序列化采样失败: %v", err)
	}
	if err := os.WriteFile(out, payload, 0o644); err != nil {
		t.Fatalf("写采样失败: %v", err)
	}

	var maxRSS, minRSS, maxHeap, minHeap uint64
	for index, item := range samples {
		if index == 0 || item.RSSBytes > maxRSS {
			maxRSS = item.RSSBytes
		}
		if index == 0 || item.RSSBytes < minRSS {
			minRSS = item.RSSBytes
		}
		if index == 0 || item.HeapAlloc > maxHeap {
			maxHeap = item.HeapAlloc
		}
		if index == 0 || item.HeapAlloc < minHeap {
			minHeap = item.HeapAlloc
		}
	}
	elapsed := time.Since(started).Seconds()
	gcDelta := int(endStats.NumGC - startStats.NumGC)
	allocDelta := endStats.TotalAlloc - startStats.TotalAlloc
	t.Logf("[%s] 采样 %d 点，窗口 %.1fs", profileLabel(), len(samples), elapsed)
	t.Logf("[%s] RSS  min=%s max=%s 摆动=%s", profileLabel(), mib(minRSS), mib(maxRSS), mib(maxRSS-minRSS))
	t.Logf("[%s] Heap min=%s max=%s 摆动=%s", profileLabel(), mib(minHeap), mib(maxHeap), mib(maxHeap-minHeap))
	t.Logf("[%s] GC   count=%d 周期=%.1fs 平均停顿=%.2fms 停顿总计=%.1fms",
		profileLabel(), gcDelta, elapsed/float64(maxInt(gcDelta, 1)),
		float64(endStats.PauseTotalNs-startStats.PauseTotalNs)/1e6/float64(maxInt(gcDelta, 1)),
		float64(endStats.PauseTotalNs-startStats.PauseTotalNs)/1e6)
	t.Logf("[%s] 分配速率 %.2f MiB/s（窗口内 %.1f MiB，%d 次分配）",
		profileLabel(), float64(allocDelta)/elapsed/1024/1024, float64(allocDelta)/1024/1024,
		endStats.Mallocs-startStats.Mallocs)
	t.Logf("[%s] 末次 NextGC=%s RSS=%s Goroutines=%d", profileLabel(), mib(endStats.NextGC),
		mib(readRSSBytes()), runtime.NumGoroutine())
	t.Logf("[%s] 采样已写 %s", profileLabel(), out)
}

// profileLabel 组成扫参表里那一列（GOGC/GOMEMLIMIT 取值）。
func profileLabel() string {
	return fmt.Sprintf("GOGC=%s GOMEMLIMIT=%dMiB", envOr(os.Getenv("GOGC"), "unset"),
		debug.SetMemoryLimit(-1)/1024/1024)
}

func envOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func mib(value uint64) string {
	return fmt.Sprintf("%.1f MiB", float64(value)/1024/1024)
}

// readRSSBytes 读 /proc/self/statm 的常驻页数（Linux；其它平台返回 0）。
func readRSSBytes() uint64 {
	raw, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := make([]uint64, 0, 3)
	current := uint64(0)
	seen := false
	for _, char := range raw {
		if char >= '0' && char <= '9' {
			current = current*10 + uint64(char-'0')
			seen = true
			continue
		}
		if seen {
			fields = append(fields, current)
			current = 0
			seen = false
		}
		if len(fields) == 2 {
			break
		}
	}
	if len(fields) < 2 {
		return 0
	}
	return fields[1] * uint64(os.Getpagesize())
}
