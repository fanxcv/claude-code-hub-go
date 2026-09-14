package adminapi

import (
	"net/http"
	"strings"
)

// 本文件是 Node 的 `/api/v1/openapi.json`、`/api/v1/docs`、`/api/v1/scalar` 的 Go 落点
// （Node 侧 `src/app/api/v1/_root/docs.ts:14,30,36`，由 `app.ts:158` 的 `registerDocs(app)` 挂载；
// 三条都在管理面前门的 `app.use("*")` 信封之内，但不带 `x-required-access` 元数据，故**未鉴权**）。
//
// **与 Node 的有意差异（其一：文档来源）**。Node 的 spec 由 Hono 的 zod-openapi 从**声明**生成；
// Go 侧从**已注册路由表**（`Router.RouteList`）生成。因此 Go 的文档描述「本进程真正能答什么」，
// 不会把未实现的路由写进去——在 Node 即将下线的前提下，这比为对齐 Node 而虚报路径更有用。
//
// **有意差异（其二：交互式 UI）**。Node 的 `/docs`（Swagger UI）与 `/scalar` 需要打包
// `swagger-ui` / `@scalar/hono-api-reference` 的前端资源；Go 镜像里没有这些 bundle，且本仓
// 的 UI 产物（`uiapp/assets`）是 Next 静态导出的页面、不含文档工具链。故这两条**退化为 302
// 跳到 `/api/v1/openapi.json`**：机器可读的 spec 是完整的，只是没有交互式渲染。若产品需要
// 真 UI，正确做法是把 bundle 作为嵌入产物加进镜像（须协调者裁决，见报告）。
//
// 不承诺 schema：本仓没有 zod 那类声明式请求/响应模型，编一份假 schema 比不编更坏——客户端会
// 据此生成错误的类型。故只给 paths/操作元数据。

// openAPIInfo 是文档的 info 段。
type openAPIInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// openAPIOperation 是一个操作。扩展字段名与 Node 同源（`x-required-access`）。
type openAPIOperation struct {
	OperationID     string   `json:"operationId,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	RequiredAccess  string   `json:"x-required-access"`
	ManagementPlane bool     `json:"x-management-plane"`
}

// openAPIDocument 是 OpenAPI 3.1 文档（只填本仓能真实保证的部分）。
type openAPIDocument struct {
	OpenAPI string                                 `json:"openapi"`
	Info    openAPIInfo                            `json:"info"`
	Paths   map[string]map[string]openAPIOperation `json:"paths"`
}

// RegisterDocsRoutes 注册文档三条。
//
// 与 Node 一致：**无需认证**（不带 x-required-access 元数据、app 级中间件只加响应头）。
// 路由表为空时仍注册（空文档优于 404：客户端至少能拿到合法 JSON）。
func RegisterDocsRoutes(router *Router, deps Deps) {
	api := &docsAPI{router: router, logger: adminLoggerOf(deps)}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/openapi.json",
		Access:      AccessPublic,
		Module:      "docs",
		OperationID: "getOpenApiDocument",
		Handler:     http.HandlerFunc(api.handleOpenAPI),
	})
	// 交互式 UI 的两条：见文件头「有意差异（其二）」。
	for _, path := range []string{"/docs", "/scalar"} {
		router.Add(Route{
			Method:      http.MethodGet,
			Path:        path,
			Access:      AccessPublic,
			Module:      "docs",
			OperationID: "getApiDocs",
			Handler:     http.HandlerFunc(api.handleDocsRedirect),
		})
	}
}

type docsAPI struct {
	router *Router
	logger interface {
		Warn(event string, fields map[string]any)
	}
}

// handleOpenAPI 从当前注册表生成文档。**每次请求重建**：路由表在进程生命周期内可能增长
// （各 registrar 按依赖就绪情况注册），缓存一份就会与真实可答的路由分叉。
func (api *docsAPI) handleOpenAPI(writer http.ResponseWriter, _ *http.Request) {
	routes := api.router.RouteList()
	paths := make(map[string]map[string]openAPIOperation, len(routes))
	for _, route := range routes {
		// 文档自身不入文档：它不是业务面，写进去只会让 spec 自指。
		if route.Module == "docs" {
			continue
		}
		// 管理面路径统一带 MountPrefix（Route.Path 是相对形式），故这里回加上，
		// 让文档里的路径与客户端实际请求的路径一字不差。
		path := route.Path
		//
		// 根级 /api/* 面（pubmeta、cloud-model-count 等）注册时写的就是绝对路径，
		// 故按前缀判断而不是无条件拼接。
		if !strings.HasPrefix(path, "/api/") {
			path = MountPrefix + path
		}
		if paths[path] == nil {
			paths[path] = map[string]openAPIOperation{}
		}
		operation := openAPIOperation{
			OperationID:     route.OperationID,
			RequiredAccess:  string(route.Access),
			ManagementPlane: !route.NoManagementEnvelope,
		}
		if route.Module != "" {
			operation.Tags = []string{route.Module}
		}
		paths[path][strings.ToLower(route.Method)] = operation
	}

	document := openAPIDocument{
		OpenAPI: "3.1.0",
		Info: openAPIInfo{
			Title:   "CC Hub Go Management API",
			Version: APIVersion,
			Description: "由 Go 进程的已注册路由表生成（见 go/internal/adminapi/docs_openapi.go）。" +
				"只列本进程能真实作答的路径；未实现的路由不在其中。",
		},
		Paths: paths,
	}
	adminWriteJSON(writer, http.StatusOK, document)
}

// handleDocsRedirect 把两条交互式 UI 路径跳到 spec（见文件头差异其二）。
func (api *docsAPI) handleDocsRedirect(writer http.ResponseWriter, request *http.Request) {
	if api.logger != nil {
		api.logger.Warn("admin_docs_ui_degraded", map[string]any{
			"path":   request.URL.Path,
			"reason": "Go 镜像未打包 Swagger UI / Scalar 前端资源，退化为跳到 openapi.json",
		})
	}
	http.Redirect(writer, request, MountPrefix+"/openapi.json", http.StatusFound)
}
