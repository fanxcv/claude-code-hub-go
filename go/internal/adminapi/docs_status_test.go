package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件钉住两条新端点的契约：
//  1. `/api/v1/openapi.json` 由**已注册路由表**生成：含真实路由、不含文档自身；
//  2. `/api/v1/docs` 与 `/api/v1/scalar` 退化为 302 跳到 spec（见 docs_openapi.go 文件头差异其二）；
//  3. `/api/admin/database/status` 在 Store 未装配时**不注册**（回退 Node，与本包同一纪律）。

func TestOpenAPIDocumentIsBuiltFromRegisteredRoutes(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	// 先注册一条业务路由（模块名非 docs），它必须出现在文档里。
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/users",
		Access:      AccessAdmin,
		Module:      "users",
		OperationID: "listUsers",
		Handler:     http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})
	RegisterDocsRoutes(router, Deps{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("文档应 200，收到 %d", recorder.Code)
	}

	var document struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]map[string]struct {
			OperationID    string `json:"operationId"`
			RequiredAccess string `json:"x-required-access"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatalf("文档不是合法 JSON: %v", err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi 版本应为 3.1.0，收到 %q", document.OpenAPI)
	}

	// 业务路由以**带挂载前缀**的绝对路径出现（客户端实际请求的路径）。
	operation, ok := document.Paths["/api/v1/users"]["get"]
	if !ok {
		t.Fatalf("文档缺少 /api/v1/users: %v", document.Paths)
	}
	if operation.OperationID != "listUsers" || operation.RequiredAccess != string(AccessAdmin) {
		t.Fatalf("操作元数据未透传: %+v", operation)
	}
	// 文档自身不入文档（自指没有信息量）。
	if _, present := document.Paths["/api/v1/openapi.json"]; present {
		t.Fatalf("文档不应包含自身路径: %v", document.Paths)
	}
}

func TestDocsAndScalarRedirectToSpec(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterDocsRoutes(router, Deps{})

	for _, path := range []string{"/docs", "/scalar"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusFound {
			t.Fatalf("%s 应 302（Go 镜像未打包 UI 资源），收到 %d", path, recorder.Code)
		}
		if location := recorder.Header().Get("Location"); location != "/api/v1/openapi.json" {
			t.Fatalf("%s 的 Location 应为 spec，收到 %q", path, location)
		}
	}
}

func TestDatabaseStatusNotRegisteredWithoutStore(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterDatabaseStatusRoutes(router, Deps{})
	if count := router.RouteCount(); count != 0 {
		t.Fatalf("Store 未装配时不该注册任何路由，收到 %d 条", count)
	}
}
