package replay

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// 冲刷参数（对齐 replay-spool.ts 的常量）。
const (
	flushIntervalMS          = 100
	flushBytesThreshold      = 64 * 1024
	maxRedisChunkBytes       = 64 * 1024
	durablePersistReadBatch  = 64
	maxQueuedWriteBytes      = 1024 * 1024
	ownerHeartbeatIntervalMS = 15 * 1000
	defaultMaxPayloadBytes   = int64(8 * 1024 * 1024)
	defaultMaxConcurrent     = int64(64)
)

// SpoolOptions 是 NewSpool 的入参（对应 ReplaySpoolOptions + 资源上限的可用子集）。
type SpoolOptions struct {
	// SourceMessageRequestID 是当前 owner 的请求记录 ID；live attach 用它区分同一
	// Replay ID 的不同代。0 表示未提供。
	SourceMessageRequestID int64
	// MaxPayloadBytes 是单响应缓存上限；0 用默认 8 MiB。超出即自失效（fail-open）。
	MaxPayloadBytes int64
	// MaxConcurrentSpools 是单节点并发 spool 上限；0 用默认 64。超限即放弃回放（不排队）。
	MaxConcurrentSpools int64
	// HeartbeatInterval 是 owner 租约心跳间隔；0 用默认 15s。
	// 仅用于测试注入（如验租约丢失后的停写），生产不调。
	HeartbeatInterval time.Duration
	// OnInactive 在 spool 失去活跃写角色时调用一次。
	OnInactive func()
	// OnTerminal 在 completed/aborted/disabled 清理释放后调用一次。
	OnTerminal func()
}

// spoolOp 是写入链上的一次作业；done 非 nil 时作业方须回写结果（终态路径用）。
type spoolOp struct {
	fn   func()
	done chan error
}

// Spool 是 owner 侧回放写入链：把客户端可见字节以 write-behind 方式喂入 Redis 热层。
//
// 并发模型：单一 writer goroutine 串行执行全部 Redis 写（保序、无锁竞争）；
// Observe 只做累积与唤醒，绝不同步阻塞。终态操作（CompleteAndPersist / Abort）把作业
// 排进同一串行链并等待，保证「先完成的冲刷先落盘；completed 只在 payload 与 PG 均已
// durable 之后出现」。
//
// 内存不变量：本地只保留待写批次；批次与在途字节有上界（maxQueuedWriteBytes），
// 超限即 disable，本地驻留因此不与流长度同阶增长。
type Spool struct {
	store      *Store
	identity   Identity
	ownerToken string
	statusCode int
	headers    map[string]string
	delivery   Delivery
	opts       SpoolOptions
	now        func() time.Time

	ctx    context.Context
	cancel context.CancelFunc

	mu               sync.Mutex
	disabled         bool
	terminal         bool
	aborting         bool
	released         bool
	metaWritten      bool
	inactiveNotified bool
	chunkCount       int64
	totalBytes       int64
	pending          []string
	pendingBytes     int64
	queuedBytes      int64

	// 终态清理请求（由写入链执行，保证与已入队的冲刷同序）：
	// cleanupRequested 一旦置位，写入链下一轮就执行清理并释放 spool。
	cleanupRequested bool
	cleanupDelete    bool
	cleanupReason    string

	ops        chan spoolOp
	stopCh     chan struct{}
	done       chan struct{}
	tick       *time.Ticker
	heartbeatT *time.Ticker
}

// NewSpool 创建 owner 侧 spool 并启动写入链。并发 spool 已达上限时返回 nil——
// 调用方负责释放 owner 租约（对应 createReplaySpoolIfOwner 的 declineOwnership）。
func NewSpool(
	store *Store,
	identity Identity,
	ownerToken string,
	statusCode int,
	headers map[string]string,
	delivery Delivery,
	opts SpoolOptions,
) *Spool {
	spoolCap := opts.MaxConcurrentSpools
	if spoolCap <= 0 {
		spoolCap = defaultMaxConcurrent
	}
	if !acquireSpool(spoolCap) {
		return nil
	}
	if opts.MaxPayloadBytes <= 0 {
		opts.MaxPayloadBytes = defaultMaxPayloadBytes
	}
	heartbeatInterval := opts.HeartbeatInterval
	if heartbeatInterval <= 0 {
		heartbeatInterval = ownerHeartbeatIntervalMS * time.Millisecond
	}
	now := store.now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	spool := &Spool{
		store:      store,
		identity:   identity,
		ownerToken: ownerToken,
		statusCode: statusCode,
		headers:    headers,
		delivery:   delivery,
		opts:       opts,
		now:        now,
		ctx:        ctx,
		cancel:     cancel,
		ops:        make(chan spoolOp, 64),
		stopCh:     make(chan struct{}),
		done:       make(chan struct{}),
		tick:       time.NewTicker(flushIntervalMS * time.Millisecond),
		heartbeatT: time.NewTicker(heartbeatInterval),
	}
	go spool.run()
	// bootstrap：立即建立 owning meta，供 attach 读者尽早看到状态。
	spool.enqueueBlocking(spoolOp{fn: spool.bootstrap})
	return spool
}

