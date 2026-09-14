package terminal

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

func intPtr(value int) *int           { return &value }
func floatPtr(value float64) *float64 { return &value }
func int64Ptr(value int64) *int64     { return &value }
func strPtr(value string) *string     { return &value }

// 终态写必须带合法状态码：状态码是终态的首要凭据，缺它写出的行不会被
// fn_is_message_request_finalized 判为终态（除非另有 blocked_by/error_message）。
func TestSettlementRequiresStatusCode(t *testing.T) {
	_, err := Settlement{StatusCode: 0}.toPatch()
	if err == nil {
		t.Fatal("状态码为 0 的结算必须被拒绝")
	}
	var incompleteErr *ErrIncompleteTerminalPatch
	if !errors.As(err, &incompleteErr) {
		t.Fatalf("错误类型应为 ErrIncompleteTerminalPatch，得到 %T", err)
	}
}

// 不变量 I3：到过上游的请求，其终态写必须与 duration_ms 同语句。
func TestSettlementRejectsProviderRequestWithoutDuration(t *testing.T) {
	settlement := Settlement{
		StatusCode:    200,
		ProviderChain: []byte(`[{"id":1,"reason":"request_success"}]`),
	}
	_, err := settlement.toPatch()
	if err == nil {
		t.Fatal("有 provider_chain 却缺 duration_ms 的终态写必须被拒绝")
	}
	if !strings.Contains(err.Error(), "duration_ms") {
		t.Fatalf("错误信息应点明 duration_ms，得到: %v", err)
	}

	settlement.DurationMS = intPtr(62)
	if _, err := settlement.toPatch(); err != nil {
		t.Fatalf("补上 duration_ms 后应当通过: %v", err)
	}
}

// 拦截类终态没有上游参与，因此允许不带 duration_ms（敏感词拦截在 TS 侧同样不带）。
func TestSettlementAllowsBlockedWithoutProviderChain(t *testing.T) {
	settlement := Settlement{
		StatusCode:   400,
		BlockedBy:    strPtr("sensitive_word"),
		ErrorMessage: strPtr(`请求包含敏感词："x"`),
	}
	if _, err := settlement.toPatch(); err != nil {
		t.Fatalf("拦截类终态不应要求 duration_ms: %v", err)
	}
}

// outbox 的 5 个监视列必须能由终态 patch 表达，且落在同一条语句里——
// 分成两条语句时，第二条不会再次派发 outbox 事件。
func TestTerminalPatchCarriesAllOutboxColumnsInOneStatement(t *testing.T) {
	patch, err := maxSettlement().toPatch()
	if err != nil {
		t.Fatalf("样本结算应当合法: %v", err)
	}
	query, args := store.BuildDetailsPatchQuery(1, patch)
	columns := setColumnsOf(query)

	present := map[string]bool{}
	for _, column := range columns {
		present[column] = true
	}
	for _, column := range outboxMonitoredColumns {
		if !present[column] {
			t.Fatalf("outbox 监视列 %s 不在终态语句的 SET 里: %s", column, query)
		}
	}
	if strings.Count(query, "UPDATE message_request SET") != 1 {
		t.Fatalf("终态写必须是单条语句: %s", query)
	}
	if len(args) != len(columns)+1 {
		t.Fatalf("参数个数 = %d，列数 = %d，二者应差 1（id 参数）", len(args), len(columns))
	}
	if !strings.Contains(query, "WHERE id = $") || !strings.Contains(query, "status_code IS NULL") {
		t.Fatalf("终态写必须带幂等谓词: %s", query)
	}
}

