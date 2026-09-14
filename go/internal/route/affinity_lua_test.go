package route

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// nodeAffinityLuaFixture 是亲和 Lua 的**冻结真源**。
//
// 退役前它是 Node 的 src/app/v1/_lib/proxy/affinity/affinity-store.ts：脚本以内联模板字面量
// 写在那里，从未进入 src/lib/redis/lua-scripts.ts 的脚本清单，所以走不了 internal/ratelimit
// 的注册表。该文件已随 Node 数据面退役删除，现为逐字节冻结的副本
// （冻结时点与语义降级见夹具头注）。
const nodeAffinityLuaFixture = "go/testdata/node_affinity_lua.txt"

// findRepoFile 从当前目录向上找仓库内文件。
//
// 用向上查找而不是写死 "../../../"：写死的相对路径只在 `go test`（cwd 为包目录）下成立，
// 编译成测试二进制在别处运行时读不到文件——而一个静默跳过的漂移闸门比没有闸门更危险，
// 它会让人以为比对过。
func findRepoFile(relative string) (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for depth := 0; depth < 8; depth++ {
		candidate := filepath.Join(dir, relative)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// TestAffinityLuaMatchesNodeSource 把 Go 侧副本与**冻结的** Node 真源逐字节比对。
//
// 语义降级（2026-09-13，Node 数据面退役）：真源由 live 源码改为 go/testdata 下的冻结快照，
// 故本闸门从「对 live Node 漂移」降为「对冻结快照漂移」：仍能抓 Go 侧副本被误改，
// 不再能抓 Node 侧将来的变动（Node 已退役，不会有变动）。
//
// 这是本包唯一的漂移闸门：generation fence 的语义藏在脚本正文里（谁先 SET NX、谁检查 GET 结果），
// 任何一侧被「顺手优化」都会让两侧对「哪次写回该被拒」产生分歧，而那种分歧在灰度切换期
// 只表现为偶发的绑定抖动，极难定位——所以宁可在这里硬报红。
func TestAffinityLuaMatchesNodeSource(t *testing.T) {
	path, found := findRepoFile(nodeAffinityLuaFixture)
	if !found {
		t.Fatalf("在仓库内找不到冻结真源 %s：本闸门读不到真源就形同虚设，不允许静默通过",
			nodeAffinityLuaFixture)
	}
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("读取冻结真源 %s 失败: %v", path, err)
	}
	source := string(raw)

	for _, testCase := range []struct {
		nodeConst string
		goSource  string
	}{
		{"LOOKUP_CANDIDATES_LUA", affinityLookupCandidatesLua},
		{"VALIDATE_LOOKUP_HIT_LUA", affinityValidateHitLua},
		{"ENSURE_GENERATION_LUA", affinityEnsureGenerationLua},
		{"CAS_WRITE_LUA", affinityCASWriteLua},
		{"INVALIDATE_LUA", affinityInvalidateLua},
	} {
		nodeLua, ok := extractTemplateLiteral(source, testCase.nodeConst)
		if !ok {
			t.Errorf("%s 中找不到 const %s = `...`;：冻结真源缺该常量", nodeAffinityLuaFixture, testCase.nodeConst)
			continue
		}
		if testCase.goSource != nodeLua {
			t.Errorf("%s 与 Node 真源不一致：\nGo  : %q\nNode: %q", testCase.nodeConst, testCase.goSource, nodeLua)
		}
	}
}

// extractTemplateLiteral 取出 `const NAME = `...`;` 的反引号内容（含首尾换行）。
func extractTemplateLiteral(source, name string) (string, bool) {
	marker := "const " + name + " = `"
	start := strings.Index(source, marker)
	if start < 0 {
		return "", false
	}
	rest := source[start+len(marker):]
	end := strings.Index(rest, "`")
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// 脚本常量必须非空且带 Node 侧的版本化注释头：空正文会让 NewScript 静默变成一次无操作的往返。
func TestAffinityLuaSourcesAreNonEmpty(t *testing.T) {
	for name, source := range map[string]string{
		"LOOKUP_CANDIDATES_LUA":   affinityLookupCandidatesLua,
		"VALIDATE_LOOKUP_HIT_LUA": affinityValidateHitLua,
		"ENSURE_GENERATION_LUA":   affinityEnsureGenerationLua,
		"CAS_WRITE_LUA":           affinityCASWriteLua,
		"INVALIDATE_LUA":          affinityInvalidateLua,
	} {
		if !strings.HasPrefix(source, "\n-- affinity_") {
			t.Errorf("%s 缺少 Node 的版本化注释头: %q", name, source[:min(40, len(source))])
		}
	}
}
