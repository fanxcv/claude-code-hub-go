package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xuri/excelize/v2"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是导出的**纯单元**测试：渲染（列集/转义/数值归一/时区）、XLSX 读回、
// 请求体解析、进度口径、上限、孤儿清扫。真实 PG/Redis 的端到端用例在
// usage_logs_export_integration_test.go。

func exportInt64Ptr(value int64) *int64 { return &value }

func exportTimePtr(value time.Time) *time.Time { return &value }

// exportSampleRow 造一行完整的示例数据（字段覆盖三类列：文本/数字/时间）。
func exportSampleRow() usageLogsExportRow {
	kind := "session_id"
	sessionID := "sess-1"
	sourceSessionID := "sess-1"
	provider := "vendor-a"
	model := "claude-sonnet"
	original := "claude-sonnet"
	endpoint := "/v1/messages"
	cost := "0.250000000000000"
	status := 200
	duration := int64(1234)
	return usageLogsExportRow{
		CreatedAt:           exportTimePtr(time.Date(2026, 6, 3, 12, 34, 56, 0, time.UTC)),
		UserName:            "alice",
		KeyName:             "key-a",
		ProviderName:        &provider,
		Model:               &model,
		OriginalModel:       &original,
		Endpoint:            &endpoint,
		StatusCode:          &status,
		InputTokens:         exportInt64Ptr(100),
		OutputTokens:        exportInt64Ptr(20),
		CacheCreation5m:     exportInt64Ptr(5),
		CacheCreation1h:     exportInt64Ptr(7),
		CacheRead:           exportInt64Ptr(40),
		TotalTokens:         exportInt64Ptr(172),
		CostUSD:             &cost,
		DurationMs:          &duration,
		SessionIdentityKind: &kind,
		SessionID:           &sessionID,
		SourceSessionID:     &sourceSessionID,
	}
}

// TestExportCsvHeaderLine 钉住表头：datetime 列带时区后缀，其余列不变。
func TestExportCsvHeaderLine(t *testing.T) {
	header := exportBuildCsvHeaderLine("Asia/Shanghai")
	want := "Time (Asia/Shanghai),User,Key,Provider,Model,Original Model,Endpoint," +
		"Status Code,Input Tokens,Output Tokens,Cache Write 5m,Cache Write 1h,Cache Read," +
		"Total Tokens,Cost (USD),Duration (ms),Prefix ID,Session ID,Retry Count"
	if header != want {
		t.Fatalf("表头不符：\n got %s\nwant %s", header, want)
	}
}

// TestExportEscapeCsvField 钉住公式注入中和与引号翻倍（Node escapeCsvField 的两个分支）。
func TestExportEscapeCsvField(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"普通", "alice", "alice"},
		{"公式等号", "=SUM(A1)", "'=SUM(A1)"},
		{"公式加号", "+1", "'+1"},
		{"公式减号", "-1", "'-1"},
		{"公式at", "@x", "'@x"},
		{"前导空白不中和", "  x", "  x"},
		{"含逗号", "a,b", `"a,b"`},
		{"含引号", `a"b`, `"a""b"`},
		{"含换行", "a\nb", "\"a\nb\""},
		{"含回车", "a\rb", "\"a\rb\""},
		{"空串", "", ""},
	}
	for _, testCase := range cases {
		if got := exportEscapeCsvField(testCase.input); got != testCase.want {
			t.Fatalf("%s: got %q want %q", testCase.name, got, testCase.want)
		}
	}
}

// TestExportNormalizeDecimalForSpreadsheet 钉住 15 位有效数字、纯十进制、去尾零。
func TestExportNormalizeDecimalForSpreadsheet(t *testing.T) {
	cases := []struct {
		name  string
		input *string
		want  string
	}{
		{"16 位有效数字被截到 15 位", exportStringPtr("1.2345678901234567890"), "1.23456789012346"},
		{"整数值", exportStringPtr("100"), "100"},
		{"去尾零", exportStringPtr("0.250000000000000"), "0.25"},
		{"极小值不走科学计数", exportStringPtr("0.0000001"), "0.0000001"},
		{"极大值不走科学计数", exportStringPtr("1e21"), "1000000000000000000000"},
		{"空视为 0", nil, "0"},
		{"空串视为 0", exportStringPtr(""), "0"},
		{"非数字视为 0", exportStringPtr("abc"), "0"},
	}
	for _, testCase := range cases {
		cell := exportCell{number: testCase.input}
		if got := exportNormalizeDecimalForSpreadsheet(cell); got != testCase.want {
			t.Fatalf("%s: got %q want %q", testCase.name, got, testCase.want)
		}
	}
}

