package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住管理面的**唯一注册点**（registerAdminRoutes）。
//
// 为什么值得单独立一个测试文件：`Router.Add` 对未注册的路径静默回退 Node，所以「registrar 写好
// 了但没人调」在运行期完全看不出来，只表现为「那一批端点永远由 Node 作答」。实测踩过**两次**：
// 第一次是资源模块齐备后只调了 RegisterShellRoutes（39 条资源路由是死码）；第二次是 error-rules
// 与 request-filters 落地后没接进唯一注册点（14 条路由是死码）。
//
// 第二次的教训是：**手写枚举表拦不住漏调**——上一版测试自己维护一份 adminRegistrars 清单，
// 新增 registrar 时清单与注册点一起漏，测试照样绿（清单的存在反而给出「已覆盖」的错觉）。
// 故此处改成**从源码结构性发现**：registrar 定义从 `internal/adminapi` 的源码里枚举，
// 注册点实际调用了哪些同样从源码里读，两者互相比对。「新增 registrar 而未接进注册点」必红，
// 且不再需要任何清单。
//
// 已知上限：这里只管「定义 ↔ 调用」这一层关系（源码即事实）。路由的**路径、方法、形状**是否与
// Node 一致由 A2 的路由存在性契约测试收口；两条测试合起来才等于「端点没丢」。

// adminAPISourceDir 是 registrar 定义所在的包目录（相对本测试的包目录 go/cmd/cchd）。
const adminAPISourceDir = "../../internal/adminapi"

// adminRegisterSource / adminRegisterFunc 是唯一注册点所在的文件与函数名。
const (
	adminRegisterSource = "admin.go"
	adminRegisterFunc   = "registerAdminRoutes"
)

// registrarVariantSuffix 是 registrar 变体（同一族入口的另一种形态）的后缀。
//
// 例：`RegisterUsageLogs` 与 `RegisterUsageLogsWith`（多一个 options 形参）是同一族的两个入口，
// 接进注册点只接一个即可。故比对时按去掉该后缀的族名归并。
const registrarVariantSuffix = "With"

// stubAdminGuard 是 Guard + CSRFIssuer 的最小实现。
//
// 本测试不关心认证语义（那是 adminapi 自己的用例的领地），只关心「注册点有没有把 registrar
// 调到位」。守卫原样放行即可；记录 wrapped 次数是为了确认路由确实过了 Guard.Wrap。
type stubAdminGuard struct {
	wrapped int
}

func (g *stubAdminGuard) Wrap(_ adminapi.AccessLevel, next http.Handler) http.Handler {
	g.wrapped++
	return next
}

func (g *stubAdminGuard) IssueCSRF(string, int64) string { return "stub-csrf" }

// stubSessionCounter / stubFixed5hWindows 是 keys 三条读档路由的运行态依赖替身。
//
// 它们存在的原因：那三条路由**条件注册**（缺任一依赖就不注册，回退 Node），因此本测试的依赖面
// 必须把它们给全，否则「keys 只有 11 条」也能绿——而那正是生产上少接一个依赖时的症状。
type stubSessionCounter struct{}

func (stubSessionCounter) KeySessionCount(context.Context, int64) (int, error) { return 0, nil }

type stubFixed5hWindows struct{}

func (stubFixed5hWindows) Fixed5hWindowState(
	context.Context, int64, time.Time,
) (limit.Fixed5hState, error) {
	return limit.Fixed5hState{}, nil
}

// adminRegisterDeps 造出「每个 registrar 都愿意注册」的**全量**依赖。
//
// 注册函数对缺失依赖的两种反应不同：Guard / Store 缺失时整块不注册（fail-closed），keys 的
// 两个运行态依赖缺失时只不注册那三条读档路由。所以这里必须把四个都填上，才能让「路由表条数」
// 成为「生产上该注册的都注册了」的判据。池子给零值指针就够（不碰真库，处理器也不会被执行）。
func adminRegisterDeps(guard adminapi.Guard) adminapi.Deps {
	logger := logx.New(nil)
	return adminapi.Deps{
		Logger:         logger,
		Guard:          guard,
		Problems:       adminapi.NewProblems(logger),
		Store:          new(store.Pools),
		SessionCounts:  stubSessionCounter{},
		Fixed5hWindows: stubFixed5hWindows{},
	}
}

// routeModules 汇总每个模块已注册的路由条数。
func routeModules(router *adminapi.Router) map[string]int {
	counts := make(map[string]int)
	for _, route := range router.RouteList() {
		counts[route.Module]++
	}
	return counts
}

// adminAPISource 是从 adminapi 源码里读出的事实。
type adminAPISource struct {
	// registrars 是 registrar 定义名，含变体（如 RegisterUsageLogsWith）。
	registrars map[string]bool
	// modules 是源码里声明过的模块名（`Route.Module` 的字面量）。
	modules map[string]bool
}

