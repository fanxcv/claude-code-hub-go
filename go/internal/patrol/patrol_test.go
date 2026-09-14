package patrol

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// fakeStore 是 UnsettledStore 的内存实现：它按与 SQL 相同的语义过滤（阈值 + (created_at,id)
// 游标），因此分页与上界逻辑可以在没有数据库的情况下被钉住。
type fakeStore struct {
	mu        sync.Mutex
	rows      []store.UnsettledRequest
	listCalls []listCall
	listErr   error
	// repairErr 按 id 注入补写错误；applyLost 按 id 注入「谓词未命中」。
	repairErr map[int64]error
	applyLost map[int64]bool
	patches   map[int64]store.DetailsPatch
	calls     []int64
}

type listCall struct {
	cutoff time.Time
	after  store.UnsettledCursor
	limit  int
}

func newFakeStore(rows ...store.UnsettledRequest) *fakeStore {
	return &fakeStore{
		rows:      rows,
		repairErr: map[int64]error{},
		applyLost: map[int64]bool{},
		patches:   map[int64]store.DetailsPatch{},
	}
}

func (f *fakeStore) ListUnsettledRequests(
	_ context.Context,
	cutoff time.Time,
	after store.UnsettledCursor,
	limit int,
) ([]store.UnsettledRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, listCall{cutoff: cutoff, after: after, limit: limit})
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]store.UnsettledRequest, 0, limit)
	for _, row := range f.rows {
		if !row.CreatedAt.Before(cutoff) {
			continue
		}
		if after.ID != 0 || !after.CreatedAt.IsZero() {
			if !(row.CreatedAt.After(after.CreatedAt) ||
				(row.CreatedAt.Equal(after.CreatedAt) && row.ID > after.ID)) {
				continue
			}
		}
		out = append(out, row)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) RepairUnsettled(
	_ context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, id)
	if err := f.repairErr[id]; err != nil {
		return false, err
	}
	if f.applyLost[id] {
		return false, nil
	}
	f.patches[id] = patch
	return true, nil
}

func (f *fakeStore) snapshot() ([]int64, map[int64]store.DetailsPatch, []listCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := append([]int64(nil), f.calls...)
	patches := make(map[int64]store.DetailsPatch, len(f.patches))
	for id, patch := range f.patches {
		patches[id] = patch
	}
	return calls, patches, append([]listCall(nil), f.listCalls...)
}

func newPatrol(t *testing.T, options Options) *Patrol {
	t.Helper()
	options.Logger = nil
	runner, err := New(options)
	if err != nil {
		t.Fatalf("构造巡检失败: %v", err)
	}
	return runner
}

// TestRunOncePagesBoundedByMaxRows 钉住三层上界里的两层：单页批量与单轮总行数。
func TestRunOncePagesBoundedByMaxRows(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := []store.UnsettledRequest{
		{ID: 1, CreatedAt: old},
		{ID: 2, CreatedAt: old.Add(time.Second)},
		{ID: 3, CreatedAt: old.Add(2 * time.Second)},
		{ID: 4, CreatedAt: old.Add(3 * time.Second)},
	}
	fake := newFakeStore(rows...)
	runner := newPatrol(t, Options{
		Store:           fake,
		UnsettledAfter:  time.Hour,
		BatchSize:       2,
		MaxRowsPerRound: 3,
		Now:             func() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) },
	})

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检轮次报错: %v", err)
	}
	if result.Found != 3 || result.Repaired != 3 {
		t.Fatalf("本应只处理 3 行（maxRowsPerRound），实际 found=%d repaired=%d", result.Found, result.Repaired)
	}
	calls, patches, listCalls := fake.snapshot()
	if len(calls) != 3 {
		t.Fatalf("补写调用数应为 3，实际 %d", len(calls))
	}
	if _, touched := patches[4]; touched {
		t.Fatal("超出单轮上限的第 4 行不应被补写")
	}
	if len(listCalls) != 2 {
		t.Fatalf("应分两页取（3 行 / 每页 2），实际 %d 页", len(listCalls))
	}
	if listCalls[1].after.ID != 2 {
		t.Fatalf("第二页游标应停在上一页最后一行 id=2，实际 %d", listCalls[1].after.ID)
	}
	if listCalls[1].limit != 1 {
		t.Fatalf("第二页的批量应被剩余额度收窄到 1，实际 %d", listCalls[1].limit)
	}
}

// TestRunOnceList failure 只返回错误、不 panic，且不补写任何行。
func TestRunOnceListFailureIsReportedNotPanicked(t *testing.T) {
	fake := newFakeStore(store.UnsettledRequest{ID: 1, CreatedAt: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)})
	fake.listErr = errors.New("库连接断了")
	runner := newPatrol(t, Options{Store: fake, UnsettledAfter: time.Hour})

	result, err := runner.RunOnce(context.Background())
	if err == nil {
		t.Fatal("查询失败必须返回错误，而不是当成「没有候选」")
	}
	if result.Found != 0 || result.Repaired != 0 {
		t.Fatalf("查询失败时不应产生补写，实际 found=%d repaired=%d", result.Found, result.Repaired)
	}
	if calls, _, _ := fake.snapshot(); len(calls) != 0 {
		t.Fatalf("查询失败时不应调用补写，实际 %d 次", len(calls))
	}
}

