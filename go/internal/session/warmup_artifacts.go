package session

import (
	"context"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

// WarmupArtifactAdapter 把守卫链的 warmup 工件缝隙接到本包的 Redis 写入上。
//
// 为什么需要一层适配：接口只能用 guard 包自有的类型（本包依赖 guard，反向不行），
// 这层只做类型搬运与「一次写四条」的编排，不含判断。
type WarmupArtifactAdapter struct {
	binder    *Binder
	artifacts SessionArtifactOptions
}

// NewWarmupArtifactAdapter 组装适配器。
func NewWarmupArtifactAdapter(binder *Binder, artifacts SessionArtifactOptions) *WarmupArtifactAdapter {
	if binder == nil {
		return nil
	}
	return &WarmupArtifactAdapter{binder: binder, artifacts: artifacts}
}

// StoreWarmupResponse 实现 guard.WarmupSessionArtifactWriter。
//
// 四条键与 Node `warmup-guard.ts:44-75` 逐条对应：响应正文集合、响应头、上游请求元信息、
// 上游响应元信息。任一条失败都返回错误（调用方只记 warn），但**不中断其余三条**——它们
// 是四份独立的留痕，一条写不进去不该吞掉另外三条。
func (a *WarmupArtifactAdapter) StoreWarmupResponse(
	ctx context.Context, request guard.WarmupArtifactRequest,
) error {
	if a == nil || a.binder == nil || request.SessionID == "" {
		return nil
	}
	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	record(a.binder.StoreSessionResponse(
		ctx, request.SessionID, request.Sequence, request.KeyID,
		[]byte(request.Body), a.artifacts,
	))
	record(a.binder.StoreSessionResponseHeaders(
		ctx, request.SessionID, request.Sequence, request.KeyID, request.Headers,
	))
	record(a.binder.StoreSessionUpstreamRequestMeta(
		ctx, request.SessionID, request.Sequence,
		SessionUpstreamRequestMeta{URL: guard.WarmupUpstreamMetaURL, Method: request.Method},
	))
	record(a.binder.StoreSessionUpstreamResponseMeta(
		ctx, request.SessionID, request.Sequence, request.KeyID,
		SessionUpstreamResponseMeta{URL: guard.WarmupUpstreamMetaURL, StatusCode: request.StatusCode},
	))
	return firstErr
}
