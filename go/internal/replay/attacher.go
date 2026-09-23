package replay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 服务分页与停滞判定（对齐 replay-guard.ts）。
const (
	attachServeBatchChunks = 64
	attachStallMS          = 30 * 1000
)

// 重放响应里排除的头（与 replay-headers.ts 的集合一致）：这些头在缓存响应里
// 没有承载语义，恢复时剔除。
var replayExcludedHeaders = map[string]bool{
	"connection":          true,
	"content-encoding":    true,
	"content-length":      true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"set-cookie":          true,
	"set-cookie2":         true,
	"te":                  true,
	"trailer":             true,
	"transfer-encoding":   true,
	"upgrade":             true,
}

// Claim 是本次请求成为 owner 的凭证，供转发层创建 spool。
type Claim struct {
	ID         Identity
	OwnerToken string
}

// AttacherOptions 是 NewAttacher 的入参。
type AttacherOptions struct {
	// Store 是共享的双层存储。
	Store *Store
	// Pools 用于命中的审计行写入（is_replay 标记）；nil 时跳过审计。
	Pools *store.Pools
	// ReplayEnabled 对应 ENABLE_REQUEST_REPLAY。
	ReplayEnabled bool
	// Now 是可注入时钟；nil 用 time.Now。
	Now func() time.Time
	// MessageFromRequest 取本请求的逻辑请求体（requestFilter 过滤后的 JSON）。
	// nil 视为接线缺口：Attach 恒不命中。
	MessageFromRequest func(ctx context.Context, req *pctx.Context) ([]byte, bool)
	// FormatOfRequest 取客户端协议格式（anthropic-messages / openai-chat / ...）。
	FormatOfRequest func(ctx context.Context, req *pctx.Context) string
	// ClaimHook 在成功 claim owner 且原子准备完成后调用（供转发层建 spool）。
	// nil 时只保留租约（TTL 兜底回收）。
	ClaimHook func(req *pctx.Context, claim Claim)
}

// Attacher 是守卫链的 ReplayAttacher 实现（guard-pipeline.ts 的 replayAttach 步骤语义）。
//
// 免费语义不变：命中重放不占限流配额、不计费，但 auth/sensitive 等前置校验一律先行。
// 一切异常 fail-open：返回 nil 让请求照常执行。
type Attacher struct {
	store *Store
	pools *store.Pools
	opts  AttacherOptions
	now   func() time.Time
}

// NewAttacher 构造命中器。
func NewAttacher(opts AttacherOptions) *Attacher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Attacher{store: opts.Store, pools: opts.Pools, opts: opts, now: opts.Now}
}

// Attach 实现 guard.ReplayAttacher。
func (a *Attacher) Attach(ctx context.Context, req *pctx.Context) (*guard.Response, error) {
	if !a.opts.ReplayEnabled || a.opts.MessageFromRequest == nil || a.store == nil || req == nil {
		return nil, nil
	}
	message, ok := a.opts.MessageFromRequest(ctx, req)
	if !ok {
		return nil, nil
	}
	format := ""
	if a.opts.FormatOfRequest != nil {
		format = a.opts.FormatOfRequest(ctx, req)
	}
	identity, err := DeriveIdentity(req, message, format)
	if err != nil || identity == nil {
		return nil, nil
	}
	bypassAttach := req.Headers().Get(BypassHeader) == "1"

	meta, err := a.store.GetMeta(ctx, identity.ReplayID)
	if err != nil {
		return nil, nil // Redis 不可用：fail-open。
	}
	if bypassAttach {
		// 有意重复采样：跳过 attach，但已完成条目不可被覆写。
		if meta != nil && meta.Status == MetaCompleted && meta.Verifier == identity.Verifier {
			return nil, nil
		}
	} else if response := a.tryServeRedis(ctx, req, identity, meta); response != nil {
		return response, nil
	}

	// 未命中可服务条目：先抢跨副本 owner，再查一次 PG。旧 owner 必须先完成 PG 写入
	// 才释放租约，因此该顺序既消除「PG miss 后旧 owner 才提交」的竞态，也让每个
	// miss 至多做一次 PG 查询。
	ownerToken := newOwnerToken()
	claimed := a.store.TryClaimOwner(ctx, identity.ReplayID, ownerToken)
	if !claimed && meta != nil && meta.Status == MetaOwning {
		return nil, nil
	}

	persisted, err := a.store.FindCompleted(ctx, identity.ReplayID)
	if err != nil {
		if claimed {
			a.store.ReleaseOwner(ctx, identity.ReplayID, ownerToken)
		}
		return nil, nil // fail-open：不把存储故障暴露成请求失败。
	}
	if persisted != nil {
		if claimed {
			a.store.ReleaseOwner(ctx, identity.ReplayID, ownerToken)
		}
		if bypassAttach {
			return nil, nil // 已有 durable winner 仍必须受保护，不可覆写。
		}
		if persisted.Verifier != identity.Verifier || persisted.Payload == "" {
			return nil, nil
		}
		a.writeAuditRow(ctx, req, *identity, persisted.StatusCode, "pg_completed", persisted.SourceMessageRequestID)
		return a.buildCompletedResponse(persisted.StatusCode, persisted.Headers, persisted.Payload), nil
	}

	if claimed {
		// PG 查询期间租约可能已过期并被接管；清旧热层与续租受 token fencing。
		if a.store.PrepareOwned(ctx, identity.ReplayID, ownerToken) && a.opts.ClaimHook != nil {
			a.opts.ClaimHook(req, Claim{ID: *identity, OwnerToken: ownerToken})
		}
	}
	return nil, nil
}

