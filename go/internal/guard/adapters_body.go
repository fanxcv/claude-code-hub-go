package guard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ingress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// BodyAccessOptions 是正文访问器的构造参数。
type BodyAccessOptions struct {
	// Ingress 是解压与三重限额参数（压缩体、解压输出、在途预算）。零值时用
	// ingress.DefaultOptions()：默认上限与 Node 一致，但**不做在途准入**——在途准入
	// 归入口，调用方通常应传入带 Decompression 限流器的参数。
	Ingress ingress.Options
	// MaxBytes 是解压后字节上限；<=0 时取 Ingress 的取值（再由 ingress 兜默认）。
	MaxBytes int64
	// Admission 是进程级正文准入（内存余量 + 在途字节）。nil 时用本包默认单例
	// defaultAdmission（按环境变量自建，仅供测试与未接线的调用方）。
	// 生产装配由 dataplane 注入唯一一份（见 assemble.go），使预算随进程配置与 GOMEMLIMIT 变化。
	Admission *ingress.Admission
}

// defaultAdmission 是**仅在未显式注入时**使用的进程级单例。
//
// 必须进程级共享：限流器按「在途总量」记账，每次读体各建一份等于没有闸门。
// 生产路径（dataplane.NewStoreBacked）恒注入自己的实例，本单例只给测试与直接调用
// NewBodyAccessor 的调用方兜底；两者都按同名环境变量构造，故兜底值不偏离配置真源。
var defaultAdmission = ingress.DefaultAdmission()

// ErrBodyTreeReleased 表示正文树已被释放（见 ReleaseTree），此后没有可用的解析结果。
//
// 为什么必须是错误而不是「重建一棵空树」：树是每请求约一份正文字节的常驻，释放它的收益全靠
// 「没有人再要它」。若释放后还静默给出一棵新树，漏掉的消费点会照常工作、收益归零，而且既无
// 错误也无日志——事后看不出来。故把这类调用钉成错误，由调用方显式处理。
var ErrBodyTreeReleased = errors.New("guard: 正文树已释放")

// BodyAccessor 是 BodyAccess 的真实实现：正文只解析一次，多处共用同一棵树。
//
// 五条不变量：
//  1. 字节只读一次：ctx.TakeBody 拿到的流读完即弃。
//  2. JSON 与 Store 返回**同一棵树**，任何一处就地修改对其它调用方可见——这正是守卫链
//     「过滤规则改正文、转发器读改后正文」所需的语义。
//  3. 未 Store 时 Bytes 返回**入站原字节**，逐字节相同——这是正确性基线。
//  4. Store 之后只在原字节上定点重写被改动的顶层成员，其余字节逐字保留（见 body_patch.go）。
//  5. ReleaseTree 之后树不可再用（JSON/Store 报错），但字节仍可读——释放的是树不是原文。
type BodyAccessor struct {
	mu       sync.Mutex
	tree     map[string]any
	released bool              // 树已释放：JSON/Store 一律报错，绝不静默重建
	original []byte            // 入站原字节：未 Store 时即出站字节，Store 后作补丁底本
	digester *bodyDigester     // 值指纹器（基线重算必须与解析期同一个 seed）
	digests  map[string]uint64 // 解析时各顶层成员的值指纹（补丁的对照基线）
	patched  []byte            // Store 之后的出站字节缓存
	dirty    bool              // 是否已被 Store 改写
}

// reservationBytes 给出正文读取前的准入申请字节数。
//
// 优先用解压后下界（无编码时即精确值），其次用 content-length（压缩体的下界）。两者都未知
// （分块且带编码的罕见情况）时申请 0：既不能用单请求解压上限顶额（它默认 100 MiB，大于进程
// 预算默认 48 MiB，会把每条请求都顶死），也不宜按压缩比臆测。这类请求仍受单请求解压上限
// 与在途解压限流器约束。
//
// 返回值是正文字节；记账成本（正文 × 放大系数）由准入层统一换算（ingress.InflightCostBytes），
// 本函数不得自行乘系数。
func reservationBytes(decodedHint, compressed int64) int64 {
	switch {
	case decodedHint > 0:
		return decodedHint
	case compressed > 0:
		return compressed
	default:
		return 0
	}
}