// run 是唯一的 Redis 写入口。
func (s *Spool) run() {
	defer close(s.done)
	for {
		select {
		case <-s.stopCh:
			s.tick.Stop()
			s.heartbeatT.Stop()
			return
		case op := <-s.ops:
			op.fn()
			if op.done != nil {
				op.done <- nil
			}
		case <-s.tick.C:
			s.maybeFlush()
			s.maybeCleanup()
		case <-s.heartbeatT.C:
			s.doHeartbeat()
		}
		// 每个作业与每个 tick 之后复查清理请求：请求不靠队列送达，故即使在 ops 满时
		// 入队被省略也不会漏掉（Node 的 writeChain 同样不会丢清理）。
		s.maybeCleanup()
	}
}

// ActiveStreams 供诊断使用：spool 是否已释放（终态清账完成）。
func (s *Spool) IsReleased() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released
}

// ChunkCount 是已被 Redis 确认的 chunk 总数（测试断言用）。
func (s *Spool) ChunkCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chunkCount
}

// HeldBytes 是本地持有的待写/在途字节（测试断言内存不变量用）。
func (s *Spool) HeldBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingBytes + s.queuedBytes
}

// Observe 在流热路径同步调用：只做累积与调度。超出单响应缓存上限即自失效。
func (s *Spool) Observe(chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	s.mu.Lock()
	if s.disabled || s.terminal {
		s.mu.Unlock()
		return
	}
	s.totalBytes += int64(len(chunk))
	if s.totalBytes > s.opts.MaxPayloadBytes {
		s.mu.Unlock()
		s.disable("payload_too_large")
		return
	}
	s.pendingBytes += int64(len(chunk))
	s.pending = appendChunks(s.pending, string(chunk), maxRedisChunkBytes)
	force := s.pendingBytes >= flushBytesThreshold
	s.mu.Unlock()
	if force {
		s.wakeWriter()
	}
}

