package dataplane

import (
	"context"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件把「提交后掉速」（二级闸）从采样器的回调接到低速写面上。
//
// 为何这必须是**独立**的一条闭合，而不能靠终态速率样本：终态样本算的是**整段生成窗口**的
// 平均速率（slowrate.Record → generationRate），而二级闸要抓的形态恰恰是「开头快、后段掉速」——
// 后段极慢会被前面的快稀释掉，整段均速仍可能在低速线之上，于是这条事实**根本不会被采样**。
// 二级闸量的是**掉速那一窗**的速率，两者不是同一件事，缺了这条就永远标不出这类慢家。
//
// 与「提交前判废」（forward 的 ProbeSlow → storeSettler.slowPrecommit）的关系：两条都写
// **同一套滑窗事实**（同一个 samplesKey、同一个幂等成员 = 请求行 id），故同一请求的两类事实
// 只占一格，不会重复计数。

// PostCommitSlowSink 写一条「提交后掉速」的低速事实。nil 即不处置（只落标定日志）。
type PostCommitSlowSink func(ctx context.Context, fact terminal.SlowPrecommit)

// postCommitSlowWriteTimeout 是旁路写入的上界，与 terminal 侧同类旁路同量级。
const postCommitSlowWriteTimeout = 3 * time.Second

// postCommitSlow 造一条按请求绑定的二级闸处置回调；未接线或作用域不全时返回 nil（等效未装）。
//
// 为何按请求绑定而不在装配期一次定死：低速事实的键是「渠道×模型」，模型分量必须取自**本次请求**
// （见 slowScopeFor），而汇入缝是进程级的。
//
// 为何要异步：回调由采样器在**读取 goroutine**（到达驱动）或定时器 goroutine 上调用，而写入是
// 一次 Redis 往返。同步做会让「上游慢但仍在吐字节」时反而顶住向客户端推字节的读路径——补救慢的
// 动作本身不能变成新的慢。写失败只 warn（落点实现负责），绝不影响本流收发。
func (h *Handler) postCommitSlow(state *RequestState, requestCtx context.Context) func(int64, int) {
	sink := h.options.PostCommitSlow
	if sink == nil || state == nil {
		return nil
	}
	return func(providerID int64, observedBytesPerSecond int) {
		if providerID <= 0 {
			return
		}
		scope := slowScopeFor(state, state.PC)
		// 作用域不全就不写：模型键或请求行 id 缺失时，写进去的键不是读侧要数的那个，
		// 标记会静默隐形（与 storeSettler.slowPrecommit 的同一道前置）。
		if scope.ModelKey == "" || scope.RequestID <= 0 {
			return
		}
		fact := terminal.SlowPrecommit{
			ProviderID: providerID,
			ModelKey:   scope.ModelKey,
			RequestID:  scope.RequestID,
			SessionID:  scope.SessionID,
			KeyID:      scope.KeyID,
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(requestCtx), postCommitSlowWriteTimeout)
		go func() {
			defer cancel()
			sink(writeCtx, fact)
		}()
		h.logger.Info("dataplane.post_commit_slow", map[string]any{
			"providerId":     providerID,
			"modelKey":       fact.ModelKey,
			"requestId":      fact.RequestID,
			"bytesPerSecond": observedBytesPerSecond,
		})
	}
}

// slowScopeFor 是低速事实的**作用域**统一解析处（渠道×模型键 + 请求身份）。
//
// 为何抽成自由函数：两类低速事实（终态速率样本、提交前判废）与本次新增的提交后掉速必须落到
// **同一把键**上；模型键或请求 id 任一不一致，写进的滑窗就不是读侧要数的那个，标记静默隐形——
// 这正是本仓反复出现的一类缺陷。故只留一处实现，三处共用。
func slowScopeFor(state *RequestState, pc *pctx.Context) slowScope {
	scope := slowScope{}
	if state == nil {
		return scope
	}
	scope.ModelKey = pubstatus.ResolveSuccessRateModelKey(&state.Model, nil)
	// 会话身份来自本请求的会话步骤记录（state.sessionID），密钥 id 来自鉴权槽位；
	// 未接线时保持零值，slowrate 会只做渠道级统计、不写会话冷却。
	scope.SessionID = state.sessionID
	if pc != nil {
		if auth, ok := pc.Auth(); ok {
			scope.KeyID = auth.KeyID
		}
		if id, ok := pc.MessageRequestID(); ok {
			scope.RequestID = id
		}
	}
	return scope
}
