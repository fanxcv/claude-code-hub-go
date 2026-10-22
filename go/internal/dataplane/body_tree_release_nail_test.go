package dataplane

import (
	"os"
	"strings"
	"testing"
)

// 本文件是**源码结构性钉子**：正文树的释放点必须晚于全部一次性消费点，且选路闭包不得
// 再持有那棵树。
//
// 为什么需要它：释放本身在 guard 侧有完整用例（JSON 报错、字节仍可读），但「dataplane
// 在正确的位置调用它」谁也盖不住——摘掉释放调用，全部用例照样全绿，收益静默归零；
// 反过来把释放点提前到某个消费点之前，那个消费点会静默读不到正文（比如思考强度列恒空）。
// 与 failover_affinity_nail_test.go / settle_wiring_nail_test.go 同一机理。
//
// 释放点在 forward 内，而三个一次性消费点在 ServeHTTP 内、且都早于 h.forward 调用，
// 故顺序分两段断言：ServeHTTP「消费点 → forward」，forward「选路/计划 → 释放」。

func readDataplaneSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("dataplane.go")
	if err != nil {
		t.Fatalf("读取 dataplane.go 失败: %v", err)
	}
	return string(raw)
}

// TestBodyTreeReleaseComesAfterConsumers 钉住释放点的位置。
func TestBodyTreeReleaseComesAfterConsumers(t *testing.T) {
	source := readDataplaneSource(t)

	serveBody := funcBody(t, source, "func (h *Handler) ServeHTTP(")
	enterForward := strings.Index(serveBody, "h.forward(writer")
	if enterForward < 0 {
		t.Fatal("ServeHTTP 里找不到 h.forward 调用：调用点改名时请同步本钉子")
	}
	// 这些消费点读的是正文树，必须早于进入转发（转发里会释放树）。
	consumers := []string{
		"h.captureRequestedEffort(",
		"captureRequestedServiceTier(",
		"h.startTelemetry(",
		"h.captureRequestSnapshot(",
	}
	for _, consumer := range consumers {
		index := strings.Index(serveBody, consumer)
		if index < 0 {
			t.Fatalf("ServeHTTP 里找不到 %q：消费点改名时请同步本钉子", consumer)
		}
		if index > enterForward {
			t.Fatalf("消费点 %q 晚于进入转发：正文树在转发内已释放，这里会静默读到空", consumer)
		}
	}

	forwardBody := funcBody(t, source, "func (h *Handler) forward(")
	release := strings.Index(forwardBody, "body.releaseTree()")
	if release < 0 {
		t.Fatal("forward 没有调用 body.releaseTree()：正文树会活到请求末尾，1N 常驻白花")
	}
	for _, later := range []string{"fwd.Select = func(", "h.planFacts("} {
		index := strings.Index(forwardBody, later)
		if index < 0 {
			t.Fatalf("forward 里找不到 %q：接线改名时请同步本钉子", later)
		}
		if index > release {
			t.Fatalf("释放点早于 %q：选路或计划编译会读不到正文（静默空值）", later)
		}
	}
}

// TestSelectionFactsDoesNotRetainTree 钉住选路事实不持有正文树。
//
// fwd.Select 闭包捕获整个 facts，facts 里只要有树字段，树就活到请求末尾——释放点再晚
// 也没用。这个字段一旦被加回来，releaseTree 就变成一条自我安慰的调用。
func TestSelectionFactsDoesNotRetainTree(t *testing.T) {
	source := readDataplaneSource(t)
	factsBody := funcBody(t, source, "func (h *Handler) selectionFacts(")
	if strings.Contains(factsBody, "facts.Body") {
		t.Fatal("selectionFacts 又把正文树装进 SelectionFacts：它会被 fwd.Select 闭包捕获，" +
			"树活到请求末尾，正文释放的收益归零")
	}
	if !strings.Contains(factsBody, "facts.Model = bodyModel(tree)") {
		t.Fatal("selectionFacts 应从正文取模型（改名时请同步本钉子）")
	}
}
