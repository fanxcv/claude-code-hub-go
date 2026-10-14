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
	// defaultAdmission；装配方（W3 接入 config.MaxInflightBytes 后）应显式注入，
	// 使预算随配置与 GOMEMLIMIT 变化。
	Admission *ingress.Admission
}

// defaultAdmission 是未显式注入准入层时的进程级单例。
//
// 必须进程级共享：限流器按「在途总量」记账，每次读体各建一份等于没有闸门。
var defaultAdmission = ingress.DefaultAdmission()

// BodyAccessor 是 BodyAccess 的真实实现：正文只解析一次，多处共用同一棵树。
//
// 三条不变量：
//  1. 字节只读一次：ctx.TakeBody 拿到的流读完即弃（对流式上游透传来说，正文此时已必须
//     重写，故 Store 之后由 Bytes 重新序列化）。
//  2. JSON 与 Store 返回**同一棵树**，任何一处就地修改对其它调用方可见——这正是守卫链
//     「过滤规则改正文、转发器读改后正文」所需的语义。
//  3. 不持有第二份正文：raw 只在 Store 之后缓存最近一次序列化结果，供转发取字节。
type BodyAccessor struct {
	mu    sync.Mutex
	tree  map[string]any
	raw   []byte
	dirty bool
}

// reservationBytes 给出正文读取前的准入申请字节数。
//
// 优先用解压后下界（无编码时即精确值），其次用 content-length（压缩体的下界）。两者都未知
// （分块且带编码的罕见情况）时申请 0：既不能用单请求解压上限顶额（它默认 100 MiB，大于进程
// 预算默认 64 MiB，会把每条请求都顶死），也不宜按压缩比臆测。这类请求仍受单请求解压上限
// 与在途解压限流器约束。
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
		return &BodyAccessor{mu: sync.Mutex{}, tree: map[string]any{}, raw: payload}, nil
	}

	var tree map[string]any
	if err := json.Unmarshal(payload, &tree); err != nil {
		return nil, fmt.Errorf("%w: 正文不是 JSON 对象: %v", ErrBodyUnavailable, err)
	}
	if tree == nil {
		tree = map[string]any{}
	}
	return &BodyAccessor{mu: sync.Mutex{}, tree: tree, raw: payload}, nil
}

// JSON 返回解析后的正文对象（同一请求内多次调用返回同一棵树）。
func (a *BodyAccessor) JSON() (map[string]any, error) {
	if a == nil {
		return nil, ErrBodyUnavailable
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tree == nil {
		a.tree = map[string]any{}
	}
	return a.tree, nil
}

// Store 写回修改后的正文。
//
// 只置脏标记，不立刻序列化：一次请求里可能被多个过滤器连续改写，每次序列化都是白做的
// 一次 O(正文)；真正要字节的时机是转发（见 Bytes）。
func (a *BodyAccessor) Store(body map[string]any) error {
	if a == nil {
		return ErrBodyUnavailable
	}
	if body == nil {
		return errors.New("guard: 正文不可为 nil")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tree = body
	a.raw = nil
	a.dirty = true
	return nil
}

// Bytes 返回当前正文的字节；被 Store 改写过后重新序列化，否则返回原始字节。
func (a *BodyAccessor) Bytes() ([]byte, error) {
	if a == nil {
		return nil, ErrBodyUnavailable
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.raw != nil {
		return a.raw, nil
	}
	if !a.dirty {
		a.raw = []byte{}
		return a.raw, nil
	}
	encoded, err := json.Marshal(a.tree)
	if err != nil {
		return nil, fmt.Errorf("guard: 正文序列化失败: %w", err)
	}
	a.raw = encoded
	return a.raw, nil
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
// 正文被改写后一定是不压缩的 JSON，故与 Node 一致地去掉 content-encoding：转发器在
// Bytes 之后必须按本函数的结论覆写这两个头部，否则会把明文当压缩体发出去。
func (a *BodyAccessor) EncodedHeaders() http.Header {
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	return headers
}
