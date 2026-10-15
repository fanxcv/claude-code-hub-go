package adminapi

import (
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// requireWriteSession 是管理面写路径的会话门：管理员，或凭据密钥允许登录 Web UI 的会话，才放行；
// 其余（读档认证会放行的只读会话）一律 403 auth.forbidden。
//
// 判据与前端进入 /dashboard 的门是同一条（src/app/[locale]/dashboard/layout.tsx 的
// forbiddenRule "admin-or-webui"，实现见 src/components/ui-session-gate.tsx）：前端只放行非只读
// 会话，服务端的写路径就必须同判——否则「只读」只挡得住界面，挡不住直接打接口的密钥。
//
// 为什么不靠路由档位解决：档位只有 read / admin 两档。挂 admin 会把能登录 Web UI 的普通用户
// （dashboard_user，正是 /dashboard 与自服务面的使用者）一并拒掉；挂 read 则等于不设门。
// 因此写动作留在 read 档位内，另加这道逐点调用、判据更强的门。
//
// detail 由调用方给：各资源的 action 文案不同，且这些文案是前端判错误码的依据。
func requireWriteSession(
	problems ProblemWriter,
	logger *logx.Logger,
	writer http.ResponseWriter,
	request *http.Request,
	detail string,
) bool {
	principal, ok := PrincipalFrom(request.Context())
	if !ok || principal.UserID == 0 {
		problems.WriteProblem(writer, request, http.StatusUnauthorized, "auth.missing",
			"Authentication is required.")
		return false
	}
	if principal.IsAdmin || principal.CanLoginWebUI {
		return true
	}
	if logger != nil {
		logger.Warn("admin_write_denied_read_only_session", map[string]any{
			"path":        request.URL.Path,
			"userId":      principal.UserID,
			"webSession":  principal.WebSession,
			"canLoginWeb": principal.CanLoginWebUI,
		})
	}
	problems.WriteProblem(writer, request, http.StatusForbidden, "auth.forbidden", detail)
	return false
}