// TestRunOnceRepairFailureDoesNotBlockLaterRows：单行报错要计数并继续，游标仍推进。
func TestRunOnceRepairFailureDoesNotBlockLaterRows(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := newFakeStore(
		store.UnsettledRequest{ID: 1, CreatedAt: old},
		store.UnsettledRequest{ID: 2, CreatedAt: old.Add(time.Second)},
	)
	fake.repairErr[1] = errors.New("约束冲突")
	runner := newPatrol(t, Options{Store: fake, UnsettledAfter: time.Hour, BatchSize: 10})

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("单行失败不应让整轮报错: %v", err)
	}
	if result.Failed != 1 || result.Repaired != 1 {
		t.Fatalf("应恰好 1 失败 1 成功，实际 failed=%d repaired=%d", result.Failed, result.Repaired)
	}
	_, patches, _ := fake.snapshot()
	if _, ok := patches[2]; !ok {
		t.Fatal("第一行失败后，第二行仍应被补写")
	}
}

// TestConcurrentRoundsDoNotDoubleRepair：谓词未命中的一轮要计 Skipped，而不是 Failed 或重复补写。
func TestConcurrentRoundsDoNotDoubleRepair(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := newFakeStore(store.UnsettledRequest{ID: 7, CreatedAt: old})
	fake.applyLost[7] = true
	runner := newPatrol(t, Options{Store: fake, UnsettledAfter: time.Hour})

	result, err := runner.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检轮次报错: %v", err)
	}
	if result.Skipped != 1 || result.Repaired != 0 || result.Failed != 0 {
		t.Fatalf("谓词未命中应只计 skipped，实际 %+v", result)
	}
}

// TestRepairPatchMatchesNodeSemantics 钉住终态口径与「不写监视列之外的东西」。
func TestRepairPatchMatchesNodeSemantics(t *testing.T) {
	if RepairStatusCode != 499 || RepairErrorMessage != "CLIENT_ABORTED" {
		t.Fatalf("终态口径必须与 Node response-handler 的客户端中断分支一致，实际 %d/%q",
			RepairStatusCode, RepairErrorMessage)
	}
	row := store.UnsettledRequest{
		ID:        42,
		CreatedAt: time.Date(2026, 9, 12, 11, 30, 0, 0, time.UTC),
	}
	runner := newPatrol(t, Options{
		Store:          newFakeStore(),
		UnsettledAfter: 30 * time.Minute,
		Now:            func() time.Time { return time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) },
	})
	patch := runner.repairPatch(row)

	if patch.StatusCode == nil || *patch.StatusCode != 499 {
		t.Fatalf("补写必须带 499 终态，实际 %v", patch.StatusCode)
	}
	if patch.ErrorMessage == nil || *patch.ErrorMessage != "CLIENT_ABORTED" {
		t.Fatalf("error_message 必须与 Node 同字面（它同时是账本 is_success 判据），实际 %v", patch.ErrorMessage)
	}
	if patch.ErrorStack == nil || !strings.HasPrefix(*patch.ErrorStack, MarkerPrefix) {
		t.Fatalf("error_stack 必须带可追溯标记前缀 %q，实际 %v", MarkerPrefix, patch.ErrorStack)
	}
	if !strings.Contains(*patch.ErrorStack, "age_ms=1800000") {
		t.Fatalf("标记应含真实年龄，实际 %q", *patch.ErrorStack)
	}
	// 不变量 P2/P3：同语句不得带 duration_ms（无从知道，编造比留空更糟），也不得带任何成本列。
	query, args := store.BuildDetailsPatchQuery(row.ID, patch)
	if !strings.Contains(query, "WHERE id = $") || !strings.Contains(query, "status_code IS NULL") {
		t.Fatalf("补写必须复用条件终态谓词，实际 %s", query)
	}
	if strings.Contains(query, "duration_ms") || strings.Contains(query, "cost_usd") {
		t.Fatalf("补写不得触碰 duration_ms 或成本列，实际 %s", query)
	}
	if len(args) != 4 {
		t.Fatalf("参数应为 status/error_message/error_stack/id 四条，实际 %d", len(args))
	}
}

// TestStopIsBounded 钉住「停巡检有界」：不得让退出序列等一个真实间隔。
func TestStopIsBounded(t *testing.T) {
	fake := newFakeStore()
	runner := newPatrol(t, Options{
		Store:          fake,
		UnsettledAfter: time.Hour,
		Interval:       5 * time.Millisecond,
	})
	runner.Start(context.Background())
	time.Sleep(20 * time.Millisecond)

	started := time.Now()
	runner.Stop()
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Stop 必须有界，实际等待 %s", elapsed)
	}
	// 幂等：重复 Stop 不阻塞、不 panic。
	runner.Stop()
}

// TestNewRejectsZeroThreshold：阈值为 0 会把刚开行的请求当成丢失，必须在构造期拒绝。
func TestNewRejectsZeroThreshold(t *testing.T) {
	if _, err := New(Options{Store: newFakeStore(), UnsettledAfter: 0}); err == nil {
		t.Fatal("阈值为 0 必须被拒绝")
	}
	if _, err := New(Options{UnsettledAfter: time.Hour}); err == nil {
		t.Fatal("缺存储必须被拒绝")
	}
}
