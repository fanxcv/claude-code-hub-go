package dataplane

import (
	"context"
	"encoding/json"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件是数据面的**会话观测接线**：把「请求开始 / 选中供应商 / 响应完成」三个时机翻成
// 会话写侧的调用（Node 侧对应 proxy-handler.ts:27-55、:130-160 与 forwarder.ts 的供应商
// 更新、response-handler.ts:7446 的滑动窗口刷新）。
//
// 为什么必须接线：管理面的会话列表与详情读的是这批 Redis 键（活跃 ZSET + session:{id}:info
// + 并发计数）。数据面只沉淀账本时，纯 Go 世界的会话列表恒空、并发数恒 0，而 UI 不报错。
// 写失败一律只 warn：Node 的写侧全在 try/catch 里，Redis 抖动不得把能成的代理请求打成 500。

// TelemetryFacts 是一次请求开启观测所需的事实。
type TelemetryFacts struct {
	// SessionID 是本次请求绑定的物理会话 id（来自会话绑定步骤）。
	SessionID string
	// KeyID/UserID 是活跃 ZSET 的 key/user 维度。
	KeyID  int64
	UserID int64
	// KeyName/UserName 写进会话详情 Hash，供管理面显示。
	KeyName  string
	UserName string
	// Model 是客户端请求的模型名（会话详情显示用）。
	Model string
	// APIType 取值 "chat" | "codex"（Node 按 originalFormat === "openai" 判定）。
	APIType string
	// Sequence 是会话内请求序号，决定请求工件的键。
	Sequence int
	// Artifacts 非 nil 时落请求侧调试工件；nil 表示本次不落（高并发模式或体积超限）。
	Artifacts *RequestArtifactInput
	// ClientURL/ClientMethod 是客户端请求的地址与方法（clientReqMeta 工件的内容）。
	//
	// 只判「本进程收到的是什么」（scheme 按连接是否 TLS 取，host 取 Host 头），与 Node 的
	// `new URL(c.req.url)` 同义——两者都是「服务进程视角的绝对地址」，不推断反代的原始地址。
	ClientURL    string
	ClientMethod string
}

// RequestArtifactInput 是请求侧工件的输入。
type RequestArtifactInput struct {
	// Body 是客户端请求正文（已过滤，未做格式转换）。
	Body map[string]any
	// Messages 是 messages 数组（可能存在也可能不存在）。
	Messages any
	// HasMessages 区分「没有 messages」与「messages 为 nil」。
	HasMessages bool
}

// TelemetryLease 是一次已开启的观测。Identity 为空表示本次未开启（后续调用一律跳过）。
type TelemetryLease struct {
	Identity  string
	SessionID string
	// Sequence/KeyID 是响应侧工件写盘所需的事实（键里要序号，所有者键要 keyId）。
	Sequence int
	KeyID    int64
}

// SessionTelemetry 是会话观测写侧的缝隙。nil 表示未接线（本轮只记一条 warn，不阻断请求）。
type SessionTelemetry interface {
	// Begin 在守卫链通过之后开启观测。
	Begin(ctx context.Context, facts TelemetryFacts) TelemetryLease
	// ProviderSelected 在选中供应商之后补写 providerId / providerName。
	ProviderSelected(ctx context.Context, lease TelemetryLease, providerID int64, providerName string)
	// Respond 在响应写完客户端之后落响应侧工件（正文 / 双侧头 / 上游元信息 / 相位快照）。
	//
	// 它必须在**客户端已断开**时也能写成功（详情页读的是这次请求的留痕，与客户端还在不在无关），
	// 故调用方传独立的后台上下文，与 Finish 同一纪律。
	Respond(ctx context.Context, lease TelemetryLease, artifacts ResponseArtifacts)
	// Finish 收尾：减并发计数、刷滑动窗口、写终态状态。
	//
	// 它必须能在**客户端已断开**之后仍然写成功（计数不能泄漏），故实现方不得依赖被取消的
	// 请求上下文——本包传入的是独立的后台上下文。
	Finish(ctx context.Context, lease TelemetryLease, status string)
}

// ResponseArtifacts 是响应侧工件的一次性输入（数据面采集→会话写侧落盘）。
//
// 四个相位快照由数据面按**采集点**组装（它知道请求/响应的哪一步对应哪个相位），会话写侧
// 只负责落盘与限额——把采集点埋在 store 包里会让「在哪一步采」这条纪律离开调用方视野。
type ResponseArtifacts struct {
	// StatusCode 是回给客户端的状态码（也是 upstreamResMeta 的 statusCode）。
	StatusCode int
	// UpstreamURL/UpstreamMethod 写 upstreamReqMeta；URL 由会话写侧过 sanitizeUrl。
	UpstreamURL    string
	UpstreamMethod string
	// RequestHeaders 是客户端请求头（已脱敏）；ResponseHeaders 是上游响应头（已脱敏）。
	RequestHeaders  map[string]string
	ResponseHeaders map[string]string
	// ResponseBody 是客户端可见响应的**有界捕获窗口**；nil 表示未开启捕获或没有正文。
	ResponseBody []byte
	// RequestBefore/RequestAfter/ResponseBefore/ResponseAfter 是四份相位快照。
	RequestBefore  session.SessionDetailPhaseSnapshot
	RequestAfter   session.SessionDetailPhaseSnapshot
	ResponseBefore session.SessionDetailPhaseSnapshot
	ResponseAfter  session.SessionDetailPhaseSnapshot
}

// telemetryTimeout 是收尾写入的超时上限。
//
// 收尾走独立后台上下文（客户端断开时请求上下文已取消，用它写等于放弃减计数），故必须自带
// 上限：宁可少刷一次滑动窗口，也不能让一个卡住的 Redis 拖住进程退出。
const telemetryTimeout = 3 * time.Second

// startTelemetry 在守卫链通过之后开启会话观测。
func (h *Handler) startTelemetry(
	requestCtx context.Context, state *RequestState, spec routeSpec, body *bodyAccess,
	capture *sessionCapture, clientURL string,
) TelemetryLease {
	observer := h.options.Telemetry
	if observer == nil {
		return TelemetryLease{}
	}
	facts := h.telemetryFacts(state, spec, body, capture)
	facts.ClientURL = clientURL
	facts.ClientMethod = state.PC.Method()
	if facts.SessionID == "" {
		return TelemetryLease{}
	}
	return observer.Begin(requestCtx, facts)
}

// Respond 落响应侧工件：正文、双侧头、上游元信息、四份相位快照。
//
// 全部落在**一个**有界超时里（不是每件一个）：这几条写的是同一个请求的同一批留痕，一起超时
// 比「一部写进去一部没写」更容易解释。任一条失败只 warn，绝不阻断（响应已经交给客户端了）。
func (t *sessionTelemetry) Respond(
	ctx context.Context, lease TelemetryLease, artifacts ResponseArtifacts,
) {
	if t.binder == nil || lease.Identity == "" || lease.SessionID == "" {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, session.ArtifactWriteTimeout)
	defer cancel()

	// 上游元信息：请求侧的那条不刷所有者、响应侧的那条刷（Node 的不对称，见 response_artifacts.go）。
	if artifacts.UpstreamURL != "" {
		if err := t.binder.StoreSessionUpstreamRequestMeta(
			writeCtx, lease.SessionID, lease.Sequence,
			session.SessionUpstreamRequestMeta{
				URL:    artifacts.UpstreamURL,
				Method: artifacts.UpstreamMethod,
			}); err != nil {
			t.logger.Warn("dataplane.session_upstream_req_meta_failed", map[string]any{"error": err.Error()})
		}
		if err := t.binder.StoreSessionUpstreamResponseMeta(
			writeCtx, lease.SessionID, lease.Sequence, lease.KeyID,
			session.SessionUpstreamResponseMeta{
				URL:        artifacts.UpstreamURL,
				StatusCode: artifacts.StatusCode,
			}); err != nil {
			t.logger.Warn("dataplane.session_upstream_res_meta_failed", map[string]any{"error": err.Error()})
		}
	}
	if len(artifacts.RequestHeaders) > 0 {
		if err := t.binder.StoreSessionRequestHeaders(
			writeCtx, lease.SessionID, lease.Sequence, artifacts.RequestHeaders,
		); err != nil {
			t.logger.Warn("dataplane.session_request_headers_failed", map[string]any{"error": err.Error()})
		}
	}
	if len(artifacts.ResponseHeaders) > 0 {
		if err := t.binder.StoreSessionResponseHeaders(
			writeCtx, lease.SessionID, lease.Sequence, lease.KeyID, artifacts.ResponseHeaders,
		); err != nil {
			t.logger.Warn("dataplane.session_response_headers_failed", map[string]any{"error": err.Error()})
		}
	}
	if len(artifacts.ResponseBody) > 0 {
		if err := t.binder.StoreSessionResponse(
			writeCtx, lease.SessionID, lease.Sequence, lease.KeyID, artifacts.ResponseBody, t.artifacts,
		); err != nil {
			t.logger.Warn("dataplane.session_response_body_failed", map[string]any{"error": err.Error()})
		}
	}
	t.storePhaseSnapshots(writeCtx, lease, artifacts)
}

// storePhaseSnapshots 落四份相位快照（kind × phase）。
func (t *sessionTelemetry) storePhaseSnapshots(
	ctx context.Context, lease TelemetryLease, artifacts ResponseArtifacts,
) {
	options := session.PhaseSnapshotOptions{StoreMessages: t.artifacts.StoreMessages}
	entries := []struct {
		kind     string
		phase    string
		snapshot session.SessionDetailPhaseSnapshot
	}{
		{"request", "before", artifacts.RequestBefore},
		{"request", "after", artifacts.RequestAfter},
		{"response", "before", artifacts.ResponseBefore},
		{"response", "after", artifacts.ResponseAfter},
	}
	for _, entry := range entries {
		fields := t.binder.StoreSessionPhaseSnapshot(
			ctx, lease.SessionID, lease.Sequence, lease.KeyID,
			entry.kind, entry.phase, entry.snapshot, options,
		)
		if len(fields) > 0 {
			continue
		}
		t.reportPhaseSnapshotEmpty(entry.kind, entry.phase, entry.snapshot)
	}
}

// reportPhaseSnapshotEmpty 上报「相位快照一个字段都没写进去」。
//
// 空结果有三类成因，只有前两类是真异常（见 phaseSnapshotHasContent）：
//   - Binder 未就绪（Redis 不可用）——整套会话观测都在降级，必须可见；
//   - 相位**有内容**却没写进去——写侧对超限字段是**删键**（数据丢失），写失败也返回空；
//   - 相位**没有内容**可写——设计内的正常结果：写侧只在「入参里有该字段」时才写，四个字段
//     全空时一个键都不写。客户端在建连上游之前断开就是这一类的常见来源：request.after 的
//     upstreamURL/method、response.before 的响应头、response.after 的已交付头此时都为空。
//
// 故判据是「有没有内容可写」而不是「是不是客户端中断」：中断只是「无内容」的来源之一，
// 守卫拒绝、选路失败、拨号失败同样会让后续相位无内容——按中断打闸既漏它们，也会把「中断时
// 本就有内容的相位」误当正常。无内容的一类降为 debug 并带 state 字段，真异常仍走 warn。
func (t *sessionTelemetry) reportPhaseSnapshotEmpty(
	kind, phase string, snapshot session.SessionDetailPhaseSnapshot,
) {
	state := "no_content"
	switch {
	case !t.binder.Ready():
		state = "binder_not_ready"
	case phaseSnapshotHasContent(snapshot):
		state = "content_dropped"
	}
	fields := map[string]any{"kind": kind, "phase": phase, "state": state}
	if state == "no_content" {
		t.logger.Debug("dataplane.session_phase_snapshot_empty", fields)
		return
	}
	t.logger.Warn("dataplane.session_phase_snapshot_empty", fields)
}

// phaseSnapshotHasContent 判定一份相位快照是否有内容可写。
//
// 镜像 session 包内 StoreSessionPhaseSnapshot 的四个入参判空（含 zeroSessionDetailMeta 的
// 四指针判空）。之所以镜像而不复用：那些判定是 session 包的私有实现细节，本包不得为此改其
// 导出面（同 session_binding.go 里镜像型的既有说明）。
func phaseSnapshotHasContent(snapshot session.SessionDetailPhaseSnapshot) bool {
	if snapshot.Body != nil || snapshot.HasMessages || len(snapshot.Headers) > 0 {
		return true
	}
	meta := snapshot.Meta
	return meta.ClientURL != nil || meta.UpstreamURL != nil ||
		meta.Method != nil || meta.StatusCode != nil
}

// finishTelemetry 收尾会话观测。
//
// 状态口径与 Node 逐字一致：`statusCode >= 200 && statusCode < 300 ? "completed" : "error"`，
// 不看内部成功标志。终态尚未落定时（流式路径的结算可能在包络之外完成）**不写状态**，
// 让会话停在 in_progress，而不是猜一个。
func (h *Handler) finishTelemetry(state *RequestState, lease TelemetryLease) {
	if h.options.Telemetry == nil || lease.Identity == "" {
		return
	}
	status := ""
	if settlement, ok := state.PC.Settlement(); ok {
		if settlement.StatusCode >= 200 && settlement.StatusCode < 300 {
			status = sessionStatusCompleted
		} else {
			status = sessionStatusError
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), telemetryTimeout)
	defer cancel()
	h.options.Telemetry.Finish(ctx, lease, status)
}

// 会话终态状态取值（与 Node 的 usage.status 同一组字面量）。
const (
	sessionStatusCompleted = "completed"
	sessionStatusError     = "error"
)

// telemetryFacts 从请求上下文与正文组装观测事实。
//
// 返回的 SessionID 只有在会话绑定步骤真的分配了会话时才非空：没有会话就没有会话观测量，
// 编一个 id 只会让管理面多出一堆假会话。
func (h *Handler) telemetryFacts(
	state *RequestState, spec routeSpec, body *bodyAccess, capture *sessionCapture,
) TelemetryFacts {
	facts := TelemetryFacts{Sequence: 1}
	// 会话 id 与序号来自绑定结果（按请求的记录视图，见 dataplane.go 的 sessionCapture）。
	if capture != nil {
		if bound, ok := capture.lookup(state.PC); ok {
			facts.SessionID = bound.SessionID
			if bound.Sequence > 0 {
				facts.Sequence = bound.Sequence
			}
		}
	}
	if auth, ok := state.PC.Auth(); ok {
		facts.KeyID = auth.KeyID
		facts.UserID = auth.UserID
		facts.KeyName = auth.KeyName
		facts.UserName = auth.UserName
	}
	facts.APIType = apiTypeFor(spec.Format)
	if body != nil {
		if tree, err := body.json(); err == nil {
			facts.Model = bodyModel(tree)
			messages, hasMessages := tree["messages"]
			if state.PC.ShouldPersistDebugArtifacts() {
				facts.Artifacts = &RequestArtifactInput{
					Body:        tree,
					Messages:    messages,
					HasMessages: hasMessages,
				}
			}
		}
	}
	return facts
}

// apiTypeFor 复刻 Node 的 `originalFormat === "openai" ? "codex" : "chat"`。
//
// 只有 openai-chat 记 codex：responses 与 gemini 都记 chat（Node 的判定只看 openai 一值）。
func apiTypeFor(format convert.ClientFormat) string {
	if format == convert.FormatOpenAI {
		return "codex"
	}
	return "chat"
}

// sessionTelemetry 是 SessionTelemetry 的真实实现：写侧全部落在 internal/session。
type sessionTelemetry struct {
	binder    *session.Binder
	artifacts session.SessionArtifactOptions
	logger    *logx.Logger
}

// newSessionTelemetry 组装真实会话观测写侧。
func newSessionTelemetry(
	binder *session.Binder, artifacts session.SessionArtifactOptions, logger *logx.Logger,
) *sessionTelemetry {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &sessionTelemetry{binder: binder, artifacts: artifacts, logger: logger}
}

// Begin 写活跃 ZSET、会话详情 Hash 与并发计数，并落请求侧工件。
func (t *sessionTelemetry) Begin(ctx context.Context, facts TelemetryFacts) TelemetryLease {
	if t.binder == nil {
		return TelemetryLease{}
	}
	lease := TelemetryLease{
		Identity:  session.PublicSessionIdentity(facts.SessionID, facts.KeyID),
		SessionID: facts.SessionID,
		Sequence:  facts.Sequence,
		KeyID:     facts.KeyID,
	}
	if err := t.binder.TrackSessionAndObserved(
		ctx, facts.SessionID, facts.KeyID, facts.UserID, lease.Identity,
	); err != nil {
		t.logger.Warn("dataplane.session_track_failed", map[string]any{
			"sessionId": facts.SessionID,
			"error":     err.Error(),
		})
	}
	if err := t.binder.StoreSessionInfo(ctx, lease.Identity, session.SessionInfo{
		UserName: facts.UserName,
		UserID:   facts.UserID,
		KeyID:    facts.KeyID,
		KeyName:  facts.KeyName,
		Model:    facts.Model,
		APIType:  facts.APIType,
	}); err != nil {
		t.logger.Warn("dataplane.session_info_failed", map[string]any{
			"sessionId": facts.SessionID,
			"error":     err.Error(),
		})
	}
	// 两个并发计数各写一个键：物理会话一条、观测身份一条（Node 同样两条都自增）。
	if err := t.binder.IncrementConcurrentCount(ctx, facts.SessionID); err != nil {
		t.logger.Warn("dataplane.session_count_failed", map[string]any{"error": err.Error()})
	}
	if err := t.binder.IncrementObservedConcurrentCount(ctx, lease.Identity); err != nil {
		t.logger.Warn("dataplane.observed_count_failed", map[string]any{"error": err.Error()})
	}
	t.persistRequestArtifacts(ctx, facts)
	return lease
}

// persistRequestArtifacts 落请求侧工件（受高并发开关与体积上限两道闸）。
//
// 两道闸分工：高并发模式由调用方在**组装事实时**判掉（facts.Artifacts 为 nil 即不落）；
// 体积上限在这里判——Node 的 shouldPersistSessionRequestArtifacts 用「整份正文的字节数」
// 一次性决定三条工件落不落，故这里同样按整份正文算一次，而不是逐件各判。
func (t *sessionTelemetry) persistRequestArtifacts(ctx context.Context, facts TelemetryFacts) {
	if facts.Artifacts == nil {
		return
	}
	// 工件写入自带上限：热路径不接受一次无界的 Redis 等待。
	writeCtx, cancel := context.WithTimeout(ctx, session.ArtifactWriteTimeout)
	defer cancel()

	// 客户端请求元信息（Node 的 storeSessionClientRequestMeta）只受高并发闸，**不受体积闸**：
	// 它只有 url 与 method 两个短字段，Node 把它放在 shouldPersistSessionRequestArtifacts
	// 判定之外。写在体积判定之前，超大正文的请求也保得住这条。
	if facts.ClientURL != "" {
		if err := t.binder.StoreSessionClientRequestMeta(
			writeCtx, facts.SessionID, facts.ClientURL, facts.ClientMethod, facts.Sequence,
		); err != nil {
			t.logger.Warn("dataplane.session_client_req_meta_failed", map[string]any{"error": err.Error()})
		}
	}
	if !requestArtifactWithinLimit(facts.Artifacts.Body, t.artifacts.ArtifactMaxBytes()) {
		t.logger.Warn("dataplane.session_artifact_oversized", map[string]any{
			"sessionId": facts.SessionID,
			"maxBytes":  t.artifacts.ArtifactMaxBytes(),
		})
		return
	}

	// 工件所有者键：详情面的四条读端点靠它判「这份工件是不是这个 keyId 写的」。
	//
	// 与 Node 的**登记**差异（仅时序）：Node 在响应侧的几个写入器（响应体、头、元信息、相位快照）
	// 里各自 refresh 这个键，而请求侧的 storeSessionRequestBody / storeSessionMessages 都不写它。
	// 本实现在请求侧就写一份（同样的键、同样的 TTL），因为响应侧的写入器本波尚未全部落地；
	// 读侧只看键的值与存在性，刷新时刻早一点不改变可观察结果。
	if err := t.binder.StoreSessionRequestOwner(
		writeCtx, facts.SessionID, facts.Sequence, facts.KeyID,
	); err != nil {
		t.logger.Warn("dataplane.session_artifact_owner_failed", map[string]any{"error": err.Error()})
	}
	if err := t.binder.StoreSessionRequestBody(
		writeCtx, facts.SessionID, facts.Artifacts.Body, facts.Sequence, t.artifacts,
	); err != nil {
		t.logger.Warn("dataplane.session_request_body_failed", map[string]any{"error": err.Error()})
	}
	if facts.Artifacts.HasMessages {
		if err := t.binder.StoreSessionMessages(
			writeCtx, facts.SessionID, facts.Artifacts.Messages, facts.Sequence, t.artifacts,
		); err != nil {
			t.logger.Warn("dataplane.session_messages_failed", map[string]any{"error": err.Error()})
		}
	}
}

// requestArtifactWithinLimit 复刻 shouldPersistSessionRequestArtifacts 的字节判定。
//
// 用「序列化后的字节数」而不是原始 buffer 长度：本包在守卫链之后拿不到原始读体（正文
// 已被解析与改写），序列化长度是同一份内容的等价度量。
func requestArtifactWithinLimit(body map[string]any, maxBytes int) bool {
	if body == nil {
		return true
	}
	size, err := serializedByteSize(body)
	if err != nil {
		// 序列化失败：按超限处理（宁可不落工件，也不落半份）。
		return false
	}
	return size <= maxBytes
}

// ProviderSelected 补写会话详情 Hash 的供应商字段。
func (t *sessionTelemetry) ProviderSelected(
	ctx context.Context, lease TelemetryLease, providerID int64, providerName string,
) {
	if t.binder == nil || lease.Identity == "" || providerID <= 0 {
		return
	}
	if err := t.binder.UpdateSessionProvider(ctx, lease.Identity, providerID, providerName); err != nil {
		t.logger.Warn("dataplane.session_provider_failed", map[string]any{
			"sessionId": lease.SessionID,
			"error":     err.Error(),
		})
	}
}

// Finish 减并发计数、刷滑动窗口并写终态状态。
func (t *sessionTelemetry) Finish(ctx context.Context, lease TelemetryLease, status string) {
	if t.binder == nil || lease.Identity == "" {
		return
	}
	if err := t.binder.DecrementConcurrentCount(ctx, lease.SessionID); err != nil {
		t.logger.Warn("dataplane.session_count_release_failed", map[string]any{"error": err.Error()})
	}
	if err := t.binder.DecrementObservedConcurrentCount(ctx, lease.Identity); err != nil {
		t.logger.Warn("dataplane.observed_count_release_failed", map[string]any{"error": err.Error()})
	}
	// 滑动窗口刷新与 Node 同义（response-handler.ts:7446）：响应完成后把身份再顶一次，
	// 让「刚跑完的会话」在列表里多留一个窗口，而不是随最后一个字节一起过期。
	if err := t.binder.RefreshObservedSession(ctx, lease.Identity, 0); err != nil {
		t.logger.Warn("dataplane.observed_refresh_failed", map[string]any{"error": err.Error()})
	}
	if status == "" {
		return
	}
	if err := t.binder.SetSessionStatus(ctx, lease.Identity, status); err != nil {
		t.logger.Warn("dataplane.session_status_failed", map[string]any{"error": err.Error()})
	}
}

// 编译期断言：真实实现满足缝隙。
var _ SessionTelemetry = (*sessionTelemetry)(nil)

// serializedByteSize 给出 JSON 序列化后的字节数（工件体积判定的尺子）。
func serializedByteSize(value any) (int, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	return len(encoded), nil
}
