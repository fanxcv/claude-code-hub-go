package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// fakePriceStore 记录同步器对 store 的每一次调用，用来在无库条件下断言写入语义。
//
// 为什么不是真的打库：整表切换会删掉「本次表内不存在的非 manual 行」，
// 用三行合成价格表驱动真实库等于清空该库的云端价格。
type fakePriceStore struct {
	manualNames map[string]struct{}
	existing    map[string]store.PriceSyncExistingRow
	cloudCount  int
	catalog     *store.CloudPricingCatalogRow

	inserts       []string
	upserts       []string
	deleteKeep    []string
	catalogWrites []store.CloudPricingCatalogInput
	// failInsert 用于让特定模型写入失败（Node 的 failed 分类）。
	failInsert map[string]bool
	// 调用计数：钉住「正常路径只走批量原语、逐条只用于失败后的退化写」。
	batchInsertCalls  int
	batchReplaceCalls int
	rowInsertCalls    int
	rowUpsertCalls    int
}

func newFakePriceStore() *fakePriceStore {
	return &fakePriceStore{
		manualNames: map[string]struct{}{},
		existing:    map[string]store.PriceSyncExistingRow{},
		failInsert:  map[string]bool{},
	}
}

func (f *fakePriceStore) ListManualPriceModelNames(context.Context) (map[string]struct{}, error) {
	return f.manualNames, nil
}

func (f *fakePriceStore) ListLatestPriceRowsForSync(context.Context) (map[string]store.PriceSyncExistingRow, error) {
	return f.existing, nil
}

func (f *fakePriceStore) InsertModelPrice(_ context.Context, modelName string, _ []byte, _ string) (int64, error) {
	f.rowInsertCalls++
	if f.failInsert[modelName] {
		return 0, errors.New("插入失败（测试注入）")
	}
	f.inserts = append(f.inserts, modelName)
	f.cloudCount++
	return int64(len(f.inserts)), nil
}

func (f *fakePriceStore) AdminUpsertModelPrice(_ context.Context, modelName string, _ json.RawMessage, _ string) (store.AdminModelPrice, error) {
	f.rowUpsertCalls++
	f.upserts = append(f.upserts, modelName)
	return store.AdminModelPrice{ModelName: modelName}, nil
}

// InsertModelPrices / ReplaceModelPrices 模拟真库的**原子**批量写：
// 先校验全部入参，任一条不可写就整批不落副作用地失败（对应真实事务回滚），
// 于是生产侧的退化写能拿到与改前逐条写完全相同的调用序列。
func (f *fakePriceStore) InsertModelPrices(_ context.Context, writes []store.ModelPriceWrite) error {
	f.batchInsertCalls++
	for _, write := range writes {
		if f.failInsert[write.ModelName] {
			return errors.New("批量插入失败（测试注入）")
		}
	}
	for _, write := range writes {
		f.inserts = append(f.inserts, write.ModelName)
		f.cloudCount++
	}
	return nil
}

func (f *fakePriceStore) ReplaceModelPrices(_ context.Context, writes []store.ModelPriceWrite) error {
	f.batchReplaceCalls++
	// 与逐条 AdminUpsertModelPrice 同口径：本替身不对替换注入失败。
	for _, write := range writes {
		f.upserts = append(f.upserts, write.ModelName)
	}
	return nil
}

func (f *fakePriceStore) DeleteCloudPricesNotIn(_ context.Context, keep []string) (int64, error) {
	f.deleteKeep = append([]string(nil), keep...)
	return 0, nil
}

func (f *fakePriceStore) CountCloudModelPrices(context.Context) (int, error) {
	return f.cloudCount, nil
}

func (f *fakePriceStore) GetCloudPricingCatalog(context.Context) (*store.CloudPricingCatalogRow, error) {
	return f.catalog, nil
}

func (f *fakePriceStore) UpsertCloudPricingCatalog(_ context.Context, input store.CloudPricingCatalogInput) error {
	f.catalogWrites = append(f.catalogWrites, input)
	f.catalog = &store.CloudPricingCatalogRow{
		Version: input.Version, Currency: input.Currency, ModelCount: input.ModelCount,
	}
	return nil
}

