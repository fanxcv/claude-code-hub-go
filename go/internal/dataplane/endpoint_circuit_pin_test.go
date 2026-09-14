package dataplane

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件是「端点候选必须过端点熔断」的**结构性钉子**。
//
// 为什么用源码断言而不是只留行为用例：行为用例钉的是**已知的**那个构建点；将来有人新写一处
// `forward.Endpoint{...}`（例如给某条族做专用端点池），行为用例照绿，而那处新入口会把
// 已熔断的端点重新放回请求路径——正是本次修掉的那类缺陷（EndpointOpen 实现了却无人调用）。
// 所以这里按**源码结构**发现所有构建点，逐个要求同函数体内出现端点熔断判定。
//
// 模块根相对本测试的包目录 go/internal/dataplane。
const moduleRoot = "../.."

// endpointCircuitPinFile 是必须含端点熔断判定的调用名（route.HealthReader.EndpointOpen）。
const endpointCircuitPinCall = "EndpointOpen"

// builderSites 扫描模块内所有非测试源码，返回「构建转发端点的函数」清单。
//
// 判据是 AST 结构而不是字符串包含：函数体里出现 `forward.Endpoint{` 组合字面量即算构建点。
// 只看类型限定名 forward.Endpoint，故本包内的 `Endpoint{...}` 不会被误判——本包的
// Endpoint 是别的类型（转发端点的构造必须显式写 forward 包名前缀）。
func builderSites(t *testing.T) map[string]string {
	t.Helper()
	// key 是 "文件:行 函数名"，value 是该函数体源码（用于二次判定是否含熔断调用）。
	sites := map[string]string{}
	fset := token.NewFileSet()
	parsed := 0

	err := filepath.WalkDir(moduleRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			// vendor 与 testdata 不是生产源码；跳过后仍会走到子目录里的 _test.go 再被过滤。
			if name := entry.Name(); name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			t.Fatalf("解析 %s 失败：%v", path, parseErr)
		}
		parsed++
		ast.Inspect(file, func(node ast.Node) bool {
			decl, ok := node.(*ast.FuncDecl)
			if !ok || decl.Body == nil {
				return true
			}
			builds := false
			ast.Inspect(decl.Body, func(inner ast.Node) bool {
				literal, ok := inner.(*ast.CompositeLit)
				if !ok {
					return true
				}
				if isForwardEndpointShape(literal.Type) {
					builds = true
				}
				return true
			})
			if !builds {
				return true
			}
			key := filepath.ToSlash(path) + " " + decl.Name.Name
			sites[key] = bodyText(fset, decl)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("遍历模块源码失败：%v", err)
	}
	if parsed == 0 {
		t.Fatalf("%s 里一个源文件都没解析到（路径写错了吗）", moduleRoot)
	}
	return sites
}

// bodyText 取函数体在源码里的原始切片，用于同一函数体内的调用判定。
func bodyText(fset *token.FileSet, decl *ast.FuncDecl) string {
	if fset == nil || decl.Body == nil {
		return ""
	}
	tokenFile := fset.File(decl.Body.Pos())
	if tokenFile == nil {
		return ""
	}
	start := fset.Position(decl.Body.Pos()).Offset
	end := fset.Position(decl.Body.End()).Offset
	raw, err := os.ReadFile(tokenFile.Name())
	if err != nil || start < 0 || end > len(raw) || start >= end {
		return ""
	}
	return string(raw[start:end])
}

// isForwardEndpointShape 判定一个字面量类型是不是「构建转发端点」的形状。
//
// 为什么不能只看 `forward.Endpoint` 显式限定：元素类型可以省略。
// `[]forward.Endpoint{{ID: 1}}` 与 `append(list, forward.Endpoint{...})` 都会构建端点，
// 但前者的**元素**字面量没有类型节点。只认显式形态会漏报，而漏报正是这类钉子最危险的失效
// （实测：第一版判据放过了 `[]forward.Endpoint{{...}}` 的新构建点）。
// 故凡「forward.Endpoint」本身、以及以它为元素的切片/数组字面量，都算构建形状。
func isForwardEndpointShape(expr ast.Expr) bool {
	switch typed := expr.(type) {
	case *ast.SelectorExpr:
		pkg, ok := typed.X.(*ast.Ident)
		return ok && pkg.Name == "forward" && typed.Sel.Name == "Endpoint"
	case *ast.ArrayType:
		return isForwardEndpointShape(typed.Elt)
	default:
		return false
	}
}

// TestEndpointCandidateBuildersConsultEndpointCircuit 是钉子本体。
func TestEndpointCandidateBuildersConsultEndpointCircuit(t *testing.T) {
	sites := builderSites(t)
	if len(sites) == 0 {
		t.Fatal("没找到任何构建 forward.Endpoint 的函数：钉子的判据失效了（漏报比误报更危险）")
	}
	for site, body := range sites {
		if body == "" {
			t.Fatalf("取不到 %s 的函数体源码，钉子无法判定", site)
		}
		if !strings.Contains(body, endpointCircuitPinCall) {
			t.Errorf(
				"构建点 %s 没有调用 %s：端点级熔断会被绕过（页面显示该端点已熔断、请求照打）。"+
					"端点候选必须先过 route.HealthReader.EndpointOpen，见 upstream.go 的 fromStore",
				site, endpointCircuitPinCall,
			)
		}
	}
}
