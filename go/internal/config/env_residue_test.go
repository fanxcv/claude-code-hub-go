package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 契约残余登记表：本口径下「无数据面消费方」的 envSpecs 字段。
//
// 口径：字段名在 go/ 下（跳过 *_test.go 与 internal/config/env.go）是否被引用。
// env.go 内的出现不算消费方——那里只有契约自身的登记、结构体标签、EnvSummary 摘要与
// 跨字段校验，均不代表有功能读取该旋钮。
//
// 钉子行为（TestEnvSpecsAreConsumedOrDocumented）：
//   - 新增一个无人消费的 env 而不登记 → 转红；
//   - 已登记的项被接线（获得消费方）→ 转红，提示从表里移除（接线是好事）。
type envResidueEntry struct {
	field  string
	reason string
}

var envSpecResidue = []envResidueEntry{
	{"DashboardLogsPollIntervalMS", "Go 侧日志页由 store 直读，没有前端轮询间隔这回事"},
	{"DetachedStreamBudgetBytes", "分离流（断开后上游续流）在 Go 侧从未实现，仅剩跨字段校验"},
	{"DetachedStreamMaxConcurrency", "分离流未实现，Go 侧无并发上限"},
	{"DetachedStreamMeteringReserveBytes", "分离流未实现，Go 侧无计量预留"},
	{"EnableCircuitBreakerOnNetworkErrors", "Go 侧熔断只按响应状态判定，无「网络错误单独触发」分支"},
	{"EnableProviderCache", "Go 侧无供应商级响应缓存功能"},
	{"MaxRetryAttemptsDefault", "Go 侧重试/竞速次数由 route 与 forward 决策，不读此 env"},
	{"ReplayLiveDedupEnabled", "Go 侧回放无「实时去重」语义"},
	{"ReplayMaxDetachedMS", "Go 侧回放无分离时长语义"},
	{"SessionResponseBodyDedupEnabled", "Go 侧无会话响应体去重布局"},
	{"SessionResponseBodyMaxBytes", "Go 侧无会话响应体去重布局的字节上限"},
}

// 待接线登记表：本口径下同样暂无数据面消费方，但有对应的落地改动负责接线（不是 Node 残留）。
// 与 envSpecResidue 的差别只在语义归类：接线落地后从本表移除即可，不是「保留以维持对账」。
var envSpecPendingWiring = []envResidueEntry{
	{"EnableRateLimit", "ENABLE_RATE_LIMIT 的语义由限流接线改动落地，落地后请移出本表"},
	{"LogLevel", "数据面日志档由 adminapi 的动态 log-level 管理，env 目前仅供 EnvSummary 摘要"},
}

// TestEnvSpecsAreConsumedOrDocumented 是「环境变量契约不得有静默死项」的机械钉子。
func TestEnvSpecsAreConsumedOrDocumented(t *testing.T) {
	goRoot := locateGoRoot(t)

	unconsumed := map[string]bool{}
	for _, spec := range envSpecs {
		if !referencedOutsideEnvSpec(goRoot, spec.field) {
			unconsumed[spec.field] = true
		}
	}

	specFields := map[string]bool{}
	for _, spec := range envSpecs {
		specFields[spec.field] = true
	}

	registered := map[string]bool{}
	for _, entry := range envSpecResidue {
		registered[entry.field] = true
		if !specFields[entry.field] {
			t.Fatalf("envSpecResidue 里的 %s 不在 envSpecs（拼写错误或已删除）", entry.field)
		}
		if !unconsumed[entry.field] {
			t.Fatalf("%s 现在有消费方了，请从 envSpecResidue 移除（并顺手删掉 envSpecs 旁的残余注释）", entry.field)
		}
	}

	pending := map[string]bool{}
	for _, entry := range envSpecPendingWiring {
		pending[entry.field] = true
		if !specFields[entry.field] {
			t.Fatalf("envSpecPendingWiring 里的 %s 不在 envSpecs（拼写错误或已删除）", entry.field)
		}
	}

	for field := range unconsumed {
		if !registered[field] && !pending[field] {
			t.Fatalf("envSpecs 里的 %s 在 Go 侧没有任何消费方：要么接线，要么在 envSpecResidue 登记并写明原因", field)
		}
	}
}

// locateGoRoot 从测试工作目录向上找到含 go.mod 的 go/ 根。
func locateGoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("取工作目录失败: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("从 %s 向上未找到 go.mod", dir)
		}
		dir = parent
	}
}

// referencedOutsideEnvSpec 判断字段名是否在 go/ 下（跳过 *_test.go 与 env.go 自身）被引用。
func referencedOutsideEnvSpec(goRoot, field string) bool {
	pattern := regexp.MustCompile(`\b` + regexp.QuoteMeta(field) + `\b`)
	found := false
	_ = filepath.WalkDir(goRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || found {
			return nil
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if path == filepath.Join(goRoot, "internal", "config", "env.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if pattern.Match(data) {
			found = true
		}
		return nil
	})
	return found
}