// cptFixture 造一张最小但结构完整的 CPT v1 表。
func cptFixture(version string) string {
	return fmt.Sprintf(`{
		"schema": "cchp.pricing-table/v1",
		"version": %q,
		"currency": "USD",
		"refreshed_at": "2026-01-02T03:04:05Z",
		"providers": {"anthropic": {"name": "Anthropic", "icon": "anthropic.svg"}},
		"models": [
			{
				"slug": "sonnet", "model_name": "claude-sonnet-4-5", "vendor": "anthropic",
				"display_name": "Claude Sonnet 4.5", "aliases": ["sonnet-4-5"],
				"max_output_tokens": 8192,
				"pricing": [{"provider": "anthropic", "official": true, "source": "official",
					"charges": {"prompt": {"price": "3", "unit": "per_M_tokens"},
					            "completion": {"price": "15", "unit": "per_M_tokens"}}}]
			},
			{
				"slug": "manual-guarded", "model_name": "manual-model", "vendor": "anthropic",
				"display_name": "Manual", 
				"pricing": [{"provider": "anthropic", "official": true, "source": "official",
					"charges": {"prompt": {"price": "1", "unit": "per_M_tokens"}}}]
			}
		]
	}`, version)
}

// newPriceSyncFixture 组装一个指向本地 httptest 的同步器。
func newPriceSyncFixture(
	t *testing.T,
	storeImpl priceStore,
	body string,
) *PriceSyncer {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	syncer, err := NewPriceSyncer(PriceSyncOptions{
		Pools: &store.Pools{},
		URL:   server.URL,
		store: storeImpl,
		acquire: func(context.Context) (func(context.Context) error, bool, error) {
			return func(context.Context) error { return nil }, true, nil
		},
	})
	if err != nil {
		t.Fatalf("构造同步器失败: %v", err)
	}
	return syncer
}

func TestPriceSyncClassifiesAddedUpdatedUnchanged(t *testing.T) {
	fake := newFakePriceStore()
	// 手动价保护的目标：既有 manual 行，且不在 cloud 表来源里。
	fake.manualNames["manual-model"] = struct{}{}
	// 既有云端行且价格与表内一致 → unchanged。
	syncer := newPriceSyncFixture(t, fake, cptFixture("v1"))

	// 预置：claude-sonnet-4-5 已有同价同源的旧行？
	// 第一轮：两个模型都不存在 → added（manual-model 会被 manual 保护跳过）。
	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if result.Total == 0 {
		t.Fatal("总模型数不应为 0")
	}
	// canonical 与别名都被写入（别名展开为独立行），manual-model 被跳过。
	sort.Strings(fake.inserts)
	wantInserted := []string{"claude-sonnet-4-5", "sonnet-4-5"}
	if strings.Join(fake.inserts, ",") != strings.Join(wantInserted, ",") {
		t.Fatalf("新增行应为 %v, 实际 %v", wantInserted, fake.inserts)
	}
	if len(result.SkippedConflicts) != 1 || result.SkippedConflicts[0] != "manual-model" {
		t.Fatalf("manual 保护应记 skippedConflicts=[manual-model], 实际 %v", result.SkippedConflicts)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("不应有失败项: %v", result.Failed)
	}
	// 整表切换的保留列表必须覆盖本次表内的**全部键**（含别名与被 manual 保护跳过的键）：
	// 保护只决定「写不写」，不决定「留不留」。
	sort.Strings(fake.deleteKeep)
	wantKeep := []string{"claude-sonnet-4-5", "manual-model", "sonnet-4-5"}
	if strings.Join(fake.deleteKeep, ",") != strings.Join(wantKeep, ",") {
		t.Fatalf("保留列表应为 %v, 实际 %v", wantKeep, fake.deleteKeep)
	}
	// 目录写入：版本带指纹、行数取写库后的实际云端行数。
	if len(fake.catalogWrites) != 1 {
		t.Fatalf("应写一次目录, 实际 %d", len(fake.catalogWrites))
	}
	if fake.catalogWrites[0].Version != "v1+cvt1" {
		t.Fatalf("版本指纹应为 v1+cvt1, 实际 %q", fake.catalogWrites[0].Version)
	}
	if fake.catalogWrites[0].ModelCount != len(fake.inserts) {
		t.Fatalf("目录行数应为实际写入行数 %d, 实际 %d",
			len(fake.inserts), fake.catalogWrites[0].ModelCount)
	}
}