// tryServeRedis 尝试从热层直接服务（completed 全量 / owning 跟尾）。
//
// 简化（本包有意裁剪，见 doc.go）：owning 条目不 attach——不吐半截流。完整的
// 「逐块跟尾」需要数据面侧把守卫响应换成流式 Body，属接线波次；届时在这里恢复
// meta.Status == MetaOwning 的分支即可。
func (a *Attacher) tryServeRedis(
	ctx context.Context,
	req *pctx.Context,
	identity *Identity,
	meta *Meta,
) *guard.Response {
	if meta == nil || meta.Verifier != identity.Verifier {
		// 哈希碰撞：绝不错发他人响应。
		return nil
	}
	if meta.Status != MetaCompleted {
		return nil
	}
	if meta.MessageRequestID == nil || meta.ChunkCount <= 0 {
		return nil
	}
	remaining := remainingTTLSeconds(meta.HeartbeatAt, a.store.ttlSeconds(ctx), a.now)
	if remaining <= 0 {
		return nil // 热层已到原固定到期点：交由 PG 持久层。
	}
	payload, ok := a.readAllGeneration(
		ctx, identity.ReplayID, *meta.MessageRequestID, meta.ChunkCount, meta.HeartbeatAt,
	)
	if !ok {
		return nil // 换代/不可用/截断：交由 PG 持久层兜底。
	}
	a.writeAuditRow(ctx, req, *identity, meta.StatusCode, "redis_completed", meta.MessageRequestID)
	return a.buildCompletedResponse(meta.StatusCode, meta.Headers, payload)
}

// readAllGeneration 按页读完一个 completed 代次的全部 chunk；每页按固定到期点重算剩余
// TTL（不得因慢客户端而延长 Redis 占用）；任一步失败返回 false。
func (a *Attacher) readAllGeneration(
	ctx context.Context,
	replayID string,
	messageRequestID, expected, heartbeatAt int64,
) (string, bool) {
	var parts []string
	for offset := int64(0); offset < expected; {
		max := expected - offset
		if max > attachServeBatchChunks {
			max = attachServeBatchChunks
		}
		remaining := remainingTTLSeconds(heartbeatAt, a.store.ttlSeconds(ctx), a.now)
		if remaining <= 0 {
			return "", false
		}
		chunks, outcome := a.store.ReadChunksForGeneration(
			ctx, replayID, messageRequestID, offset, max, time.Duration(remaining)*time.Second,
		)
		if outcome != ReadOK || len(chunks) == 0 || offset+int64(len(chunks)) > expected {
			return "", false
		}
		parts = append(parts, strings.Join(chunks, ""))
		offset += int64(len(chunks))
	}
	return strings.Join(parts, ""), true
}

// buildCompletedResponse 组装重放命中响应（headers 恢复 + x-cch-replay 标记）。
func (a *Attacher) buildCompletedResponse(statusCode int, stored map[string]string, payload string) *guard.Response {
	headers := http.Header{}
	for name, value := range stored {
		if !replayExcludedHeaders[name] {
			headers.Set(name, value)
		}
	}
	contentType := headers.Get("content-type")
	if contentType == "" {
		headers.Set("content-type", "text/event-stream")
		contentType = "text/event-stream"
	}
	if strings.Contains(contentType, "text/event-stream") {
		headers.Set("cache-control", "no-cache")
	}
	headers.Set("x-cch-replay", "completed")
	code := statusCode
	if code == 0 {
		code = http.StatusOK
	}
	return guard.NewResponse(code, headers, []byte(payload))
}

