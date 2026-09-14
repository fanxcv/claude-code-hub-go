package adminapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把 Node 的导出作业层逐条移植（src/actions/usage-logs.ts:36-300）：
// 作业状态与结果都存 Redis、TTL 与键名照抄，异步执行把「导出」从请求线程里摘出去。
//
// 三处**有意的偏离**（都在下面就地写明理由）：
//
//  1. Node 的 doWork 靠进程内无界 `setTimeout`，并发没有上限；这里用**有界作业池**
//     （固定 worker 数 + 有界队列），队列满时拒绝并如实报错——无界并发下的导出会把进程拖崩。
//  2. 明细分行数/内容字节上限（见 usage_logs_export_xlsx.go 的 exportMaxRows/exportMaxContent）。
//  3. 清扫孤儿结果键（Node 只靠 TTL；这里多一条有界扫描，见 sweepUsageLogsExportOrphans）。

const (
	// usageLogsExportStatusPrefix 复刻 usageLogsExportStatusStore 的 prefix。
	usageLogsExportStatusPrefix = "cch:usage-logs:export:status:"
	// usageLogsExportResultPrefix 复刻 usageLogsExportResultStore 的 prefix。
	usageLogsExportResultPrefix = "cch:usage-logs:export:result:"
	// usageLogsExportResultSuffix 复刻 usageLogsExportResultKey：结果键是 `${jobId}:result`。
	usageLogsExportResultSuffix = ":result"
	// usageLogsExportJobTTL 复刻 USAGE_LOGS_EXPORT_JOB_TTL_SECONDS（15 分钟）。
	//
	// 两侧必须一致：切换期间 Go 与 Node 会互相看到对方的作业，TTL 不同会让「刚投的作业在
	// 另一侧查不到」。
	usageLogsExportJobTTL = 15 * time.Minute

	// usageLogsExportProgressInterval 复刻 USAGE_LOGS_EXPORT_PROGRESS_UPDATE_INTERVAL_MS：
	// 进度写 Redis 的节流间隔（800ms），否则每批 100 行写一次会把 Redis 打满。
	usageLogsExportProgressInterval = 800 * time.Millisecond

	// usageLogsExportBatchSize 复刻 USAGE_LOGS_EXPORT_BATCH_SIZE。
	//
	// 注意它**不生效**：Node 的 findUsageLogsBatch 把 limit 夹到 1..100（usage-logs.ts:344），
	// 所以 500 实际被夹成 100。Go 的 FindUsageLogsBatch 夹取相同，两侧因此仍同形。
	usageLogsExportBatchSize = 500

	// usageLogsExportWorkerCount 是并发导出的 worker 数（Node 无上限，见文件头的偏离 1）。
	usageLogsExportWorkerCount = 2
	// usageLogsExportQueueDepth 是等待队列长度。
	usageLogsExportQueueDepth = 8
	// usageLogsExportSweepEvery 是孤儿结果键的清扫间隔。
	usageLogsExportSweepEvery = 5 * time.Minute
	// usageLogsExportSweepLimit 是每轮清扫扫描的键数上限（有界，避免大 key 空间上长扫）。
	usageLogsExportSweepLimit = 500
)

// errUsageLogsExportQueueFull 表示导出队列已满（Node 无此语义，因为它是无界并发）。
var errUsageLogsExportQueueFull = errors.New("usage_logs.export_queue_full")

// UsageLogsExportKV 是导出作业的键值面。
//
// 窄接口而非 `redis.UniversalClient`：本模块只用得上这五个动作，而失败分支需要替身
// （「结果键丢失」「状态键过期」这类语义用真 Redis 只能靠睡够 TTL 来触发）。
type UsageLogsExportKV interface {
	// SetEx 写入并附 TTL。
	SetEx(ctx context.Context, key string, payload []byte, ttl time.Duration) error
	// Get 读取；不存在返回 found=false（不是错误）。
	Get(ctx context.Context, key string) (payload []byte, found bool, err error)
	// Del 删除若干键。
	Del(ctx context.Context, keys ...string) error
	// Scan 按 pattern 返回至多 limit 个键（有界扫描，供孤儿清扫用）。
	Scan(ctx context.Context, pattern string, limit int) (keys []string, err error)
	// Exists 判断键存在。
	Exists(ctx context.Context, key string) (bool, error)
}

