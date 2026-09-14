package limit

import (
	"context"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// SessionSource 提供本次请求已分配的物理会话 id。
//
// 会话绑定步骤（guard.SessionBinder.Ensure）把结果写在 pctx 之外由实现自持，故这里要的是
// 一个「按请求取值」的函数，而不是 pctx 上的槽位；返回空串表示本次请求没有会话身份，
// check 会按 cfg.SessionID 的兜底语义现场生成并留痕。
type SessionSource func(*pctx.Context) (string, error)

// sessionBound 是限流服务的按请求视图：窗口、并发记账与认证节流三份状态都与进程级实例共享
// （它们的键里带 key/user id，按请求分身在语义上是同一份），只有会话 id 的取值换成
// 调用方给的来源。
//
// 为什么不做成共享字段：会话 id 是每请求事实，写进进程级实例会串号（同 sessionCapture
// 的理由）。视图是轻量值拷贝，每请求构造一次。
type sessionBound struct {
	service *Service
	source  SessionSource
}

// Throttle 实现 guard.RateLimiter：认证节流在鉴权步骤发生，与会话 id 无关。
func (b sessionBound) Throttle(
	ctx context.Context,
	clientIP string,
	candidateKey string,
) (guard.ThrottleDecision, error) {
	return b.service.Throttle(ctx, clientIP, candidateKey)
}

// RecordAuthSuccess 实现 guard.RateLimiter。
func (b sessionBound) RecordAuthSuccess(ctx context.Context, clientIP string, candidateKey string) {
	b.service.RecordAuthSuccess(ctx, clientIP, candidateKey)
}

// RecordAuthFailure 实现 guard.RateLimiter。
func (b sessionBound) RecordAuthFailure(ctx context.Context, clientIP string, candidateKey string) {
	b.service.RecordAuthFailure(ctx, clientIP, candidateKey)
}

// Check 实现 guard.RateLimiter：优先用调用方给的会话来源，来源缺失或未给出 id 时退回服务自身的
// 取值链（cfg.SessionID 或现场生成 + warn）。
//
// 退回而不是短路成空串是必须的：空串会被当成一个合法会话 id 记进 Redis（于是同一个无会话身份的
// 请求彼此看起来像“同一会话”），而退回至少还能得到「现场生成 + 留痕」的语义，与接线前一致。
func (b sessionBound) Check(ctx context.Context, req *pctx.Context) (*guard.RateLimitBlock, error) {
	return b.service.check(ctx, req, func() (string, error) {
		if b.source != nil {
			id, err := b.source(req)
			if err != nil {
				return "", err
			}
			if id != "" {
				return id, nil
			}
		}
		return b.service.sessionID(req)
	})
}

// WithSession 返回绑定会话来源的视图，供接线方在**每请求装配处**注入。
//
// 复刻守卫链里 Provider/Message 的 WithBody 手法：进程级共享实例保持不变，每请求注入一个
// 携带该请求事实的视图。source 为 nil 时行为等价于原服务本身（退回现场生成 + warn）。
func (s *Service) WithSession(source SessionSource) guard.RateLimiter {
	if s == nil {
		return nil
	}
	return sessionBound{service: s, source: source}
}
