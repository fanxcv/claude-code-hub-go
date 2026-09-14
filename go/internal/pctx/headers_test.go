package pctx

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestHeaderViewReads(t *testing.T) {
	ctx := newTestContext(t, Init{Headers: http.Header{
		"X-B": {"2"},
		"X-A": {"1", "1b"},
	}})
	view := ctx.Headers()

	if got := view.Get("x-a"); got != "1" {
		t.Fatalf("Get 应大小写不敏感，得到 %q", got)
	}
	if got := view.Values("X-A"); len(got) != 2 || got[1] != "1b" {
		t.Fatalf("Values = %v", got)
	}
	if !view.Has("x-a") || view.Has("x-missing") {
		t.Fatal("Has 判定错误")
	}
	if view.Len() != 2 {
		t.Fatalf("Len = %d, want 2", view.Len())
	}
	if keys := view.Keys(); keys[0] != "X-A" || keys[1] != "X-B" {
		t.Fatalf("Keys 应有序，得到 %v", keys)
	}

	var visited []string
	view.Each(func(key string, values []string) bool {
		visited = append(visited, key)
		return true
	})
	if strings.Join(visited, ",") != "X-A,X-B" {
		t.Fatalf("Each 顺序 = %v", visited)
	}

	stopped := 0
	view.Each(func(key string, values []string) bool {
		stopped++
		return false
	})
	if stopped != 1 {
		t.Fatalf("Each 应在回调返回 false 时停止，实际遍历 %d 次", stopped)
	}
}

func TestHeaderViewCopiesDoNotLeak(t *testing.T) {
	ctx := newTestContext(t, Init{Headers: http.Header{"X-A": {"original"}}})

	values := ctx.Headers().Values("X-A")
	values[0] = "mutated"

	if got := ctx.Headers().Get("X-A"); got != "original" {
		t.Fatalf("Values 返回值改写泄漏进上下文: %q", got)
	}

	ctx.Headers().Each(func(key string, got []string) bool {
		got[0] = "mutated-in-each"
		return true
	})
	if got := ctx.Headers().Get("X-A"); got != "original" {
		t.Fatalf("Each 回调内改写泄漏进上下文: %q", got)
	}

	cloned := ctx.Headers().Clone()
	cloned.Set("X-A", "mutated-clone")
	cloned.Set("X-New", "1")
	if got := ctx.Headers().Get("X-A"); got != "original" {
		t.Fatalf("Clone 改写泄漏进上下文: %q", got)
	}
	if ctx.Headers().Has("X-New") {
		t.Fatal("Clone 新增键泄漏进上下文")
	}
}

func TestHeaderViewHasNoMutators(t *testing.T) {
	// 只读视图的「只读」必须是类型事实：一旦有人给视图加写方法，本用例即失败。
	typ := reflect.TypeOf(HeaderView{})
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		for _, prefix := range []string{"Set", "Add", "Del", "Write", "Put"} {
			if strings.HasPrefix(name, prefix) {
				t.Fatalf("HeaderView 不应有写方法: %s", name)
			}
		}
	}
}

func TestHeaderMutationsGoThroughContext(t *testing.T) {
	ctx := newTestContext(t, Init{Headers: http.Header{"X-Keep": {"1"}}})

	if ctx.IsHeaderModified("X-Keep") {
		t.Fatal("未改动时不应判为已改动")
	}

	ctx.SetHeader("X-Keep", "2")
	if !ctx.IsHeaderModified("X-Keep") {
		t.Fatal("改写值应判为已改动")
	}

	ctx.DeleteHeader("X-Keep")
	if !ctx.IsHeaderModified("X-Keep") {
		t.Fatal("删除键应判为已改动")
	}

	ctx.AddHeader("X-Added", "1")
	if !ctx.IsHeaderModified("X-Added") {
		t.Fatal("新增键应判为已改动")
	}

	// 原始视图保持入口原值，供审计比对。
	if got := ctx.OriginalHeaders().Get("X-Keep"); got != "1" {
		t.Fatalf("原始视图被改动: %q", got)
	}
	if ctx.OriginalHeaders().Has("X-Added") {
		t.Fatal("原始视图不应包含新增键")
	}

	// 多值顺序变化也算改动。
	multi := newTestContext(t, Init{Headers: http.Header{"X-M": {"a", "b"}}})
	multi.SetHeader("X-M", "b")
	multi.AddHeader("X-M", "a")
	if !multi.IsHeaderModified("X-M") {
		t.Fatal("多值顺序变化应判为已改动")
	}
}

