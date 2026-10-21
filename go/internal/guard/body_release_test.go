package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件钉住「正文树可释放」这一契约的两半：释放后 JSON/Store 必须报错（绝不静默重建），
// 但字节仍可读；亲和指纹必须在释放之前算好，释放之后仍能按同一条链选路。
//
// 为什么第一半要钉成错误：树是每请求一份与正文同量级的常驻（实测 1.00× 正文字节），
// 收益全靠「没有人再要它」。若释放后静默给出一棵新树，漏掉的消费点照常工作、收益归零，
// 而且没有错误也没有日志——只能靠量堆才能发现。故这里断言的是「报错」而不是「能用」。

// releaseTestBody 是构建正文访问器用的 claude 正文（系统段 + 一条消息 ⇒ tip 深度非 0）。
const releaseTestBody = `{"model":"claude-sonnet-4-5","system":"sys","messages":[{"role":"user","content":"hi"}]}`

// newRealAccessor 造一个真正的 *BodyAccessor（假的 BodyAccess 实现没有树可释放）。
func newRealAccessor(t *testing.T, payload string) *BodyAccessor {
	t.Helper()
	pc := contextWithEncoding(t, "", io.NopCloser(bytes.NewReader([]byte(payload))))
	accessor, err := NewBodyAccessor(pc, BodyAccessOptions{})
	if err != nil {
		t.Fatalf("构造正文访问器失败: %v", err)
	}
	return accessor
}

// TestJSONAfterReleaseReturnsError 是核心钉子：释放后再要树必须报错。
func TestJSONAfterReleaseReturnsError(t *testing.T) {
	accessor := newRealAccessor(t, releaseTestBody)
	tree, err := accessor.JSON()
	if err != nil || tree["model"] != "claude-sonnet-4-5" {
		t.Fatalf("释放前应能取到树: %v %v", tree, err)
	}

	accessor.ReleaseTree()
	if _, err := accessor.JSON(); !errors.Is(err, ErrBodyTreeReleased) {
		t.Fatalf("释放后 JSON 必须返回 ErrBodyTreeReleased（静默重建会让收益归零且无痕迹），实际 %v", err)
	}
	// 幂等：重复释放不得把状态翻回去，也不得报错/panic。
	accessor.ReleaseTree()
	if _, err := accessor.JSON(); !errors.Is(err, ErrBodyTreeReleased) {
		t.Fatalf("重复释放后 JSON 仍须报 ErrBodyTreeReleased，实际 %v", err)
	}
	if err := accessor.Store(map[string]any{"model": "x"}); !errors.Is(err, ErrBodyTreeReleased) {
		t.Fatalf("释放后 Store 必须报到同一个错（否则树被静默装回来），实际 %v", err)
	}
}

