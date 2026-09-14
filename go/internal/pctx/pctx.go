// Package pctx 承载一次代理请求的上下文与契约，对应 src/app/v1/_lib/proxy/session.ts 的职责切面。
//
// 三条不变量：
//  1. 归属在入口一次性决定。SetOwner 只允许成功一次；再次设置返回错误并记日志，绝不静默覆盖。
//     理由：message_request 没有幂等键，两侧同时结算必然错账
//
// 探针 3 的「后写全胜且产生混合终态」）。
// 同一理由适用于 SetMessageRequestID：一行请求只允许开一次。
//  2. 正文只被消费一次。TakeBody 成功后再取返回可判别错误，用于防止同一份字节被多处重复驻留。
//  3. 本包只做上下文与契约，不做业务。选路、限流、结算的判定都在各自包里，本包只提供槽位。
//
// 本包不依赖数据库、Redis 与任何出站网络客户端，只依赖标准库与 internal/logx。
package pctx

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 可判别错误。调用方一律用 errors.Is 判定，不要比较文案。
var (
	// ErrInvalidInit 表示初始化参数非法。
	ErrInvalidInit = errors.New("pctx: 初始化参数非法")
	// ErrUnknownOwner 表示归属取值不在已知集合内。
	ErrUnknownOwner = errors.New("pctx: 归属取值非法")
	// ErrOwnerAlreadySet 表示归属已经决定，不允许改判。
	ErrOwnerAlreadySet = errors.New("pctx: 归属已决定，不可改判")
	// ErrBodyAlreadyTaken 表示正文已经被消费过。
	ErrBodyAlreadyTaken = errors.New("pctx: 正文已被消费")
	// ErrNoBody 表示本次请求没有正文。
	ErrNoBody = errors.New("pctx: 本次请求没有正文")
	// ErrInvalidMessageRequestID 表示传入的日志行标识不是合法行 id。
	ErrInvalidMessageRequestID = errors.New("pctx: 请求日志行标识非法")
	// ErrMessageRequestIDAlreadySet 表示日志行标识已经写入，不允许改写。
	ErrMessageRequestIDAlreadySet = errors.New("pctx: 请求日志行标识已写入，不可改写")
)

// AuthState 是 API Key 鉴权结果槽位。
//
// APIKey 是上游拨号与限流键所必需的原文，**绝不允许进入日志、错误文案或调试工件**。
type AuthState struct {
	KeyID  int64
	UserID int64
	// UserName 与 KeyName 是**展示用**身份（管理面的会话详情写的就是这两个名字）。
	UserName string
	KeyName  string
	APIKey   string
}

// ProviderSelection 是选路结果槽位。
//
// 字段保持最小充分：扩展由选路包按自身需要追加，本包不推导业务含义。
type ProviderSelection struct {
	ProviderID int64
	Name       string
	// Type 是供应商类型（对应 TS 侧 ProviderType）。
	Type string
	// Endpoint 是选定的上游端点基址。
	Endpoint string
}

// Settlement 是终态结算的入账结果。
//
// 它是进程内的「只结算一次」断言，不替代数据库侧的终态幂等谓词（store 的 IfUnfinalized）。
type Settlement struct {
	StatusCode int
	Success    bool
	At         time.Time
}

// Init 是构造上下文所需的入口事实。它刻意不含正文内容：构造期绝不读体。
type Init struct {
	// Method 是 HTTP 方法；大小写不敏感，内部归一为大写。
	Method string
	// Path 是请求路径；为空时归一为 "/"。
	Path string
	// Query 是客户端原始查询串（不含 "?"）；拼上游 URL 时原样透传。
	Query string
	// Headers 是入口 headers；构造时做深拷贝，调用方之后改动自己的 map 不影响上下文。
	Headers http.Header
	// Body 是请求正文读取器；延迟到 TakeBody 才暴露，构造期不读。
	Body io.ReadCloser
	// ClientIP 是入口解析出的客户端 IP（解析规则见 ip_extraction_config）。
	ClientIP string
	// ProtocolFrom 是客户端侧协议族；入口判不出时留空。
	ProtocolFrom egress.Family
	// PersistDebugArtifacts 是调试工件开关的初值；零值为不落盘（生产默认）。
	PersistDebugArtifacts bool
	// Logger 是日志器；为空时写 stderr。
	Logger *logx.Logger
	// Now 是可注入时钟；为空时用 time.Now。
	Now func() time.Time
}