// writeAuditRow 写命中审计行（message_request, is_replay=true）。
//
// 未知字段（会话身份/亲和指纹/请求序号/blocked_reason 溯源）留空：store 侧数据面
// 缝隙由接线波次补齐。审计失败不影响响应（尽力而为，与 Node 一致）。
func (a *Attacher) writeAuditRow(
	ctx context.Context,
	req *pctx.Context,
	identity Identity,
	statusCode int,
	source string,
	sourceRequestID *int64,
) {
	if a.pools == nil {
		return
	}
	auth, ok := req.Auth()
	if !ok {
		return
	}
	model := stringPtr(identity.Model)
	userAgent := req.Headers().Get("user-agent")
	var replaySource *int
	if sourceRequestID != nil {
		value := int(*sourceRequestID)
		replaySource = &value
	}
	userAgentPtr := stringPtr(userAgent)
	endpoint := identity.Endpoint
	requestSequence := 0
	// 审计行只靠行标识（这里连它也不用），用窄路径开行：避免 jsonb 大列随 RETURNING 回传。
	createdID, err := a.pools.CreateMessageRequestID(ctx, store.CreateMessageRequestData{
		ProviderID:            0,
		UserID:                identity.UserID,
		Key:                   auth.APIKey,
		Model:                 model,
		CostUSD:               stringPtr("0"),
		IsReplay:              true,
		ReplaySourceRequestID: replaySource,
		RequestSequence:       &requestSequence,
		UserAgent:             userAgentPtr,
		Endpoint:              &endpoint,
	})
	if err != nil {
		return
	}
	a.settleAuditRow(ctx, createdID, statusCode)
}

// auditSettleTimeout 是审计行补终态的独立时限（见 settleAuditRow）。
const auditSettleTimeout = 5 * time.Second

// settleAuditRow 给刚开的审计行补终态。
//
// 为何必须补：message_request 的终态列只允许写一次（谓词 `status_code IS NULL`），建行而不落
// 终态的行会永远停在「请求中」，随后被 patrol 以 499/CLIENT_ABORTED 补写（见 patrol.RepairStatusCode）
// ——那是「客户端断开」的语义，与事实相反：本次请求是**成功**由回放存档返回的。生产实测：近 24h
// 全部未终态与 499 行里 290/306 都是这个缺口（`provider_id=0`、`is_replay=true`、无 provider_chain）。
//
// 为何用 context.WithoutCancel：客户端可能在命中返回后立刻断开，请求上下文随之取消；而「已由回放
// 服务」是已经发生的事实，终态不得因客户端离开而丢失。时限自建，与同仓 terminal/rollup.go、
// terminal/slow_rate_seam.go 的异步终态写同法。
func (a *Attacher) settleAuditRow(ctx context.Context, id int64, statusCode int) {
	if id <= 0 {
		return
	}
	code := statusCode
	if code <= 0 {
		code = http.StatusOK
	}
	duration := 0
	zero := int64(0)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditSettleTimeout)
	defer cancel()
	// 失败即放弃：这是一行审计，不值得让回放命中失败（与建行同为尽力而为）。
	_, _ = a.pools.UpdateDetailsIfUnfinalized(writeCtx, id, store.DetailsPatch{
		StatusCode:   &code,
		DurationMS:   &duration,
		InputTokens:  &zero,
		OutputTokens: &zero,
	})
}

// remainingTTLSeconds 计算到固定到期点的剩余秒（负值取 0）。
func remainingTTLSeconds(heartbeatAt, ttlSeconds int64, now func() time.Time) int64 {
	expiresAt := time.UnixMilli(heartbeatAt).Add(time.Duration(ttlSeconds) * time.Second)
	remaining := int64(expiresAt.Sub(now()).Seconds())
	if remaining < 0 {
		return 0
	}
	return remaining
}

var tokenRandSource = func(b []byte) error {
	_, err := rand.Read(b)
	return err
}

func newOwnerToken() string {
	buf := make([]byte, 16)
	if err := tokenRandSource(buf); err != nil {
		// 随机源不可用时仍要完成请求（fail-open），用时间戳兜底。
		return "fallback-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", "")
	}
	return hex.EncodeToString(buf)
}
