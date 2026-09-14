package pricing

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// fakeStore 是写入分类的无库替身：记录每次插入/替换，供逐条断言分类结果。
type fakeStore struct {
	manual   map[string]struct{}
	existing map[string]store.PriceSyncExistingRow

	inserted []capturedWrite
	upserted []capturedWrite
	failOn   map[string]bool
}

type capturedWrite struct {
	modelName string
	source    string
	priceData string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		manual:   map[string]struct{}{},
		existing: map[string]store.PriceSyncExistingRow{},
		failOn:   map[string]bool{},
	}
}

func (f *fakeStore) ListManualPriceModelNames(context.Context) (map[string]struct{}, error) {
	return f.manual, nil
}

func (f *fakeStore) ListLatestPriceRowsForSync(context.Context) (map[string]store.PriceSyncExistingRow, error) {
	return f.existing, nil
}

func (f *fakeStore) InsertModelPrice(_ context.Context, modelName string, priceData []byte, source string) (int64, error) {
	if f.failOn[modelName] {
		return 0, errWriteRefused
	}
	f.inserted = append(f.inserted, capturedWrite{modelName: modelName, source: source, priceData: string(priceData)})
	return int64(len(f.inserted)), nil
}

func (f *fakeStore) AdminUpsertModelPrice(_ context.Context, modelName string, priceData json.RawMessage, source string) (store.AdminModelPrice, error) {
	if f.failOn[modelName] {
		return store.AdminModelPrice{}, errWriteRefused
	}
	f.upserted = append(f.upserted, capturedWrite{modelName: modelName, source: source, priceData: string(priceData)})
	return store.AdminModelPrice{ModelName: modelName, Source: source, PriceData: priceData}, nil
}

var errWriteRefused = &writeRefusedError{}

type writeRefusedError struct{}

func (e *writeRefusedError) Error() string { return "写入被测试替身拒绝" }

func testLogger() *logx.Logger { return logx.New(io.Discard) }

func entry(name, priceData string) Entry {
	return Entry{Name: name, Data: json.RawMessage(priceData)}
}

// TestWriteEntriesClassification 逐条覆盖 Node 的分类判定顺序与结果（无库）。
func TestWriteEntriesClassification(t *testing.T) {
	fake := newFakeStore()
	fake.manual["manual-keep"] = struct{}{}
	fake.manual["manual-override"] = struct{}{}
	fake.existing["changed-source"] = store.PriceSyncExistingRow{
		ModelName: "changed-source", Source: "litellm",
		PriceData: map[string]any{"mode": "chat", "input_cost_per_token": 0.5},
	}
	fake.existing["same-row"] = store.PriceSyncExistingRow{
		ModelName: "same-row", Source: SourceCloud,
		PriceData: map[string]any{"mode": "chat", "input_cost_per_token": 1.0, "vendor": "anthropic"},
	}
	// 已存在的 manual 行 + 列入 overwriteManual → 必须走替换（而非被保护跳过）。
	fake.existing["manual-override"] = store.PriceSyncExistingRow{
		ModelName: "manual-override", Source: SourceManual,
		PriceData: map[string]any{"mode": "chat", "input_cost_per_token": 1.5},
	}
	fake.failOn["boom"] = true

	entries := []Entry{
		entry("brand-new", `{"mode":"chat","input_cost_per_token":0.1}`),
		entry("changed-source", `{"mode":"chat","input_cost_per_token":0.9}`),
		entry("same-row", `{"vendor":"anthropic","input_cost_per_token":1,"mode":"chat"}`), // 键序不同，值相同
		entry("manual-keep", `{"mode":"chat","input_cost_per_token":1}`),
		entry("manual-override", `{"mode":"chat","input_cost_per_token":2}`),
		entry("no-mode", `{"input_cost_per_token":1}`),
		entry("not-object", `"text"`),
		entry("null-value", `null`),
		entry("boom", `{"mode":"chat","input_cost_per_token":1}`),
		entry("   ", `{"mode":"chat"}`),
		entry("sample_spec", `{"mode":"chat"}`),
	}

	result, err := WriteEntries(context.Background(), fake, entries, SourceCloud,
		[]string{"manual-override"}, testLogger())
	if err != nil {
		t.Fatalf("WriteEntries 失败: %v", err)
	}

	if got, want := strings.Join(result.Added, ","), "brand-new"; got != want {
		t.Errorf("added: got=%q want=%q", got, want)
	}
	// changed-source（来源不同）、manual-override（显式覆盖 → 走替换而非新增）；顺序 = 入参顺序。
	if got, want := strings.Join(result.Updated, ","), "changed-source,manual-override"; got != want {
		t.Errorf("updated: got=%q want=%q", got, want)
	}
	// manual-keep（被保护，同时记 unchanged）、same-row（值相同）、manual-override 之外无它
	if got, want := strings.Join(result.Unchanged, ","), "same-row,manual-keep"; got != want {
		t.Errorf("unchanged: got=%q want=%q", got, want)
	}
	if got, want := strings.Join(result.Failed, ","), "no-mode,not-object,null-value,boom"; got != want {
		t.Errorf("failed: got=%q want=%q", got, want)
	}
	if got, want := strings.Join(result.SkippedConflicts, ","), "manual-keep"; got != want {
		t.Errorf("skippedConflicts: got=%q want=%q", got, want)
	}
	// total 计数与 Node 一致：入参条目数（含会被判失败/被保护的条目，不含既已过滤的空名与元数据字段）
	if want := len(entries); result.Total != want {
		t.Errorf("total: got=%d want=%d", result.Total, want)
	}
	if result.SkippedConflicts == nil {
		t.Error("skippedConflicts 必须是数组（Node 恒为 []，不是 null）")
	}
	// 写入来源必须照传入参，而不是硬编码 cloud。
	for _, write := range append(append([]capturedWrite{}, fake.inserted...), fake.upserted...) {
		if write.source != SourceCloud {
			t.Errorf("写入 %s 的 source 应为 %s，实际 %s", write.modelName, SourceCloud, write.source)
		}
	}
}

