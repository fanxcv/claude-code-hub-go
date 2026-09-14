package usersreset

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件复刻 reset-queue.ts 的入队与对账（:349-460），并把 Bull 的调用面替换成 Go 侧的两件事：
// ZSET 成员是否在队（= Bull 的 getJob 是否存在）与状态记录是否终态（= Bull 的 getState）。
//
// 为什么可以这样替换：Node 那两处判断的**意图**是「认领指向的作业是否真的还会被执行」。
// Bull 里这个信息由 job 是否存在 + state 表达；Go 里同一信息由 ZSET 成员 + 状态键表达。
// 逐个搬 Bull 的 state 名字反而会引入一个 Go 侧不存在的对象（job id 与 task 的两层结构）。
//
// 保持 Node 的判定**顺序**：先看状态是否活跃、再看作业是否终态、最后看作业是否还在队。
// 顺序即语义——三支都指向「释放认领并重排」，但错误码不同（ACTIVE_STATUS_MISSING /
// ACTIVE_JOB_TERMINAL），而错误码会原样进 UI。

// jobEnvelope 是 Go 专有队列里的载荷。
//
// Attempts 刻意不写进状态记录：状态记录是跨端契约，Node 会把它 JSON.parse 后原样投影进响应，
// 多一个字段就是多一处与 Node 的响应体差异。进程重启后重试次数由队列载荷续上。
type jobEnvelope struct {
	ResetID  string `json:"resetId"`
	Attempts int    `json:"attempts"`
}

// jobData 是入队与准备阶段流转的作业参数（reset-queue.ts:8-19 的 UserStatisticsResetJobData）。
type jobData struct {
	ResetID                   string
	UserID                    int64
	RequestedAt               string
	Fixed5hKeyIDs             []int64
	Fixed5hPreparationVersion *int
}

// QueueOptions 是 Queue 的构造参数。
type QueueOptions struct {
	// Status 是状态键与认领键的读写门面，必填（nil 时所有方法返回 REDIS_UNAVAILABLE）。
	Status *StatusStore
	// Pools 提供「用户名下未软删的键 id」这一查询（准备 5h 固定窗口要用）。
	Pools *store.Pools
	// Logger 为 nil 时静默。
	Logger *logx.Logger
}

// Queue 是入队与查询的门面：管理面的两条路由只碰它，不碰 Redis 细节。
type Queue struct {
	status *StatusStore
	pools  *store.Pools
	logger *logx.Logger
}

// NewQueue 构造队列门面。
func NewQueue(options QueueOptions) *Queue {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	return &Queue{status: options.Status, pools: options.Pools, logger: logger}
}

// Available 表示队列所需的两个依赖都装配了（缺一即不可用：没有 Redis 无处落状态，没有 PG
// 取不到键清单）。
func (q *Queue) Available() bool {
	return q != nil && q.status != nil && q.status.Available() && q.pools != nil
}

// Enqueue 排一次用户统计重置并返回已入队的公开记录（reset-queue.ts:349-357）。
//
// Node 的调用方还会传 fixed5hKeyIds（它自己先查一次键清单）；这里由队列自己查
// （ensurePrepared 的键清单查询与 Node 的 findStatisticsResetKeyIds 同条件），少一次管理面到 PG 的往返。
func (q *Queue) Enqueue(ctx context.Context, userID int64) (PublicRecord, error) {
	return q.enqueueWithReconciliation(ctx, userID, isoMillis(time.Now()), true)
}

// Find 按 (用户, 作业) 查公开记录；用户不匹配时返回 nil（reset-queue.ts:462-468）。
//
// 用户不匹配当作「不存在」而不是 403：作业 id 是 uuid，跨用户猜中即等于读到别人的作业状态。
func (q *Queue) Find(ctx context.Context, userID int64, resetID string) (*PublicRecord, error) {
	record, err := q.status.Get(ctx, resetID)
	if err != nil {
		return nil, err
	}
	if record == nil || record.UserID != userID {
		return nil, nil
	}
	public := record.Public()
	return &public, nil
}