// 写列闭包：账本触发器监视的每一列都必须有明确的写入方，否则账本行会缺字段。
// 「缺列即失败」——失败信息直接点名缺失的列。
func TestWriteColumnClosureCoversLedgerMonitorSet(t *testing.T) {
	covered := map[string]string{}
	register := func(owner string, columns []string) {
		for _, column := range columns {
			if _, exists := covered[column]; !exists {
				covered[column] = owner
			}
		}
	}
	register("settle", SettleTimeColumns())
	register("create", createTimeColumns)
	register("cost", costTimeColumns)
	register("database", dbManagedColumns)
	register("legacy", legacyColumns)

	var missing []string
	for _, column := range ledgerMonitoredColumns {
		if _, ok := covered[column]; !ok {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("账本监视列没有写入方: %s", strings.Join(missing, ", "))
	}

	var missingOutbox []string
	for _, column := range outboxMonitoredColumns {
		if _, ok := covered[column]; !ok {
			missingOutbox = append(missingOutbox, column)
		}
	}
	if len(missingOutbox) > 0 {
		t.Fatalf("outbox 监视列没有写入方: %s", strings.Join(missingOutbox, ", "))
	}
}

// 监视集本身要与证据一致：37 列账本、5 列 outbox。数量对不上说明有人改了清单，
// 集成测试会用库内 pg_trigger 的定义再核一次。
func TestMonitorColumnCountsMatchEvidence(t *testing.T) {
	if len(ledgerMonitoredColumns) != 37 {
		t.Fatalf("账本监视列数 = %d，证据实测为 37", len(ledgerMonitoredColumns))
	}
	if len(outboxMonitoredColumns) != 5 {
		t.Fatalf("outbox 监视列数 = %d，证据实测为 5", len(outboxMonitoredColumns))
	}
}

// 插入列集镜像必须与真实行的形状一致。黄金样本来自一次真实 Node 请求写入的行，
// 因此这份镜像一旦与真实 schema 脱节就会红——不需要数据库。
func TestCreateTimeColumnsMatchGoldenRow(t *testing.T) {
	row := goldenRow(t, "message_request_row.json")
	for _, column := range createTimeColumns {
		if _, ok := row[column]; !ok {
			t.Fatalf("插入列集镜像里的 %s 不存在于黄金样本行", column)
		}
	}
	// 反向抽查：黄金样本里的监视列不能被镜像整列漏掉（漏了就会写出 NULL）。
	for _, column := range []string{"provider_id", "cost_usd", "key", "endpoint", "client_ip"} {
		if _, ok := row[column]; !ok {
			t.Fatalf("黄金样本缺少关键列 %s", column)
		}
	}
}

// 不变量 I4：迟到补写不得触碰监视列。routing_trace 是非监视列，是允许的补写面。
func TestLatePatchMustNotTouchMonitoredColumns(t *testing.T) {
	routingTracePatch := store.DetailsPatch{RoutingTrace: []byte(`{"version":1}`)}
	if hits := MonitoredColumnsInLatePatch(routingTracePatch); len(hits) != 0 {
		t.Fatalf("只补 routing_trace 不应触碰监视列，得到 %v", hits)
	}

	statusCode := 200
	badPatch := store.DetailsPatch{StatusCode: &statusCode, RoutingTrace: []byte(`{}`)}
	hits := MonitoredColumnsInLatePatch(badPatch)
	if len(hits) != 1 || hits[0] != "status_code" {
		t.Fatalf("迟到补写 status_code 必须被识别，得到 %v", hits)
	}

	duration := 10
	blockedBy := "warmup"
	bothPatch := store.DetailsPatch{DurationMS: &duration, BlockedBy: &blockedBy}
	if got := len(MonitoredColumnsInLatePatch(bothPatch)); got != 2 {
		t.Fatalf("应识别出 2 个监视列，得到 %d", got)
	}
}

// 终态 patch 里的非监视列不算违规（routing_trace 与 error_stack 都属此类）。
func TestTerminalPatchLateWriteGuardAllowsNonMonitoredColumns(t *testing.T) {
	patch, err := maxSettlement().toPatch()
	if err != nil {
		t.Fatalf("样本结算应当合法: %v", err)
	}
	hits := MonitoredColumnsInLatePatch(patch)
	// 终态写本身当然会带监视列；这里断言守卫能完整识别出来，而不是漏报。
	for _, column := range []string{"status_code", "duration_ms", "blocked_by"} {
		found := false
		for _, hit := range hits {
			if hit == column {
				found = true
			}
		}
		if !found {
			t.Fatalf("守卫漏报监视列 %s（命中集 %v）", column, hits)
		}
	}
}

// 空 patch 不应产生任何 SET 列（store 只写 updated_at）。
func TestEmptyPatchHasNoColumns(t *testing.T) {
	query, _ := store.BuildDetailsPatchQuery(1, store.DetailsPatch{})
	if columns := setColumnsOf(query); len(columns) != 0 {
		t.Fatalf("空 patch 不应有 SET 列，得到 %v", columns)
	}
}

func goldenRow(t *testing.T, name string) map[string]json.RawMessage {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "golden", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取黄金样本失败: %v", err)
	}
	var envelope struct {
		Row map[string]json.RawMessage `json:"row"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("解析黄金样本失败: %v", err)
	}
	if len(envelope.Row) == 0 {
		t.Fatalf("黄金样本 %s 没有 row 字段", name)
	}
	return envelope.Row
}
