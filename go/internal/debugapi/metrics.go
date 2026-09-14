package debugapi

import (
	"encoding/json"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"time"
)

// processStartedAt 用于算 uptime：包加载即进程启动附近的时刻，精度够用。
var processStartedAt = time.Now()

// Collector 是一个具名指标来源。
//
// 用函数而不是注册表：指标面只读既有计数器，不该因为它而引入全局状态
// （也让测试能直接注入一个返回已知值的收集器）。
type Collector struct {
	Name string
	// Collect 返回本次读数；返回 nil 表示本轮无数据（整段省略）。
	Collect func() map[string]any
}

// Snapshot 是指标端点的响应体。
//
// 字段全部是数字、版本号与时间戳：**不含任何配置值或凭据**——
// 凭据只存在于 config.Config，进不了这里（见 metrics_test.go 的反证）。
type Snapshot struct {
	Timestamp  string          `json:"timestamp"`
	Process    processSnapshot `json:"process"`
	GC         gcSnapshot      `json:"gc"`
	Memory     memorySnapshot  `json:"memory"`
	Collectors map[string]any  `json:"collectors,omitempty"`
}

type processSnapshot struct {
	PID           int     `json:"pid"`
	GoVersion     string  `json:"goVersion"`
	GOOS          string  `json:"goos"`
	GOARCH        string  `json:"goarch"`
	NumCPU        int     `json:"numCpu"`
	GOMAXPROCS    int     `json:"gomaxprocs"`
	Goroutines    int     `json:"goroutines"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

type gcSnapshot struct {
	// GOGCPercent 为 nil 表示本运行时未提供该指标（正常构建下不会）。
	GOGCPercent *int `json:"goGCPercent"`
	// GOMemLimitBytes 为 nil 同上；math.MaxInt64 表示未设上限。
	GOMemLimitBytes *int64  `json:"goMemLimitBytes"`
	NumGC           uint32  `json:"numGC"`
	PauseTotalMs    float64 `json:"pauseTotalMs"`
	PauseLastMs     float64 `json:"pauseLastMs"`
	LastGCUnixMs    int64   `json:"lastGcUnixMs"`
}

type memorySnapshot struct {
	AllocBytes        uint64 `json:"allocBytes"`
	TotalAllocBytes   uint64 `json:"totalAllocBytes"`
	SysBytes          uint64 `json:"sysBytes"`
	HeapAllocBytes    uint64 `json:"heapAllocBytes"`
	HeapInuseBytes    uint64 `json:"heapInuseBytes"`
	HeapIdleBytes     uint64 `json:"heapIdleBytes"`
	HeapReleasedBytes uint64 `json:"heapReleasedBytes"`
	HeapObjects       uint64 `json:"heapObjects"`
	StackInuseBytes   uint64 `json:"stackInuseBytes"`
	GCSysBytes        uint64 `json:"gcSysBytes"`
	NextGCBytes       uint64 `json:"nextGCBytes"`
}

// newMetricsHandler 建 /debug/metrics 处理器。
func newMetricsHandler(collectors []Collector) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		// 指标是即时读数，缓存住就失去意义（也避免中间设备留下堆读数副本）。
		writer.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(writer).Encode(snapshot(collectors)); err != nil {
			// 客户端断开是常态（脚本用 Ctrl-C），不记日志也不重试。
			return
		}
	})
}

func snapshot(collectors []Collector) Snapshot {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// 最近一次 GC 停顿：PauseNs 是 256 环形缓冲，取 NumGC-1 位置。
	pauseLastNs := uint64(0)
	if memStats.NumGC > 0 {
		pauseLastNs = memStats.PauseNs[(memStats.NumGC-1)%uint32(len(memStats.PauseNs))]
	}
	lastGCUnixMs := int64(0)
	if memStats.LastGC > 0 {
		lastGCUnixMs = int64(memStats.LastGC / 1e6)
	}

	gogc, memLimit := readGCSettings()

	return Snapshot{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Process: processSnapshot{
			PID:           os.Getpid(),
			GoVersion:     runtime.Version(),
			GOOS:          runtime.GOOS,
			GOARCH:        runtime.GOARCH,
			NumCPU:        runtime.NumCPU(),
			GOMAXPROCS:    runtime.GOMAXPROCS(0),
			Goroutines:    runtime.NumGoroutine(),
			UptimeSeconds: time.Since(processStartedAt).Seconds(),
		},
		GC: gcSnapshot{
			GOGCPercent:     gogc,
			GOMemLimitBytes: memLimit,
			NumGC:           memStats.NumGC,
			PauseTotalMs:    float64(memStats.PauseTotalNs) / 1e6,
			PauseLastMs:     float64(pauseLastNs) / 1e6,
			LastGCUnixMs:    lastGCUnixMs,
		},
		Memory: memorySnapshot{
			AllocBytes:        memStats.Alloc,
			TotalAllocBytes:   memStats.TotalAlloc,
			SysBytes:          memStats.Sys,
			HeapAllocBytes:    memStats.HeapAlloc,
			HeapInuseBytes:    memStats.HeapInuse,
			HeapIdleBytes:     memStats.HeapIdle,
			HeapReleasedBytes: memStats.HeapReleased,
			HeapObjects:       memStats.HeapObjects,
			StackInuseBytes:   memStats.StackInuse,
			GCSysBytes:        memStats.GCSys,
			NextGCBytes:       memStats.NextGC,
		},
		Collectors: collect(collectors),
	}
}

// readGCSettings 读 GOGC 与 GOMEMLIMIT。
//
// 走 runtime/metrics 而不是 debug.SetGCPercent(-1)/SetMemoryLimit(-1) 的取值惯用法：
// 后者会**短暂关掉 GC**（SetGCPercent(-1) 的语义就是关闭），在一个诊断端点上动 GC 开关
// 是不可接受的副作用；runtime/metrics 是纯读。
func readGCSettings() (*int, *int64) {
	samples := []metrics.Sample{
		{Name: "/gc/gogc:percent"},
		{Name: "/gc/gomemlimit:bytes"},
	}
	metrics.Read(samples)

	var gogc *int
	if samples[0].Value.Kind() == metrics.KindUint64 {
		raw := int64(samples[0].Value.Uint64())
		// GOGC=off 在运行时里是负值，经 uint64 取出会变成很大的数；还原成 -1 表达「关闭」。
		if raw > int64(^uint32(0)>>1) {
			raw = -1
		}
		value := int(raw)
		gogc = &value
	}
	var memLimit *int64
	if samples[1].Value.Kind() == metrics.KindUint64 {
		value := int64(samples[1].Value.Uint64())
		memLimit = &value
	}
	return gogc, memLimit
}

// collect 依次取各收集器读数。
//
// 具名重复时后者覆盖前者（无静默合并：装配侧应当不要重名，重名在测试里会被钉住）。
func collect(collectors []Collector) map[string]any {
	if len(collectors) == 0 {
		return nil
	}
	out := make(map[string]any, len(collectors))
	for _, collector := range collectors {
		if collector.Collect == nil {
			continue
		}
		values := collector.Collect()
		if len(values) == 0 {
			continue
		}
		out[collector.Name] = values
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
