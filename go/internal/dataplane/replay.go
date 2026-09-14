package dataplane

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/replay"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是回放子系统与数据面之间的接线：命中在守卫链里短路（replayAttach 步骤），
// 未命中则由本层把客户端可见字节逐块喂给 spool，并在**计费落库之后**才置 completed。
//
// 三条不可协商的次序（对应 Node response-handler.ts 的 F2 终态屏障）：
//
//  1. **命中短路**：命中即由守卫步直接返回缓存响应，既不拨上游也不进本层，因此重放请求
//     不占限流配额、不计费（与 Node 同为「免费语义」）。
//  2. **只 claim 不算数**：未命中的 owner 必须在响应路径上真建 spool 并逐块写入；只 claim
//     不写会在切换期留下「Node 侧看到 owning 却永远等不到正文」的悬空条目。
//  3. **completed 晚于计费**：完成屏障在终态结算之后执行（见 completeAfterSettle），
//     因此被重放命中的响应必然已带完整账本行。
//
// 内存纪律：本层只做「搬运」——字节从泵直接进 spool 的待写批次（批次上界在 replay 包），
// 本层不累积正文。任何在本文件里 join 正文的改动都会让单流驻留与流长度同阶增长
// （issue-1408 的根因）。

// replayCompleteTimeout 是终态完成（尾批冲刷 + PG 持久化 + completed 翻转）的上限。
//
// 为什么与请求上下文解耦：客户端可能刚好在终态帧之后断开，此时请求上下文已取消，
// 而「正文已经完整送达」这一事实不因客户端断开而改变；用被取消的上下文去做完成屏障
// 只会把可完成的条目退化成 owning，等 TTL 过期后由 PG 兜底（多一次 miss）。
// 有界是必须的：完成屏障跑在流终态路径上，无界会把退出序列拖住。
const replayCompleteTimeout = 10 * time.Second

// ReplayWiring 是回放接线的进程级依赖。
//
// 命中器（replay.Attacher）不在这里：它的请求体来源、格式与身份都是每请求事实
// （见 newReplaySession），进程级实例只能拿到一份过期或错位的视图。
type ReplayWiring struct {
	// Store 是回放双层存储（Redis 热层 + PG 持久层），必填。
	Store *replay.Store
	// Pools 用于命中审计行（is_replay 标记）；nil 时跳过审计。
	Pools *store.Pools
	// Enabled 解析运行时开关：system_settings.replay_enabled 优先，缺省回落
	// ENABLE_REQUEST_REPLAY（与 Node 的 `settings.replayEnabled ?? env` 同序）。
	// nil 视为关闭。
	Enabled func(ctx context.Context) bool
	// MaxPayloadBytes 是单响应缓存上限；0 用 replay 包默认（8 MiB）。
	MaxPayloadBytes int64
	// MaxConcurrentSpools 是单节点并发 spool 上限；0 用 replay 包默认（64）。
	MaxConcurrentSpools int64
	// Now 可注入时钟；nil 用数据面时钟。
	Now func() time.Time
}

// replaySession 是一次请求的回放接线视图：持有本次请求的 owner claim 与其 spool。
//
// 为什么按请求构造：身份推导要读**本请求过滤后的正文**，格式与端点也是每请求事实；
// 且 claim 只能属于一条请求，进程级共享会把 A 请求的 owner token 用到 B 请求上。
type replaySession struct {
	wiring   *ReplayWiring
	attacher *replay.Attacher
	body     *bodyAccess
	format   convert.ClientFormat
	logger   *logx.Logger

	pc    *pctx.Context
	mu    sync.Mutex
	claim *replay.Claim
	spool *replay.Spool
}

