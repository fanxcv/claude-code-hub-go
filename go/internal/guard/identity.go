package guard

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// rawHeaders 把只读视图摊平成 map[string][]string，供缝隙使用。
func rawHeaders(headers pctx.HeaderView) map[string][]string {
	cloned := headers.Clone()
	flattened := make(map[string][]string, len(cloned))
	for key, values := range cloned {
		flattened[key] = values
	}
	return flattened
}

// authState 读取鉴权结果。
func authState(ctx *pctx.Context) (pctx.AuthState, bool) {
	return ctx.Auth()
}

// userAgent 读取请求的 User-Agent。
func userAgent(ctx *pctx.Context) string {
	return ctx.Headers().Get("user-agent")
}

// currentUser 读取当前请求的用户属性。
//
// 返回 ok=false 的三种情形含义不同，调用方要区分对待：
//   - 未鉴权：认证守卫本应已拦截，跳过限制检查（与 Node 一致）。
//   - 用户目录缝隙未接线：限制无法执行，按 fail-open 跳过并留 warn，这是显式的接线缺口。
//   - 读取失败：返回 error，由调用方翻译为 500——读不到用户就放行会让白名单形同虚设。
func (d Deps) currentUser(ctx *pctx.Context) (User, bool, error) {
	auth, ok := authState(ctx)
	if !ok || auth.UserID == 0 {
		return User{}, false, nil
	}
	if d.Users == nil {
		d.logger().Warn("guard.user_directory.missing", map[string]any{
			"note":   "用户目录缝隙未接线，客户端与模型限制本请求未执行",
			"userId": auth.UserID,
		})
		return User{}, false, nil
	}
	user, err := d.Users.User(d.runContext(ctx), auth.UserID)
	if err != nil {
		return User{}, false, err
	}
	return user, true, nil
}