// redisUsageLogsExportKV 是 UsageLogsExportKV 的 Redis 实现。
type redisUsageLogsExportKV struct {
	client redis.UniversalClient
}

// NewRedisUsageLogsExportKV 用命令连接装配导出作业存储；client 为 nil 时返回 nil
// （装配处据此不注册三条导出路由，而不是注册出「投了永远查不到」的假实现）。
func NewRedisUsageLogsExportKV(client redis.UniversalClient) UsageLogsExportKV {
	if client == nil {
		return nil
	}
	return &redisUsageLogsExportKV{client: client}
}

func (kv *redisUsageLogsExportKV) SetEx(
	ctx context.Context, key string, payload []byte, ttl time.Duration,
) error {
	return kv.client.SetEx(ctx, key, payload, ttl).Err()
}

func (kv *redisUsageLogsExportKV) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := kv.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (kv *redisUsageLogsExportKV) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return kv.client.Del(ctx, keys...).Err()
}

// Scan 是 SCAN + COUNT 的有界分页：cursor 走到 0 或凑满 limit 即返回（不做全量遍历）。
func (kv *redisUsageLogsExportKV) Scan(
	ctx context.Context, pattern string, limit int,
) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	found := make([]string, 0, limit)
	var cursor uint64
	for {
		keys, next, err := kv.client.Scan(ctx, cursor, pattern, int64(limit)).Result()
		if err != nil {
			return nil, err
		}
		found = append(found, keys...)
		cursor = next
		if cursor == 0 || len(found) >= limit {
			break
		}
	}
	if len(found) > limit {
		found = found[:limit]
	}
	return found, nil
}

