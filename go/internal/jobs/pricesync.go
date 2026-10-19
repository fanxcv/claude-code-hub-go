package jobs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件复刻云价格同步：src/lib/price-sync/cloud-price-updater.ts +
// src/lib/price-sync/cloud-price-table.ts + src/actions/model-prices.ts:112 processPriceTableInternal。
//
// 一轮的步骤（与 Node 逐条对应）：
//  1. 拉取 CPT v1 JSON（Accept: application/json；30 秒超时；重定向到非预期地址即失败）
//     拉取分两段：先读开头 4 KiB 取 version，未变即关闭正文（不必下载完整 32 MB），变了续读余下正文。
//  2. 解析（cpt-schema）并转换（cpt-convert）为以 bare model_name 为键的价格表
//  3. 版本短路：目录指纹一致且云端行数一致时整轮跳过（第一段的头读已判过一遍）
//  4. 逐模型写入：manual 保护（除非显式列入 overwrite）→ 新增 / 替换 / 不变
//  5. 整表切换：删除本次表内不存在的非 manual 行
//  6. 写 cloud_pricing_catalog（版本指纹 + provider 字典 + vendor 汇总 + 实际云端行数）
//
// 与 Node 的差异（有意为之，见 internal/jobs/README.md）：
//   - 新增 PG advisory 锁（Node 只有进程内去重），使多实例只跑一个。
//   - 每 ProgressEvery 个模型记一条 job_progress，便于观察 1.1 万行的长任务。
//   - 拉取体加上限（128 MiB）：对端异常返回超大正文时直接失败，而不是把内存吃光。

const (
	// defaultCloudPriceTableURL 对应 cloud-price-table.ts:7 的 CLOUD_PRICE_TABLE_URL。
	defaultCloudPriceTableURL = "https://cch-plus.com/pricing/v1/models.json"
	// cloudPriceFetchTimeout 对应 FETCH_TIMEOUT_MS = 30000。
	cloudPriceFetchTimeout = 30 * time.Second
	// cloudPriceMaxBodyBytes 是本实现的硬上限（Node 无此限制），实测正文约 28 MiB。
	cloudPriceMaxBodyBytes = 128 << 20
	// cloudPriceHeadProbeBytes 是头读的窗口：实测正文前 200 字节内即含 version，4 KiB 给足余量。
	// 头读命中时只花一次小请求（实测 335 ms）就能判「未变更」，不必下载完整正文（实测 56.4 s / 32.4 MB）。
	cloudPriceHeadProbeBytes = 4 << 10
	// defaultPriceSyncInterval 对应 instrumentation.ts:222 的 30 分钟。
	defaultPriceSyncInterval = 30 * time.Minute
	// cptConverterRevision 对应 CPT_CONVERTER_REV = 1：转换逻辑变更时递增，
	// 使版本指纹失配、绕过短路、强制重写整表。Go 侧与 TS 共用同一个修订号。
	cptConverterRevision = 1
	// metadataFieldSampleSpec 对应 processPriceTableInternal 的 METADATA_FIELDS。
	metadataFieldSampleSpec = "sample_spec"
	// cloudPriceSource 是写入 model_prices.source 的云端来源标签。
	cloudPriceSource = "cloud"
	// defaultProgressEvery 是进度日志的间隔模型数。
	defaultProgressEvery = 2000
)

// PriceUpdateResult 对应 TS 的 PriceUpdateResult。
type PriceUpdateResult struct {
	Added            []string `json:"added"`
	Updated          []string `json:"updated"`
	Unchanged        []string `json:"unchanged"`
	Failed           []string `json:"failed"`
	Total            int      `json:"total"`
	SkippedConflicts []string `json:"skippedConflicts"`
}

