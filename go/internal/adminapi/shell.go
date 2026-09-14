package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// 本文件实现管理面前门自身的两条路由——Node 的 /api/v1 应用壳（src/app/api/v1/_root/app.ts:61-131）
// 里不属于任何资源 router 的那两条：
//
//	GET /health     存活：public 档位，不认证
//	GET /auth/csrf  签发与调用方令牌绑定的 CSRF token（read 档位）
//
// 为什么值得单独实现：
// 它们是「管理面是否已经被 Go 接管」最直接的两条证据——前者证明路由确实注册在 Go 上（未注册的
// 路由会静默回退 Node），后者证明守卫与 Problem 信封在 Node 完全不参与的情况下依然完整。
//
// 它们与资源模块的边界：本文件不碰任何业务资源，注册函数只注册这两条。

// shellHealthResponse 逐字对应 Node 的 z.object({status: z.literal("ok"), apiVersion: ...})。
//
// 用结构体而不是 map：字段顺序与 Node 的 JSON.stringify 一致（{"status":...,"apiVersion":...}），
// 便于 A2 做逐字节对拍。
type shellHealthResponse struct {
	Status     string `json:"status"`
	APIVersion string `json:"apiVersion"`
}

// shellCSRFResponse 逐字对应 Node 的 z.object({csrfToken: z.string()})。
type shellCSRFResponse struct {
	CSRFToken string `json:"csrfToken"`
}

// CSRFIssuer 是签发 CSRF token 的能力面（由 AuthGuard 实现）。
//
// 为什么用窄接口而不是把方法挂进 Guard：签发与校验必须出自同一份 secret 推导链
// （csrf.ts:58-61 的 CSRF_SECRET → ADMIN_TOKEN → 会话令牌），拆成两份实现就是两条会互相漂移的
// 推导——签出来的 token 在 CSRF 门上验不过，表现为「浏览器登录后所有写操作 403」。
type CSRFIssuer interface {
	// IssueCSRF 生成绑定调用方令牌的 CSRF token；令牌为空时返回空串。
	IssueCSRF(authToken string, userID int64) string
}

// RegisterShellRoutes 注册管理面前门的两条路由。
//
// issuer 为 nil 时不注册 /auth/csrf（且记一条 warn）：此时请求回退 Node，那里有完整的签发逻辑。
// 宁可回退，也不要 Go 签发一个 Node 验不过的 token。
func RegisterShellRoutes(router *Router, deps Deps, issuer CSRFIssuer) {
	router.Add(Route{
		Method: http.MethodGet,
		Path:   "/health",
		// Node 侧 x-required-access 为 public（app.ts:68）。
		Access:      AccessPublic,
		Module:      "shell",
		OperationID: "getHealth",
		Handler:     http.HandlerFunc(handleShellHealth),
	})

	if issuer == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_shell_csrf_unwired", map[string]any{
				"path":   "/auth/csrf",
				"action": "route_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/auth/csrf",
		Access:      AccessRead,
		Module:      "shell",
		OperationID: "getAuthCsrf",
		Handler:     http.HandlerFunc(handleShellCSRF(deps, issuer)),
	})
}

// handleShellHealth 作答管理面存活（app.ts:83-90）。
func handleShellHealth(writer http.ResponseWriter, _ *http.Request) {
	writeShellJSON(writer, http.StatusOK, shellHealthResponse{Status: "ok", APIVersion: APIVersion})
}

// handleShellCSRF 签发 CSRF token（app.ts:113-131）。
//
// Node 侧在 `!auth.token || !auth.session` 时作答 401 auth.invalid；Go 的守卫在 read 档位下要么
// 拒绝，要么一定留下令牌与身份，故这个分支是「守卫未装配/被绕过」的 fail-closed 兜底，不是常规
// 路径。作答形状与守卫的 401 完全一致，调用方分辨不出差别。
func handleShellCSRF(deps Deps, issuer CSRFIssuer) http.HandlerFunc {
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(deps.Logger)
	}
	return func(writer http.ResponseWriter, request *http.Request) {
		principal, ok := PrincipalFrom(request.Context())
		if !ok || principal.Token == "" {
			problems.WriteProblem(writer, request, http.StatusUnauthorized, "auth.invalid",
				"Authentication is invalid or expired.")
			return
		}
		token := issuer.IssueCSRF(principal.Token, principal.UserID)
		if token == "" {
			// 签发不出 token 说明守卫的 secret 链为空（ADMIN_TOKEN 与 CSRF_SECRET 都没配）：
			// 与 Node 的 createCsrfToken 返回 null 不同，这里明确作答 500——返回
			// {"csrfToken":""} 会让前端拿一个空 token 去写操作，失败点会离根因很远。
			problems.WriteProblem(writer, request, http.StatusInternalServerError, "", "")
			return
		}
		writeShellJSON(writer, http.StatusOK, shellCSRFResponse{CSRFToken: token})
	}
}

// IssueCSRF 实现 CSRFIssuer：复刻 csrf.ts:23-31 的 createCsrfToken。
//
// 格式 `<bucket>.<base64url(HMAC-SHA256(secret, "<authToken>:<userId>:<bucket>"))>`，与
// verifyCSRF 用的派生逐字同源（同一个 signCSRF / csrfPayload / 同一条 secret 取值链）。
func (g *AuthGuard) IssueCSRF(authToken string, userID int64) string {
	if g == nil || authToken == "" {
		return ""
	}
	secret := g.csrfSecret
	if secret == "" {
		secret = g.adminToken
	}
	if secret == "" {
		secret = authToken
	}
	bucket := g.clock().UnixMilli() / csrfWindow.Milliseconds()
	return fmt.Sprintf("%d.%s", bucket, signCSRF(secret, csrfPayload(authToken, userID, bucket)))
}

// writeShellJSON 作答一份 JSON 正文。
//
// Content-Type 逐字取 Node 的 jsonResponse（response-helpers.ts:4-9 的 "application/json"，
// 不带 charset）；版本头与不缓存由 Router 的信封统一补。
func writeShellJSON(writer http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		// 两个响应体都是定长结构体，序列化不可能失败；真失败说明代码被改坏了。
		http.Error(writer, "", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}