// TestHeaderViewIsLivePinned 钉住视图的实时语义：拿到视图之后 Context 再写入，
// 视图能看到新值；而 Clone 是调用那一刻的快照。
//
// 这是刻意的取舍（相对于「取视图即冻结」）：视图持有的是原子发布槽位而非具体映射，
// 读取侧因此无需加锁，也不会与写锁互等；需要冻结值时用 Clone。
func TestHeaderViewIsLivePinned(t *testing.T) {
	ctx := newTestContext(t, Init{Headers: http.Header{"X-A": {"1"}}})

	view := ctx.Headers()
	frozen := view.Clone()

	ctx.SetHeader("X-A", "2")
	ctx.AddHeader("X-New", "n")

	if got := view.Get("X-A"); got != "2" {
		t.Fatalf("视图应实时反映后续写入，得到 %q", got)
	}
	if !view.Has("X-New") || view.Len() != 2 {
		t.Fatalf("视图应看到新增键，Has=%v Len=%d", view.Has("X-New"), view.Len())
	}

	ctx.DeleteHeader("X-A")
	if view.Has("X-A") {
		t.Fatal("视图应看到删除")
	}

	if got := frozen.Get("X-A"); got != "1" {
		t.Fatalf("Clone 应是调用那一刻的快照，得到 %q", got)
	}
	if got := frozen.Get("X-New"); got != "" {
		t.Fatalf("Clone 不应看到之后的写入，得到 %q", got)
	}
}

// TestZeroHeaderViewIsEmpty 钉住零值视图按空 headers 处理，不 panic。
func TestZeroHeaderViewIsEmpty(t *testing.T) {
	var view HeaderView

	if view.Len() != 0 || view.Get("X-A") != "" || view.Has("X-A") {
		t.Fatalf("零值视图应为空: Len=%d Get=%q Has=%v", view.Len(), view.Get("X-A"), view.Has("X-A"))
	}
	if view.Values("X-A") != nil {
		t.Fatalf("零值视图的 Values 应为 nil，得到 %v", view.Values("X-A"))
	}
	visited := 0
	view.Each(func(key string, values []string) bool {
		visited++
		return true
	})
	if visited != 0 || len(view.Keys()) != 0 || len(view.Clone()) != 0 {
		t.Fatalf("零值视图不应有内容: visited=%d keys=%v", visited, view.Keys())
	}
}

// TestConcurrentHeaderWritesAndViewReads 并发跑全部视图方法与全部写入方法。
//
// 修复前 HeaderView 直接持有可变 map，读取侧无锁，SetHeader 与 Values/Keys 并发即
// 数据竞争（map 读写竞争，重则直接崩）。本用例在 -race 下跑，是该缺陷的回归钉子：
// 除了让竞争检测器失声，还断言读取到的是「完整值」而非写了一半的结果。
func TestConcurrentHeaderWritesAndViewReads(t *testing.T) {
	ctx := newTestContext(t, Init{Headers: http.Header{"X-A": {"1"}}})

	const (
		writers = 8
		readers = 8
		rounds  = 200
	)

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				ctx.SetHeader("X-W", "writer-"+strconv.Itoa(index)+"-"+strconv.Itoa(round))
				ctx.AddHeader("X-Multi", "v")
				ctx.DeleteHeader("X-Multi")
				_ = ctx.IsHeaderModified("X-W")
			}
		}(i)
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				view := ctx.Headers()
				if got := view.Get("X-W"); got != "" && !strings.HasPrefix(got, "writer-") {
					t.Errorf("读到撕裂的值: %q", got)
				}
				_ = view.Values("X-W")
				_ = view.Has("X-W")
				_ = view.Len()
				_ = view.Keys()
				view.Each(func(key string, values []string) bool { return false })
				_ = view.Clone()

				// 原始视图在构造后不再被改写，并发下必须恒定，否则审计比对失真。
				if got := ctx.OriginalHeaders().Get("X-A"); got != "1" {
					t.Errorf("原始视图被改写: %q", got)
				}
			}
		}()
	}
	wg.Wait()

	if !ctx.Headers().Has("X-W") {
		t.Fatal("并发写入后应能看到写入结果")
	}
	if ctx.Headers().Has("X-Multi") {
		t.Fatal("写入侧先加后删，终态不应残留 X-Multi")
	}
}