// enqueueWithReconciliation 是入队主体（reset-queue.ts:359-460）。
//
// allowReconciliation 只允许递归一次：Node 的写法也是「重排一次」，递归无上限会在
// 「认领键永远指向一个已终态的作业」时变成死循环（那个键有 7 天 TTL，代价是不可接受的）。
func (q *Queue) enqueueWithReconciliation(
	ctx context.Context,
	userID int64,
	requestedAt string,
	allowReconciliation bool,
) (PublicRecord, error) {
	if !q.Available() {
		return PublicRecord{}, ErrRedisUnavailable
	}
	if requestedAt == "" {
		requestedAt = isoMillis(time.Now())
	}
	resetID, err := newResetID()
	if err != nil {
		return PublicRecord{}, err
	}
	data := jobData{ResetID: resetID, UserID: userID, RequestedAt: requestedAt, Fixed5hKeyIDs: []int64{}}
	queued := createQueuedRecord(data)
	if err := q.status.Set(ctx, queued); err != nil {
		return PublicRecord{}, err
	}

	acquired, existingID, err := q.status.ClaimActive(ctx, userID, resetID)
	if err != nil {
		return PublicRecord{}, err
	}
	if !acquired {
		return q.reconcileActive(ctx, userID, requestedAt, resetID, existingID, allowReconciliation)
	}
	return q.enqueueClaimed(ctx, data)
}

// reconcileActive 处理「认领已被别人持有」的分支（reset-queue.ts:376-419）。
// currentID 是本次生成、但没抢到认领的那个作业 id。
//
// 比 Node 少一个条件：Node 在状态检查之后还查一次 Bull 的 job state（failed/completed 时释放认领
// 并报 ACTIVE_JOB_TERMINAL）。Go 的队列没有「作业状态机」这个独立对象——作业是否还会被执行完全
// 由状态记录（active/terminal）+ ZSET 成员表达，于是那一支被状态判定与下面的恢复支吸收。
// 被吸收的后果是 ACTIVE_JOB_TERMINAL 这个错误码在 Go 侧不会出现（见 state.go 的注释）。
func (q *Queue) reconcileActive(
	ctx context.Context,
	userID int64,
	requestedAt string,
	currentID string,
	existingID string,
	allowReconciliation bool,
) (PublicRecord, error) {
	// 先删掉本次刚写下的状态：认领没抢到，那条记录永远不会有人执行，留着只会成为
	// 「一个永远 queued 的作业」（白占 7 天 TTL）。删除失败不影响主流程，记 warn 继续。
	if err := q.status.Delete(ctx, currentID); err != nil {
		q.warn("go_usersreset_orphan_status_delete_failed", map[string]any{
			"resetId": currentID,
			"error":   err.Error(),
		})
	}

	existing, err := q.status.Get(ctx, existingID)
	if err != nil {
		return PublicRecord{}, err
	}
	if existing == nil || existing.UserID != userID || !isActiveStatus(existing.Status) {
		// 认领指向的作业已不存在或已终态：释放这个陈旧认领，然后重排一次。
		if err := q.releaseQuietly(ctx, userID, existingID); err != nil {
			return PublicRecord{}, err
		}
		return q.reconcileOrFail(ctx, userID, requestedAt, allowReconciliation, ErrCodeActiveStatusMissing)
	}
	queued, err := q.jobQueued(ctx, existingID)
	if err != nil {
		return PublicRecord{}, err
	}
	if !queued {
		// 状态活跃但队列里没有它：作业被落下了（上一轮入队后进程没了、或对端排的作业随对端下线）。
		// 走准备流程（可能补上 5h 切点）后重新入队——与 Node 的 `!existingJob` 分支逐条对应。
		recovered, err := q.ensurePrepared(ctx, dataFromRecord(*existing), *existing)
		if err != nil {
			return PublicRecord{}, err
		}
		if err := q.pushJob(ctx, recovered); err != nil {
			return PublicRecord{}, err
		}
		return recovered.Public(), nil
	}
	return existing.Public(), nil
}

