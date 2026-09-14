// Package debugapi 提供性能剖析面：pprof 端点与运行时指标。
//
// # 为什么要单独一条监听
//
// 剖析面（尤其堆剖析与 goroutine 栈）会泄漏内存内容与调用栈，故它**不挂在对外端口上**，
// 而是自己绑一条**只回环**的监听（默认 127.0.0.1:3100）。把闸门放在「监听地址」而不是
// 「路径前缀 + 令牌」上，是因为这样泄露面由内核的地址绑定决定：公网面根本没有这条路径，
// 不存在「令牌写错就泄」的失败模式。
//
// # 关闭态为什么仍然绑监听
//
// 关闭时**一条路由都不注册**，所有路径（含 /debug/pprof/*）一律 404。这与「干脆不绑」
// 的差别只在一点：404 让「这个面存在但没开」与「这个面不存在」在观测上不可区分——
// 即不泄漏存在性（401 会泄漏）。代价是关闭时进程仍持一条回环套接字（无路由、无读出）。
//
// # 起不来的语义
//
// Open 的失败（端口被占、地址非法）由调用方记为**降级**：
// 剖析面是观测手段，缺席不该拖倒数据面。
package debugapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// profileNames 是可取的单份剖析；其余入口（index/profile/trace/symbol/cmdline）单独注册。
var profileNames = []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"}

// Options 是剖析面的装配输入。
type Options struct {
	// Enabled 为假时不注册任何路由：所有路径一律 404（见包注释）。
	Enabled bool
	// Addr 是监听地址（host:port），**不得为空**：未显式配置时由配置层取默认
	// （config.DefaultPprofAddr）。这里不设第二份默认，避免两处默认值分叉。
	Addr string
	// Logger 为 nil 时静默（测试友好）。
	Logger *logx.Logger
	// Listen 注入以便测试绑定随机端口；nil 时用 net.Listen("tcp", addr)。
	Listen func(addr string) (net.Listener, error)
	// Collectors 是额外指标来源（连接池等既有计数器）。名称即 JSON 里的键。
	Collectors []Collector
	// BlockProfileRate 是阻塞剖析采样阈值（纳秒）：0 关闭。只采超过阈值的事件，
	// 故开销可忽略，而仍能回答「谁在等谁」。
	BlockProfileRate int
	// MutexProfileFraction 是互斥锁剖析采样率（1/N）：0 关闭。
	// 争用越高代价越大，故默认关（要查锁争用时才开）。
	MutexProfileFraction int
}

// Plane 是一条已起好的剖析面监听。
type Plane struct {
	server   *http.Server
	listener net.Listener
	logger   *logx.Logger
	enabled  bool
}

// Open 绑定剖析面并开始服务。
//
// 返回的错误全部属「降级」类（地址非法、端口冲突、绑定失败），调用方记日志后继续。
func Open(options Options) (*Plane, error) {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(io.Discard)
	}

	addr := strings.TrimSpace(options.Addr)
	if addr == "" {
		return nil, errors.New("debugapi: 剖析面地址为空（CCH_PPROF_ADDR）")
	}
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("debugapi: 剖析面地址需为 host:port，收到 %q", addr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("debugapi: 剖析面地址端口非法，收到 %q", addr)
	}
	// 与 Node 回退端口同端口的那道拒绝已删除：回退端口随 Node 退役一并删了，
	// 不再存在「观测面占住回退目标套接字」这一风险

	listen := options.Listen
	if listen == nil {
		listen = func(target string) (net.Listener, error) { return net.Listen("tcp", target) }
	}
	listener, err := listen(addr)
	if err != nil {
		return nil, fmt.Errorf("debugapi: 绑定 %s 失败: %w", addr, err)
	}

	if options.Enabled {
		// 采样率只在开启时设置：关闭态连运行时的剖析开关都不该动。
		runtime.SetBlockProfileRate(options.BlockProfileRate)
		runtime.SetMutexProfileFraction(options.MutexProfileFraction)
	}

	plane := &Plane{
		server: &http.Server{
			Handler: newHandler(options),
			// 只读头超时：剖析请求本身可以长跑（profile?seconds=60），故不设整体超时。
			ReadHeaderTimeout: 5 * time.Second,
		},
		listener: listener,
		logger:   logger,
		enabled:  options.Enabled,
	}
	go func() {
		if serveErr := plane.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("debug_plane_serve_failed", map[string]any{"addr": addr, "error": serveErr.Error()})
		}
	}()

	bound := listener.Addr().String()
	loopback := isLoopbackHost(host)
	logger.Info("debug_plane_listening", map[string]any{
		"addr":     bound,
		"enabled":  options.Enabled,
		"loopback": loopback,
	})
	if !loopback {
		logger.Warn("debug_plane_non_loopback_bind", map[string]any{
			"addr": bound,
			"hint": "非回环地址会把调用栈与堆内容暴露给该网段；只应在受控调试环境使用",
		})
	}
	return plane, nil
}

// Addr 返回实际绑定的地址（绑 0 端口时即内核分配的端口）。
func (p *Plane) Addr() string {
	if p == nil || p.listener == nil {
		return ""
	}
	return p.listener.Addr().String()
}

// Enabled 报告路由是否已注册（关闭态只有 404）。
func (p *Plane) Enabled() bool {
	return p != nil && p.enabled
}

// Close 停止接纳新请求并等待在途请求，超期即硬关。
//
// 不用无限期优雅关：剖析请求可以长跑（profile?seconds=60），等它会让进程退出无限延后。
// 调用方应给 ctx 一个短期限（boot 用 1.5s）。
//
// **显式关监听**（而不是只靠 Shutdown）：`http.Server.Shutdown` 只关「已被 Serve 登记」的
// 监听，而本包的 Serve 起在 goroutine 里——若 Close 先于它执行，登记尚未发生，那条监听就
// 永远不会被 Shutdown 关掉（端口泄漏到进程退出）。实测该竞态约 2/3 概率复现
// （TestCloseDebugPlaneReleasesListener 三连跑 2 红）。这里持有 listener 自己关，幂等且确定。
func (p *Plane) Close(ctx context.Context) error {
	if p == nil || p.server == nil {
		return nil
	}
	shutdownErr := p.server.Shutdown(ctx)
	// 幂等：Shutdown 已关时这里只拿到 ErrClosed，忽略即可。
	listenerErr := p.listener.Close()
	if shutdownErr != nil {
		// 过期即硬关：长跑的 profile 请求随之失败，这是有意的取舍。
		closeErr := p.server.Close()
		return errors.Join(shutdownErr, closeErr, ignoreClosed(listenerErr))
	}
	return ignoreClosed(listenerErr)
}

// ignoreClosed 把「已经关了」当成成功：Close 的契约是「关掉」，不是「是我关的」。
func ignoreClosed(err error) error {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// isLoopbackHost 判断监听主机是否只回环。
//
// 空主机（如 ":3100"）会绑全网卡，故判为非回环并告警。
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// newHandler 建路由表。
//
// 关闭态返回**空 mux**：ServeMux 对未注册路径答 404，故关闭态与「路径不存在」不可区分。
func newHandler(options Options) http.Handler {
	mux := http.NewServeMux()
	if !options.Enabled {
		return mux
	}
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	for _, name := range profileNames {
		mux.Handle("/debug/pprof/"+name, pprof.Handler(name))
	}
	mux.Handle("/debug/metrics", newMetricsHandler(options.Collectors))
	return mux
}
