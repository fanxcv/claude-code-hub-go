package adminapi

import (
	"context"
	"net/http"
)

// 本文件是**只读会话解析入口**：把「这个请求带的凭据是谁」解析成一份快照，供 UI 壳注入
// （`cmd/cchd/ui.go` 的 `window.__CCH_BOOTSTRAP__`）消费。
//
// 为什么必须有它：静态导出后既没有中间件也没有服务端 layout，登录态只能在壳里于**首帧**给出；
// 而壳要判角色敏感跳转（`requireRole="admin"`），就需要真实角色。默认的回退探针
// （`/api/v1/me/metadata`）只能证明「已登录」，拿不到角色 —— 于是 admin 页在被推进去之前
// 无人把关（见 src/components/ui-session-gate.tsx 的说明）。
//
// 唯一的解析实现是 AuthGuard.resolveToken 的四条分支（裸 ADMIN_TOKEN → 签名令牌 →
// 不透明会话 → legacy/dual 的裸 Key）。本入口只做「解析 + 读档门」，**不作答、不写任何东西**：
// 需要拒绝语义的调用方仍然走 Wrap。

// SessionSnapshot 是一次只读会话解析的结论。
//
// 字段只留前端契约真正消费的四个（src/components/ui-session-gate.tsx 的 UiSessionSnapshot）：
// 多带字段就必须解释它的语义，而多出来的字段没人读。
type SessionSnapshot struct {
	// UserID / UserName 是操作主体；ADMIN_TOKEN 身份取 adminPrincipal 的合成值（-1 / "Admin Token"）。
	UserID   int64
	UserName string
	// Role 取 "admin" / "user"（前端 user.role 的取值域；两者之外没有第三种）。
	Role string
	// CanLoginWebUI 是**本次凭据所用密钥**的 can_login_web_ui（ADMIN_TOKEN 的合成密钥恒 true）。
	// 它是写路径的门（见 Principal.CanLoginWebUI），壳只把它带出去，不据此放行。
	CanLoginWebUI bool
}

// SessionResolver 是只读会话解析入口。
type SessionResolver interface {
	// ResolveSession 解析该请求的会话；未登录返回 (nil, nil)。
	//
	// err 只在依赖故障时非 nil（此时既不能当作未登录，也不能谎报身份）：调用方按未登录处理
	// 并记一次 warn，与 resolveToken 对 Redis 故障的处置同向。
	ResolveSession(ctx context.Context, request *http.Request) (*SessionSnapshot, error)
}

// ResolveSession 实现 SessionResolver：复刻 AuthGuard.authenticate 的**读档**（AccessRead）
// 链路，但不作答。
//
// 与 authenticate 的三处差别都是有意为之，逐条：
//
//  1. **只认 cookie**。Node 的页面侧会话解析（`auth()` 读 AUTH_COOKIE_NAME）不认
//     Authorization / x-api-key 头；页面面的登录态判定必须与它同源，否则「带 Key 头请求页面」
//     会在两侧得到不同结论。头凭据是 API 面的凭据形态，由 Wrap 处理。
//  2. **不判档位**。壳要的是「是不是管理员」这个事实本身，而不是拒绝 —— 拒绝由资源端点做。
//  3. **不校验 CSRF**。只有 GET（壳）会走到这里，mutation 与本入口无关。
//
// can_login_web_ui=false 的密钥仍解析成会话（读档放行它），该字段原样带出：只读会话能不能
// 登录 Web UI 是前端的展示决策，写路径另有自己的门（requireKeyWriteSession）。
func (g *AuthGuard) ResolveSession(ctx context.Context, request *http.Request) (*SessionSnapshot, error) {
	if g == nil || request == nil {
		return nil, nil
	}
	token := cookieValue(request.Header.Get("Cookie"), authCookieName)
	if token == "" {
		return nil, nil
	}

	resolved, err := g.resolveToken(ctx, credential{token: token, source: credentialSourceCookie})
	if err != nil {
		return nil, err
	}
	if resolved.failure != nil {
		return nil, nil
	}
	// 用户被禁用/过期一律不是会话（validateKey 的公共校验，auth.ts:96-101）。
	if !resolved.userEnabled {
		return nil, nil
	}
	if resolved.userExpiresAt != nil && !resolved.userExpiresAt.After(g.clock()) {
		return nil, nil
	}

	return &SessionSnapshot{
		UserID:        resolved.principal.UserID,
		UserName:      resolved.principal.Username,
		Role:          sessionRole(resolved.principal.IsAdmin),
		CanLoginWebUI: resolved.keyCanLoginWebUI,
	}, nil
}

// sessionRole 把布尔判定投影成前端取值域。
func sessionRole(isAdmin bool) string {
	if isAdmin {
		return "admin"
	}
	return "user"
}