// priceStore 是同步器用到的 store 面。
//
// 抽成接口只有一个理由：整表同步的写入路径会删掉「本次表内不存在的非 manual 行」，
// 用一个三行合成表去驱动真实库，等价于清空该库的云端价格。合成表的语义验证必须在
// 无库的假实现上做（store_methods_test 与 pricesync_test），真实库只跑只读与
// 「保留列表包含全部现存行」的安全路径。
type priceStore interface {
	ListManualPriceModelNames(ctx context.Context) (map[string]struct{}, error)
	ListLatestPriceRowsForSync(ctx context.Context) (map[string]store.PriceSyncExistingRow, error)
	InsertModelPrice(ctx context.Context, modelName string, priceData []byte, source string) (int64, error)
	AdminUpsertModelPrice(ctx context.Context, modelName string, priceData json.RawMessage, source string) (store.AdminModelPrice, error)
	// 批量写：正常路径只走这两个；逐条方法保留给批量失败后的退化写（归因需要）。
	InsertModelPrices(ctx context.Context, writes []store.ModelPriceWrite) error
	ReplaceModelPrices(ctx context.Context, writes []store.ModelPriceWrite) error
	DeleteCloudPricesNotIn(ctx context.Context, keepModelNames []string) (int64, error)
	CountCloudModelPrices(ctx context.Context) (int, error)
	GetCloudPricingCatalog(ctx context.Context) (*store.CloudPricingCatalogRow, error)
	UpsertCloudPricingCatalog(ctx context.Context, input store.CloudPricingCatalogInput) error
}

// lockAcquirer 申请 leader 锁；返回的 release 在任务结束时调用。
type lockAcquirer func(ctx context.Context) (release func(context.Context) error, acquired bool, err error)

// PriceSyncOptions 是同步器的装配参数。
type PriceSyncOptions struct {
	// Pools 是共享连接池；必填。
	Pools *store.Pools
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// URL 覆盖价格表地址（测试指向本地 httptest）。
	URL string
	// Client 覆盖 HTTP 客户端（测试注入）；超时由本包统一施加，注入的客户端无需自带。
	Client *http.Client
	// OverwriteManual 是允许覆盖的 manual 模型名（对应 overwriteManual）。
	OverwriteManual []string
	// ProgressEvery 覆盖进度日志间隔。
	ProgressEvery int

	// store 与 acquire 是包内测试的替换点（生产走 Pools + advisory 锁）。
	store   priceStore
	acquire lockAcquirer
}

// PriceSyncer 是云价格同步器；可跨轮复用（内部只持有无状态配置与节流位）。
type PriceSyncer struct {
	pools           *store.Pools
	store           priceStore
	acquire         lockAcquirer
	logger          *logx.Logger
	url             string
	client          *http.Client
	overwriteManual []string
	progressEvery   int

	// throttle 对应 Node 的 globalThis 节流位：上次同步完成时间 + 是否正在调度。
	throttleMu   sync.Mutex
	lastSyncedAt time.Time
	scheduling   bool
	// running 保证同一进程内不会并发跑两轮（advisory 锁管跨进程）。
	running atomic.Bool
}

// NewPriceSyncer 校验装配参数并返回同步器。
func NewPriceSyncer(options PriceSyncOptions) (*PriceSyncer, error) {
	if options.Pools == nil {
		return nil, errors.New("jobs: 价格同步缺少连接池")
	}
	tableURL := options.URL
	if tableURL == "" {
		tableURL = defaultCloudPriceTableURL
	}
	if _, err := url.Parse(tableURL); err != nil {
		return nil, fmt.Errorf("jobs: 价格表地址非法: %w", err)
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	progressEvery := options.ProgressEvery
	if progressEvery <= 0 {
		progressEvery = defaultProgressEvery
	}
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: cloudPriceFetchTimeout}
	}
	syncerStore := options.store
	if syncerStore == nil {
		syncerStore = options.Pools
	}
	acquire := options.acquire
	if acquire == nil {
		acquire = func(ctx context.Context) (func(context.Context) error, bool, error) {
			lock, acquired, err := AcquireLeader(ctx, options.Pools, priceSyncLockName)
			if err != nil || !acquired {
				return nil, acquired, err
			}
			return lock.Release, true, nil
		}
	}
	return &PriceSyncer{
		pools:           options.Pools,
		store:           syncerStore,
		acquire:         acquire,
		logger:          logger,
		url:             tableURL,
		client:          client,
		overwriteManual: append([]string(nil), options.OverwriteManual...),
		progressEvery:   progressEvery,
	}, nil
}