// NewBodyAccessor 从上下文取出请求体并解析成 JSON 对象。
//
// 正文不可解析系失败一律返回把 ErrBodyUnavailable 包在里面的错误：调用方（各守卫步骤）据此走
// 各自的 fail-open 语义，而不是把「没有正文」误当成「守卫失败」翻成 500。
// 例外是准入拒绝（容量不足）：原样返回，由 forward 映射为 503，不得 fail-open。
func NewBodyAccessor(ctx *pctx.Context, options BodyAccessOptions) (*BodyAccessor, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: 上下文为空", ErrBodyUnavailable)
	}
	ingressOptions := options.Ingress
	if ingressOptions.MaxDecodedBytes == 0 && ingressOptions.MaxCompressedBytes == 0 &&
		ingressOptions.MaxLayers == 0 && ingressOptions.Decompression == nil {
		ingressOptions = ingress.DefaultOptions()
	}
	if options.MaxBytes > 0 {
		ingressOptions.MaxDecodedBytes = options.MaxBytes
	}

	source, err := ctx.TakeBody()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyUnavailable, err)
	}
	defer source.Close()

	headers := ctx.Headers().Clone()
	contentEncoding := headers.Get("Content-Encoding")
	compressed, decodedHint, err := ingress.PeekSize(headers, ingressOptions)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyUnavailable, err)
	}

	admission := options.Admission
	if admission == nil {
		admission = defaultAdmission
	}
	lease, err := admission.AcquireBody(reservationBytes(decodedHint, compressed))
	if err != nil {
		// 容量不足原样返回（不包 ErrBodyUnavailable）：forward 依据 ingress.IsCapacityError
		// 判为本地过载 → 503，与「避免重试上游」的语义一致；包成 ErrBodyUnavailable 会被
		// 守卫当成「正文缺失」fail-open，闸门即失效。
		return nil, fmt.Errorf("guard: 正文准入被拒: %w", err)
	}
	defer lease.Release()

	reader, err := ingress.NewReader(source, contentEncoding, compressed, ingressOptions)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyUnavailable, err)
	}
	defer reader.Close()

	payload, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBodyUnavailable, err)
	}
	if len(payload) == 0 {
		// 空体：解析成空对象而不是报错，与 Node 的 `{}` 语义一致（无正文的端点也会走到守卫）。
		return &BodyAccessor{tree: map[string]any{}, original: payload}, nil
	}

	var tree map[string]any
	if err := json.Unmarshal(payload, &tree); err != nil {
		return nil, fmt.Errorf("%w: 正文不是 JSON 对象: %v", ErrBodyUnavailable, err)
	}
	if tree == nil {
		tree = map[string]any{}
	}
	digester := newBodyDigester()
	return &BodyAccessor{tree: tree, original: payload, digester: digester, digests: digester.digestBodyMembers(tree)}, nil
}

// JSON 返回解析后的正文对象（同一请求内多次调用返回同一棵树）。
func (a *BodyAccessor) JSON() (map[string]any, error) {
	if a == nil {
		return nil, ErrBodyUnavailable
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released {
		return nil, ErrBodyTreeReleased
	}
	if a.tree == nil {
		a.tree = map[string]any{}
	}
	return a.tree, nil
}

// ReleaseTree 丢弃解析后的正文树，释放一份与正文同量级的常驻（实测 1.00× 正文字节）。
//
// 只释放树：入站原字节与已物化的补丁字节都保留，故 Bytes 仍可读——回放、定点重写与
// 「按字节再读一次」都不受影响。释放后 JSON/Store 返回 ErrBodyTreeReleased，绝不静默重建
// （理由见该错误的说明）。幂等：重复调用无副作用。
func (a *BodyAccessor) ReleaseTree() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tree = nil
	a.released = true
}