// TestExportNormalizeNegativeZero 钉住 -0：JS 的 Intl 输出 "-0"，不能归一成 "0"。
func TestExportNormalizeNegativeZero(t *testing.T) {
	if got := exportNormalizeDecimalForSpreadsheet(exportCell{number: exportStringPtr("-0")}); got != "-0" {
		t.Fatalf("got %q want %q", got, "-0")
	}
}

// TestExportCsvRows 钉住一行的渲染：时间按时区、numeric 列归一、空值语义。
func TestExportCsvRows(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("载入时区失败: %v", err)
	}
	rows := []usageLogsExportRow{exportSampleRow()}
	lines := exportBuildCsvRows(rows, location)
	if len(lines) != 1 {
		t.Fatalf("行数不符: %d", len(lines))
	}
	want := "2026-06-03 20:34:56,alice,key-a,vendor-a,claude-sonnet,claude-sonnet,/v1/messages," +
		"200,100,20,5,7,40,172,0.25,1234,,sess-1,0"
	if lines[0] != want {
		t.Fatalf("\n got %s\nwant %s", lines[0], want)
	}
}

// TestExportCsvBlankCells 钉住两类空值：zeroWhenNull 写 0，其余数字列留空。
func TestExportCsvBlankCells(t *testing.T) {
	location := time.UTC
	rows := []usageLogsExportRow{{UserName: "bob", KeyName: "k"}}
	line := exportBuildCsvRows(rows, location)[0]
	fields := strings.Split(line, ",")
	// 列序：Time,User,Key,Provider,Model,Original,Endpoint,Status,Input,Output,5m,1h,Read,Total,Cost,Duration,Prefix,Session,Retry
	if fields[0] != "" {
		t.Fatalf("缺失时间应为空串: %q", fields[0])
	}
	if fields[7] != "" || fields[15] != "" {
		t.Fatalf("非 zeroWhenNull 列应为空: status=%q duration=%q", fields[7], fields[15])
	}
	for _, index := range []int{8, 9, 10, 11, 12, 13, 14, 18} {
		if fields[index] != "0" {
			t.Fatalf("zeroWhenNull 列应为 0，第 %d 列是 %q", index, fields[index])
		}
	}
}

// TestExportRetryCount 钉住 getRetryCount 的三态：hedge 归零、成功计数减一、解析失败归零。
func TestExportRetryCount(t *testing.T) {
	cases := []struct {
		name  string
		chain string
		want  int
	}{
		{"两次成功", `[{"reason":"request_success","statusCode":200},{"reason":"retry_success","statusCode":200}]`, 1},
		{"一次成功", `[{"reason":"request_success","statusCode":200}]`, 0},
		{"失败也算实际请求", `[{"reason":"retry_failed"},{"reason":"request_success","statusCode":200}]`, 1},
		{"hedge 竞速归零", `[{"reason":"hedge_triggered"},{"reason":"hedge_winner","statusCode":200}]`, 0},
		{"中间状态不计", `[{"reason":"initial_selection"},{"reason":"request_success","statusCode":200}]`, 0},
		{"成功但无状态码不计", `[{"reason":"request_success"}]`, 0},
		{"链缺失", ``, 0},
		{"链损坏", `{not json`, 0},
	}
	for _, testCase := range cases {
		row := usageLogsExportRow{}
		if testCase.chain != "" {
			row.ProviderChain = []byte(testCase.chain)
		}
		if got := exportRetryCountOf(&row); got != testCase.want {
			t.Fatalf("%s: got %d want %d", testCase.name, got, testCase.want)
		}
	}
}