// Context 是一次代理请求的上下文，可被同一条请求的多个 goroutine 并发使用。
type Context struct {
	mu sync.Mutex

	startedAt time.Time
	method    string
	path      string
	// query 是客户端原始查询串（不含 "?"）。
	//
	// 为何要它：拼上游 URL 时必须连查询串一起带上（Node 用的是整个 requestUrl）。
	// Gemini 的 `?alt=sse` 就是上游「回 SSE 还是 JSON 数组」的开关，丢了它请求不会报错，
	// 只会静默拿到另一种响应形状。
	query string
	// live / original 存的是「原子发布的映射指针」：写入侧先拷贝再替换，读取侧
	// （HeaderView）因此可以无锁读任何已发布的映射，见 mutateHeaders。
	live         atomic.Pointer[http.Header]
	original     atomic.Pointer[http.Header]
	body         io.ReadCloser
	bodyTaken    bool
	clientIP     string
	protocolFrom egress.Family

	auth       *AuthState
	provider   *ProviderSelection
	settlement *Settlement
	// affinity 是亲和终态写回能力（见 affinity.go）；nil 表示本次不写亲和。
	affinity AffinityWriteback
	// affinityIdentity 是本次请求的亲和身份事实（仅两个字符串，不是 route 的类型）：
	// 请求日志的 session_identity_kind 靠它判定「前缀亲和」还是「客户端会话」。
	affinityIdentity    AffinityIdentity
	affinityIdentitySet bool

	// selectionChainEntry 是选择期 provider_chain 条目（链首）的原样快照，见 selection_chain.go。
	selectionChainEntry []byte

	messageRequestID    int64
	messageRequestIDSet bool

	owner    egress.Owner
	ownerSet bool

	persistDebug bool

	log *logx.Logger
	now func() time.Time
}

// New 构造上下文。
//
// 构造期只做两件与正文无关的事：归一 method/path、深拷贝 headers。正文读取器原样保存，
// 直到 TakeBody 才交给调用方——这是「不在构造时读体」这条纪律的落点。
func New(init Init) (*Context, error) {
	method := strings.ToUpper(strings.TrimSpace(init.Method))
	if method == "" {
		return nil, fmt.Errorf("%w: method 不能为空", ErrInvalidInit)
	}
	path := init.Path
	if path == "" {
		path = "/"
	}
	query := strings.TrimPrefix(strings.TrimSpace(init.Query), "?")

	logger := init.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	now := init.Now
	if now == nil {
		now = time.Now
	}

	live := cloneHeader(init.Headers)
	original := cloneHeader(live)

	ctx := &Context{
		startedAt:    now(),
		method:       method,
		path:         path,
		query:        query,
		body:         init.Body,
		clientIP:     init.ClientIP,
		protocolFrom: init.ProtocolFrom,
		persistDebug: init.PersistDebugArtifacts,
		log:          logger.With(map[string]any{"method": method, "path": path}),
		now:          now,
	}
	ctx.live.Store(&live)
	ctx.original.Store(&original)
	return ctx, nil
}

// cloneHeader 深拷贝 headers，使上下文的内部映射与调用方的 map 互不影响。
func cloneHeader(header http.Header) http.Header {
	cloned := make(http.Header, len(header))
	for key, values := range header {
		copied := make([]string, len(values))
		copy(copied, values)
		cloned[key] = copied
	}
	return cloned
}

// StartedAt 是上下文建立时刻。
func (c *Context) StartedAt() time.Time { return c.startedAt }

