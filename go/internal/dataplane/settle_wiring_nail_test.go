package dataplane

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 本文件是**源码结构性钉子**：它读 upstream.go 的源码文本，断言两个「生产端调用点」
// 真的存在，而不是只断言被调用的函数自己正确。
//
// 为什么必须有它：settlement 的字段由纯函数算出来（cachescore.go，有单测），
// 写库管道由真库集成测试证明（terminal 包），但「settler 到底有没有调用它们」
// 这两件事都盖不住——摘掉调用点时，那两组用例照样全绿（实测：摘掉
// applyCostMultipliers 的调用后，真库用例仍 ok）。
// 这正是「测试全绿但功能没接」的经典盲区，故用源码发现把它钉死。

var (
	// 行首允许有 tab 缩进；匹配的是调用语句本身。
	costMultiplierCall = regexp.MustCompile(`(?m)^\s*applyCostMultipliers\(&settlement, s\.state\)\s*$`)
	cacheScoreCall     = regexp.MustCompile(`(?m)^\s*s\.applyCacheScoreFields\(ctx, &settlement, pc, outcome\)\s*$`)
)

// funcBody 取出某个方法/函数的函数体文本（从签名到下一个顶层 `}`）。
//
// 用大括号配平而不是正则贪婪匹配：正则会把后面的函数一起吞进来，
// 于是「Stream 里有调用」会被 NonStream 里的调用蒙混过关。
func funcBody(t *testing.T, source, signature string) string {
	t.Helper()
	start := strings.Index(source, signature)
	if start < 0 {
		t.Fatalf("在 upstream.go 里找不到签名 %q：方法改名时请同步本钉子", signature)
	}
	depth := 0
	for i := start; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start : i+1]
			}
		}
	}
	t.Fatalf("签名 %q 的函数体没有闭合大括号", signature)
	return ""
}

func readUpstreamSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("upstream.go"))
	if err != nil {
		t.Fatalf("读取 upstream.go 失败: %v", err)
	}
	return string(raw)
}

// TestSettleWiringCallsCostMultipliersInBothPaths 断言倍率的生产端**两条路径都接了**：
// 非流式与流式各自都要写入。只接一条的后果是「界面上一半行有倍率、一半没有」，
// 这种半接状态在只看函数单测时完全看不出来。
func TestSettleWiringCallsCostMultipliersInBothPaths(t *testing.T) {
	source := readUpstreamSource(t)

	nonStream := funcBody(t, source, "func (s *storeSettler) NonStream(")
	stream := funcBody(t, source, "func (s *storeSettler) Stream(")

	for name, body := range map[string]string{"NonStream": nonStream, "Stream": stream} {
		if !costMultiplierCall.MatchString(body) {
			t.Fatalf("%s 没有调用 applyCostMultipliers：倍率列在该路径上不会落库", name)
		}
	}
}

// TestSettleWiringCallsCacheScoreOnlyInStream 断言 F3b 的生产端**只在流式路径**：
// Node 的 computeCacheScoreFields 只有一处调用点（response-handler.ts:5431，流式结算），
// 非流式行在两侧都必须保持 NULL——多接一处会让同一列在两种路径上有两种含义。
func TestSettleWiringCallsCacheScoreOnlyInStream(t *testing.T) {
	source := readUpstreamSource(t)

	stream := funcBody(t, source, "func (s *storeSettler) Stream(")
	if !cacheScoreCall.MatchString(stream) {
		t.Fatal("Stream 没有调用 applyCacheScoreFields：F3b 五列在该路径上不会落库")
	}

	nonStream := funcBody(t, source, "func (s *storeSettler) NonStream(")
	if cacheScoreCall.MatchString(nonStream) {
		t.Fatal("NonStream 不应调用 applyCacheScoreFields：Node 只在流式结算产出这五列")
	}
}

// TestSettleWiringNailIsNotVacuous 防「钉子自己变成空跑」：若 upstream.go 被改名或
// 挪走，上面的正则会静默不匹配——本用例先证明两个签名真的能取到函数体。
func TestSettleWiringNailIsNotVacuous(t *testing.T) {
	source := readUpstreamSource(t)
	if len(source) < 1000 {
		t.Fatalf("upstream.go 内容异常短（%d 字节），钉子可能读到了错的文件", len(source))
	}
	for _, signature := range []string{
		"func (s *storeSettler) NonStream(",
		"func (s *storeSettler) Stream(",
	} {
		if body := funcBody(t, source, signature); len(body) < 200 {
			t.Fatalf("签名 %q 的函数体只有 %d 字节，函数体提取显然失效", signature, len(body))
		}
	}
}