// TestExportSessionIdentityColumns 钉住 prefix_affinity 下的两列取值（Node columns.ts:39-47）。
func TestExportSessionIdentityColumns(t *testing.T) {
	prefixKind := "prefix_affinity"
	sessionKind := "session_id"
	identity := "prefix-abc"
	physical := "sess-xyz"

	prefix := usageLogsExportRow{
		SessionIdentityKind: &prefixKind, SessionID: &identity, SourceSessionID: &physical,
	}
	if got := exportPrefixIDOf(&prefix); got != identity {
		t.Fatalf("Prefix ID: got %q want %q", got, identity)
	}
	if got := exportPhysicalSessionIDOf(&prefix); got != physical {
		t.Fatalf("Session ID: got %q want %q", got, physical)
	}

	wide := usageLogsExportRow{
		SessionIdentityKind: &sessionKind, SessionID: &physical, SourceSessionID: &identity,
	}
	if got := exportPrefixIDOf(&wide); got != "" {
		t.Fatalf("非前缀亲和时 Prefix ID 应为空: %q", got)
	}
	if got := exportPhysicalSessionIDOf(&wide); got != physical {
		t.Fatalf("非前缀亲和时 Session ID 应取 sessionId: %q", got)
	}
}

// TestExportCsvWriterBOM 钉住正文形状：BOM 开头、行间 \n、行尾无换行。
func TestExportCsvWriterBOM(t *testing.T) {
	writer := newUsageLogsExportCsvWriter("UTC")
	if err := writer.addBatch([]usageLogsExportRow{exportSampleRow(), exportSampleRow()}); err != nil {
		t.Fatalf("addBatch 失败: %v", err)
	}
	content, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}
	if !strings.HasPrefix(content, "\ufeff") {
		t.Fatal("正文缺 BOM")
	}
	if strings.HasSuffix(content, "\n") {
		t.Fatal("正文不该以换行结尾")
	}
	if got := strings.Count(content, "\n"); got != 2 {
		t.Fatalf("期望 2 个换行（表头 + 两行数据），实得 %d", got)
	}
}

// TestExportProgress 钉住 buildUsageLogsExportProgress 的三条分支。
func TestExportProgress(t *testing.T) {
	cases := []struct {
		name               string
		processed, total   int64
		hasMore            bool
		wantTotal, wantPct int
	}{
		{"有更多时上限 99", 50, 100, true, 100, 50},
		{"有更多时总数被抬高", 100, 100, true, 101, 99},
		{"收尾即 100", 100, 100, false, 100, 100},
		{"总数为 0 即 100", 0, 0, false, 0, 100},
	}
	for _, testCase := range cases {
		total, percent := exportProgress(testCase.processed, testCase.total, testCase.hasMore)
		if total != int64(testCase.wantTotal) || percent != testCase.wantPct {
			t.Fatalf("%s: got total=%d pct=%d, want total=%d pct=%d",
				testCase.name, total, percent, testCase.wantTotal, testCase.wantPct)
		}
	}
}