// discoverAdminAPISource 解析 `internal/adminapi` 的非测试源码，收集 registrar 定义与模块名。
//
// 判据是**签名**而不是清单：包内的导出函数 `Register*`，第一个形参 `*Router`、第二个 `Deps`，
// 就是资源 registrar（前门能力面 RegisterShellRoutes 多一个 CSRFIssuer 形参，同样落在判据里）。
func discoverAdminAPISource(t *testing.T) adminAPISource {
	t.Helper()
	entries, err := os.ReadDir(adminAPISourceDir)
	if err != nil {
		t.Fatalf("读取 adminapi 源码目录失败（%s）：%v", adminAPISourceDir, err)
	}
	source := adminAPISource{registrars: map[string]bool{}, modules: map[string]bool{}}
	fset := token.NewFileSet()
	parsed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(adminAPISourceDir, name), nil, 0)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", name, err)
		}
		parsed++
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.FuncDecl:
				if typed.Recv == nil && typed.Name.IsExported() &&
					strings.HasPrefix(typed.Name.Name, "Register") &&
					startsWithRouterDeps(typed.Type) {
					source.registrars[typed.Name.Name] = true
				}
			case *ast.KeyValueExpr:
				key, ok := typed.Key.(*ast.Ident)
				if !ok || key.Name != "Module" {
					return true
				}
				literal, ok := typed.Value.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				if module, err := strconv.Unquote(literal.Value); err == nil {
					source.modules[module] = true
				}
			}
			return true
		})
	}
	if parsed == 0 || len(source.registrars) == 0 {
		t.Fatalf("%s 里没解析到 registrar（源文件 %d 个）：路径写错了吗", adminAPISourceDir, parsed)
	}
	return source
}

// startsWithRouterDeps 判断形参是否以 `(*Router, Deps)` 开头（即它是 registrar 的签名）。
func startsWithRouterDeps(signature *ast.FuncType) bool {
	types := paramTypes(signature)
	if len(types) < 2 {
		return false
	}
	router, ok := types[0].(*ast.StarExpr)
	if !ok {
		return false
	}
	routerType, ok := router.X.(*ast.Ident)
	if !ok || routerType.Name != "Router" {
		return false
	}
	deps, ok := types[1].(*ast.Ident)
	return ok && deps.Name == "Deps"
}

// paramTypes 按形参位置展开类型（`a, b int` 展开成两个 int）。
func paramTypes(signature *ast.FuncType) []ast.Expr {
	if signature.Params == nil {
		return nil
	}
	var types []ast.Expr
	for _, field := range signature.Params.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		for i := 0; i < count; i++ {
			types = append(types, field.Type)
		}
	}
	return types
}

// wiredRegistrars 从唯一注册点的源码里读出它实际调用了哪些 `adminapi.Register*`。
func wiredRegistrars(t *testing.T) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), adminRegisterSource, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", adminRegisterSource, err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == adminRegisterFunc {
			body = fn.Body
		}
	}
	if body == nil {
		t.Fatalf("%s 里找不到唯一注册点函数 %s：它被改名或搬走了吗", adminRegisterSource, adminRegisterFunc)
	}
	called := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := selector.X.(*ast.Ident)
		if ok && pkg.Name == "adminapi" && strings.HasPrefix(selector.Sel.Name, "Register") {
			called[selector.Sel.Name] = true
		}
		return true
	})
	return called
}

// registrarFamily 把变体归并到族名（RegisterUsageLogsWith → RegisterUsageLogs）。
func registrarFamily(name string) string {
	return strings.TrimSuffix(name, registrarVariantSuffix)
}

