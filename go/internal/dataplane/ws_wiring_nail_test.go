package dataplane

import (
	"os"
	"regexp"
	"testing"
)

// 本文件是**源码结构性钉子**：上游 WS 的三处生产接线（拨号器、资格判定、跳过上报）都在
// assemble.go 的同一个 forward.Deps 字面量里，而那个函数需要真库与真 Redis 才跑得起来——
// 没有任何用例能实例化它（见 ws_wiring.go 的说明）。
//
// 为什么必须钉：摘掉任一处都不会让别的用例变红。资格判定被摘掉时，端到端用例用的仍是同一份
// 构造函数（它自己注入），照样绿；这正是 2026-09-19 那次事故的形态——「测试全绿、生产不生效」。
// 与 settle_wiring_nail_test.go 同一套做法：读源码文本，断言调用点真的在。

var (
	// 三处接线：字面量的键名与取值都必须精确，值被换成别的实现即报错。
	wsDialerWiring   = regexp.MustCompile(`(?m)^\s*WS:\s+wsDialer,\s*$`)
	wsEligibleWiring = regexp.MustCompile(`(?m)^\s*WSEligible:\s+wsEligibility\(adapters\.Settings\),\s*$`)
	wsNoticeWiring   = regexp.MustCompile(`(?m)^\s*WSNotice:\s+newWSNotice\(logger\),\s*$`)
)

func TestUpstreamWSProductionWiringNail(t *testing.T) {
	source, err := os.ReadFile("assemble.go")
	if err != nil {
		t.Fatalf("读 assemble.go 失败: %v", err)
	}
	for name, pattern := range map[string]*regexp.Regexp{
		"WS 拨号器": wsDialerWiring,
		"资格判定":   wsEligibleWiring,
		"跳过上报缝":  wsNoticeWiring,
	} {
		if !pattern.Match(source) {
			t.Fatalf("生产装配里找不到「%s」的接线（assemble.go 的 forward.Deps 字面量）："+
				"少了它，上游 WS 会在生产上静默失效，而本包其余用例不会变红", name)
		}
	}
}