// Method 是归一为大写的 HTTP 方法。
func (c *Context) Method() string { return c.method }

// Path 是请求路径（原样保留查询串之外的部分，不做归一化）。
func (c *Context) Path() string { return c.path }

// Query 是客户端原始查询串（不含 "?"；无查询串时为空串）。
func (c *Context) Query() string { return c.query }

// Headers 返回当前 headers 的实时只读视图。
//
// 视图反映之后发生的写入；需要把某一刻的值冻结下来时用 HeaderView.Clone。
func (c *Context) Headers() HeaderView { return HeaderView{slot: &c.live} }

// OriginalHeaders 返回入口原始 headers 的只读视图，用于改动检测与审计。
//
// 入口值在构造期发布一次，之后再不改写，因此该视图内容恒定。
func (c *Context) OriginalHeaders() HeaderView { return HeaderView{slot: &c.original} }

// mutateHeaders 以 copy-on-write 方式改 headers 并原子发布新版本。
//
// 为什么拷贝而不原地改：读取侧（HeaderView）无锁，必须保证任何已发布的映射在被读到
// 期间不被改写。header 写入远少于读取（只在请求过滤器里改几处，头数在几十量级），
// 每次拷一份换读取侧无锁是划算的。
//
// 调用方必须已持有 c.mu：写锁保证同一时刻只有一个写者，发布顺序与写入顺序一致。
func (c *Context) mutateHeaders(apply func(http.Header)) {
	next := cloneHeader(*c.live.Load())
	apply(next)
	c.live.Store(&next)
}

// SetHeader 覆盖写入一个 header。
//
// 所有写入都经本方法：上下文才能在写入侧统一记录改动，只读视图因此可以不放任何写入口。
func (c *Context) SetHeader(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mutateHeaders(func(headers http.Header) { headers.Set(key, value) })
}

// AddHeader 追加一个 header 值，保留已有值。
func (c *Context) AddHeader(key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mutateHeaders(func(headers http.Header) { headers.Add(key, value) })
}

// DeleteHeader 删除一个 header。
func (c *Context) DeleteHeader(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mutateHeaders(func(headers http.Header) { headers.Del(key) })
}

// IsHeaderModified 报告某个 header 相对入口原始值是否已改动。
//
// 值被改写、被删除、或从不存在变为存在，三者都算改动。
func (c *Context) IsHeaderModified(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !equalValues(c.original.Load().Values(key), c.live.Load().Values(key))
}

func equalValues(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// HasBody 报告本次请求是否带正文，以及正文是否尚未被消费。
func (c *Context) HasBody() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body != nil && !c.bodyTaken
}

// TakeBody 交出正文读取器，且只允许成功一次。
//
// 一次性语义是「同一份字节只有一处持有者」的强制手段：第二个调用方拿到的是
// ErrBodyAlreadyTaken，而不是又一份可独立推进的读取器。
func (c *Context) TakeBody() (io.ReadCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.body == nil {
		return nil, ErrNoBody
	}
	if c.bodyTaken {
		return nil, ErrBodyAlreadyTaken
	}
	c.bodyTaken = true
	return c.body, nil
}

// ClientIP 是入口解析出的客户端 IP，未解析时为空串。
func (c *Context) ClientIP() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.clientIP
}

// SetClientIP 写入客户端 IP。
func (c *Context) SetClientIP(ip string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clientIP = ip
}

// ProtocolFrom 是客户端侧协议族，未判定时为空。
func (c *Context) ProtocolFrom() egress.Family {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocolFrom
}

// SetProtocolFrom 写入客户端侧协议族。
func (c *Context) SetProtocolFrom(family egress.Family) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.protocolFrom = family
}

// SetAuth 写入鉴权结果。
func (c *Context) SetAuth(state AuthState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copied := state
	c.auth = &copied
}

// Auth 读取鉴权结果；未鉴权时返回 false。
func (c *Context) Auth() (AuthState, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.auth == nil {
		return AuthState{}, false
	}
	return *c.auth, true
}