func TestPriceSyncSecondRunUpdatesOnlyChangedRows(t *testing.T) {
	fake := newFakePriceStore()
	syncer := newPriceSyncFixture(t, fake, cptFixture("v1"))
	if _, err := syncer.RunOnce(context.Background()); err != nil {
		t.Fatalf("第一轮失败: %v", err)
	}

	// 把第一轮写入的行原样放进「既有」，再跑第二轮：应全部判为 unchanged。
	// 夹具表共三键：canonical、别名展开项、以及 manual-model（本用例未列入 manual 保护，
	// 故也会被当作云端行写入）。
	for _, name := range []string{"claude-sonnet-4-5", "sonnet-4-5", "manual-model"} {
		fake.existing[name] = store.PriceSyncExistingRow{
			ModelName: name, Source: cloudPriceSource, PriceData: syncerStoredRow(t, name),
		}
	}
	fake.inserts = nil
	fake.upserts = nil
	// 版本短路会让第二轮直接跳过，故清空目录以走到写入循环。
	fake.catalog = nil

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("第二轮失败: %v", err)
	}
	if len(result.Unchanged) != 3 {
		t.Fatalf("内容未变应判 unchanged=3, 实际 %v", result.Unchanged)
	}
	if len(fake.upserts) != 0 || len(fake.inserts) != 0 {
		t.Fatalf("不变的行不应写库, inserts=%v upserts=%v", fake.inserts, fake.upserts)
	}

	// 改变其中一行的价格 → 该行必须走「删旧插新」（upsert）。
	fake.existing["claude-sonnet-4-5"] = store.PriceSyncExistingRow{
		ModelName: "claude-sonnet-4-5", Source: cloudPriceSource,
		PriceData: map[string]any{"mode": "chat", "input_cost_per_token": 999.0},
	}
	fake.upserts = nil
	// 清空目录：否则第二轮写入的指纹+行数会让第三轮走版本短路，测不到写入分支。
	fake.catalog = nil
	result, err = syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("第三轮失败: %v", err)
	}
	if len(fake.upserts) != 1 || fake.upserts[0] != "claude-sonnet-4-5" {
		t.Fatalf("价格变化的行应走 upsert, 实际 %v", fake.upserts)
	}
	if len(result.Updated) != 1 || result.Updated[0] != "claude-sonnet-4-5" {
		t.Fatalf("updated 应为该行, 实际 %v", result.Updated)
	}
}

// syncerStoredRow 取同步器对某模型实际生成的 JSON 对象（用于构造「既有同价行」）。
func syncerStoredRow(t *testing.T, modelName string) any {
	t.Helper()
	table, err := ParseCptTable([]byte(cptFixture("v1")))
	if err != nil {
		t.Fatalf("解析夹具失败: %v", err)
	}
	converted := ConvertCptTable(table)
	row, exists := converted.Models[modelName]
	if !exists {
		t.Fatalf("夹具里没有模型 %s", modelName)
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	return decoded
}

func TestPriceSyncOverwriteManualReplacesGuardedRow(t *testing.T) {
	fake := newFakePriceStore()
	fake.manualNames["manual-model"] = struct{}{}
	syncer := newPriceSyncFixture(t, fake, cptFixture("v1"))
	syncer.overwriteManual = []string{"manual-model"}

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(result.SkippedConflicts) != 0 {
		t.Fatalf("显式列入覆盖列表后不应再跳过, 实际 %v", result.SkippedConflicts)
	}
	found := false
	for _, name := range fake.inserts {
		if name == "manual-model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("覆盖列表内的 manual 模型应被写入, 实际 inserts=%v", fake.inserts)
	}
}

func TestPriceSyncVersionShortCircuitSkipsAllWrites(t *testing.T) {
	fake := newFakePriceStore()
	// 目录指纹与云端行数与本轮一致 → 整轮跳过。
	fake.catalog = &store.CloudPricingCatalogRow{Version: "v1+cvt1", Currency: "USD", ModelCount: 3}
	fake.cloudCount = 3
	syncer := newPriceSyncFixture(t, fake, cptFixture("v1"))

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(result.Unchanged) != 3 || len(result.Added) != 0 || len(result.Updated) != 0 {
		t.Fatalf("短路应全部判 unchanged, 实际 %+v", result)
	}
	if len(fake.inserts) != 0 || len(fake.upserts) != 0 || fake.deleteKeep != nil {
		t.Fatalf("短路后不应有任何写入: inserts=%v upserts=%v keep=%v",
			fake.inserts, fake.upserts, fake.deleteKeep)
	}
	if len(fake.catalogWrites) != 0 {
		t.Fatalf("短路后不应重写目录")
	}
}

func TestPriceSyncVersionShortCircuitRequiresMatchingRowCount(t *testing.T) {
	fake := newFakePriceStore()
	// 指纹一致但行数不一致（例如上一轮有写入失败）→ 必须整表重放。
	fake.catalog = &store.CloudPricingCatalogRow{Version: "v1+cvt1", ModelCount: 999}
	fake.cloudCount = 1
	syncer := newPriceSyncFixture(t, fake, cptFixture("v1"))

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(result.Added) == 0 {
		t.Fatalf("行数不匹配时必须重放整表, 实际 %+v", result)
	}
}

func TestPriceSyncLockHeldSkipsWithoutWrites(t *testing.T) {
	fake := newFakePriceStore()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(cptFixture("v1")))
	}))
	defer server.Close()
	syncer, err := NewPriceSyncer(PriceSyncOptions{
		Pools: &store.Pools{}, URL: server.URL, store: fake,
		acquire: func(context.Context) (func(context.Context) error, bool, error) {
			return nil, false, nil
		},
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("未取得锁不应报错: %v", err)
	}
	if result != nil {
		t.Fatalf("未取得锁应返回空结果, 实际 %+v", result)
	}
	if len(fake.inserts) != 0 || len(fake.upserts) != 0 {
		t.Fatal("未取得锁时不得写库")
	}
}