// TestExportSummaryAccumulator 钉住「多日按天、单日按小时」与合计行。
func TestExportSummaryAccumulator(t *testing.T) {
	location := time.UTC
	day := func(hour int) *time.Time {
		return exportTimePtr(time.Date(2026, 6, 3, hour, 0, 0, 0, time.UTC))
	}
	cost := "0.5"
	row := func(at *time.Time) usageLogsExportRow {
		return usageLogsExportRow{
			CreatedAt: at, InputTokens: exportInt64Ptr(10), TotalTokens: exportInt64Ptr(10),
			CostUSD: &cost,
		}
	}

	single := newUsageLogsExportSummaryAccumulator(location)
	single.Add(&[]usageLogsExportRow{row(day(1))}[0])
	single.Add(&[]usageLogsExportRow{row(day(1))}[0])
	single.Add(&[]usageLogsExportRow{row(day(2))}[0])
	got := single.Finalize()
	if got.Granularity != "hourly" {
		t.Fatalf("单日应按时段汇总，实得 %s", got.Granularity)
	}
	if len(got.Rows) != 2 || got.Rows[0].Period != "2026-06-03 01:00" {
		t.Fatalf("小时桶不符: %+v", got.Rows)
	}
	if got.Total.Requests != 3 || got.Total.Cost != 1.5 {
		t.Fatalf("合计不符: %+v", got.Total)
	}

	multi := newUsageLogsExportSummaryAccumulator(location)
	multi.Add(&[]usageLogsExportRow{row(day(1))}[0])
	multi.Add(&[]usageLogsExportRow{
		row(exportTimePtr(time.Date(2026, 6, 4, 5, 0, 0, 0, time.UTC))),
	}[0])
	got = multi.Finalize()
	if got.Granularity != "daily" || len(got.Rows) != 2 {
		t.Fatalf("跨日应按天汇总: %+v", got)
	}
	if got.Rows[0].Period != "2026-06-03" || got.Rows[1].Period != "2026-06-04" {
		t.Fatalf("天桶不符: %+v", got.Rows)
	}

	unknown := newUsageLogsExportSummaryAccumulator(location)
	unknown.Add(&usageLogsExportRow{UserName: "x"})
	got = unknown.Finalize()
	if len(got.Rows) != 1 || got.Rows[0].Period != exportUnknownPeriod {
		t.Fatalf("无时间行应落在 Unknown: %+v", got.Rows)
	}
}

// TestExportXlsxReadBack 用 excelize 读回，钉住表名、表头、单元格类型与汇总表。
func TestExportXlsxReadBack(t *testing.T) {
	location := time.UTC
	writer, err := newUsageLogsExportXlsxWriter(location, "UTC")
	if err != nil {
		t.Fatalf("建写出器失败: %v", err)
	}
	if err := writer.addBatch([]usageLogsExportRow{exportSampleRow(), exportSampleRow()}); err != nil {
		t.Fatalf("addBatch 失败: %v", err)
	}
	payload, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}

	file, err := excelize.OpenReader(strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("读回 XLSX 失败: %v", err)
	}
	defer func() { _ = file.Close() }()

	if got := file.GetSheetList(); len(got) != 2 ||
		got[0] != exportXlsxDetailSheet || got[1] != exportXlsxHourlySheet {
		t.Fatalf("表名不符: %v", got)
	}

	rows, err := file.GetRows(exportXlsxDetailSheet)
	if err != nil {
		t.Fatalf("读明细表失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("明细表行数应为 1 表头 + 2 数据，实得 %d", len(rows))
	}
	if rows[0][0] != "Time (UTC)" || rows[0][18] != "Retry Count" {
		t.Fatalf("表头不符: %v", rows[0])
	}
	if rows[1][0] != "2026-06-03 12:34:56" {
		t.Fatalf("时间单元格不符: %q", rows[1][0])
	}
	if rows[1][8] != "100" || rows[1][14] != "0.25" {
		t.Fatalf("数字单元格不符: input=%q cost=%q", rows[1][8], rows[1][14])
	}

	// 数字列必须是真数字：单元格不带类型属性（数值即无 t），而不是 inlineStr/sharedString，
	// 否则 Excel 的 SUM() 会失效（numeric.ts 文件头）。
	cellType, err := file.GetCellType(exportXlsxDetailSheet, "I2")
	if err != nil {
		t.Fatalf("取单元格类型失败: %v", err)
	}
	if cellType == excelize.CellTypeInlineString || cellType == excelize.CellTypeSharedString {
		t.Fatalf("Input Tokens 被写成了文本单元格: %v", cellType)
	}
	rawValue, err := file.GetCellValue(exportXlsxDetailSheet, "I2")
	if err != nil {
		t.Fatalf("取单元格值失败: %v", err)
	}
	if _, err := strconv.ParseFloat(rawValue, 64); err != nil {
		t.Fatalf("Input Tokens 不是可计算的数值: %q", rawValue)
	}
	// 时间列同理：是日期序列（数值）而不是字符串，否则无法排序与相减。
	timeType, err := file.GetCellType(exportXlsxDetailSheet, "A2")
	if err != nil {
		t.Fatalf("取时间单元格类型失败: %v", err)
	}
	if timeType == excelize.CellTypeInlineString || timeType == excelize.CellTypeSharedString {
		t.Fatalf("时间被写成了文本单元格: %v", timeType)
	}

	summary, err := file.GetRows(exportXlsxHourlySheet)
	if err != nil {
		t.Fatalf("读汇总表失败: %v", err)
	}
	if len(summary) != 3 {
		t.Fatalf("汇总表应为 表头 + 1 时段 + 合计，实得 %d 行", len(summary))
	}
	if summary[0][0] != "Period" || summary[0][8] != "Cost (USD)" {
		t.Fatalf("汇总表头不符: %v", summary[0])
	}
	if summary[1][1] != "2" {
		t.Fatalf("时段请求数应 2，实得 %q", summary[1][1])
	}
	// 合计成本读回是**格式化后**的文本（0.5 在 0.00###### 下显示 0.50）：单元格里存的是数值，
	// 格式串与 Node 的 Cost (USD) 列一致，故此处断言的是 Excel 会显示的值。
	if summary[2][0] != "Total" || summary[2][8] != "0.50" {
		t.Fatalf("合计行不符: %v", summary[2])
	}
}