// dataFromRecord 把状态记录还原成作业参数（reset-queue.ts:411-417）。
func dataFromRecord(record Record) jobData {
	return jobData{
		ResetID:                   record.ResetID,
		UserID:                    record.UserID,
		RequestedAt:               record.RequestedAt,
		Fixed5hKeyIDs:             record.Fixed5hKeyIDs,
		Fixed5hPreparationVersion: record.Fixed5hPreparationVersion,
	}
}

// reconcileOrFail 在允许对账时重排一次，否则报出给定的错误码（reset-queue.ts:391-407）。
func (q *Queue) reconcileOrFail(
	ctx context.Context,
	userID int64,
	requestedAt string,
	allowReconciliation bool,
	code string,
) (PublicRecord, error) {
	if !allowReconciliation {
		return PublicRecord{}, newError(code)
	}
	return q.enqueueWithReconciliation(ctx, userID, requestedAt, false)
}

// enqueueClaimed 处理「已抢到认领」的分支：准备 5h 固定窗口、落状态、入队
// （reset-queue.ts:421-459）。
func (q *Queue) enqueueClaimed(ctx context.Context, data jobData) (PublicRecord, error) {
	prepared, err := q.ensurePrepared(ctx, data, createQueuedRecord(data))
	if err != nil {
		return PublicRecord{}, err
	}
	if err := q.pushJob(ctx, prepared); err != nil {
		// 入队失败：状态已在 ensurePrepared 里落成 queued，认领也还在。如实报错让调用方
		// 重试或让管理员看见失败——不在这里回滚状态，因为对账路径（下一次 POST）正是
		// 靠「状态 queued + 队列里没有」这一组合来恢复的。
		q.warn("go_usersreset_enqueue_failed", map[string]any{
			"resetId": prepared.ResetID,
			"userId":  prepared.UserID,
			"error":   err.Error(),
		})
		return PublicRecord{}, err
	}
	return prepared.Public(), nil
}

// ensurePrepared 保证作业带上 5h 固定窗口切点（reset-queue.ts:71-132）。
//
// 三种情形：状态里已有准备版本号 -> 直接用；传入的作业数据带版本号 -> 写进状态；都没有 ->
// 取键清单、调 Redis 准备脚本、写回状态与切点。
func (q *Queue) ensurePrepared(ctx context.Context, data jobData, current Record) (Record, error) {
	if current.Fixed5hPreparationVersion != nil && *current.Fixed5hPreparationVersion == 1 {
		return current, nil
	}
	if data.Fixed5hPreparationVersion != nil && *data.Fixed5hPreparationVersion == 1 {
		prepared := current
		prepared.RequestedAt = data.RequestedAt
		prepared.Fixed5hKeyIDs = data.Fixed5hKeyIDs
		version := 1
		prepared.Fixed5hPreparationVersion = &version
		if err := q.status.Set(ctx, prepared); err != nil {
			return Record{}, err
		}
		return prepared, nil
	}

	if q.pools == nil {
		return Record{}, errors.New("usersreset: 未配置数据库连接池，无法取键清单")
	}
	keys, err := q.pools.ListAdminUserResetKeys(ctx, data.UserID)
	if err != nil {
		return Record{}, err
	}
	keyIDs := make([]int64, 0, len(keys))
	for _, item := range keys {
		keyIDs = append(keyIDs, item.ID)
	}
	cutoff, err := q.status.PrepareFixed5h(ctx, data.ResetID, data.UserID, keyIDs)
	if err != nil {
		return Record{}, err
	}

	prepared := current
	prepared.RequestedAt = isoMillis(cutoff)
	prepared.Fixed5hKeyIDs = keyIDs
	version := 1
	prepared.Fixed5hPreparationVersion = &version
	if err := q.status.Set(ctx, prepared); err != nil {
		return Record{}, err
	}
	return prepared, nil
}