func TestPriceSyncFetchFailures(t *testing.T) {
	fixtureTable := cptFixture("v1")
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "HTTP 500",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: "HTTP 500",
		},
		{
			name: "空正文",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("   \n"))
			},
			wantErr: "内容为空",
		},
		{
			name: "重定向到非预期地址",
			handler: func(w http.ResponseWriter, r *http.Request) {
				// 目标路径返回 200：这样客户端能跟到底，守卫判的是**最终地址**。
				// 指向别的域名会在 DNS 层先失败，测不到守卫。
				if r.URL.Path != "/elsewhere.json" {
					http.Redirect(w, r, "/elsewhere.json", http.StatusFound)
					return
				}
				_, _ = w.Write([]byte("{}"))
			},
			wantErr: "重定向到非预期地址",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(testCase.handler)
			defer server.Close()
			syncer, err := NewPriceSyncer(PriceSyncOptions{
				Pools: &store.Pools{}, URL: server.URL, store: newFakePriceStore(),
				acquire: func(context.Context) (func(context.Context) error, bool, error) {
					return func(context.Context) error { return nil }, true, nil
				},
			})
			if err != nil {
				t.Fatalf("构造失败: %v", err)
			}
			_, err = syncer.RunOnce(context.Background())
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("应报含 %q 的错误, 实际 %v", testCase.wantErr, err)
			}
			_ = fixtureTable
		})
	}
}

func TestPriceSyncEmptyConvertedTableIsRejected(t *testing.T) {
	// 表结构合法但没有任何可计费变体 → 转换结果为空，必须拒绝（否则会清空全部非 manual 行）。
	body := `{
		"schema": "cchp.pricing-table/v1", "version": "v1", "currency": "USD",
		"providers": {"anthropic": {"name": "Anthropic"}},
		"models": [{"slug": "s", "model_name": "empty", "vendor": "anthropic",
			"pricing": [{"provider": "anthropic", "official": true, "source": "official",
				"charges": {"prompt": {"price": "1", "unit": "per_second"}}}]}]
	}`
	syncer := newPriceSyncFixture(t, newFakePriceStore(), body)
	if _, err := syncer.RunOnce(context.Background()); err == nil {
		t.Fatal("空模型集必须报错")
	}
}

func TestPriceSyncFailedModelIsClassifiedAndOthersProceed(t *testing.T) {
	fake := newFakePriceStore()
	fake.failInsert["sonnet-4-5"] = true
	syncer := newPriceSyncFixture(t, fake, cptFixture("v1"))

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("单模型失败不应让整轮失败: %v", err)
	}
	if len(result.Failed) != 1 || result.Failed[0] != "sonnet-4-5" {
		t.Fatalf("失败分类应为 sonnet-4-5, 实际 %v", result.Failed)
	}
	if len(result.Added) != 2 {
		t.Fatalf("其余模型应正常写入, 实际 added=%v", result.Added)
	}
}