// Task 返回可登记进 Scheduler 的任务定义（启动即跑一次，之后每 interval 一次）。
func (s *PriceSyncer) Task(interval time.Duration) Task {
	if interval <= 0 {
		interval = defaultPriceSyncInterval
	}
	return Task{
		Name:     "cloud-price-sync",
		Interval: interval,
		// 同步要拉 28 MiB 并写约 1.1 万行，超时给足（实测秒级，慢网络下长尾更长）。
		Timeout: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			_, runErr := s.RunOnce(ctx)
			return runErr
		},
	}
}

// RunOnce 执行一轮同步。
//
// 返回值：result 为各分类的模型名清单（短路时为「全部 unchanged」），err 为失败原因。
// Node 以 ok=false + error 字符串表达失败，Go 用 error，分类信息在 result 里。
func (s *PriceSyncer) RunOnce(ctx context.Context) (*PriceUpdateResult, error) {
	if !s.running.CompareAndSwap(false, true) {
		return nil, errors.New("jobs: 价格同步已有一轮在执行")
	}
	defer s.running.Store(false)

	release, acquired, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	if !acquired {
		s.logger.Info("price_sync_skipped_lock_held", map[string]any{"lock": priceSyncLockName})
		return nil, nil
	}
	if release != nil {
		defer func() {
			if releaseErr := release(context.WithoutCancel(ctx)); releaseErr != nil {
				s.logger.Warn("price_sync_lock_release_failed", map[string]any{"error": releaseErr.Error()})
			}
		}()
	}

	started := time.Now()
	// 头读与全量共用同一次请求：先读开头 4 KiB 判版本，未变就关闭正文（不下载剩余 32 MB），
	// 变了就在同一连接内续读余下正文（不重发请求）。
	stream, err := s.openTableStream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.close()
	if result, skipped := s.headShortCircuit(ctx, stream.head); skipped {
		return result, nil
	}
	rawTable, err := stream.readAll(cloudPriceMaxBodyBytes)
	if err != nil {
		return nil, err
	}
	s.logger.Info("price_sync_fetched", map[string]any{
		"bytes":     len(rawTable),
		"elapsedMs": time.Since(started).Milliseconds(),
	})

	table, err := ParseCptTable(rawTable)
	if err != nil {
		return nil, err
	}
	converted := ConvertCptTable(table)
	if len(converted.Models) == 0 {
		// 与 Node 同口径：转换结果为空会退化成「清空全部非 manual 行」，必须拒绝。
		return nil, errors.New("云端价格表转换结果为空模型集,跳过同步以避免误删现有价格")
	}
	s.logger.Info("price_sync_converted", map[string]any{
		"models":    len(converted.Models),
		"vendors":   len(converted.Vendors),
		"version":   converted.Version,
		"elapsedMs": time.Since(started).Milliseconds(),
	})

	if result, skipped, err := s.shortCircuit(ctx, converted); err != nil {
		return nil, err
	} else if skipped {
		return result, nil
	}

	result, err := s.apply(ctx, converted)
	if err != nil {
		return nil, err
	}

	// 整表切换：清理云端已不存在的非 manual 行（含旧版价格表遗留行）。
	// 失败只告警：价格已写入，收尾失败不该把整轮判为失败（与 Node 一致）。
	if removed, cleanupErr := s.store.DeleteCloudPricesNotIn(ctx, modelNames(converted.Models)); cleanupErr != nil {
		s.logger.Warn("price_sync_stale_cleanup_failed", map[string]any{"error": cleanupErr.Error()})
	} else if removed > 0 {
		s.logger.Info("price_sync_stale_rows_removed", map[string]any{"removed": removed})
	}

	// 目录写入同样只告警：它是版本短路的依据，不是计费数据。
	if err := s.writeCatalog(ctx, converted); err != nil {
		s.logger.Warn("price_sync_catalog_write_failed", map[string]any{"error": err.Error()})
	}

	s.throttleMu.Lock()
	s.lastSyncedAt = time.Now()
	s.throttleMu.Unlock()

	s.logger.Info("price_sync_completed", map[string]any{
		"added":            len(result.Added),
		"updated":          len(result.Updated),
		"unchanged":        len(result.Unchanged),
		"failed":           len(result.Failed),
		"skippedConflicts": len(result.SkippedConflicts),
		"total":            result.Total,
		"elapsedMs":        time.Since(started).Milliseconds(),
	})
	return result, nil
}

