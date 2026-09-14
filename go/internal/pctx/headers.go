package pctx

import (
	"net/http"
	"sort"
	"sync/atomic"
)

// HeaderView 是 headers 的实时只读视图。
//
// 视图内部持有的是 Context 的原子发布槽位，而不是某一份具体 map：每次读取都取当时
// 已发布的那份不可变映射。由此得到两条调用方可依赖的一致性：
//
//  1. 单次方法调用看到的是某一次写入完成后的完整状态，绝不会读到写了一半的 map；
//  2. 视图是实时的——拿到视图之后 Context 再写入，随后调用视图方法能看到新值。
//     需要把某一刻的值冻结下来时用 Clone。
//
// 因此本类型无锁，也不会与 Context 的写锁互等：写入侧一律先拷贝再原子发布
// Context.mutateHeaders），已发布的映射永不再被改写。
//
// 它刻意不提供任何写入方法：下游拿到视图就无法改上下文里的 headers，改动一律经
// Context.SetHeader / AddHeader / DeleteHeader，改动记录因此不会漏。
// 取切片的方法（Values）返回副本，避免下游改到内部切片。
type HeaderView struct {
	slot *atomic.Pointer[http.Header]
}

// published 取当前已发布的 headers 映射；槽位为空时返回 nil。
//
// 已发布的映射不再被改写，所以这里可以无锁读取。零值 HeaderView（slot 为 nil）
// 按「空 headers」处理，不 panic。
func (v HeaderView) published() http.Header {
	if v.slot == nil {
		return nil
	}
	if header := v.slot.Load(); header != nil {
		return *header
	}
	return nil
}

// Get 返回首个值，不存在时为空串。
func (v HeaderView) Get(key string) string { return v.published().Get(key) }

// Values 返回 key 的全部值（副本）。
func (v HeaderView) Values(key string) []string {
	values := v.published().Values(key)
	if values == nil {
		return nil
	}
	copied := make([]string, len(values))
	copy(copied, values)
	return copied
}

// Has 报告 key 是否存在（含空值项）。
func (v HeaderView) Has(key string) bool {
	_, ok := v.published()[http.CanonicalHeaderKey(key)]
	return ok
}

// Len 是 header 键的个数。
func (v HeaderView) Len() int { return len(v.published()) }

// Keys 返回排序后的键列表，顺序稳定，便于断言与日志。
func (v HeaderView) Keys() []string {
	header := v.published()
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Each 按排序后的键顺序遍历；fn 返回 false 时提前结束。
//
// 传入的键与值都是副本，fn 内改写它们不影响上下文。遍历以实时视图为准：每次取键与
// 取值都是一次独立读取，期间发生的写入会在其后生效（例如某键被删除后取到空值）。
func (v HeaderView) Each(fn func(key string, values []string) bool) {
	for _, key := range v.Keys() {
		if !fn(key, v.Values(key)) {
			return
		}
	}
}

// Clone 返回可自由改写的副本。
//
// 副本是调用那一刻的快照：之后上下文再写 headers，副本不再变化。
// 需要写 headers 时改副本，改副本不影响上下文——这是把「只读」落到类型上而非约定上。
func (v HeaderView) Clone() http.Header { return cloneHeader(v.published()) }