// Store 写回修改后的正文。
//
// 只置脏标记，不立刻序列化：一次请求里可能被多个过滤器连续改写，每次序列化都是白做的
// 一次 O(正文)；真正要字节的时机是转发（见 Bytes）。写回后 Bytes 只重写被改动的顶层成员。
func (a *BodyAccessor) Store(body map[string]any) error {
	if a == nil {
		return ErrBodyUnavailable
	}
	if body == nil {
		return errors.New("guard: 正文不可为 nil")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.released {
		// 释放后再写回等于把树重新装回来：与 JSON 同一道闸，否则收益会被一条漏网的
		// Store 静默吃掉，而调用方看不出任何异常。
		return ErrBodyTreeReleased
	}
	a.tree = body
	a.patched = nil
	a.dirty = true
	return nil
}

// Bytes 返回当前正文的字节。
//
// 未经 Store 时逐字返回入站原字节（正确性基线）；Store 之后在原字节上定点重写被改动的
// 顶层成员，其余字节（键序、空白、转义、数字写法）一律保留。
func (a *BodyAccessor) Bytes() ([]byte, error) {
	if a == nil {
		return nil, ErrBodyUnavailable
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.patched != nil {
		return a.patched, nil
	}
	if !a.dirty {
		// 未经 Store：原字节即出站字节（含空正文的零长切片）。就地改树而不 Store 的改动
		// 对出站不可见——与旧实现同一条语义，转发器不读未写回的改动。
		if a.original == nil {
			a.original = []byte{}
		}
		return a.original, nil
	}
	if a.released {
		// 树已释放且补丁字节还没物化：无从重写。绝不拿一棵 nil 树去拼一份空的正文。
		return nil, ErrBodyTreeReleased
	}
	patched, err := a.patchLocked()
	if err != nil {
		return nil, err
	}
	a.patched = patched
	return a.patched, nil
}

// patchLocked 在原字节上定点重写被改动的顶层成员。
//
// 原文不是 JSON 对象（空正文，或 `null` 一类的异构形状）时退回整份序列化：那种场景本来就没有
// 「未改动的字节」可保。
func (a *BodyAccessor) patchLocked() ([]byte, error) {
	layout, err := scanBodyLayout(a.original)
	if err != nil {
		return appendEncodedJSON(nil, a.tree)
	}
	changed := make(map[string]bool, len(layout.members))
	rewritten := 0
	for _, span := range layout.members {
		value, exists := a.tree[span.key]
		if !exists {
			rewritten++
			continue
		}
		if before, known := a.digests[span.key]; known && a.digester.digest(value) == before {
			continue
		}
		changed[span.key] = true
		rewritten++
	}
	// 树与原文语义相同（例如过滤规则命中却没改动正文）：不复制、不重写，原字节就是答案。
	if rewritten == 0 && len(a.tree) == len(layout.members) {
		if a.original == nil {
			a.original = []byte{}
		}
		return a.original, nil
	}
	return appendPatchedObject(a.digester, make([]byte, 0, len(a.original)+64), a.original, layout, a.tree, func(key string) bool {
		return changed[key]
	})
}

// Factory 返回绑定到本访问器的工厂。
func (a *BodyAccessor) Factory() BodyFactory {
	return func(*pctx.Context) (BodyAccess, error) {
		if a == nil {
			return nil, ErrBodyUnavailable
		}
		return a, nil
	}
}

// ContentType 是转发时应当写回的内容类型。
//
// 正文被改写后一定是不压缩的 JSON，故与 Node 一致地不带 content-encoding：该头部不是由
// 本函数回写的，而是被出站黑名单无条件剔除（见 forward 的 outboundTransportHeaderBlacklist）。
// 出站正文只有明文一种来源，留着客户端的声明就是让上游按压缩体去解一段明文。
func (a *BodyAccessor) EncodedHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return headers
}