// newReplaySession 构造本请求的回放接线；未启用时返回 nil（守卫步据此跳过）。
func newReplaySession(
	ctx context.Context,
	wiring *ReplayWiring,
	pc *pctx.Context,
	body *bodyAccess,
	format convert.ClientFormat,
	logger *logx.Logger,
) *replaySession {
	if wiring == nil || wiring.Store == nil {
		return nil
	}
	if wiring.Enabled == nil || !wiring.Enabled(ctx) {
		return nil
	}
	session := &replaySession{wiring: wiring, pc: pc, body: body, format: format, logger: logger}
	session.attacher = replay.NewAttacher(replay.AttacherOptions{
		Store:         wiring.Store,
		Pools:         wiring.Pools,
		ReplayEnabled: true,
		Now:           wiring.Now,
		// 身份基准是 requestFilter 之后的逻辑正文（与 Node 的 session.request.message 同源）：
		// 过滤器规则变更后自然产生新身份，绝不命中旧规则时代的条目。
		MessageFromRequest: func(_ context.Context, _ *pctx.Context) ([]byte, bool) {
			if body == nil {
				return nil, false
			}
			payload, err := body.bytes()
			if err != nil || len(payload) == 0 {
				return nil, false
			}
			return payload, true
		},
		// 客户端协议格式：与 Node 的 session.originalFormat 同为 EndpointClientFormat 取值。
		FormatOfRequest: func(context.Context, *pctx.Context) string { return string(format) },
		ClaimHook: func(_ *pctx.Context, claim replay.Claim) {
			session.mu.Lock()
			session.claim = &claim
			session.mu.Unlock()
		},
	})
	return session
}

// Attach 实现 guard.ReplayAttacher：命中即返回缓存响应（短路），未命中放行并登记 owner。
func (s *replaySession) Attach(ctx context.Context, req *pctx.Context) (*guard.Response, error) {
	if s == nil || s.attacher == nil {
		return nil, nil
	}
	return s.attacher.Attach(ctx, req)
}

// startStream 在流已提交时建立 owner spool。
//
// 不可回放的三种情形一律**释放 owner 租约**（对应 Node 的 declineOwnership）：残留租约会把
// 同一请求的重试挡满整个 TTL，且切换期会让 Node 侧看到永远不会被写入的 owning 条目。
func (s *replaySession) startStream(ctx context.Context, statusCode int, headers http.Header) *replay.Spool {
	if s == nil {
		return nil
	}
	delivery := replay.DeliveryStream
	contentType := strings.ToLower(headers.Get("Content-Type"))
	isSSE := strings.Contains(contentType, "text/event-stream")
	if statusCode < 200 || statusCode >= 300 || !isSSE {
		s.release(ctx, "stream_delivery_ineligible")
		return nil
	}
	return s.create(ctx, statusCode, headers, delivery)
}

// startBuffered 在非流式正文已就绪时建立 owner spool（客户端请求 stream=true 而上游回答
// 非流式正文的情形，与 Node 的 buffered delivery 同）。
func (s *replaySession) startBuffered(ctx context.Context, statusCode int, headers http.Header) *replay.Spool {
	if s == nil {
		return nil
	}
	contentType := strings.ToLower(headers.Get("Content-Type"))
	if statusCode < 200 || statusCode >= 300 || strings.Contains(contentType, "text/event-stream") {
		s.release(ctx, "buffered_delivery_ineligible")
		return nil
	}
	return s.create(ctx, statusCode, headers, replay.DeliveryBuffered)
}