func TestPriceSyncRequestSyncThrottles(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(cptFixture("v1")))
	}))
	defer server.Close()

	fake := newFakePriceStore()
	syncer, err := NewPriceSyncer(PriceSyncOptions{
		Pools: &store.Pools{}, URL: server.URL, store: fake,
		acquire: func(context.Context) (func(context.Context) error, bool, error) {
			return func(context.Context) error { return nil }, true, nil
		},
	})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	// 首轮：lastSyncedAt 为零值，允许触发。
	if !syncer.RequestSync("missing-model", time.Hour) {
		t.Fatal("首次请求应被接受")
	}
	// 等首轮把节流位写上。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		syncer.throttleMu.Lock()
		done := !syncer.lastSyncedAt.IsZero() && !syncer.scheduling
		syncer.throttleMu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if syncer.RequestSync("missing-model", time.Hour) {
		t.Fatal("节流窗口内不应再次触发")
	}
}

// TestPriceSyncUsesBatchWritesNotRowByRow 钉住本次改动的性能契约：
// 一轮同步的写往返是**常数次**（新增一次、替换一次），而不是每个模型一次。
//
// 改前：每个「新增」一次 InsertModelPrice；每个「替换」四次往返（BEGIN/DELETE/INSERT/COMMIT）。
// 改后：两类各一次；逐条方法只在批量失败后的退化写里出现。
func TestPriceSyncUsesBatchWritesNotRowByRow(t *testing.T) {
	fake := newFakePriceStore()
	// 既有行来源不同 → 判 updated（走批量替换）；别名 sonnet-4-5 不存在 → added（走批量插入）。
	fake.existing["claude-sonnet-4-5"] = store.PriceSyncExistingRow{
		ModelName: "claude-sonnet-4-5", Source: "litellm",
		PriceData: map[string]any{"mode": "chat"},
	}
	syncer := newPriceSyncFixture(t, fake, cptFixture("v1"))

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if len(result.Added) == 0 || len(result.Updated) == 0 {
		t.Fatalf("夹具应同时产生 added 与 updated：added=%v updated=%v", result.Added, result.Updated)
	}
	if fake.batchInsertCalls != 1 {
		t.Fatalf("新增应只走一次批量插入，实际 %d 次", fake.batchInsertCalls)
	}
	if fake.batchReplaceCalls != 1 {
		t.Fatalf("替换应只走一次批量替换，实际 %d 次", fake.batchReplaceCalls)
	}
	if fake.rowInsertCalls != 0 || fake.rowUpsertCalls != 0 {
		t.Fatalf("批量成功时不该退化逐条写：insert=%d upsert=%d", fake.rowInsertCalls, fake.rowUpsertCalls)
	}
	if len(fake.inserts) != len(result.Added) || len(fake.upserts) != len(result.Updated) {
		t.Fatalf("落库行数应与分类结果一致：inserts=%v added=%v upserts=%v updated=%v",
			fake.inserts, result.Added, fake.upserts, result.Updated)
	}
}

// TestPriceSyncFallsBackToRowByRowOnBatchFailure 钉住退化写的语义：
// 批量语句是原子的，任一行不可写就整批回滚；此时必须逐条重放，才能把
// 「哪个模型进了 failed」恢复得与改前逐条写一致。
func TestPriceSyncFallsBackToRowByRowOnBatchFailure(t *testing.T) {
	fake := newFakePriceStore()
	fake.failInsert["sonnet-4-5"] = true
	syncer := newPriceSyncFixture(t, fake, cptFixture("v1"))

	result, err := syncer.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if fake.batchInsertCalls != 1 {
		t.Fatalf("应先试一次批量插入，实际 %d 次", fake.batchInsertCalls)
	}
	if fake.rowInsertCalls == 0 {
		t.Fatal("批量失败后必须退化回逐条写，实际一次逐条插入都没发生")
	}
	if len(result.Failed) != 1 || result.Failed[0] != "sonnet-4-5" {
		t.Fatalf("失败归因应精确到 model：期望 [sonnet-4-5]，实际 %v", result.Failed)
	}
	// 同一批里的其他项照旧落库（夹具里还有 manual-model，故不要求恰好一条）。
	if !slices.Contains(result.Added, "claude-sonnet-4-5") {
		t.Fatalf("未失败的行应照旧落库：实际 added=%v", result.Added)
	}
	if slices.Contains(result.Added, "sonnet-4-5") {
		t.Fatalf("注入失败的行不该出现在 added：%v", result.Added)
	}
}