// TestExportXlsxDailySheetName 钉住跨日时的第二张表名。
func TestExportXlsxDailySheetName(t *testing.T) {
	writer, err := newUsageLogsExportXlsxWriter(time.UTC, "UTC")
	if err != nil {
		t.Fatalf("建写出器失败: %v", err)
	}
	first := exportSampleRow()
	second := exportSampleRow()
	second.CreatedAt = exportTimePtr(time.Date(2026, 6, 4, 1, 0, 0, 0, time.UTC))
	if err := writer.addBatch([]usageLogsExportRow{first, second}); err != nil {
		t.Fatalf("addBatch 失败: %v", err)
	}
	payload, err := writer.Finish()
	if err != nil {
		t.Fatalf("Finish 失败: %v", err)
	}
	file, err := excelize.OpenReader(strings.NewReader(string(payload)))
	if err != nil {
		t.Fatalf("读回 XLSX 失败: %v", err)
	}
	defer func() { _ = file.Close() }()
	sheets := file.GetSheetList()
	if len(sheets) != 2 || sheets[1] != exportXlsxDailySheet {
		t.Fatalf("跨日第二张表应为 %s: %v", exportXlsxDailySheet, sheets)
	}
}

// TestExportLimits 钉住两类上限：行数与内容字节（都必须是可判别的哨兵错误）。
func TestExportLimits(t *testing.T) {
	writer := newUsageLogsExportCsvWriter("UTC")
	overRows := make([]usageLogsExportRow, exportMaxRows+1)
	if err := writer.addBatch(overRows); !errors.Is(err, errUsageLogsExportRowLimit) {
		t.Fatalf("超行数应报 errUsageLogsExportRowLimit，实得 %v", err)
	}

	xlsxWriter, err := newUsageLogsExportXlsxWriter(time.UTC, "UTC")
	if err != nil {
		t.Fatalf("建写出器失败: %v", err)
	}
	if err := xlsxWriter.addBatch(overRows); !errors.Is(err, errUsageLogsExportRowLimit) {
		t.Fatalf("XLSX 超行数应报 errUsageLogsExportRowLimit，实得 %v", err)
	}

	big := exportSampleRow()
	big.UserName = strings.Repeat("x", exportMaxContent+1)
	sizeWriter := newUsageLogsExportCsvWriter("UTC")
	if err := sizeWriter.addBatch([]usageLogsExportRow{big}); err != nil {
		t.Fatalf("addBatch 失败: %v", err)
	}
	if _, err := sizeWriter.Finish(); !errors.Is(err, errUsageLogsExportSizeLimit) {
		t.Fatalf("超内容应报 errUsageLogsExportSizeLimit，实得 %v", err)
	}
}