// TestReleaseTreeKeepsBytes 钉住「释放的是树不是原文」：Bytes 两条路径都仍可读。
func TestReleaseTreeKeepsBytes(t *testing.T) {
	t.Run("未改写", func(t *testing.T) {
		accessor := newRealAccessor(t, releaseTestBody)
		before, err := accessor.Bytes()
		if err != nil {
			t.Fatalf("释放前取字节失败: %v", err)
		}
		accessor.ReleaseTree()
		after, err := accessor.Bytes()
		if err != nil {
			t.Fatalf("释放树后原始字节仍应可读（回放与转发都靠它）: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("释放前后的字节不一致:\nbefore=%s\nafter=%s", before, after)
		}
	})

	t.Run("已改写", func(t *testing.T) {
		accessor := newRealAccessor(t, releaseTestBody)
		tree, err := accessor.JSON()
		if err != nil {
			t.Fatalf("取树失败: %v", err)
		}
		tree["model"] = "claude-opus-4-6"
		if err := accessor.Store(tree); err != nil {
			t.Fatalf("写回失败: %v", err)
		}
		// 与生产同序：planFacts 先取字节（物化补丁），释放点在其后。
		patched, err := accessor.Bytes()
		if err != nil || !bytes.Contains(patched, []byte("claude-opus-4-6")) {
			t.Fatalf("改写后的字节应在释放前物化: %v %s", err, patched)
		}
		accessor.ReleaseTree()
		after, err := accessor.Bytes()
		if err != nil {
			t.Fatalf("补丁字节已物化，释放树后仍应可读: %v", err)
		}
		if !bytes.Equal(patched, after) {
			t.Fatalf("释放后的字节与释放前不一致:\nbefore=%s\nafter=%s", patched, after)
		}
	})
}

// TestAffinityFingerprintPrecomputedSurvivesTreeRelease 钉住「释放后仍能正确选路」。
//
// 这是「指纹已在释放前算好」的可观测证据：释放之后正文树取不到（JSON 报错），
// 但只带预计算链的请求照样按亲和提名——若预计算缺失，它只能退化成加权随机。
func TestAffinityFingerprintPrecomputedSurvivesTreeRelease(t *testing.T) {
	accessor := newRealAccessor(t, releaseTestBody)
	tree, err := accessor.JSON()
	if err != nil {
		t.Fatalf("取树失败: %v", err)
	}
	format := convert.FormatClaude
	router := &ProviderRouter{selector: route.NewSelector(route.Options{
		Source:   affinityFakeSource{provider: affinityTestProvider()},
		Affinity: route.NewAffinityStore(route.AffinityOptions{Window: 8}),
	})}

	chain := router.affinityFingerprint(tree, format)
	if chain == nil {
		t.Fatal("亲和已装配时守卫阶段必须算出指纹链")
	}
	// 与旧口径逐字节一致：同一窗口、同一算法。
	legacy, ok := route.Fingerprint(tree, format, 8)
	if !ok {
		t.Fatal("旧口径现算失败")
	}
	encodedChain, err := json.Marshal(chain)
	if err != nil {
		t.Fatalf("预计算链序列化失败: %v", err)
	}
	encodedLegacy, err := json.Marshal(legacy)
	if err != nil {
		t.Fatalf("现算链序列化失败: %v", err)
	}
	if !bytes.Equal(encodedChain, encodedLegacy) {
		t.Fatalf("预计算链与旧实现不一致:\nnew=%s\nold=%s", encodedChain, encodedLegacy)
	}

	// 释放之后：树不可再取。
	accessor.ReleaseTree()
	if _, err := accessor.JSON(); !errors.Is(err, ErrBodyTreeReleased) {
		t.Fatalf("释放后 JSON 应报错: %v", err)
	}

	selector := route.NewSelector(route.Options{
		Source:   affinityFakeSource{provider: affinityTestProvider()},
		Affinity: route.NewAffinityStore(route.AffinityOptions{Window: 8}),
	})
	lookup := &route.AffinityLookup{
		Hint:       &route.AffinityHint{ProviderID: affinityTestProvider().ID, MatchedFP: chain.Tip().FP},
		IdentityFP: "idfp",
		Generation: "v3:gen",
	}
	result, err := selector.Select(context.Background(), route.Request{
		Model: "claude-sonnet-4-5", Format: format, KeyID: 1,
		// AffinityBody 刻意留空：树已经释放了。
		AffinityFingerprint: chain,
		AffinityLookup:      lookup,
	})
	if err != nil {
		t.Fatalf("释放后选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != affinityTestProvider().ID {
		t.Fatalf("预计算链应提名供应商 %d，实际 %+v", affinityTestProvider().ID, result.Provider)
	}
	if result.Method != route.MethodPrefixAffinity {
		t.Fatalf("选路方式应为 %s，实际 %s", route.MethodPrefixAffinity, result.Method)
	}

	// 对照：没有链、也没有正文时亲和不参与（证明上面的命中确实来自预计算链）。
	control, err := selector.Select(context.Background(), route.Request{
		Model: "claude-sonnet-4-5", Format: format, KeyID: 1,
	})
	if err != nil {
		t.Fatalf("对照选路失败: %v", err)
	}
	if control.Affinity != nil || control.Method == route.MethodPrefixAffinity {
		t.Fatalf("没有指纹来源时不应产生亲和提名: %+v", control.Affinity)
	}
}

// TestAffinityFingerprintPrecomputedIsWired 是接线钉子：守卫的 provider 步骤必须真的把
// 预算链放进 route.Request。摘掉那一行时，release 之后亲和不参与（静默退化），而
// route 侧用例照样全绿——它们盖不住接线。
func TestAffinityFingerprintPrecomputedIsWired(t *testing.T) {
	source, err := os.ReadFile("adapters_route.go")
	if err != nil {
		t.Fatalf("读取 adapters_route.go 失败：%v", err)
	}
	const want = "AffinityFingerprint: r.affinityFingerprint(body, format),"
	if !strings.Contains(string(source), want) {
		t.Fatalf("adapters_route.go 里找不到把预算指纹放进 route.Request 的接线：\n"+
			"  期望形如 `%s`\n"+
			"  该行缺失时，正文树释放后亲和指纹无从再算 ⇒ 亲和不参与，选路静默退化为加权随机。", want)
	}
}

// BenchmarkBodyReleaseTreeHeap 量「释放前 / 释放后」的活堆差异（同一份 6.67 MiB 正文）。
//
// 只在显式 -bench 时运行：它测的是常驻量级，不适合当 CI 断言（GC 噪声会红）。
func BenchmarkBodyReleaseTreeHeap(b *testing.B) {
	payload := heapProbePayload(6_670_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		pc, err := newHeapProbeContext(payload)
		if err != nil {
			b.Fatalf("构造上下文失败: %v", err)
		}
		accessor, err := NewBodyAccessor(pc, BodyAccessOptions{})
		if err != nil {
			b.Fatalf("构造正文访问器失败: %v", err)
		}
		runtime.GC()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		b.StartTimer()

		accessor.ReleaseTree()

		b.StopTimer()
		// 先读一次字节：原文必须仍活着，否则量到的是「原文 + 树」一起被回收（会把数
		// 放大一倍，释放收益看起来像 2N）。
		if kept, err := accessor.Bytes(); err != nil || len(kept) == 0 {
			b.Fatalf("释放后原文应仍可读: %v", err)
		}
		runtime.GC()
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(accessor)
		b.ReportMetric(float64(before.HeapAlloc-after.HeapAlloc)/(1<<20), "MiB-released")
	}
}

// heapProbePayload 造一份约 size 字节的 claude 正文（一条消息承担绝大部分字节）。
func heapProbePayload(size int) []byte {
	content := bytes.Repeat([]byte("a"), size)
	payload := append([]byte(`{"model":"claude-sonnet-4-5","system":"sys","messages":[{"role":"user","content":"`), content...)
	return append(payload, []byte(`"}]}`)...)
}

// newHeapProbeContext 造一个只带正文的请求上下文：堆量测不需要其它事实。
func newHeapProbeContext(payload []byte) (*pctx.Context, error) {
	return pctx.New(pctx.Init{Method: "POST", Path: "/v1/messages", Body: io.NopCloser(bytes.NewReader(payload))})
}