// 定义与调用必须一一对上：漏调一个 registrar，它的端点就会静默回退 Node。
func TestRegisterAdminRoutesWiresEveryRegistrar(t *testing.T) {
	source := discoverAdminAPISource(t)
	called := wiredRegistrars(t)

	missing := make([]string, 0)
	for name := range source.registrars {
		if called[name] || called[registrarFamily(name)] {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("以下 registrar 已在 internal/adminapi 定义，但没有被唯一注册点调用：%v\n"+
			"它们的端点在 Node 不启动时表现为 502（静默回退）。请在 %s 的 %s 里补上调用。",
			missing, adminRegisterSource, adminRegisterFunc)
	}

	stray := make([]string, 0)
	for name := range called {
		if source.registrars[name] || source.registrars[registrarFamily(name)] {
			continue
		}
		stray = append(stray, name)
	}
	sort.Strings(stray)
	if len(stray) > 0 {
		t.Fatalf("唯一注册点调用了不存在的 registrar：%v（拼错了吗）", stray)
	}

	guard := &stubAdminGuard{}
	deps := adminRegisterDeps(guard)

	// 根级认证面（/api/auth/*）需要一个 issuer：账户与会话两个缝用桩注入，避免测试去连库/Redis。
	issuer, err := adminapi.NewAuthIssuer(adminapi.AuthIssuerOptions{
		Deps:     deps,
		Accounts: stubLoginAccounts{},
		Sessions: stubAuthSessions{},
	})
	if err != nil {
		t.Fatalf("构造根级认证面失败：%v", err)
	}

	router := adminapi.New(adminapi.Options{Deps: deps})
	registerAdminRoutes(router, deps, guard, issuer, nil, nil, adminapi.PublicStatusReadOptions{
		Store: registerTestPublicStatusStore{},
	})

	total := router.RouteCount()
	if total == 0 {
		t.Fatal("唯一注册点一条路由都没注册：管理面全部静默回退 Node")
	}
	if guard.wrapped != total {
		t.Fatalf("已注册 %d 条但只有 %d 条过了守卫：存在未认证即可达的管理路由", total, guard.wrapped)
	}

	// 源码里声明过的每个模块都必须在路由表里现身：registrar 被调到了但提前 return（例如依赖
	// 不足）时一条都不会注册，那批端点同样是死码。
	modules := routeModules(router)
	for module := range source.modules {
		if modules[module] == 0 {
			t.Fatalf("模块 %s 在源码里声明过，但路由表里一条都没有：它的 registrar 提前 return 了", module)
		}
	}
	for module := range modules {
		if !source.modules[module] {
			t.Fatalf("路由表里出现了源码未声明的模块 %s：模块名写错或来自动态拼接", module)
		}
	}

	// keys 的三条读档路由是**条件注册**的（缺运行态依赖就不注册）。依赖给全时必须凑齐 14 条，
	// 否则「有一个依赖没接进生产装配」会以「keys 只有 11 条」的形式静静发生。
	if modules["keys"] != keysRouteCount {
		t.Fatalf("keys 应注册 %d 条（含三条读档），实际 %d：有依赖没接进装配",
			keysRouteCount, modules["keys"])
	}

	t.Logf("唯一注册点注册 %d 条管理路由：%v", total, modules)
}

// keysRouteCount 是 keys 模块的端点总数（RegisterKeysRoutes 的 14 条 + 批量成本读数 1 条）；改路由表时必须同步这一个常量。
const keysRouteCount = 15

// keysRegistrarRouteCount 只是 RegisterKeysRoutes **单个 registrar** 的条数：批量成本端点
// 由另一个 registrar（RegisterCostBatchRoutes）注册，故只调前者时不应把它算进来。
const keysRegistrarRouteCount = 14

// 守卫缺失时必须一条都不注册（fail-closed）：宁可由 Node 作答，也不放行未认证的管理请求。
func TestRegisterAdminRoutesRefusesWithoutGuard(t *testing.T) {
	deps := adminRegisterDeps(nil)
	router := adminapi.New(adminapi.Options{Deps: deps})
	// issuer 传 nil：根级认证面自己的注册函数对 nil issuer 直接返回，正是这条 fail-closed 的同类语义
	// （认证面没装起来时宁可不答，让 Node 去答）。
	registerAdminRoutes(router, deps, nil, nil, nil, nil, adminapi.PublicStatusReadOptions{})
	if count := router.RouteCount(); count != 0 {
		t.Fatalf("守卫未装配时不得注册任何路由，收到 %d 条", count)
	}
}

// stubLoginAccounts 是根级认证面的账户缝桩：本文件只关心「注册点有没有把 registrar 接进来」，
// 不关心登录语义（那是 internal/adminapi 自己的用例的领地）。
type stubLoginAccounts struct{}

// LoginAccountByKey 恒返回「未找到」。
func (stubLoginAccounts) LoginAccountByKey(context.Context, string) (adminapi.LoginAccount, bool, error) {
	return adminapi.LoginAccount{}, false, nil
}

// stubAuthSessions 是根级认证面的会话缝桩。
type stubAuthSessions struct{}

// CreateAuthSession 恒返回固定会话 id。
func (stubAuthSessions) CreateAuthSession(
	context.Context, string, adminapi.LoginAccount, string, time.Duration,
) (string, error) {
	return "sid_stub", nil
}

// RevokeAuthSession 是空操作。
func (stubAuthSessions) RevokeAuthSession(context.Context, string) error { return nil }

// registerTestPublicStatusStore 是注册点测试用的最小公共状态存储替身。
//
// 为什么要替身：`RegisterPublicStatusReadRoutes` 在缺存储时**整组不注册**（回退 Node 的
// fail-closed 语义），而注册点那条钉子要求「源码声明过的模块必须在路由表现身」——给它一个
// 非空实现，才能验证「生产装配里这一路确实接上了」，而不是把钉子放宽掉。
type registerTestPublicStatusStore struct{}

func (registerTestPublicStatusStore) Ready(context.Context) bool { return true }
func (registerTestPublicStatusStore) Get(context.Context, string) (string, bool) {
	return "", false
}
func (registerTestPublicStatusStore) PTTL(context.Context, string) (time.Duration, error) {
	return -2, nil
}
func (registerTestPublicStatusStore) SetEX(context.Context, string, string, time.Duration) error {
	return nil
}
func (registerTestPublicStatusStore) SetPX(context.Context, string, string, time.Duration) error {
	return nil
}
func (registerTestPublicStatusStore) Set(context.Context, string, string) error { return nil }
