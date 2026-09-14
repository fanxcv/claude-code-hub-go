package forward

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ingress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// Category 是错误分类，逐项对齐 errors.ts 的 ErrorCategory。
//
// 分类决定三件事：是否重试当前供应商、重试耗尽后是否切换供应商、是否计入熔断器。
type Category int

const (
	// CategoryProviderError 表示供应商问题（真实 4xx/5xx HTTP 错误、空响应、
	// 以 400 回传的存储容量故障）。计熔断器；重试当前供应商；耗尽后切换。
	CategoryProviderError Category = iota
	// CategorySystemError 表示系统/网络问题（连接失败、DNS、超时、连接被重置）。
	// 默认不计熔断器（Node 侧由 ENABLE_CIRCUIT_BREAKER_ON_NETWORK_ERRORS 决定）；
	// 重试当前供应商并推进端点索引；耗尽后切换。
	CategorySystemError
	// CategoryClientAbort 表示客户端主动中断。不重试、不切换、不计熔断。
	CategoryClientAbort
	// CategoryNonRetryableClientError 表示客户端输入错误（由错误规则命中判定）。
	// 不重试、不切换、不计熔断。
	CategoryNonRetryableClientError
	// CategoryResourceNotFound 表示上游 404。不计熔断；可重试；耗尽后切换。
	CategoryResourceNotFound
	// CategoryLocalOverload 表示本地准入过载（数据库连接池准入、入站内存准入）。
	// 不重试、不切换、不计任何熔断器——问题在本进程，惩罚供应商是错的。
	CategoryLocalOverload
)

// String 返回与 Node 侧错误分类同名的英文标识，供日志与落链使用。
//
// 跨语言约束：`CategoryNonRetryableClientError` 必须返回 `client_error_non_retryable`——
// 该词同时是链上 reason（见 chainreason.go 的词表说明）与 Node 排除词表的条目；
// 历史实现返回的 `non_retryable_client_error` 是 Go 自造词，会让消费者把它算成失败。
func (c Category) String() string {
	switch c {
	case CategoryProviderError:
		return "provider_error"
	case CategorySystemError:
		return ReasonSystemError
	case CategoryClientAbort:
		return ReasonClientAbort
	case CategoryNonRetryableClientError:
		return ReasonClientErrorNonRetryable
	case CategoryResourceNotFound:
		return ReasonResourceNotFound
	case CategoryLocalOverload:
		return "local_overload"
	default:
		return "unknown"
	}
}

// RetriesSameProvider 报告该分类是否在重试当前供应商。
func (c Category) RetriesSameProvider() bool {
	switch c {
	case CategoryProviderError, CategorySystemError, CategoryResourceNotFound:
		return true
	default:
		return false
	}
}

// SwitchesProvider 报告该分类在重试耗尽后是否切换到下一个供应商。
func (c Category) SwitchesProvider() bool {
	switch c {
	case CategoryProviderError, CategorySystemError, CategoryResourceNotFound:
		return true
	default:
		return false
	}
}

// CountsTowardCircuit 报告该分类在重试耗尽时是否计入供应商熔断器。
//
// 网络错误（CategorySystemError）在 Node 侧默认不计入，由开关决定，故此处返回 false，
// 计入与否由 Deps.CountNetworkFailureTowardCircuit 在调用点决定。
func (c Category) CountsTowardCircuit() bool {
	return c == CategoryProviderError
}

// RetryableStatusMarker 是「以 400 回传的上游存储容量故障」标记，逐条对齐
// errors.ts 的 RETRYABLE_UPSTREAM_STORAGE_ERROR_MARKERS。
var RetryableStatusMarker = []string{
	"disk storage creation failed",
	"disk free-space floor reached",
}

// RuleMatcher 判定上游错误内容是否命中错误规则。
//
// Node 侧由数据库错误规则驱动（cfgsync 的 error_rules 通道），命中即归为
// CategoryNonRetryableClientError。为 nil 时不做规则匹配——此时客户端输入错误会被
// 归为 CategoryProviderError，退化为「重试并切换」，与 Node 冷启动规则未装载时的行为一致。
type RuleMatcher interface {
	Matches(content string) bool
}

// BodyErrorDetector 判定「HTTP 200 但正文实为错误」的上游响应（Node 的 fake-200 检测）。
//
// 实体属后续波次；为 nil 时不做检测，这类响应被当作成功，缺口记录在 Result.DetectorMissing。
type BodyErrorDetector interface {
	// Detect 返回推断出的状态码、错误文案与是否命中；未命中返回 false。
	Detect(protocol string, isSSE bool, body string) (statusCode int, message string, ok bool)
}

// Failure 是一次尝试失败的完整归因，承载重试决策与落链所需的全部事实。
type Failure struct {
	// Category 是分类结果，决定重试与切换。
	Category Category
	// StatusCode 是上游 HTTP 状态码；0 表示没有上游响应（传输层失败）。
	StatusCode int
	// Message 是错误文案；来自上游正文的提取结果，或传输层错误描述。
	Message string
	// Body 是上游正文，已按 BodyLimit 截断。
	Body string
	// Synthetic 为真表示状态码由 200 正文推断而来（fake-200），不是真实传输状态。
	Synthetic bool
	// EmptyResponse 为真表示上游返回了空正文（Node 的 EmptyResponseError）。
	EmptyResponse bool
	// Internal 为真表示错误由本进程产生（计划构造失败、本地过载），没有上游参与。
	Internal bool
	// RequestScoped 为真表示失败由请求内容决定，同一请求在任何供应商上都复现。
	//
	// 它仍然 failover（客户端确实拿不到可用响应），但不计入供应商健康度：把「毒性请求」
	// 记成供应商故障，会让客户端重试把健康供应商的熔断器打开。判定口径见
	// gate.IsRequestScopedGateFailure。
	RequestScoped bool

	ProviderID   int64
	ProviderName string
	EndpointID   int64
	EndpointURL  string
	Attempt      int

	// Err 是底层错误，保留 errors.Is 链路。
	Err error
}