func (kv *redisUsageLogsExportKV) Exists(ctx context.Context, key string) (bool, error) {
	count, err := kv.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// usageLogsExportJobStatus 是作业状态（Node 的 UsageLogsExportStatus，四个取值）。
type usageLogsExportJobStatus string

const (
	exportStatusQueued    usageLogsExportJobStatus = "queued"
	exportStatusRunning   usageLogsExportJobStatus = "running"
	exportStatusCompleted usageLogsExportJobStatus = "completed"
	exportStatusFailed    usageLogsExportJobStatus = "failed"
)

// usageLogsExportJobRecord 是 Redis 里存的作业记录（Node 的 UsageLogsExportJobRecord）。
//
// 字段顺序与 Node 一致，两侧互读时人眼可比。`error` 用指针 + omitempty 复刻 Node 的
// `error: undefined`（JSON.stringify 会丢掉该键）。
type usageLogsExportJobRecord struct {
	JobID           string                   `json:"jobId"`
	OwnerUserID     int64                    `json:"ownerUserId"`
	Status          usageLogsExportJobStatus `json:"status"`
	ProcessedRows   int64                    `json:"processedRows"`
	TotalRows       int64                    `json:"totalRows"`
	ProgressPercent int                      `json:"progressPercent"`
	Format          string                   `json:"format"`
	Error           *string                  `json:"error,omitempty"`
}

// usageLogsExportStatus 是状态接口的响应体（Node 的 toUsageLogsExportStatus 去掉了属主）。
type usageLogsExportStatus struct {
	JobID           string                   `json:"jobId"`
	Status          usageLogsExportJobStatus `json:"status"`
	ProcessedRows   int64                    `json:"processedRows"`
	TotalRows       int64                    `json:"totalRows"`
	ProgressPercent int                      `json:"progressPercent"`
	Format          string                   `json:"format"`
	Error           *string                  `json:"error,omitempty"`
}

func exportStatusOf(job usageLogsExportJobRecord) usageLogsExportStatus {
	return usageLogsExportStatus{
		JobID:           job.JobID,
		Status:          job.Status,
		ProcessedRows:   job.ProcessedRows,
		TotalRows:       job.TotalRows,
		ProgressPercent: job.ProgressPercent,
		Format:          job.Format,
		Error:           job.Error,
	}
}

func usageLogsExportStatusKey(jobID string) string {
	return usageLogsExportStatusPrefix + jobID
}

func usageLogsExportResultKey(jobID string) string {
	return usageLogsExportResultPrefix + jobID + usageLogsExportResultSuffix
}

func exportFileExtension(format string) string {
	if format == "xlsx" {
		return "xlsx"
	}
	return "csv"
}

func exportEncodingFor(format string) string {
	if format == "xlsx" {
		return "base64"
	}
	return "utf8"
}

// exportProgress 复刻 buildUsageLogsExportProgress。
//
// 三处易错：totalRows 会被抬到「已处理 + 1」（还有更多时，避免进度显示成 100% 却还在跑）；
// 有更多时上限 99%；总数 <= 0 时直接 100%。
func exportProgress(processedRows, totalRows int64, hasMore bool) (int64, int) {
	effectiveTotal := totalRows
	if hasMore && processedRows+1 > effectiveTotal {
		effectiveTotal = processedRows + 1
	}
	if effectiveTotal <= 0 {
		return effectiveTotal, 100
	}
	if hasMore {
		percent := int(processedRows * 100 / effectiveTotal)
		if percent > 99 {
			percent = 99
		}
		return effectiveTotal, percent
	}
	return effectiveTotal, 100
}

// usageLogsExportTask 是一次待执行的导出。
type usageLogsExportTask struct {
	jobID   string
	filters store.UsageLogFilters
	format  string
}

// usageLogsExportPool 是有界作业池：固定 worker 数 + 有界队列（见文件头偏离 1）。
type usageLogsExportPool struct {
	tasks chan usageLogsExportTask
}

func newUsageLogsExportPool(run func(context.Context, usageLogsExportTask)) *usageLogsExportPool {
	pool := &usageLogsExportPool{tasks: make(chan usageLogsExportTask, usageLogsExportQueueDepth)}
	for worker := 0; worker < usageLogsExportWorkerCount; worker++ {
		go func() {
			for task := range pool.tasks {
				run(context.Background(), task)
			}
		}()
	}
	return pool
}

// submit 投递任务；队列满时返回 errUsageLogsExportQueueFull（调用方据此如实报错）。
func (pool *usageLogsExportPool) submit(task usageLogsExportTask) error {
	select {
	case pool.tasks <- task:
		return nil
	default:
		return errUsageLogsExportQueueFull
	}
}

// --- 时间与时区 --------------------------------------------------------------

// usageLogsExportRuntime 是导出侧的运行态：键值面 + 有界作业池。nil 表示未装配（三条导出
// 路由不注册，原样回退 Node）。
type usageLogsExportRuntime struct {
	kv   UsageLogsExportKV
	pool *usageLogsExportPool
}

// newUsageLogsExportRuntime 是**零调用的死重复**，**不要拿它做装配**。
//
// 真正在跑的装配在 registerUsageLogsExports（usage_logs_exports.go:62）：它内联构造同一个
// 结构体，**并额外点亮孤儿清扫循环**（`go module.runUsageLogsExportSweeper(...)`）。
// 若改用本函数，导出作业照跑，但清扫循环会**静默不跑**——残留的结果键只能等 TTL 自清，
// 且没有任何日志能看出少了什么。本函数留着只会被人误用，待死码清理批次一并删除
// 的 U1000 清单）。
func newUsageLogsExportRuntime(
	kv UsageLogsExportKV,
	run func(context.Context, usageLogsExportTask),
) *usageLogsExportRuntime {
	if kv == nil {
		return nil
	}
	return &usageLogsExportRuntime{kv: kv, pool: newUsageLogsExportPool(run)}
}

// --- 键值读写 ----------------------------------------------------------------

func (m *usageLogsModule) exportKV() UsageLogsExportKV {
	if m.exports == nil {
		return nil
	}
	return m.exports.kv
}

func (m *usageLogsModule) readExportJob(ctx context.Context, jobID string) (*usageLogsExportJobRecord, error) {
	kv := m.exportKV()
	if kv == nil {
		return nil, nil
	}
	payload, found, err := kv.Get(ctx, usageLogsExportStatusKey(jobID))
	if err != nil || !found {
		return nil, err
	}
	var record usageLogsExportJobRecord
	if err := json.Unmarshal(payload, &record); err != nil {
		// 解析失败按「查不到」处理：脏值不该让状态接口 500（Node 侧 JSON.parse 会抛，
		// 那里表现为 actionError；此处对齐「作业不存在或已过期」更贴近用户能做的事）。
		m.logExportEvent("admin_usage_logs_export_status_corrupt", jobID)
		return nil, nil
	}
	return &record, nil
}

func (m *usageLogsModule) writeExportJob(ctx context.Context, record usageLogsExportJobRecord) bool {
	kv := m.exportKV()
	if kv == nil {
		return false
	}
	payload, err := json.Marshal(record)
	if err != nil {
		m.logExportEvent("admin_usage_logs_export_persist_failed", record.JobID)
		return false
	}
	if err := kv.SetEx(ctx, usageLogsExportStatusKey(record.JobID), payload, usageLogsExportJobTTL); err != nil {
		m.logExportEvent("admin_usage_logs_export_persist_failed", record.JobID)
		return false
	}
	return true
}

func (m *usageLogsModule) logExportEvent(event, jobID string) {
	if m.deps.Logger == nil {
		return
	}
	m.deps.Logger.Error(event, map[string]any{"module": "usage-logs", "jobId": jobID})
}

// --- 时间与时区 --------------------------------------------------------------

// usageLogsExportSystemLocation 复刻 resolveSystemTimezone 的三级取值：
// system_settings.timezone → 环境变量 TZ（未设置取 `Asia/Shanghai`）→ UTC；
// 同时返回可用作表头后缀的时区名。
//
// 取值链的唯一实现在 config.ResolveLocation（默认值就在那里）；本函数只附加「回带时区名」
// 这一导出作业特有的需求：库值原样回带，退到环境 TZ 时也回带 IANA 名（Node 的表头后缀同此）。
func usageLogsExportSystemLocation(ctx context.Context, pools *store.Pools) (*time.Location, string, error) {
	raw, err := pools.AdminSystemTimezone(ctx)
	if err != nil {
		return nil, "", err
	}
	location := config.ResolveLocationFromEnv(raw)
	// 库列存在且合法时按原值作答（Node 的 formatExportTimestamp 用配置里的原字符串），
	// 否则回到解析出的时区名（默认为 Asia/Shanghai，而非 UTC）。
	if raw != nil {
		if name := strings.TrimSpace(*raw); isIANATimezone(name) {
			return location, name, nil
		}
	}
	return location, location.String(), nil
}

// --- 装配（buildUsageLogsExport） --------------------------------------------

// usageLogsExportBuild 是一次导出的装配参数。
type usageLogsExportBuild struct {
	filters    store.UsageLogFilters
	format     string
	location   *time.Location
	timezone   string
	onProgress func(processedRows, totalRows int64, hasMore bool) error
}

// buildUsageLogsExport 复刻 buildUsageLogsExport：按批流式读全量匹配行并拼出导出内容。
//
// CSV 返回「BOM + 表头 + 数据行」的文本；XLSX 返回 base64（Node 的 ResultStore 存的就是
// base64 文本，两条下载路径据此解码）。
func (m *usageLogsModule) buildUsageLogsExport(
	ctx context.Context,
	build usageLogsExportBuild,
) (string, error) {
	estimatedTotal, err := m.estimateUsageLogsExportRows(ctx, build.filters)
	if err != nil {
		return "", err
	}

	csvWriter := (*usageLogsExportCsvWriter)(nil)
	var xlsxWriter *usageLogsExportXlsxWriter
	if build.format == "xlsx" {
		xlsxWriter, err = newUsageLogsExportXlsxWriter(build.location, build.timezone)
		if err != nil {
			return "", err
		}
	} else {
		csvWriter = newUsageLogsExportCsvWriter(build.timezone)
	}

	var cursor *store.UsageLogCursor
	var processedRows int64
	useLedger := false
	for {
		rows, hasMore, nextCursor, ledger, err := m.readUsageLogsExportBatch(
			ctx, build.filters, cursor, useLedger)
		if err != nil {
			return "", err
		}
		useLedger = ledger

		if len(rows) > 0 {
			switch {
			case xlsxWriter != nil:
				if err := xlsxWriter.addBatch(rows); err != nil {
					return "", err
				}
			default:
				if err := csvWriter.addBatch(rows); err != nil {
					return "", err
				}
			}
			processedRows += int64(len(rows))
		}

		// 注意进度用的是 store 的 hasMore/nextCursor，与 Node 一致：两者任一为「无更多」即停止。
		effectiveTotal, percent := exportProgress(processedRows, estimatedTotal, hasMore)
		estimatedTotal = effectiveTotal
		if build.onProgress != nil {
			if err := build.onProgress(processedRows, effectiveTotal, hasMore); err != nil {
				return "", err
			}
		}
		_ = percent

		if !hasMore || nextCursor == nil {
			break
		}
		cursor = nextCursor
	}

	if xlsxWriter != nil {
		bytes, err := xlsxWriter.Finish()
		if err != nil {
			return "", err
		}
		return base64.StdEncoding.EncodeToString(bytes), nil
	}
	return csvWriter.Finish()
}

// estimateUsageLogsExportRows 复刻 Node 的两步估数：先读分页 total，为 0 再读聚合统计。
func (m *usageLogsModule) estimateUsageLogsExportRows(
	ctx context.Context,
	filters store.UsageLogFilters,
) (int64, error) {
	page, err := m.deps.Store.FindUsageLogsWithDetails(ctx, filters, 1, 1)
	if err != nil {
		return 0, err
	}
	if page != nil && page.Total > 0 {
		return page.Total, nil
	}
	summary, err := m.deps.Store.FindUsageLogsStats(ctx, filters, m.ledgerOnlyForExport(ctx))
	if err != nil {
		return 0, err
	}
	return summary.TotalRequests, nil
}

// readUsageLogsExportBatch 读一批导出行，返回的 ledger 表示本批是否走了账本回退。
//
// 回退条件与 Node 的 findUsageLogsBatch 内建回退一致：message_request 侧查不到行、
// 且处于 ledger-only 模式、且没有重试次数筛选（账本没有重试维度）。一旦回退，
// 后续批次沿用账本，避免中途换源导致漏行或重复。
func (m *usageLogsModule) readUsageLogsExportBatch(
	ctx context.Context,
	filters store.UsageLogFilters,
	cursor *store.UsageLogCursor,
	useLedger bool,
) (rows []usageLogsExportRow, hasMore bool, nextCursor *store.UsageLogCursor, ledger bool, err error) {
	if !useLedger {
		messageRows, more, next, readErr := m.deps.Store.FindUsageLogsBatch(
			ctx, filters, cursor, usageLogsExportBatchSize, nil)
		if readErr != nil {
			return nil, false, nil, false, readErr
		}
		if len(messageRows) > 0 {
			rows = make([]usageLogsExportRow, 0, len(messageRows))
			for index := range messageRows {
				rows = append(rows, exportRowFromMessage(&messageRows[index]))
			}
			return rows, more, next, false, nil
		}
		if !m.ledgerOnlyForExport(ctx) || filters.MinRetryCount > 0 {
			return nil, more, next, false, nil
		}
	}

	ledgerRows, more, next, readErr := m.deps.Store.FindUsageLogsBatchLedger(
		ctx, filters, cursor, exportLedgerLimit(usageLogsExportBatchSize), nil)
	if readErr != nil {
		return nil, false, nil, true, readErr
	}
	rows = make([]usageLogsExportRow, 0, len(ledgerRows))
	for index := range ledgerRows {
		rows = append(rows, exportRowFromLedger(&ledgerRows[index]))
	}
	return rows, more, next, true, nil
}

// exportLedgerLimit 给账本查询一个显式 limit。
//
// Node 的账本分支用的是**未被夹取的**原始 limit（500），与 message 分支的夹取值（100）不同——
// 账本查询自己会把 limit 夹到 1..100（Go 侧 FindUsageLogsBatchLedger 同样处理），
// 故这里传原始值即可，与 Node 同形。
func exportLedgerLimit(limit int) *int { return &limit }

// ledgerOnlyForExport 与列表路径的 ledgerOnlyCache 同义，但**不缓存**。
//
// 作业线程没有 *http.Request（缓存类型的方法签名要 request），而导出对它的调用次数有界：只在
// 「message 侧首批查不到行」与「估数为 0 再读统计」两处各一次，没必要为此加缓存。
func (m *usageLogsModule) ledgerOnlyForExport(ctx context.Context) bool {
	if m.deps.Store == nil {
		return false
	}
	exists, err := m.deps.Store.MessageRequestExists(ctx)
	if err != nil {
		return false
	}
	return !exists
}

// --- 作业执行 ----------------------------------------------------------------

// startUsageLogsExport 复刻 startUsageLogsExport：写 queued 记录后投递到作业池。
func (m *usageLogsModule) startUsageLogsExport(
	ctx context.Context,
	ownerUserID int64,
	filters store.UsageLogFilters,
	format string,
	jobID string,
) error {
	record := usageLogsExportJobRecord{
		JobID:       jobID,
		OwnerUserID: ownerUserID,
		Status:      exportStatusQueued,
		Format:      format,
	}
	if !m.writeExportJob(ctx, record) {
		return errors.New("Export job initialization failed")
	}
	exports := m.exports
	if exports == nil || exports.pool == nil {
		return errors.New("Export job initialization failed")
	}
	if err := exports.pool.submit(usageLogsExportTask{
		jobID: jobID, filters: filters, format: format,
	}); err != nil {
		return err
	}
	return nil
}

// runUsageLogsExportJob 复刻 runUsageLogsExportJob：先置 running，逐批写进度，末态 completed/failed。
func (m *usageLogsModule) runUsageLogsExportJob(ctx context.Context, task usageLogsExportTask) {
	current, err := m.readExportJob(ctx, task.jobID)
	if err != nil || current == nil {
		// 作业已被删除（用户删了记录 / 键过期）：静默退出，与 Node 的 `if (!existingJob) return` 同义。
		return
	}
	current.Status = exportStatusRunning
	current.Error = nil
	if !m.writeExportJob(ctx, *current) {
		return
	}

	location, timezone, err := usageLogsExportSystemLocation(ctx, m.deps.Store)
	if err != nil {
		m.failUsageLogsExportJob(ctx, task.jobID, err)
		return
	}

	lastProgressAt := time.Time{}
	content, err := m.buildUsageLogsExport(ctx, usageLogsExportBuild{
		filters:  task.filters,
		format:   task.format,
		location: location,
		timezone: timezone,
		onProgress: func(processedRows, totalRows int64, hasMore bool) error {
			now := m.now()
			_, percent := exportProgress(processedRows, totalRows, hasMore)
			if percent < 100 && now.Sub(lastProgressAt) < usageLogsExportProgressInterval {
				return nil
			}
			lastProgressAt = now
			job, readErr := m.readExportJob(ctx, task.jobID)
			if readErr != nil || job == nil {
				return nil
			}
			job.Status = exportStatusRunning
			job.ProcessedRows = processedRows
			job.TotalRows = totalRows
			job.ProgressPercent = percent
			m.writeExportJob(ctx, *job)
			return nil
		},
	})
	if err != nil {
		m.failUsageLogsExportJob(ctx, task.jobID, err)
		return
	}

	job, readErr := m.readExportJob(ctx, task.jobID)
	if readErr != nil || job == nil {
		return
	}
	kv := m.exportKV()
	if kv == nil {
		return
	}
	if err := kv.SetEx(ctx, usageLogsExportResultKey(task.jobID), []byte(content),
		usageLogsExportJobTTL); err != nil {
		job.Status = exportStatusFailed
		job.ProgressPercent = 0
		message := "Failed to persist export to Redis"
		job.Error = &message
		m.writeExportJob(ctx, *job)
		return
	}
	job.Status = exportStatusCompleted
	job.ProgressPercent = 100
	job.Error = nil
	m.writeExportJob(ctx, *job)
}

func (m *usageLogsModule) failUsageLogsExportJob(ctx context.Context, jobID string, cause error) {
	if m.deps.Logger != nil {
		m.deps.Logger.Error("admin_usage_logs_export_failed", map[string]any{
			"module": "usage-logs",
			"jobId":  jobID,
			"error":  cause.Error(),
		})
	}
	job, err := m.readExportJob(ctx, jobID)
	if err != nil || job == nil {
		return
	}
	job.Status = exportStatusFailed
	job.ProgressPercent = 0
	message := cause.Error()
	job.Error = &message
	m.writeExportJob(ctx, *job)
}

// --- 孤儿清扫 ----------------------------------------------------------------

// sweepUsageLogsExportOrphans 删掉「没有对应状态键」的结果键。
//
// 为什么需要（Node 只靠 TTL）：状态键与结果键分开写、TTL 各自计时，进程在「结果写成功、
// 状态写失败」之间崩掉时结果键会留在 Redis 里直到过期；清扫把这段窗口收掉。
// 有界：每轮至多扫 usageLogsExportSweepLimit 个键，不做全量遍历。
func (m *usageLogsModule) sweepUsageLogsExportOrphans(ctx context.Context) (int, error) {
	kv := m.exportKV()
	if kv == nil {
		return 0, nil
	}
	keys, err := kv.Scan(ctx, usageLogsExportResultPrefix+"*", usageLogsExportSweepLimit)
	if err != nil {
		return 0, err
	}
	orphans := make([]string, 0, len(keys))
	for _, key := range keys {
		jobID := exportJobIDFromResultKey(key)
		if jobID == "" {
			continue
		}
		exists, existsErr := kv.Exists(ctx, usageLogsExportStatusKey(jobID))
		if existsErr != nil {
			return len(orphans), existsErr
		}
		if !exists {
			orphans = append(orphans, key)
		}
	}
	if err := kv.Del(ctx, orphans...); err != nil {
		return 0, err
	}
	return len(orphans), nil
}

// exportJobIDFromResultKey 从结果键反推作业 id；形状不符时返回空串（不动不是我们写的键）。
func exportJobIDFromResultKey(key string) string {
	if !strings.HasPrefix(key, usageLogsExportResultPrefix) ||
		!strings.HasSuffix(key, usageLogsExportResultSuffix) {
		return ""
	}
	jobID := strings.TrimSuffix(
		strings.TrimPrefix(key, usageLogsExportResultPrefix), usageLogsExportResultSuffix)
	// 作业 id 是 UUID v4（crypto.randomUUID）：形状不对的一律不认。清扫会**删键**，
	// 认错了就把别的产者的数据删了，因此宁可漏扫也不宽认。
	if !exportLooksLikeUUID(jobID) {
		return ""
	}
	return jobID
}

// exportLooksLikeUUID 判断字符串是否为 8-4-4-4-12 的十六进制 UUID 形状。
func exportLooksLikeUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, char := range value {
		switch index {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			isHex := (char >= '0' && char <= '9') ||
				(char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// runUsageLogsExportSweeper 起一条有界的清扫循环（进程生命周期，随进程退出结束）。
func (m *usageLogsModule) runUsageLogsExportSweeper(ctx context.Context) {
	ticker := time.NewTicker(usageLogsExportSweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := m.sweepUsageLogsExportOrphans(ctx); err != nil && m.deps.Logger != nil {
				m.deps.Logger.Warn("admin_usage_logs_export_sweep_failed", map[string]any{
					"module": "usage-logs",
					"error":  err.Error(),
				})
			}
		}
	}
}

// newUsageLogsExportJobID 生成作业 id（UUID v4，Node 的 crypto.randomUUID）。
func newUsageLogsExportJobID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
