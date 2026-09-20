package dataplane

import (
	"os"
	"regexp"
	"testing"
)

// 本文件是**源码结构性钉子**：终态上报的接线落在 assemble.go 里 `terminal.Options` 的
// 大装配字面量里，而那个函数需要真库与真 Redis 才跑得起来——没有用例能实例化它。
//
// 为什么必须钉：这组变量的历史形态正是「契约里解析了、实现里没有」——LANGFUSE_* 长期只出现在
// envSpecs 与启动摘要里，填了 key 也不会有任何数据出现，而全套用例照样绿。摘掉这一行接线
// 会重演同一个形态，且没有任何用例会变红。
var tracerWiring = regexp.MustCompile(`(?m)^\s*Tracer:\s+options\.Tracer,\s*$`)

func TestTerminalTracerProductionWiringNail(t *testing.T) {
	source, err := os.ReadFile("assemble.go")
	if err != nil {
		t.Fatalf("读 assemble.go 失败: %v", err)
	}
	if !tracerWiring.Match(source) {
		t.Fatalf("生产装配里找不到终态上报的接线（assemble.go 的主结算器 terminal.Options）：" +
			"少了它，配了 LANGFUSE_* 也不会有任何上报，而本包其余用例不会变红")
	}
}