// pushJob 把作业放进待办 ZSET：score 为到期毫秒，立即执行即当前时刻。
func (q *Queue) pushJob(ctx context.Context, record Record) error {
	return q.pushJobWithDelay(ctx, record.ResetID, 0)
}

// pushJobWithDelay 以给定延迟入队（重试用）。
func (q *Queue) pushJobWithDelay(ctx context.Context, resetID string, delay time.Duration) error {
	payload, err := json.Marshal(jobEnvelope{ResetID: resetID})
	if err != nil {
		return fmt.Errorf("usersreset: 序列化队列载荷失败: %w", err)
	}
	due := time.Now().Add(delay).UnixMilli()
	if err := q.client().ZAdd(ctx, pendingKey, redis.Z{Score: float64(due), Member: payload}).Err(); err != nil {
		return fmt.Errorf("usersreset: 作业入队失败: %w", err)
	}
	return nil
}

// jobQueued 判断作业是否还在待办队列里（= Bull 的 getJob 存在）。
func (q *Queue) jobQueued(ctx context.Context, resetID string) (bool, error) {
	// member 里带 attempts，故按内容比对不可行，用「枚举并比对 resetId」实现。
	// 队列长度由「每用户同一时刻至多一个在途作业」界定，实际是小集合（见 worker.go 的说明）。
	members, err := q.pendingMembers(ctx, 0, -1)
	if err != nil {
		return false, err
	}
	for _, member := range members {
		envelope, ok := decodeEnvelope(member)
		if !ok {
			continue
		}
		if envelope.ResetID == resetID {
			return true, nil
		}
	}
	return false, nil
}

// pendingMembers 取队列成员（按 score 升序）。
func (q *Queue) pendingMembers(ctx context.Context, min, max int64) ([]string, error) {
	members, err := q.client().ZRange(ctx, pendingKey, min, max).Result()
	if err != nil {
		return nil, fmt.Errorf("usersreset: 读取待办队列失败: %w", err)
	}
	return members, nil
}

// decodeEnvelope 解析队列载荷；坏载荷返回 false（调用方负责丢弃）。
func decodeEnvelope(member string) (jobEnvelope, bool) {
	var envelope jobEnvelope
	if err := json.Unmarshal([]byte(member), &envelope); err != nil || envelope.ResetID == "" {
		return jobEnvelope{}, false
	}
	return envelope, true
}

// releaseQuietly 释放认领；失败只记 warn（调用方已经要重排，释放失败不该让请求失败）。
func (q *Queue) releaseQuietly(ctx context.Context, userID int64, resetID string) error {
	if err := q.status.ReleaseActive(ctx, userID, resetID); err != nil {
		q.warn("go_usersreset_release_active_failed", map[string]any{
			"resetId": resetID,
			"userId":  userID,
			"error":   err.Error(),
		})
	}
	return nil
}

// client 返回命令连接；调用前必须过 Available。
func (q *Queue) client() redis.UniversalClient { return q.status.client }

// warn 记一条告警。
func (q *Queue) warn(event string, fields map[string]any) { q.logger.Warn(event, fields) }

// createQueuedRecord 构造 queued 状态记录（reset-queue.ts:45-58）。
func createQueuedRecord(data jobData) Record {
	return Record{
		ResetID:                   data.ResetID,
		UserID:                    data.UserID,
		Status:                    StatusQueued,
		RequestedAt:               data.RequestedAt,
		Fixed5hKeyIDs:             emptyIfNil(data.Fixed5hKeyIDs),
		Fixed5hPreparationVersion: data.Fixed5hPreparationVersion,
	}
}

// emptyIfNil 把 nil 切片归一成空切片（Node 的 `?? []`）。
func emptyIfNil(values []int64) []int64 {
	if values == nil {
		return []int64{}
	}
	return values
}

// newResetID 生成 uuid v4（Node 侧为 randomUUID，作业 id 的形状是 uuid）。
func newResetID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("usersreset: 生成作业 id 失败: %w", err)
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buf[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}