// Error 实现 error；文案不含密钥与请求正文。
func (f *Failure) Error() string {
	if f == nil {
		return "forward: 尝试失败"
	}
	target := f.ProviderName
	if target == "" {
		target = fmt.Sprintf("provider#%d", f.ProviderID)
	}
	if f.StatusCode > 0 {
		return fmt.Sprintf("forward: %s 返回 %d（%s，第 %d 次尝试）", target, f.StatusCode, f.Category, f.Attempt)
	}
	return fmt.Sprintf("forward: %s 失败（%s，第 %d 次尝试）：%s", target, f.Category, f.Attempt, f.Message)
}

// Unwrap 暴露底层错误。
func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Err
}

// IsLocalOverloadError 报告错误是否属于「本进程过载」——数据库连接池准入与入站内存准入。
//
// 两者都必须先于传输层判定：Node 侧注释明确写了「避免重试上游或惩罚 Provider/endpoint circuit」。
func IsLocalOverloadError(err error) bool {
	if err == nil {
		return false
	}
	var admission *store.AdmissionError
	if errors.As(err, &admission) {
		return true
	}
	return ingress.IsCapacityError(err)
}

// IsClientAbortError 报告错误是否代表客户端主动中断。
//
// 判定顺序对齐 Node：真实上游 5xx 优先于中断启发式（调用方在 Classify 里先判状态码），
// 这里只负责「取消」这一类。
func IsClientAbortError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	return errors.Is(err, dial.ErrContextCanceled)
}

// ClassifyInput 是一次失败归因所需的全部输入。
type ClassifyInput struct {
	// StatusCode 是上游 HTTP 状态码；0 表示没有上游响应。
	StatusCode int
	// Synthetic 为真表示状态码由 200 正文推断而来，不能作为权威传输状态。
	Synthetic bool
	// Err 是底层错误（传输层或本地错误）。
	Err error
	// Body 是上游正文，用于 400 存储容量标记与错误规则匹配。
	Body string
	// EmptyResponse 为真表示上游返回了空正文（Node 的 EmptyResponseError）。
	EmptyResponse bool
	// ProviderLocalModelUnavailable 为真表示上游以 404 报「本账号池不支持该模型」，
	// 属供应商局部能力缺口而非请求错误。
	ProviderLocalModelUnavailable bool
	// Rules 为 nil 时跳过错误规则匹配。
	Rules RuleMatcher
}

// Classify 按 errors.ts 的 categorizeErrorAsync 优先级链给出分类。
//
// 顺序不可调整，每一步都有明确理由（均出自 Node 侧注释）：
//  1. 真实上游 5xx 永远是供应商故障，必须先于中断与传输启发式——否则 5xx 正文里的
//     "canceled"/"timeout" 字样会把供应商故障误判成客户端行为。
//  2. 客户端中断：不重试、不切换。
//  3. 本地准入过载：问题在本进程，不重试上游也不惩罚供应商。
//  4. 传输错误：始终系统错误，不受正文内容影响。
//  5. 供应商局部模型缺口（404）：可换供应商重试，不按客户端错误处理。
//  6. 以 400 回传的存储容量故障：属供应商故障，必须先于宽泛的客户端错误规则。
//  7. 错误规则命中：客户端输入错误。
//  8. 其余 HTTP 错误：404 单列，其余都是供应商故障。
//  9. 空响应：供应商故障。
//  10. 兜底：系统错误。
func Classify(in ClassifyInput) Category {
	if in.StatusCode >= 500 && in.StatusCode < 600 && !in.Synthetic {
		return CategoryProviderError
	}
	if IsClientAbortError(in.Err) {
		return CategoryClientAbort
	}
	if IsLocalOverloadError(in.Err) {
		return CategoryLocalOverload
	}
	if in.Err != nil && isTransportError(in.Err) {
		return CategorySystemError
	}
	if in.ProviderLocalModelUnavailable {
		return CategoryResourceNotFound
	}
	if in.StatusCode == 400 && !in.Synthetic && hasStorageCapacityMarker(in.Body) {
		return CategoryProviderError
	}
	if in.Rules != nil && in.Rules.Matches(in.Body) {
		return CategoryNonRetryableClientError
	}
	if in.StatusCode > 0 {
		if in.StatusCode == 404 {
			return CategoryResourceNotFound
		}
		return CategoryProviderError
	}
	if in.EmptyResponse {
		return CategoryProviderError
	}
	return CategorySystemError
}

func hasStorageCapacityMarker(body string) bool {
	lower := strings.ToLower(body)
	if lower == "" {
		return false
	}
	for _, marker := range RetryableStatusMarker {
		if !strings.Contains(lower, marker) {
			return false
		}
	}
	return true
}

// isTransportError 报告错误是否来自传输层（拨号包的可判别分类）。
func isTransportError(err error) bool {
	for _, target := range []error{
		dial.ErrConnect,
		dial.ErrConnectTimeout,
		dial.ErrHeadersTimeout,
		dial.ErrBodyIdleTimeout,
		dial.ErrUpstreamClosed,
		dial.ErrUnsupportedUpstreamTransport,
		dial.ErrRequestBuild,
	} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
