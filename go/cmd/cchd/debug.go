// 剖析面（pprof + 运行时指标）的装配。
//
// 与其它装配分开成文件：它是一个**观测**面，失败语义与数据面/管理面都不同——
// 起不来只降级（记日志），绝不影响对外服务。
package main

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/debugapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// debugOptions 是剖析面的装配输入。
type debugOptions struct {
	Cfg        config.Config
	Logger     *logx.Logger
	Collectors []debugapi.Collector
}

// openDebugPlane 按配置起剖析面。
//
// 默认（CCH_PPROF_ENABLED=false）也会绑那条回环监听，但一条路由都不注册：
// 所有路径 404，与「路径不存在」不可区分（见 debugapi 包注释的取舍）。
func openDebugPlane(options debugOptions) (*debugapi.Plane, error) {
	return debugapi.Open(debugapi.Options{
		Enabled:              options.Cfg.Pprof.Enabled,
		Addr:                 options.Cfg.Pprof.Addr,
		Logger:               options.Logger,
		Collectors:           options.Collectors,
		BlockProfileRate:     options.Cfg.Pprof.BlockProfileRate,
		MutexProfileFraction: options.Cfg.Pprof.MutexProfileFraction,
	})
}

// newDebugCollectors 把连接池的既有计数器汇进指标面。
//
// 为什么是池读数：CPU 与内存的波动常与「连接排队 / 会话建立」相关，而这几个数池内部
// 早就算着（outstanding 是本地计数，其余取自 pgxpool.Stat），此前只是没有出口。
//
// 读数为空的分道直接略过——池还没建起来时不去凭空造一个（那会多开连接）。
func newDebugCollectors(pools *store.Pools) []debugapi.Collector {
	if pools == nil {
		return nil
	}
	return []debugapi.Collector{{
		Name: "dbPool",
		Collect: func() map[string]any {
			// 按物理分道取值：writer 的预算为 0 时复用 control 池，
			// 故键取 pool.Lane()（物理名）而不是请求的逻辑名，免得同一池出现两次。
			lanes := []config.Lane{config.LaneData, config.LaneControl, config.LaneWriter}
			byLane := make(map[string]any, len(lanes))
			for _, lane := range lanes {
				pool, err := pools.Lane(lane)
				if err != nil || pool == nil {
					continue
				}
				entry := map[string]any{
					"outstanding":    pool.Outstanding(),
					"maxOutstanding": pool.MaxOutstanding(),
				}
				if raw := pool.Raw(); raw != nil {
					stat := raw.Stat()
					entry["totalConns"] = stat.TotalConns()
					entry["acquiredConns"] = stat.AcquiredConns()
					entry["idleConns"] = stat.IdleConns()
					entry["maxConns"] = stat.MaxConns()
					entry["acquireCount"] = stat.AcquireCount()
					entry["emptyAcquireCount"] = stat.EmptyAcquireCount()
					entry["canceledAcquireCount"] = stat.CanceledAcquireCount()
					entry["acquireDurationMs"] = float64(stat.AcquireDuration().Microseconds()) / 1000
				}
				byLane[string(pool.Lane())] = entry
			}
			if len(byLane) == 0 {
				return nil
			}
			return map[string]any{"lanes": byLane}
		},
	}}
}