// TestParseUsageLogsExportCreateBody 钉住请求体解析的六条语义。
func TestParseUsageLogsExportCreateBody(t *testing.T) {
	parse := func(body string) (usageLogsExportInput, []usageLogsValidationIssue) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/usage-logs/exports",
			strings.NewReader(body))
		return parseUsageLogsExportCreateBody(request)
	}

	input, issues := parse(`{"userId":7,"format":"xlsx","replayFilter":"replay","minRetryCount":2}`)
	if len(issues) != 0 {
		t.Fatalf("合法体不该有错误: %+v", issues)
	}
	if input.Query.UserID == nil || *input.Query.UserID != 7 || input.Format != "xlsx" ||
		input.Query.MinRetry != 2 || input.Query.Replay != store.UsageLogReplayFilter("replay") {
		t.Fatalf("解析结果不符: %+v", input)
	}

	input, issues = parse(`{}`)
	if len(issues) != 0 || input.Format != "csv" {
		t.Fatalf("空体的 format 应默认 csv: %+v %+v", input, issues)
	}
	if input.Query.UserID != nil || input.Query.StatusCode != nil {
		t.Fatal("空体不该带筛选")
	}

	// .strict()：未知字段是错误（zod 的 unrecognized_keys）。
	if _, issues = parse(`{"unknownField":1}`); len(issues) != 1 ||
		issues[0].Code != "unrecognized_keys" || issues[0].Path[0] != "unknownField" {
		t.Fatalf("未知字段应报 unrecognized_keys: %+v", issues)
	}

	// enum：非法取值报 invalid_enum_value。
	if _, issues = parse(`{"format":"pdf"}`); len(issues) != 1 ||
		issues[0].Code != "invalid_enum_value" {
		t.Fatalf("非法 format 应报 invalid_enum_value: %+v", issues)
	}
	if _, issues = parse(`{"replayFilter":"maybe"}`); len(issues) != 1 ||
		issues[0].Code != "invalid_enum_value" {
		t.Fatalf("非法 replayFilter 应报 invalid_enum_value: %+v", issues)
	}

	// coerce.number()：字符串 "7" 与布尔 true 都被接受（Number("7")=7、Number(true)=1）。
	input, issues = parse(`{"userId":"7"}`)
	if len(issues) != 0 || input.Query.UserID == nil || *input.Query.UserID != 7 {
		t.Fatalf("字符串数值应被 coerce: %+v %+v", input, issues)
	}

	// 布尔联合：JSON 布尔与字符串布尔都接受，其它形状报 invalid_union。
	if input, issues = parse(`{"excludeStatusCode200":true}`); len(issues) != 0 ||
		input.Query.StatusExcl == nil || !*input.Query.StatusExcl {
		t.Fatalf("JSON 布尔应被接受: %+v %+v", input, issues)
	}
	if input, issues = parse(`{"actualResponseModelMismatch":"true"}`); len(issues) != 0 ||
		!input.Query.Mismatch {
		t.Fatalf("字符串布尔应被接受: %+v %+v", input, issues)
	}
	if _, issues = parse(`{"excludeStatusCode200":"yes"}`); len(issues) != 1 ||
		issues[0].Code != "invalid_union" {
		t.Fatalf("非法布尔应报 invalid_union: %+v", issues)
	}

	// .int()：小数不是整数。
	if _, issues = parse(`{"statusCode":200.5}`); len(issues) != 1 ||
		issues[0].Code != "invalid_type" {
		t.Fatalf("小数状态码应报 invalid_type: %+v", issues)
	}
	// min(0)：负数越界。
	if _, issues = parse(`{"minRetryCount":-1}`); len(issues) != 1 ||
		issues[0].Code != "too_small" {
		t.Fatalf("负重试次数应报 too_small: %+v", issues)
	}
	// 类型不符：对象不是数值。
	if _, issues = parse(`{"userId":{"a":1}}`); len(issues) != 1 ||
		issues[0].Code != "invalid_type" {
		t.Fatalf("对象用户 id 应报 invalid_type: %+v", issues)
	}
	// 字符串字段收到数字：invalid_type。
	if _, issues = parse(`{"model":123}`); len(issues) != 1 || issues[0].Code != "invalid_type" {
		t.Fatalf("数字模型名应报 invalid_type: %+v", issues)
	}
}