// TestWriteEntriesManualSourceBypassesProtection 用户显式上传（source=manual）是权威导入：
// 即使模型已存在 manual 行、且未列入 overwriteManual，也必须正常替换（Node 的
// `source !== 'manual' && isManualPrice` 分支不会命中）。
func TestWriteEntriesManualSourceBypassesProtection(t *testing.T) {
	fake := newFakeStore()
	fake.manual["existing-manual"] = struct{}{}
	fake.existing["existing-manual"] = store.PriceSyncExistingRow{
		ModelName: "existing-manual", Source: SourceManual,
		PriceData: map[string]any{"mode": "chat", "input_cost_per_token": 9.0},
	}

	result, err := WriteEntries(context.Background(), fake,
		[]Entry{entry("existing-manual", `{"mode":"chat","input_cost_per_token":0.2}`)},
		SourceManual, nil, testLogger())
	if err != nil {
		t.Fatalf("WriteEntries 失败: %v", err)
	}
	if len(result.SkippedConflicts) != 0 {
		t.Errorf("source=manual 不该触发 manual 保护：skippedConflicts=%v", result.SkippedConflicts)
	}
	if got, want := strings.Join(result.Updated, ","), "existing-manual"; got != want {
		t.Errorf("updated: got=%q want=%q", got, want)
	}
	if len(fake.upserted) != 1 || fake.upserted[0].source != SourceManual {
		t.Fatalf("应以 manual 来源替换一行，实际写入: %+v", fake.upserted)
	}
}

// TestWriteEntriesTrimmedNames 名字 trim 后再参与 manual 保护与写入（Node 同判）。
func TestWriteEntriesTrimmedNames(t *testing.T) {
	fake := newFakeStore()
	fake.manual["  padded-model  "] = struct{}{} // Node 侧 manual 记录入库时已 trim，故库里是裸名
	fake.manual["padded-model"] = struct{}{}

	result, err := WriteEntries(context.Background(), fake,
		[]Entry{entry("  padded-model  ", `{"mode":"chat","input_cost_per_token":1}`)},
		SourceCloud, nil, testLogger())
	if err != nil {
		t.Fatalf("WriteEntries 失败: %v", err)
	}
	if len(result.SkippedConflicts) != 1 || result.SkippedConflicts[0] != "padded-model" {
		t.Errorf("trim 后的名字应命中 manual 保护：%v", result.SkippedConflicts)
	}
	if len(fake.inserted) != 0 || len(fake.upserted) != 0 {
		t.Errorf("被保护的条目不该写入：inserted=%v upserted=%v", fake.inserted, fake.upserted)
	}
}