// fetchTable 拉取价格表正文（cloud-price-table.ts:63 fetchCloudPriceTableJson）。
func (s *PriceSyncer) fetchTable(ctx context.Context) ([]byte, error) {
	stream, err := s.openTableStream(ctx)
	if err != nil {
		return nil, err
	}
	defer stream.close()
	return stream.readAll(cloudPriceMaxBodyBytes)
}

// tableStream 是一次请求的正文：开头 head 已读入内存，余下 body 未读。
//
// 拆成两段是为了让头读与全量共享同一次请求（见 RunOnce）。
type tableStream struct {
	head []byte
	body io.Reader
	// close 关闭响应体并收回请求的超时上下文，必须在读完正文后调用。
	close func()
}

// readAll 读完整个正文（已读的 head 在内），limit 是正文硬上限。
func (t *tableStream) readAll(limit int) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(io.MultiReader(bytes.NewReader(t.head), t.body), int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("云端价格表拉取失败：%w", err)
	}
	if len(body) > limit {
		return nil, fmt.Errorf("云端价格表拉取失败：正文超过 %d 字节上限", limit)
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, errors.New("云端价格表拉取失败：内容为空")
	}
	return body, nil
}

// tableVersionFromHead 从正文开头取顶层 version；取不到就返回空串（调用方据此降级全量）。
//
// 逐 token 走而不直接 json.Unmarshal：头读窗口按定义只装得下正文开头，整段 JSON 是残的，
// 严格解码必然失败。version 之前的字段只跳过、不解析（窗口内，代价有限）。
func tableVersionFromHead(head []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(head))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return ""
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}
		key, ok := token.(string)
		if !ok {
			return ""
		}
		if key == "version" {
			var version string
			if err := decoder.Decode(&version); err != nil {
				return ""
			}
			return version
		}
		var skipped json.RawMessage
		if err := decoder.Decode(&skipped); err != nil {
			return ""
		}
	}
	return ""
}

// openTableStream 发起请求、校验响应（重定向守卫 + 非 2xx 即失败）并读入正文开头。
func (s *PriceSyncer) openTableStream(ctx context.Context) (*tableStream, error) {
	response, cancel, err := s.openTable(ctx)
	if err != nil {
		return nil, err
	}
	closeStream := func() {
		_ = response.Body.Close()
		cancel()
	}
	head, err := io.ReadAll(io.LimitReader(response.Body, cloudPriceHeadProbeBytes))
	if err != nil {
		closeStream()
		return nil, fmt.Errorf("云端价格表拉取失败：%w", err)
	}
	return &tableStream{head: head, body: response.Body, close: closeStream}, nil
}