// SetProvider 写入选路结果。
func (c *Context) SetProvider(selection ProviderSelection) {
	c.mu.Lock()
	defer c.mu.Unlock()
	copied := selection
	c.provider = &copied
}

// Provider 读取选路结果；未选路时返回 false。
func (c *Context) Provider() (ProviderSelection, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.provider == nil {
		return ProviderSelection{}, false
	}
	return *c.provider, true
}

// SetMessageRequestID 写入本次请求的 message_request 行标识，只允许成功一次。
//
// 为什么是一次性：一条请求只允许开一行（开行方是守卫链的 messageContext 步骤），
// 而该行没有幂等键——第二处开行会产生两行、两套账。第二次写入（无论取值是否相同）
// 返回 ErrMessageRequestIDAlreadySet，让重复开行在调用点就炸掉，而不是静默错账。
func (c *Context) SetMessageRequestID(id int64) error {
	if id <= 0 {
		return fmt.Errorf("%w: %d", ErrInvalidMessageRequestID, id)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.messageRequestIDSet {
		err := fmt.Errorf("%w: 现有 %d，请求写入 %d", ErrMessageRequestIDAlreadySet, c.messageRequestID, id)
		c.log.Warn("pctx.message_request_id.rejected", map[string]any{
			"existing": c.messageRequestID,
			"attempt":  id,
		})
		return err
	}
	c.messageRequestID = id
	c.messageRequestIDSet = true
	return nil
}

// MessageRequestID 读取本次请求的日志行标识；未开行时返回 false。
func (c *Context) MessageRequestID() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.messageRequestID, c.messageRequestIDSet
}

// SetOwner 写入归属标记，只允许成功一次。
//
// 第二次调用（无论取值是否相同）返回 ErrOwnerAlreadySet 并记一条 warn：静默覆盖会让
// 「谁结算」这件事在两个调度器之间漂移，是错账的直接来源。
func (c *Context) SetOwner(owner egress.Owner) error {
	if owner != egress.OwnerGo && owner != egress.OwnerNode {
		return fmt.Errorf("%w: %q", ErrUnknownOwner, owner)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownerSet {
		err := fmt.Errorf("%w: 现有归属 %q，请求改判为 %q", ErrOwnerAlreadySet, c.owner, owner)
		c.log.Warn("pctx.owner.rejected", map[string]any{
			"existing": string(c.owner),
			"attempt":  string(owner),
		})
		return err
	}
	c.owner = owner
	c.ownerSet = true
	return nil
}

// Owner 读取归属标记；未决定时返回 false。
func (c *Context) Owner() (egress.Owner, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.ownerSet {
		return "", false
	}
	return c.owner, true
}

// MarkSettled 记录终态结算结果，返回是否本次调用完成了结算。
//
// 已结算时返回 false 且不改动既有结果：重复结算在数据库侧由 store 的终态谓词兜底，
// 这里是同一条请求内的第一道闸。
func (c *Context) MarkSettled(settlement Settlement) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.settlement != nil {
		return false
	}
	if settlement.At.IsZero() {
		settlement.At = c.now()
	}
	copied := settlement
	c.settlement = &copied
	return true
}

// Settlement 读取结算结果；未结算时返回 false。
func (c *Context) Settlement() (Settlement, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.settlement == nil {
		return Settlement{}, false
	}
	return *c.settlement, true
}

// SetPersistDebugArtifacts 设置调试工件开关。
func (c *Context) SetPersistDebugArtifacts(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persistDebug = enabled
}

// ShouldPersistDebugArtifacts 报告是否允许持久化调试工件。
//
// 默认关闭：高并发模式下调试工件（请求正文快照、逐帧记录）是每流内存的主要放大器，
// 对应 TS 侧 shouldPersistSessionDebugArtifacts 的取反语义。
func (c *Context) ShouldPersistDebugArtifacts() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persistDebug
}