// exportFakeKV 是 UsageLogsExportKV 的内存替身：失败分支（键缺失 / 删除）用真 Redis 只能靠
// 睡够 TTL 来触发，那是等真实长超时的坏测试。
type exportFakeKV struct {
	values  map[string][]byte
	ttls    map[string]time.Duration
	deleted []string
	scan    []string
}

func newExportFakeKV() *exportFakeKV {
	return &exportFakeKV{values: map[string][]byte{}, ttls: map[string]time.Duration{}}
}

func (kv *exportFakeKV) SetEx(
	_ context.Context, key string, payload []byte, ttl time.Duration,
) error {
	kv.values[key] = payload
	kv.ttls[key] = ttl
	return nil
}

func (kv *exportFakeKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	value, ok := kv.values[key]
	return value, ok, nil
}

func (kv *exportFakeKV) Del(_ context.Context, keys ...string) error {
	for _, key := range keys {
		kv.deleted = append(kv.deleted, key)
		delete(kv.values, key)
	}
	return nil
}

func (kv *exportFakeKV) Scan(_ context.Context, _ string, limit int) ([]string, error) {
	if limit < len(kv.scan) {
		return kv.scan[:limit], nil
	}
	return kv.scan, nil
}

func (kv *exportFakeKV) Exists(_ context.Context, key string) (bool, error) {
	_, ok := kv.values[key]
	return ok, nil
}

// TestExportJobStatusTTLAndShape 钉住键名、TTL 与状态正文形状（两侧互读的契约）。
func TestExportJobStatusTTLAndShape(t *testing.T) {
	kv := newExportFakeKV()
	module := &usageLogsModule{exports: &usageLogsExportRuntime{kv: kv}, now: time.Now}

	record := usageLogsExportJobRecord{
		JobID: "job-1", OwnerUserID: 9, Status: exportStatusQueued, Format: "csv",
	}
	if !module.writeExportJob(context.Background(), record) {
		t.Fatal("写状态失败")
	}

	statusKey := "cch:usage-logs:export:status:job-1"
	if _, ok := kv.values[statusKey]; !ok {
		t.Fatalf("状态键名不符，实得 %v", keysOfExportFake(kv.values))
	}
	if kv.ttls[statusKey] != 15*time.Minute {
		t.Fatalf("状态键 TTL 应为 15 分钟，实得 %v", kv.ttls[statusKey])
	}

	payload := string(kv.values[statusKey])
	for _, fragment := range []string{
		`"jobId":"job-1"`, `"ownerUserId":9`, `"status":"queued"`, `"format":"csv"`,
	} {
		if !strings.Contains(payload, fragment) {
			t.Fatalf("状态正文缺 %s: %s", fragment, payload)
		}
	}
	if strings.Contains(payload, `"error"`) {
		t.Fatalf("未失败时不该出现 error 键: %s", payload)
	}

	// 读回：字段必须一模一样（切换期 Node 写的那份也要能读）。
	back, err := module.readExportJob(context.Background(), "job-1")
	if err != nil || back == nil {
		t.Fatalf("读回失败: %v %v", back, err)
	}
	if back.OwnerUserID != 9 || back.Status != exportStatusQueued {
		t.Fatalf("读回内容不符: %+v", back)
	}

	// 结果键名与 Node 一致：prefix + jobId + ":result"。
	if got := usageLogsExportResultKey("job-1"); got != "cch:usage-logs:export:result:job-1:result" {
		t.Fatalf("结果键名不符: %s", got)
	}

	// 损坏的状态值按「查不到」处理（脏值不该让状态接口 500）。
	kv.values[statusKey] = []byte("{not json")
	if job, err := module.readExportJob(context.Background(), "job-1"); err != nil || job != nil {
		t.Fatalf("损坏值应视为不存在: %v %v", job, err)
	}
}

func keysOfExportFake(values map[string][]byte) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