// create 建 spool；并发上限已满时释放租约并放弃回放（不排队，与 Node 一致）。
func (s *replaySession) create(
	ctx context.Context,
	statusCode int,
	headers http.Header,
	delivery replay.Delivery,
) *replay.Spool {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	claim := s.claim
	if claim == nil || s.spool != nil {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	var sourceRequestID int64
	if id, ok := s.messageRequestID(); ok {
		sourceRequestID = id
	}
	fallbackContentType := ""
	if delivery == replay.DeliveryStream {
		// 流式响应必须记下权威 content-type：命中重放要用它恢复 cache-control 与响应头集合。
		fallbackContentType = "text/event-stream"
	}
	captured := replay.CaptureResponseHeaders(headers, fallbackContentType)
	spool := replay.NewSpool(
		s.wiring.Store, claim.ID, claim.OwnerToken, statusCode, captured, delivery,
		replay.SpoolOptions{
			SourceMessageRequestID: sourceRequestID,
			MaxPayloadBytes:        s.wiring.MaxPayloadBytes,
			MaxConcurrentSpools:    s.wiring.MaxConcurrentSpools,
		},
	)
	if spool == nil {
		s.release(ctx, "concurrent_spool_cap")
		return nil
	}
	s.mu.Lock()
	s.spool = spool
	s.mu.Unlock()
	return spool
}

// messageRequestID 取本次请求的 message_request 行标识（Node 的 messageContext.id）。
// 它让命中端能区分同一 Replay ID 的不同代次。
func (s *replaySession) messageRequestID() (int64, bool) {
	if s == nil || s.pc == nil {
		return 0, false
	}
	return s.pc.MessageRequestID()
}

// observe 把客户端可见字节喂给 spool（热路径：只累积与调度，绝不同步阻塞）。
func (s *replaySession) observe(chunk []byte) {
	if s == nil || len(chunk) == 0 {
		return
	}
	s.mu.Lock()
	spool := s.spool
	s.mu.Unlock()
	if spool != nil {
		spool.Observe(chunk)
	}
}

// completeAfterSettle 在终态结算之后收尾本请求的回放：可重放的终态才置 completed。
//
// 完成条件与 Node 的 isReplayableSuccess 同形：终态为「干净完成」（有协议终止标记且无上游
// 错误文案）。任何失败终态都必须 abort——被 aborted 的条目绝不被命中，否则客户端会拿到
// 半截流，而半截流在重放语义里比 miss 更糟（它计费为 0 却交付了残缺内容）。
func (s *replaySession) completeAfterSettle(ctx context.Context, outcome forward.StreamOutcome) {
	if s == nil {
		return
	}
	s.mu.Lock()
	spool := s.spool
	s.mu.Unlock()
	if spool == nil {
		return
	}
	if outcome.Kind != forward.TerminalCompleted || outcome.Observation.ErrorText != "" {
		spool.Abort(replayAbortReason(outcome.Kind))
		return
	}
	var messageRequestID int64
	if id, ok := s.messageRequestID(); ok {
		messageRequestID = id
	}
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replayCompleteTimeout)
	defer cancel()
	if err := spool.CompleteAndPersist(completeCtx, messageRequestID); err != nil {
		s.logger.Debug("dataplane.replay_complete_failed", map[string]any{"error": err.Error()})
	}
}

// completeBuffered 在非流式正文已交付后收尾：正文已完整，故直接走完成屏障。
//
// 次序不变量照旧：非流式结算在 forward 内已经发生（ForwardStream 的 settleNonStream），
// 本函数因此处于计费之后。
func (s *replaySession) completeBuffered(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	spool := s.spool
	s.mu.Unlock()
	if spool == nil {
		return
	}
	var messageRequestID int64
	if id, ok := s.messageRequestID(); ok {
		messageRequestID = id
	}
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), replayCompleteTimeout)
	defer cancel()
	if err := spool.CompleteAndPersist(completeCtx, messageRequestID); err != nil {
		s.logger.Debug("dataplane.replay_complete_failed", map[string]any{"error": err.Error()})
	}
}

// release 放弃本请求的 owner 角色（不建 spool 的路径一律走到这里）。
//
// 幂等判据是「claim 是否还在手上」而不是另一个布尔位：一个请求可能连续判定两次交付类型
// （流式与非流式各有一次），第二次必须是空操作；用一个额外的 closed 位会让第一次释放后
// 的 claim 再也放不掉，残留租约就把同一请求的重试挡满整个 TTL。
func (s *replaySession) release(ctx context.Context, reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	claim := s.claim
	s.claim = nil
	s.mu.Unlock()
	if claim == nil {
		return
	}
	// 释放是尽力而为（Redis 不可用时残留租约到期自解），失败不升级为请求失败：
	// 本请求已经决定不做回放，释放失败只影响同一请求的下一次重试时机。
	s.wiring.Store.ReleaseOwner(ctx, claim.ID.ReplayID, claim.OwnerToken)
	s.logger.Debug("dataplane.replay_ownership_released", map[string]any{"reason": reason})
}

// replayAbortReason 把流终态翻成 abort 理由（写入 meta.abort_reason，供排障）。
func replayAbortReason(kind forward.TerminalKind) string {
	switch kind {
	case forward.TerminalClientAborted:
		return "client_abort"
	case forward.TerminalIdleTimeout:
		return "streaming_idle"
	case forward.TerminalUpstreamError:
		return "upstream_error"
	case forward.TerminalUpstreamTruncated:
		return "upstream_truncated"
	case forward.TerminalLocalError:
		return "local_error"
	default:
		return "non_replayable_terminal"
	}
}
