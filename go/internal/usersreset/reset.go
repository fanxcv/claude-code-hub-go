package usersreset

import (
	"context"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件复刻 executeUserStatisticsReset（reset-service.ts:198-245）：删两表的历史行、清成本重置
// 标记、清 Redis 运行态。三段顺序不可调换：
//
//  1. **先删行、后清标记**：切点是请求时刻。先清标记再删行，会让限额检查在删行的间隙里读到
//     「标记已重置 + 旧行还在」，于是旧消费被重新累计一次。
//  2. **两表都要删**：message_request 是请求流水（UI 统计的来源），usage_ledger 是账本（限额累计
//     的来源）。只删一个会得到「统计归零但限额还在」这种自相矛盾的状态。
//  3. **最后清 Redis**：Redis 里的成本窗口是删行的结果缓存。先清它，下一批请求会把尚未删掉的旧行
//     重新算进窗口——数据要删、缓存却把旧值重建了出来。
//
// 失败时的进度必须带出来（reset-service.ts:34-51）：worker 要把重试轮的条数累加进状态键，
// 否则 UI 上的已删条数会随每次重试回退。

// ResetExecutor 执行一次重置。
type ResetExecutor struct {
	pools   *store.Pools
	cleaner *CostCleaner
	logger  *logx.Logger
}

// NewResetExecutor 构造执行器。
func NewResetExecutor(pools *store.Pools, cleaner *CostCleaner, logger *logx.Logger) *ResetExecutor {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &ResetExecutor{pools: pools, cleaner: cleaner, logger: logger}
}

// Execute 执行一次重置并回报进度。
//
// onProgress 每删满一批调用一次（可为 nil）；它返回错误即中止本次执行——状态写不下去时继续删行
// 只会让「进度」与「已删数据」永久不一致。
func (e *ResetExecutor) Execute(
	ctx context.Context,
	userID int64,
	requestedAt string,
	onProgress func(Progress) error,
) (Progress, error) {
	if e == nil || e.pools == nil {
		return Progress{}, newError(ErrCodeOperationFailed)
	}
	cut, err := parseIsoMillis(requestedAt)
	if err != nil {
		return Progress{}, newError(ErrCodeInvalidCutoff)
	}

	progress := Progress{}
	report := func() error {
		if onProgress == nil {
			return nil
		}
		return onProgress(progress)
	}

	deletedRequests, err := e.drainTable(ctx, "message_request", userID, cut, &progress, report)
	if err != nil {
		return e.fail(progress, err)
	}
	progress.DeletedMessageRequests = deletedRequests

	deletedLedger, err := e.drainTable(ctx, "usage_ledger", userID, cut, &progress, report)
	if err != nil {
		return e.fail(progress, err)
	}
	progress.DeletedUsageLedger = deletedLedger

	if err := e.finishReset(ctx, userID, cut); err != nil {
		return e.fail(progress, err)
	}
	return progress, nil
}

// fail 复刻外层 catch（reset-service.ts:203-221）：非本包错误且尚无任何进度时原样抛出
// （例如 ctx 取消），否则统一成「码 + 最大进度」的错误。
func (e *ResetExecutor) fail(progress Progress, err error) (Progress, error) {
	merged := mergeProgress(progress, err)
	if asResetError(err) == nil && merged == (Progress{}) {
		return Progress{}, err
	}
	return merged, &Error{Code: errorCode(err), Progress: merged}
}

// mergeProgress 按字段取最大值（reset-service.ts:212-220）。
//
// 取最大值而不是覆盖：drain 内部报的进度只含它自己那张表（另一表为 0），若直接覆盖，
// 已删完的 message_request 条数会被下一张表的 0 抹掉。
func mergeProgress(base Progress, err error) Progress {
	fromErr := errorProgress(err)
	return Progress{
		DeletedMessageRequests: max(base.DeletedMessageRequests, fromErr.DeletedMessageRequests),
		DeletedUsageLedger:     max(base.DeletedUsageLedger, fromErr.DeletedUsageLedger),
	}
}

// drainTable 把一个表删干净（reset-service.ts:124-198 的 drainTable）。
//
// 循环条件照抄：一批删满 batch 就再来一批，不足 batch 即认为删到尾巴。之后的**存在性检查不是
// 冗余**：`FOR UPDATE SKIP LOCKED` 会跳过被并发事务持锁的行，那些行不会被任何一批删掉；只有
// 这一问能把「仍有残留」变成 ROWS_LOCKED（可重试的错误），而不是以「成功」收尾却留着旧数据。
func (e *ResetExecutor) drainTable(
	ctx context.Context,
	table string,
	userID int64,
	cut time.Time,
	progress *Progress,
	report func() error,
) (int64, error) {
	var deleted int64
	fail := func() error {
		return &Error{Code: ErrCodeOperationFailed, Progress: tableProgress(table, deleted)}
	}
	for {
		batch, err := e.deleteBatch(ctx, table, userID, cut)
		if err != nil {
			e.logger.Warn("go_usersreset_delete_batch_failed", map[string]any{
				"table":  table,
				"userId": userID,
				"error":  err.Error(),
			})
			return deleted, fail()
		}
		deleted += batch
		if batch > 0 {
			applyTableProgress(table, progress, deleted)
			if err := report(); err != nil {
				// 状态写不下去即中止：进度与已删数据必须同步，否则重试会重算一次已删的批。
				return deleted, fail()
			}
		}
		if batch < int64(store.UserStatisticsResetBatchSize) {
			break
		}
	}
	remaining, err := e.pools.HasAdminUserRemainingRows(ctx, table, userID, cut)
	if err != nil {
		e.logger.Warn("go_usersreset_remaining_check_failed", map[string]any{
			"table":  table,
			"userId": userID,
			"error":  err.Error(),
		})
		return deleted, fail()
	}
	if remaining {
		return deleted, &Error{Code: ErrCodeRowsLocked, Progress: tableProgress(table, deleted)}
	}
	return deleted, nil
}

// deleteBatch 删一批。
func (e *ResetExecutor) deleteBatch(
	ctx context.Context,
	table string,
	userID int64,
	cut time.Time,
) (int64, error) {
	if table == "message_request" {
		return e.pools.DrainAdminUserMessageRequests(
			ctx, userID, cut, store.UserStatisticsResetBatchSize)
	}
	return e.pools.DrainAdminUserUsageLedger(
		ctx, userID, cut, store.UserStatisticsResetBatchSize)
}

// tableProgress 构造「只有这张表有值」的进度（reset-service.ts:140-150）。
func tableProgress(table string, deleted int64) Progress {
	if table == "message_request" {
		return Progress{DeletedMessageRequests: deleted}
	}
	return Progress{DeletedUsageLedger: deleted}
}

// applyTableProgress 把某表的已删条数写进累计进度。
func applyTableProgress(table string, progress *Progress, deleted int64) {
	if table == "message_request" {
		progress.DeletedMessageRequests = deleted
		return
	}
	progress.DeletedUsageLedger = deleted
}

// finishReset 清成本重置标记与 Redis 运行态（reset-service.ts:223-243）。
func (e *ResetExecutor) finishReset(ctx context.Context, userID int64, cut time.Time) error {
	keys, err := e.pools.ListAdminUserResetKeys(ctx, userID)
	if err != nil {
		return err
	}
	if _, err := e.pools.ClearAdminUserResetTimestamps(ctx, userID, cut); err != nil {
		return err
	}
	e.cleaner.InvalidateCachedUser(ctx, userID)

	keyIDs := make([]int64, 0, len(keys))
	keyValues := make([]string, 0, len(keys))
	for _, item := range keys {
		keyIDs = append(keyIDs, item.ID)
		keyValues = append(keyValues, item.Key)
	}
	// preserveFixed5h 固定为 true：5h 固定窗口的键已在准备阶段删掉并记下切点，这里再删一次会把
	// 「切点已记、键已删」破坏成「切点已记、键还在旧值上」。
	result, err := e.cleaner.ClearUserCostCache(ctx, userID, keyIDs, keyValues, true)
	if err != nil {
		return newError(ErrCodeCacheCleanupFailed)
	}
	if result.CleanupFailed {
		return newError(ErrCodeCacheCleanupFailed)
	}
	e.logger.Info("go_usersreset_cache_cleared", map[string]any{
		"userId":          userID,
		"costKeysDeleted": result.CostKeysDeleted,
		"keys":            len(keyIDs),
	})
	return nil
}