// openTable 发起请求并校验响应（重定向守卫 + 非 2xx 即失败），返回仍可读的响应。
//
// cancel 必须由调用方在读完正文后调用：它收回请求的超时上下文，与响应体的生命周期无关。
func (s *PriceSyncer) openTable(ctx context.Context) (*http.Response, context.CancelFunc, error) {
	expected, err := url.Parse(s.url)
	if err != nil {
		return nil, nil, fmt.Errorf("价格表地址非法: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, cloudPriceFetchTimeout)

	request, err := http.NewRequestWithContext(reqCtx, http.MethodGet, s.url, nil)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("价格表请求构造失败: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := s.client.Do(request)
	if err != nil {
		cancel()
		return nil, nil, fmt.Errorf("云端价格表拉取失败：%w", err)
	}

	// 重定向到非预期地址即失败（安全硬化，与 Node 同口径）。
	if response.Request != nil && response.Request.URL != nil {
		final := response.Request.URL
		if final.Scheme != expected.Scheme || final.Host != expected.Host || final.Path != expected.Path {
			_ = response.Body.Close()
			cancel()
			return nil, nil, errors.New("云端价格表拉取失败：重定向到非预期地址")
		}
	}

	if response.StatusCode < 200 || response.StatusCode > 299 {
		_ = response.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("云端价格表拉取失败：HTTP %d", response.StatusCode)
	}
	return response, cancel, nil
}

// headShortCircuit 用正文开头的 version 判定「未变更」，命中时调用方直接关闭正文，不再下载剩余部分。
//
// 上游无 ETag / Last-Modified / Range，条件请求不可用，故退一步：正文前 4 KiB 内即含 version
// （实测 <200 字节），读这么点就能算指纹。任一条件不成立（无 overwriteManual、版本为空、
// 目录读不到、指纹或行数不匹配、头读取不到 version）都判未命中，由调用方续读全量——
// 读不到 version 时退回的是改动前的行为，绝不会更差。
func (s *PriceSyncer) headShortCircuit(ctx context.Context, head []byte) (*PriceUpdateResult, bool) {
	if len(s.overwriteManual) > 0 {
		return nil, false
	}
	catalog, err := s.store.GetCloudPricingCatalog(ctx)
	if err != nil {
		s.logger.Debug("price_sync_short_circuit_check_failed", map[string]any{"error": err.Error()})
		return nil, false
	}
	if catalog == nil || catalog.Version == "" {
		return nil, false
	}
	version := tableVersionFromHead(head)
	if version == "" || versionFingerprint(version) != catalog.Version {
		return nil, false
	}
	cloudCount, err := s.store.CountCloudModelPrices(ctx)
	if err != nil {
		s.logger.Debug("price_sync_short_circuit_check_failed", map[string]any{"error": err.Error()})
		return nil, false
	}
	if cloudCount != catalog.ModelCount {
		return nil, false
	}

	// 模型名只存在于未下载的正文里，故短路路径不返回名单；消费方只读 len()。
	s.logger.Info("price_sync_unchanged_skipping_write", map[string]any{
		"version": version,
		"total":   catalog.ModelCount,
	})
	return &PriceUpdateResult{
		Added:            []string{},
		Updated:          []string{},
		Unchanged:        []string{},
		Failed:           []string{},
		Total:            catalog.ModelCount,
		SkippedConflicts: []string{},
	}, true
}

// shortCircuit 复刻版本短路（cloud-price-updater.ts:167-201）。
//
// 条件与 Node 完全一致：无 overwriteManual、指纹一致、且云端行数与上次写入一致。
// 读写失败按 Node 的降级语义记 debug 后继续整表重放。
func (s *PriceSyncer) shortCircuit(
	ctx context.Context,
	converted *ConvertedCptTable,
) (*PriceUpdateResult, bool, error) {
	if len(s.overwriteManual) > 0 || converted.Version == "" {
		return nil, false, nil
	}
	catalog, err := s.store.GetCloudPricingCatalog(ctx)
	if err != nil {
		s.logger.Debug("price_sync_short_circuit_check_failed", map[string]any{"error": err.Error()})
		return nil, false, nil
	}
	if catalog == nil || catalog.Version != versionFingerprint(converted.Version) {
		return nil, false, nil
	}
	cloudCount, err := s.store.CountCloudModelPrices(ctx)
	if err != nil {
		s.logger.Debug("price_sync_short_circuit_check_failed", map[string]any{"error": err.Error()})
		return nil, false, nil
	}
	if cloudCount != catalog.ModelCount {
		return nil, false, nil
	}

	names := modelNames(converted.Models)
	s.logger.Info("price_sync_unchanged_skipping_write", map[string]any{
		"version": converted.Version,
		"total":   len(names),
	})
	return &PriceUpdateResult{
		Added:            []string{},
		Updated:          []string{},
		Unchanged:        names,
		Failed:           []string{},
		Total:            len(names),
		SkippedConflicts: []string{},
	}, true, nil
}

// apply 复刻 processPriceTableInternal 的写入循环（src/actions/model-prices.ts:135-220）。
func (s *PriceSyncer) apply(
	ctx context.Context,
	converted *ConvertedCptTable,
) (*PriceUpdateResult, error) {
	manualNames, err := s.store.ListManualPriceModelNames(ctx)
	if err != nil {
		return nil, err
	}
	existing, err := s.store.ListLatestPriceRowsForSync(ctx)
	if err != nil {
		return nil, err
	}

	overwrite := make(map[string]struct{}, len(s.overwriteManual))
	for _, name := range s.overwriteManual {
		overwrite[name] = struct{}{}
	}

	names := modelNames(converted.Models)
	result := &PriceUpdateResult{
		Added:            make([]string, 0, len(names)/2),
		Updated:          make([]string, 0, 256),
		Unchanged:        make([]string, 0, len(names)/2),
		Failed:           make([]string, 0, 8),
		Total:            len(names),
		SkippedConflicts: make([]string, 0, 8),
	}

	started := time.Now()
	// 待写队列：分类一条写一条的老路径里，每次「整行替换」都要付一次
	// BEGIN/DELETE/INSERT/COMMIT（4 次往返），一批上万模型就是数万次。这里先把分类结果
	// 攒下来，循环结束后整批提交（插入 1 次、替换 4 次往返）。
	pending := make([]pendingPriceWrite, 0, len(names))
	// 进度日志里的 added/updated 是**已分类待写**数（写入在循环后统一 flush）。
	// 最终落库真值见 price_sync_completed（由 result 计数驱动）。
	classifiedAdded, classifiedUpdated := 0, 0
	for index, rawModelName := range names {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("价格同步被中断（已处理 %d/%d）: %w", index, len(names), err)
		}
		// 与 manual 记录入库时（upsertModelPrice 用 trim 后的名称）保持一致地归一化。
		modelName := trimSpace(rawModelName)
		if modelName == "" || modelName == metadataFieldSampleSpec {
			continue
		}
		priceData := converted.Models[rawModelName]

		// 本地优先：云端同步跳过用户手动维护的模型，除非显式列入覆盖列表。
		if _, isManual := manualNames[modelName]; isManual {
			if _, overridden := overwrite[modelName]; !overridden {
				result.SkippedConflicts = append(result.SkippedConflicts, modelName)
				result.Unchanged = append(result.Unchanged, modelName)
				continue
			}
		}

		encoded, err := json.Marshal(priceData)
		if err != nil {
			result.Failed = append(result.Failed, modelName)
			s.logger.Warn("price_sync_model_encode_failed", map[string]any{
				"model": modelName, "error": err.Error(),
			})
			continue
		}

		existingRow, found := existing[modelName]
		// 库里的价格只存了原始字节，仅在「来源一致」时才解析比对（解析后立即丢弃，不驻留）。
		priceUnchanged := false
		if found && existingRow.Source == cloudPriceSource {
			equal, equalErr := priceDataEqualRaw(existingRow.PriceData, priceData)
			if equalErr != nil {
				s.logger.Warn("price_sync_existing_price_decode_failed", map[string]any{
					"model": modelName, "error": equalErr.Error(),
				})
			}
			priceUnchanged = equal
		}
		switch {
		case !found:
			pending = append(pending, pendingPriceWrite{modelName: modelName, encoded: encoded, added: true})
			classifiedAdded++
		case !priceUnchanged:
			// 与 Node 同口径：整行替换（先删后插），而不是原地更新。
			pending = append(pending, pendingPriceWrite{modelName: modelName, encoded: encoded})
			classifiedUpdated++
		default:
			result.Unchanged = append(result.Unchanged, modelName)
		}

		if s.progressEvery > 0 && (index+1)%s.progressEvery == 0 {
			s.logger.Info("price_sync_progress", map[string]any{
				"done":      index + 1,
				"total":     len(names),
				"added":     classifiedAdded,
				"updated":   classifiedUpdated,
				"failed":    len(result.Failed),
				"elapsedMs": time.Since(started).Milliseconds(),
			})
		}
	}
	s.applyWrites(ctx, pending, result)
	return result, nil
}

// pendingPriceWrite 是一条已分类、待落库的写入。
//
// added=true 表示库中原本无这个模型（走插入），false 表示已存在且需要整行替换。
// 分类用的 existing 快照在循环开始前取一次，故分类结果与写入顺序无关。
type pendingPriceWrite struct {
	modelName string
	encoded   []byte
	added     bool
}

// applyWrites 把分类阶段攒下的写入整批交给 store，批量失败则退化回逐条写。
//
// 退化不是可选项：批量语句是原子的（插入单条语句、替换单事务），失败会把整批一起回滚，
// 而调用方要的是「哪些模型进了 added/updated、哪些进了 failed」。逐条重放能把结果与
// 归因恢复成与改前逐字一致；成功路径则省掉 N-1 轮的往返。
//
// 结果列表的追加顺序仍是**循环顺序**（先按 pending 原序补齐 added，再补齐 updated），
// 与改前逐条写时的顺序一致。
func (s *PriceSyncer) applyWrites(ctx context.Context, pending []pendingPriceWrite, result *PriceUpdateResult) {
	inserts := make([]store.ModelPriceWrite, 0, len(pending))
	replaces := make([]store.ModelPriceWrite, 0, len(pending))
	for _, item := range pending {
		write := store.ModelPriceWrite{ModelName: item.modelName, PriceData: json.RawMessage(item.encoded), Source: cloudPriceSource}
		if item.added {
			inserts = append(inserts, write)
			continue
		}
		replaces = append(replaces, write)
	}

	insertBatchOK := false
	if len(inserts) > 0 {
		if err := s.store.InsertModelPrices(ctx, inserts); err != nil {
			s.logger.Warn("price_sync_batch_insert_failed_fallback", map[string]any{
				"count": len(inserts), "error": err.Error(),
			})
		} else {
			insertBatchOK = true
		}
	}
	replaceBatchOK := false
	if len(replaces) > 0 {
		if err := s.store.ReplaceModelPrices(ctx, replaces); err != nil {
			s.logger.Warn("price_sync_batch_upsert_failed_fallback", map[string]any{
				"count": len(replaces), "error": err.Error(),
			})
		} else {
			replaceBatchOK = true
		}
	}

	for _, item := range pending {
		if !item.added {
			continue
		}
		if insertBatchOK {
			result.Added = append(result.Added, item.modelName)
			continue
		}
		if _, err := s.store.InsertModelPrice(ctx, item.modelName, item.encoded, cloudPriceSource); err != nil {
			result.Failed = append(result.Failed, item.modelName)
			s.logger.Warn("price_sync_model_insert_failed", map[string]any{
				"model": item.modelName, "error": err.Error(),
			})
			continue
		}
		result.Added = append(result.Added, item.modelName)
	}

	for _, item := range pending {
		if item.added {
			continue
		}
		if replaceBatchOK {
			result.Updated = append(result.Updated, item.modelName)
			continue
		}
		if _, err := s.store.AdminUpsertModelPrice(ctx, item.modelName, json.RawMessage(item.encoded), cloudPriceSource); err != nil {
			result.Failed = append(result.Failed, item.modelName)
			s.logger.Warn("price_sync_model_upsert_failed", map[string]any{
				"model": item.modelName, "error": err.Error(),
			})
			continue
		}
		result.Updated = append(result.Updated, item.modelName)
	}
}

// writeCatalog 复刻 cloud-price-updater.ts:125-146 的目录写入。
//
// modelCount 记的是**写库后的实际非 manual 行数**，不是云端全量数：manual 冲突被跳过的模型
// 不会落库，记全量数会让版本短路的行数比对永久失配。
func (s *PriceSyncer) writeCatalog(ctx context.Context, converted *ConvertedCptTable) error {
	cloudRowCount, err := s.store.CountCloudModelPrices(ctx)
	if err != nil {
		return err
	}
	providers, err := json.Marshal(converted.Providers)
	if err != nil {
		return fmt.Errorf("provider 字典序列化失败: %w", err)
	}
	vendors, err := json.Marshal(converted.Vendors)
	if err != nil {
		return fmt.Errorf("vendor 汇总序列化失败: %w", err)
	}
	version := converted.Version
	if version != "" {
		version = versionFingerprint(version)
	}
	var refreshedAt *string
	if converted.RefreshedAt != "" {
		if parsed, parseErr := time.Parse(time.RFC3339, converted.RefreshedAt); parseErr == nil {
			iso := parsed.UTC().Format("2006-01-02T15:04:05.000Z")
			refreshedAt = &iso
		}
		// 无法解析时保持 NULL：Node 侧同样把非法时间落成 null。
	}
	return s.store.UpsertCloudPricingCatalog(ctx, store.CloudPricingCatalogInput{
		Version:     version,
		Currency:    converted.Currency,
		RefreshedAt: refreshedAt,
		Providers:   providers,
		Vendors:     vendors,
		ModelCount:  cloudRowCount,
	})
}

// RequestSync 复刻 requestCloudPriceTableSync 的节流与去重语义（异步执行）。
//
// 调用点（Node：response-handler.ts:6735 命中未知模型时）在 Go 数据面里尚未接线，
// 接线只需在响应处理里调本方法一次；接线前本方法没有调用点。
func (s *PriceSyncer) RequestSync(reason string, throttle time.Duration) bool {
	if throttle <= 0 {
		throttle = 5 * time.Minute
	}
	s.throttleMu.Lock()
	if !s.lastSyncedAt.IsZero() && time.Since(s.lastSyncedAt) < throttle {
		s.throttleMu.Unlock()
		return false
	}
	if s.scheduling {
		s.throttleMu.Unlock()
		return false
	}
	s.scheduling = true
	s.throttleMu.Unlock()

	// 与 Node 的 AsyncTaskManager 去重等价：正在跑就不再排在后面。
	if s.running.Load() {
		s.throttleMu.Lock()
		s.scheduling = false
		s.throttleMu.Unlock()
		return false
	}

	go func() {
		defer func() {
			s.throttleMu.Lock()
			s.scheduling = false
			s.throttleMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := s.RunOnce(ctx); err != nil {
			s.logger.Warn("price_sync_requested_run_failed", map[string]any{
				"reason": reason, "error": err.Error(),
			})
		}
	}()
	return true
}

// versionFingerprint 复刻 versionFingerprint：`<version>+cvt<rev>`。
func versionFingerprint(version string) string {
	return fmt.Sprintf("%s+cvt%d", version, cptConverterRevision)
}

// modelNames 返回模型的排序键列表（消除 map 随机序，也让日志与写入顺序确定）。
func modelNames(models map[string]map[string]any) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// priceDataEqualRaw 比对「库中原始 jsonb 文本」与「云端表里已解析的价格对象」。
//
// 为什么不在读行时解析：见 store.PriceSyncExistingRow（驻留会把峰值堆放大 ~5x）。这里只在
// 判定时解析库侧那一个值，判完即弃，不驻留。
//
// 解析失败返回 (false, err)：按「不相等」处理（该行重写一次），由调用方记 warn。
func priceDataEqualRaw(existing []byte, incoming any) (bool, error) {
	var decoded any
	if err := json.Unmarshal(existing, &decoded); err != nil {
		return false, fmt.Errorf("解析既有价格数据失败: %w", err)
	}
	return priceDataEqual(decoded, incoming), nil
}

// priceDataEqual 复刻 src/actions/model-prices.ts:42 isPriceDataEqual。
//
// 两侧都先做规范化（对象键排序、跳过原型污染键、数组保序），再比对文本。
// Go 的 encoding/json 对 map 键排序，故规范化只需处理「跳过危险键」与递归。
// 数值按数值比对（不是按文本）：jsonb 的文本形式与 Go 的浮点格式可能不同，
// 而两端要回答的是「价格是否变化」这个问题。
func priceDataEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(canonicalizePriceNode(left))
	rightJSON, rightErr := json.Marshal(canonicalizePriceNode(right))
	if leftErr != nil || rightErr != nil {
		return false
	}
	return bytes.Equal(leftJSON, rightJSON)
}

func canonicalizePriceNode(node any) any {
	switch typed := node.(type) {
	case nil:
		return nil
	case []any:
		canonical := make([]any, 0, len(typed))
		for _, item := range typed {
			canonical = append(canonical, canonicalizePriceNode(item))
		}
		return canonical
	case map[string]any:
		canonical := make(map[string]any, len(typed))
		for key, value := range typed {
			if isUnsafeKey(key) {
				continue
			}
			canonical[key] = canonicalizePriceNode(value)
		}
		return canonical
	default:
		return node
	}
}

// trimSpace 对应 TS 的 String#trim（含 Unicode 空白）。
func trimSpace(value string) string {
	return strings.TrimFunc(value, isSpaceRune)
}