// TestSweepUsageLogsExportOrphans 钉住孤儿清扫：只删「没有状态键」的结果键。
func TestSweepUsageLogsExportOrphans(t *testing.T) {
	kv := newExportFakeKV()
	module := &usageLogsModule{exports: &usageLogsExportRuntime{kv: kv}, now: time.Now}

	keepJob := "11111111-1111-4111-8111-111111111111"
	orphanJob := "22222222-2222-4222-8222-222222222222"
	alien := "cch:usage-logs:export:result:not-a-uuid:result"

	kv.values[usageLogsExportResultKey(keepJob)] = []byte("csv")
	kv.values[usageLogsExportStatusKey(keepJob)] = []byte("{}")
	kv.values[usageLogsExportResultKey(orphanJob)] = []byte("csv")
	kv.values[alien] = []byte("csv")
	kv.scan = []string{
		usageLogsExportResultKey(keepJob),
		usageLogsExportResultKey(orphanJob),
		alien,
	}

	deleted, err := module.sweepUsageLogsExportOrphans(context.Background())
	if err != nil {
		t.Fatalf("清扫失败: %v", err)
	}
	if deleted != 1 || len(kv.deleted) != 1 ||
		kv.deleted[0] != usageLogsExportResultKey(orphanJob) {
		t.Fatalf("应只删孤儿结果键，实得 %d: %v", deleted, kv.deleted)
	}
	if _, ok := kv.values[usageLogsExportResultKey(keepJob)]; !ok {
		t.Fatal("有状态键的结果不该被删")
	}
	if _, ok := kv.values[alien]; !ok {
		t.Fatal("形状不符的键不该被删")
	}
}

// TestExportJobIDFromResultKey 钉住反推的形状约束。
func TestExportJobIDFromResultKey(t *testing.T) {
	jobID := "11111111-1111-4111-8111-111111111111"
	if got := exportJobIDFromResultKey(usageLogsExportResultKey(jobID)); got != jobID {
		t.Fatalf("反推失败: %q", got)
	}
	for _, key := range []string{
		"cch:usage-logs:export:status:" + jobID,
		"cch:usage-logs:export:result:" + jobID,
		"cch:usage-logs:export:result::result",
		"other:key",
	} {
		if got := exportJobIDFromResultKey(key); got != "" {
			t.Fatalf("%s 不该被认作结果键，实得 %q", key, got)
		}
	}
}

// TestExportQueueFullIsReported 钉住「队列满如实报错」而不是静默丢弃。
//
// 不能拿真作业池测：它的 worker 会立刻把队列里的任务抽走，「把队列填满」在活跃池上无法稳定复现
// （队列深度 8，且有 2 个 worker 一直在消费）。故直接构造一个已满的池结构。
func TestExportQueueFullIsReported(t *testing.T) {
	pool := &usageLogsExportPool{tasks: make(chan usageLogsExportTask, 1)}
	if err := pool.submit(usageLogsExportTask{jobID: "first"}); err != nil {
		t.Fatalf("未满时不该拒: %v", err)
	}
	if err := pool.submit(usageLogsExportTask{jobID: "overflow"}); !errors.Is(err, errUsageLogsExportQueueFull) {
		t.Fatalf("队列满应报 errUsageLogsExportQueueFull，实得 %v", err)
	}

	// 装配出的真池必须是**有界**的（无界队列就回到了 Node 的无界并发，偏离就白写了）。
	live := newUsageLogsExportPool(func(context.Context, usageLogsExportTask) {})
	if got := cap(live.tasks); got != usageLogsExportQueueDepth {
		t.Fatalf("作业队列深度应为 %d，实得 %d", usageLogsExportQueueDepth, got)
	}
}

// TestExportStatusObjectOmitsEmptyError 钉住 error 键的省略语义（Node 的 `error: undefined`）。
func TestExportStatusObjectOmitsEmptyError(t *testing.T) {
	payload, err := exportStatusObject(usageLogsExportStatus{
		JobID: "j", Status: exportStatusCompleted, Format: "csv", ProgressPercent: 100,
	}).marshalJSON()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	if strings.Contains(string(payload), "error") {
		t.Fatalf("无错误时不该出现 error 键: %s", payload)
	}
	message := "boom"
	payload, err = exportStatusObject(usageLogsExportStatus{
		JobID: "j", Status: exportStatusFailed, Format: "csv", Error: &message,
	}).marshalJSON()
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if decoded["error"] != "boom" {
		t.Fatalf("失败时应带 error: %s", payload)
	}
}