// TestFindConflicts 复刻 checkLiteLLMSyncConflicts：只比 manual 行、只在云端同名且带 mode 时算冲突。
func TestFindConflicts(t *testing.T) {
	fake := newFakeStore()
	fake.existing["manual-hit"] = store.PriceSyncExistingRow{
		ModelName: "manual-hit", Source: SourceManual,
		PriceData: map[string]any{"mode": "chat", "input_cost_per_token": 1.0},
	}
	fake.existing["manual-no-cloud"] = store.PriceSyncExistingRow{
		ModelName: "manual-no-cloud", Source: SourceManual,
		PriceData: map[string]any{"mode": "chat"},
	}
	fake.existing["cloud-row"] = store.PriceSyncExistingRow{
		ModelName: "cloud-row", Source: SourceCloud,
		PriceData: map[string]any{"mode": "chat"},
	}

	cloud := map[string]map[string]any{
		"manual-hit":       {"mode": "chat", "input_cost_per_token": 2.0},
		"manual-no-mode":   {"input_cost_per_token": 3.0},
		"cloud-row":        {"mode": "chat"},
		"manual-no-cloud2": {"mode": "chat"},
	}
	fake.existing["manual-no-mode"] = store.PriceSyncExistingRow{
		ModelName: "manual-no-mode", Source: SourceManual,
		PriceData: map[string]any{"mode": "chat"},
	}

	result, err := FindConflicts(context.Background(), fake, cloud)
	if err != nil {
		t.Fatalf("FindConflicts 失败: %v", err)
	}
	if !result.HasConflicts {
		t.Fatal("应判为存在冲突")
	}
	// 只有 manual-hit（manual + 云端同名 + 云端带 mode）算一条；
	// manual-no-mode 的**云端行**没有 mode → 不算；cloud-row 不是 manual → 不算。
	if len(result.Conflicts) != 1 {
		t.Fatalf("冲突数应为 1，实际 %d：%+v", len(result.Conflicts), result.Conflicts)
	}
	conflict := result.Conflicts[0]
	if conflict.ModelName != "manual-hit" {
		t.Errorf("冲突模型应为 manual-hit，实际 %s", conflict.ModelName)
	}
	var manualPrice, cloudPrice map[string]any
	if err := json.Unmarshal(conflict.ManualPrice, &manualPrice); err != nil {
		t.Fatalf("manualPrice 不是 JSON 对象: %v", err)
	}
	if err := json.Unmarshal(conflict.CloudPrice, &cloudPrice); err != nil {
		t.Fatalf("cloudPrice 不是 JSON 对象: %v", err)
	}
	if manualPrice["input_cost_per_token"] != 1.0 || cloudPrice["input_cost_per_token"] != 2.0 {
		t.Errorf("冲突两侧价格不符：manual=%v cloud=%v", manualPrice, cloudPrice)
	}
}

// TestFindConflictsEmpty 无冲突时 hasConflicts=false 且 conflicts 是空数组（不是 null）。
func TestFindConflictsEmpty(t *testing.T) {
	result, err := FindConflicts(context.Background(), newFakeStore(), map[string]map[string]any{})
	if err != nil {
		t.Fatalf("FindConflicts 失败: %v", err)
	}
	if result.HasConflicts {
		t.Error("空表不该判为有冲突")
	}
	if result.Conflicts == nil {
		t.Error("conflicts 必须是数组（Node 恒为 []）")
	}
	encoded, _ := json.Marshal(result)
	if !strings.Contains(string(encoded), `"conflicts":[]`) {
		t.Errorf("序列化后应为空数组：%s", encoded)
	}
}

// TestWriteEntriesEmptyIsSuccess upload 传空表（{}）时 Node 返回成功且各数组为空。
func TestWriteEntriesEmptyIsSuccess(t *testing.T) {
	result, err := WriteEntries(context.Background(), newFakeStore(), nil, SourceManual, nil, testLogger())
	if err != nil {
		t.Fatalf("空表不该失败: %v", err)
	}
	if result.Total != 0 || len(result.Added) != 0 || len(result.Updated) != 0 ||
		len(result.Unchanged) != 0 || len(result.Failed) != 0 || len(result.SkippedConflicts) != 0 {
		t.Fatalf("空表结果应全空：%+v", result)
	}
}

// 编译期确认 *store.Pools 满足 UpdateStore（真库路径不必写适配器）。
var _ UpdateStore = (*store.Pools)(nil)

// 编译期确认 jobs.PriceUpdateResult 承载本包的返回（HTTP 层直接序列化它）。
var _ = func(result *jobs.PriceUpdateResult) *jobs.PriceUpdateResult { return result }
