// Package egress 是 cchd 的操作面入口骨架：进程级在途闸门 + 未实现路径的终端。
//
// 本包此前是 node/go 双后端切换的唯一入口（逐路由归属判定 + 判给 Node 的请求原样反代回
// Node）。Node 退役后，归属仲裁没有对象了
// ——本进程承载全部路径——因此**归属判定、路由白名单、Node 反代与回退目标全部删除**。
// 留下两件仍然在役的事：
//
//  1. **在途闸门**：`Drain` 停止接纳新请求并等在途归零，`boot` 的退出序列据此确保「请求还没
//     结束就关依赖」不会把尚未落库的终态一并带走；`/readyz` 在排空窗口内报 503。
//  2. **未实现路径的终端**：本进程路由表里没有的路径回 404（原先交回 Node 反代）。
package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// unimplementedErrorCode 是本进程未实现路径的 404 响应体错误码。
//
// 形状与其它错误体一致（`{"error":"..."}`），便于客户端与日志按同一规则解析。
const unimplementedErrorCode = "not_found"

// FrontDoor 是操作面入口，并发安全。
type FrontDoor struct {
	admission *admission
	log       *logx.Logger
}

// New 建造前门。logger 为 nil 时丢弃日志（测试友好）。
//
// 无失败模式也不收编排参数：原「路由语法非法 / 缺少回退目标」两类校验随归属白名单与回退
// 目标一并删除；排空窗口由退出序列自己拥有（`options.DrainTimeout` 是唯一口径），
// 故不再在这里存第二份。
func New(logger *logx.Logger) *FrontDoor {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &FrontDoor{
		admission: newAdmission(),
		log:       logger,
	}
}

// InFlight 返回当前在途请求数。
func (f *FrontDoor) InFlight() int64 {
	return f.admission.inFlight()
}

// Draining 报告前门是否已停止接纳新请求。
func (f *FrontDoor) Draining() bool {
	return f.admission.isDraining()
}

// Drain 停止接纳新请求并等待在途归零；ctx 先结束则返回 ErrDrainTimeout。
//
// 语义要点：**一旦进入排空就不再重新开放**（重开需新建实例）。退出序列依赖「在途归零」
// 这一时刻——中途重新放行会把归零时刻无限推迟，而此刻正是关连接池、停后台任务的安全点。
func (f *FrontDoor) Drain(ctx context.Context) error {
	if f.admission.beginDrain() {
		f.log.Warn("egress_draining", map[string]any{
			"inFlight": f.admission.inFlight(),
		})
	}
	return f.admission.waitEmpty(ctx)
}

// Middleware 返回入口中间件：过排空闸门后把请求交给 next。
//
// 两个挂载点共用它——数据面/管理面处理器，以及非 API 路径的终端（页面面）。
func (f *FrontDoor) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := f.admission.tryEnter(); err != nil {
			// 排空窗口内拒绝新请求：调用方应重试或改打新实例（滚动重启的正常代价）。
			f.log.Warn("egress_admission_closed", map[string]any{
				"path": NormalizePath(r.URL.Path),
			})
			writeJSONError(w, http.StatusServiceUnavailable, "shutting_down")
			return
		}
		defer f.admission.leave()
		next.ServeHTTP(w, r)
	})
}

// Unimplemented 返回「本进程未实现该路径」的终端处理器（JSON 404）。
//
// 用途：数据面与管理面装配时把它设为未命中路由的落点。原语义是「继续走 Node 原路径」——
// 归属规则可以先声明尚未落地的路由，此刻返回 501/503 会被客户端误判成协议不支持而放弃重试。
// Node 退役后不再有第三方承载者：这类路径在本进程就是**没有实现**，如实回 404。
//
// 生产影响（有意，已由用户裁决）：白名单内 `/api/admin/database/{export,import}` 与
// `/api/internal/data-gen` 三条端点此前在无 Node 的环境里回 502 `node_backend_unreachable`，
// 现在是 404 `not_found` —— 它们本就是「已裁决接受下线」的端点。
func (f *FrontDoor) Unimplemented() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.log.Debug("egress_unimplemented", map[string]any{
			"method": r.Method,
			"path":   NormalizePath(r.URL.Path),
		})
		writeJSONError(w, http.StatusNotFound, unimplementedErrorCode)
	})
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body, err := json.Marshal(map[string]string{"error": code})
	if err != nil {
		body = []byte(fmt.Sprintf(`{"error":%q}`, code))
	}
	_, _ = w.Write(body)
}