// bootstrap 建立 owning meta（无正文批次），只执行一次。
func (s *Spool) bootstrap() {
	s.mu.Lock()
	deactivated := s.disabled || s.aborting || s.metaWritten
	s.mu.Unlock()
	if deactivated {
		return
	}
	count, outcome := s.store.WriteOwned(
		s.ctx, s.identity.ReplayID, s.ownerToken, s.buildMeta(MetaOwning), nil,
	)
	s.mu.Lock()
	if s.disabled || s.aborting {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if outcome != WriteOK {
		s.failOutcome(outcome)
		return
	}
	s.mu.Lock()
	s.chunkCount = count
	s.metaWritten = true
	s.mu.Unlock()
}

// maybeFlush 收集当前待写批次并写 Redis；无待写或已停时为空操作。
func (s *Spool) maybeFlush() {
	s.mu.Lock()
	batch, batchBytes, ok := s.collectPendingLocked()
	s.mu.Unlock()
	if !ok {
		s.disable("write_backlog_too_large")
		return
	}
	if len(batch) == 0 {
		return
	}
	s.flushBatch(batch, batchBytes)
}

func (s *Spool) flushBatch(batch []string, batchBytes int64) {
	expected := func() int64 {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.chunkCount + int64(len(batch))
	}()
	count, outcome := s.store.WriteOwned(
		s.ctx, s.identity.ReplayID, s.ownerToken,
		s.buildMeta(MetaOwning, chunkCountExtra(expected)), batch,
	)
	s.unreserve(batchBytes)

	s.mu.Lock()
	deactivated := s.disabled || s.aborting
	s.mu.Unlock()
	if deactivated {
		return
	}
	if outcome != WriteOK {
		s.failOutcome(outcome)
		return
	}
	s.mu.Lock()
	s.chunkCount = count
	s.metaWritten = true
	s.mu.Unlock()
}

// failOutcome 把非 OK 的写结果翻译成 spool 级动作。
func (s *Spool) failOutcome(outcome WriteOutcome) {
	switch outcome {
	case WriteLeaseLost:
		s.halt("owner_lease_lost")
	default:
		s.disable("redis_unavailable")
	}
}

// CompleteAndPersist 是完成屏障：尾部冲刷 -> PG 持久化 -> meta 置 completed。
//
// 顺序不变量：completed 只在 payload 与计费（由调用方先行完成）均已 durable 之后出现；
// PG 不 durable 绝不置 completed。返回的 error 供调用方记日志。
func (s *Spool) CompleteAndPersist(ctx context.Context, messageRequestID int64) error {
	s.mu.Lock()
	if s.disabled || s.terminal {
		s.mu.Unlock()
		return nil
	}
	s.terminal = true
	batch, batchBytes, ok := s.collectPendingLocked()
	s.mu.Unlock()
	if !ok {
		s.disable("write_backlog_too_large")
		return nil
	}
	done := make(chan error, 1)
	var completeErr error
	select {
	case s.ops <- spoolOp{
		fn: func() {
			completeErr = s.finalize(ctx, batch, batchBytes, messageRequestID)
		},
		done: done,
	}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("replay: spool 写入链已退出")
	}
	select {
	case <-done:
		return completeErr
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return errors.New("replay: spool 写入链已退出")
	}
}

// finalize 在写入链内执行终态序列。
func (s *Spool) finalize(
	ctx context.Context,
	batch []string,
	batchBytes int64,
	messageRequestID int64,
) error {
	defer s.release()
	defer s.unreserve(batchBytes)

	s.mu.Lock()
	deactivated := s.disabled || s.aborting
	s.mu.Unlock()
	if deactivated {
		return nil
	}

	expected := s.chunkCount + int64(len(batch))
	if len(batch) > 0 {
		count, outcome := s.store.WriteOwned(
			ctx, s.identity.ReplayID, s.ownerToken,
			s.buildMeta(MetaOwning, chunkCountExtra(expected)), batch,
		)
		s.mu.Lock()
		deactivated = s.disabled || s.aborting
		s.mu.Unlock()
		if deactivated {
			return nil
		}
		if outcome != WriteOK {
			// 尾批丢失或租约已失：热层不完整，绝不能置 completed。
			return errors.New("replay: 终态冲刷失败")
		}
		s.mu.Lock()
		s.chunkCount = count
		s.mu.Unlock()
	}

	var sourceRequestID *int64
	if messageRequestID > 0 {
		requestID := messageRequestID
		sourceRequestID = &requestID
	}
	// 重建 + 落库同处一门：两者必须与 Node 一样整体串行，否则并发收尾仍会叠加 N 倍正文。
	_, persistErr := withDurablePersistence(func() (struct{}, error) {
		payload, err := s.readAllOwned(ctx)
		if err != nil {
			return struct{}{}, err
		}
		row := PersistedRow{
			ReplayID:               s.identity.ReplayID,
			Verifier:               s.identity.Verifier,
			ScopeTag:               s.identity.ScopeTag,
			KeyID:                  s.identity.KeyID,
			UserID:                 s.identity.UserID,
			Format:                 s.identity.Format,
			Model:                  stringPtr(s.identity.Model),
			StatusCode:             s.statusCode,
			Headers:                s.headers,
			Payload:                payload,
			ByteSize:               s.totalBytes,
			SourceMessageRequestID: sourceRequestID,
			ExpiresAt:              s.now().Add(s.store.ttl),
		}
		_, err = s.store.PersistCompleted(ctx, row)
		return struct{}{}, err
	})
	if persistErr != nil {
		var conflict *DurableConflictError
		if errors.As(persistErr, &conflict) {
			// 已有的 durable winner 胜出，不写 aborted 遮蔽它。
			s.store.DiscardOwned(ctx, s.identity.ReplayID, s.ownerToken)
			return nil
		}
		s.store.AbortOwned(
			ctx, s.identity.ReplayID, s.ownerToken,
			s.buildMeta(MetaAborted, abortReasonExtra("complete_failed")),
		)
		return persistErr
	}

	completed := s.buildMeta(MetaCompleted, messageRequestExtra(messageRequestID))
	if !s.store.CompleteOwned(ctx, s.identity.ReplayID, s.ownerToken, completed) {
		// payload 已 durable，仅 completed 翻转失败（多因租约过期）：热层封死为 aborted
		// 仍正确，过期后由 PG 持久层继续服务。
		s.store.AbortOwned(
			ctx, s.identity.ReplayID, s.ownerToken,
			s.buildMeta(MetaAborted, abortReasonExtra("complete_failed")),
		)
		return errors.New("replay: completed meta 翻转失败")
	}
	return nil
}

// readAllOwned 分页重建最终正文；单页 join 限住 Redis 单次回复峰值。
func (s *Spool) readAllOwned(ctx context.Context) (string, error) {
	var pages []string
	for offset := int64(0); offset < s.chunkCount; {
		count := s.chunkCount - offset
		if count > durablePersistReadBatch {
			count = durablePersistReadBatch
		}
		chunks, outcome := s.store.ReadOwnedChunks(
			ctx, s.identity.ReplayID, s.ownerToken, offset, count,
		)
		if outcome != ReadOK || len(chunks) == 0 || offset+int64(len(chunks)) > s.chunkCount {
			return "", errors.New("replay: 终态前 chunk 不可读")
		}
		if len(chunks) == 1 {
			pages = append(pages, chunks[0])
		} else {
			pages = append(pages, strings.Join(chunks, ""))
		}
		offset += int64(len(chunks))
	}
	if len(pages) == 1 {
		return pages[0], nil
	}
	return strings.Join(pages, ""), nil
}

// Abort 终态失败：meta 置 aborted + 删块；已 aborted 的条目绝不被重放命中。
func (s *Spool) Abort(reason string) {
	s.mu.Lock()
	if s.terminal {
		s.mu.Unlock()
		return
	}
	s.terminal = true
	s.aborting = true
	s.notifyInactiveLocked()
	s.pending = nil
	s.pendingBytes = 0
	s.mu.Unlock()
	s.requestCleanup(reason, true)
}

// disable 失效并删除条目（payload 超限 / Redis 不可用 / 冲刷异常）。
func (s *Spool) disable(reason string) {
	s.teardown(reason, true)
}

// halt 所有权已失：停止 spool 但绝不删条目——新 owner 可能已在写同一 LIST。
func (s *Spool) halt(reason string) {
	s.teardown(reason, false)
}

func (s *Spool) teardown(reason string, deleteEntry bool) {
	s.mu.Lock()
	if s.disabled {
		s.mu.Unlock()
		return
	}
	s.disabled = true
	s.notifyInactiveLocked()
	s.pending = nil
	s.pendingBytes = 0
	s.mu.Unlock()
	s.requestCleanup(reason, deleteEntry)
}

// requestCleanup 登记终态清理请求。不由调用方直接入队：ops 是有界队列，热路径
// （Observe 触发 disable）与写入链自身（failOutcome 触发 halt）都可能遇到满队列，
// 而清理请求一旦丢弃就永远不释放 spool 名额（并发上限会被逐次吃掉）。
// 因此请求存在标志上，由写入链在作业/tick 边界执行；仅负责唤醒空闲的写入链。
func (s *Spool) requestCleanup(reason string, deleteEntry bool) {
	s.mu.Lock()
	if s.released || s.cleanupRequested {
		s.mu.Unlock()
		return
	}
	s.cleanupRequested = true
	s.cleanupDelete = deleteEntry
	s.cleanupReason = reason
	s.mu.Unlock()
	s.wakeWriter()
}

// maybeCleanup 在写入链内执行已登记的终态清理（与已入队的冲刷同序）。
// 清理后条目被删/租约被释放，因此不得再写回 meta 覆盖新 owner。
func (s *Spool) maybeCleanup() {
	s.mu.Lock()
	if !s.cleanupRequested || s.released {
		s.mu.Unlock()
		return
	}
	deleteEntry := s.cleanupDelete
	reason := s.cleanupReason
	s.mu.Unlock()

	if deleteEntry {
		if !s.store.AbortOwned(
			s.ctx, s.identity.ReplayID, s.ownerToken,
			s.buildMeta(MetaAborted, abortReasonExtra(reason)),
		) {
			// 租约已失 / Redis 不可用：只释放自己的租约，绝不覆盖新 owner。
			s.store.ReleaseOwner(s.ctx, s.identity.ReplayID, s.ownerToken)
		}
	} else {
		s.store.ReleaseOwner(s.ctx, s.identity.ReplayID, s.ownerToken)
	}
	s.release()
}

// doHeartbeat 续 owner 租约；失败保守视为租约已失。
func (s *Spool) doHeartbeat() {
	s.mu.Lock()
	if s.disabled || s.aborting || s.released {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if !s.store.HeartbeatOwned(
		s.ctx, s.identity.ReplayID, s.ownerToken, s.now().UnixMilli(),
	) {
		s.halt("owner_lease_lost")
	}
}

// release 归还预算并触发终态回调，幂等。
func (s *Spool) release() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	onTerminal := s.opts.OnTerminal
	s.mu.Unlock()
	s.cancel()
	close(s.stopCh)
	releaseSpool()
	if onTerminal != nil {
		onTerminal()
	}
}

// collectPendingLocked 收取当前待写批次；在途字节超限时返回 ok=false。
// 调用方持锁。返回后 pending 被清空；超限时 disable 由调用方触发。
func (s *Spool) collectPendingLocked() (batch []string, batchBytes int64, ok bool) {
	if len(s.pending) == 0 {
		return nil, 0, true
	}
	if s.queuedBytes+s.pendingBytes > maxQueuedWriteBytes {
		return nil, 0, false
	}
	batch = s.pending
	batchBytes = s.pendingBytes
	s.pending = nil
	s.pendingBytes = 0
	s.queuedBytes += batchBytes
	return batch, batchBytes, true
}

// unreserve 在批次落盘（或放弃）后归还在途字节计数。
func (s *Spool) unreserve(batchBytes int64) {
	s.mu.Lock()
	s.queuedBytes -= batchBytes
	if s.queuedBytes < 0 {
		s.queuedBytes = 0
	}
	s.mu.Unlock()
}

// wakeWriter 唤醒写入链（请求一次尽快冲刷，顺带让已登记的清理请求被看到）；
// ops 满时说明链正在跑，由 tick 兜底。
func (s *Spool) wakeWriter() {
	select {
	case s.ops <- spoolOp{fn: s.maybeFlush}:
	default:
	}
}

// enqueueBlocking 阻塞入队作业（终态路径专用；writer 退出时不会挂死）。
func (s *Spool) enqueueBlocking(op spoolOp) {
	select {
	case s.ops <- op:
	default:
		// writer 慢时等待；writer 已退出则放弃（终态兜底由调用方日志体现）。
		select {
		case s.ops <- op:
		case <-s.done:
		}
	}
}

func (s *Spool) notifyInactiveLocked() {
	if s.inactiveNotified {
		return
	}
	s.inactiveNotified = true
	onInactive := s.opts.OnInactive
	if onInactive != nil {
		onInactive()
	}
}

// buildMeta 组装当前状态的 meta。写入链与热路径 Observe 并发，故累计值一律持锁快照。
func (s *Spool) buildMeta(status MetaStatus, extras ...func(*Meta)) *Meta {
	s.mu.Lock()
	chunkCount := s.chunkCount
	totalBytes := s.totalBytes
	s.mu.Unlock()
	var model *string
	if s.identity.Model != "" {
		model = stringPtr(s.identity.Model)
	}
	meta := &Meta{
		Status:      status,
		Verifier:    s.identity.Verifier,
		ScopeTag:    s.identity.ScopeTag,
		StatusCode:  s.statusCode,
		Headers:     s.headers,
		Delivery:    s.delivery,
		Format:      s.identity.Format,
		Model:       model,
		ChunkCount:  chunkCount,
		ByteSize:    totalBytes,
		HeartbeatAt: s.now().UnixMilli(),
	}
	if s.opts.SourceMessageRequestID > 0 {
		requestID := s.opts.SourceMessageRequestID
		meta.MessageRequestID = &requestID
	}
	for _, apply := range extras {
		apply(meta)
	}
	return meta
}

func chunkCountExtra(count int64) func(*Meta) {
	return func(meta *Meta) { meta.ChunkCount = count }
}

func abortReasonExtra(reason string) func(*Meta) {
	return func(meta *Meta) { meta.AbortReason = reason }
}

func messageRequestExtra(requestID int64) func(*Meta) {
	return func(meta *Meta) {
		if requestID > 0 {
			meta.MessageRequestID = &requestID
		}
	}
}

func stringPtr(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
