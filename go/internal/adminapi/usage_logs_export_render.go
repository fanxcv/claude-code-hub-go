package adminapi

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把 Node 导出渲染层逐条移植
// （src/lib/usage-logs/export/{columns,csv,numeric,format,summary}.ts）。
//
// 三条不变式（照 Node 的设计意图，改动任一处要同时想另两处）：
//
//  1. **列集只有一份**：CSV 与 XLSX 共用同一张 `exportDetailColumns`，两种格式永不漂移。
//  2. **数值一律走 spreadsheet 归一**：Excel 只保留 15 位有效数字，`numeric(21,15)` 的成本列
//     有 16 位，直接写进去会被 Excel 当文本、`SUM()` 失效。归一为「<=15 位有效数字、纯十进制、
//     去尾零」，代价是极小值被抹平——这是 Node 的既有取舍（numeric.ts 文件头）。
//  3. **时间按系统时区渲染**，时区只在表头上出现一次（`Time (Asia/Shanghai)`），
//     这样单元格本身仍是 Excel 可解析的干净 datetime。

// exportCellKind 复刻 DetailColumnKind。
type exportCellKind int

const (
	exportCellText exportCellKind = iota
	exportCellNumber
	exportCellDatetime
)

// exportNumberFormats 是 Node 的三个 numFmt 常量（columns.ts:34-36）。
const (
	exportCostNumFmt     = "0.00######"
	exportIntNumFmt      = "0"
	exportDatetimeNumFmt = "yyyy-mm-dd hh:mm:ss"
)

// exportCell 是一格的原始值。三态与 Node 的 `string | number | Date | null` 对应：
// 文本用 text，数字用 number（保留来源的十进制文本，numeric 列在库里就是文本），
// 时间用 date；三者都为「空」即 Node 的 null/undefined。
type exportCell struct {
	text   string
	number *string
	date   *time.Time
}

func exportTextCell(value string) exportCell { return exportCell{text: value} }

// exportIntCell 造一个整数格。pointer 为 nil 即 SQL NULL。
func exportIntCell(value *int64) exportCell {
	if value == nil {
		return exportCell{}
	}
	return exportCell{number: exportStringPtr(strconv.FormatInt(*value, 10))}
}

func exportIntPtrCell(value *int) exportCell {
	if value == nil {
		return exportCell{}
	}
	return exportCell{number: exportStringPtr(strconv.Itoa(*value))}
}

// exportDecimalCell 造一个 numeric 文本格（成本等列在库里是 numeric→::text）。
func exportDecimalCell(value *string) exportCell {
	if value == nil {
		return exportCell{}
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return exportCell{}
	}
	return exportCell{number: &trimmed}
}

func exportDateCell(value *time.Time) exportCell { return exportCell{date: value} }

// exportOptionalText 复刻 `?? ""`：nil 与空串在导出里同形。
func exportOptionalText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func exportStringPtr(value string) *string { return &value }

// exportDetailColumn 复刻 columns.ts 的 DetailColumn。
type exportDetailColumn struct {
	header       string
	kind         exportCellKind
	get          func(row *usageLogsExportRow) exportCell
	zeroWhenNull bool
	costFormat   bool
}

// usageLogsExportRow 是导出用的一行：两种数据源（message_request 与 usage_ledger 回退）
// 归一后的字段集。字段名与 Node 的 UsageLogRow 同名，便于逐列对照。
type usageLogsExportRow struct {
	CreatedAt           *time.Time
	UserName            string
	KeyName             string
	ProviderName        *string
	Model               *string
	OriginalModel       *string
	Endpoint            *string
	StatusCode          *int
	InputTokens         *int64
	OutputTokens        *int64
	CacheCreation5m     *int64
	CacheCreation1h     *int64
	CacheRead           *int64
	TotalTokens         *int64
	CostUSD             *string
	DurationMs          *int64
	SessionIdentityKind *string
	SessionID           *string
	SourceSessionID     *string
	// ProviderChain 是原始 JSONB 字节；回退路径（账本）没有这一列，故为 nil。
	ProviderChain []byte
}

// exportPrefixIDOf 复刻 columns.ts:39-41 的 prefixIdOf。
func exportPrefixIDOf(row *usageLogsExportRow) string {
	if row.SessionIdentityKind != nil && *row.SessionIdentityKind == "prefix_affinity" {
		return exportOptionalText(row.SessionID)
	}
	return ""
}

// exportPhysicalSessionIDOf 复刻 columns.ts:43-47 的 physicalSessionIdOf。
func exportPhysicalSessionIDOf(row *usageLogsExportRow) string {
	if row.SessionIdentityKind != nil && *row.SessionIdentityKind == "prefix_affinity" {
		return exportOptionalText(row.SourceSessionID)
	}
	return exportOptionalText(row.SessionID)
}

// exportDetailColumns 是列集（顺序即输出顺序，改动即破坏与 Node 的逐列对齐）。
var exportDetailColumns = []exportDetailColumn{
	{header: "Time", kind: exportCellDatetime, get: func(row *usageLogsExportRow) exportCell {
		return exportDateCell(row.CreatedAt)
	}},
	{header: "User", kind: exportCellText, get: func(row *usageLogsExportRow) exportCell {
		return exportTextCell(row.UserName)
	}},
	{header: "Key", kind: exportCellText, get: func(row *usageLogsExportRow) exportCell {
		return exportTextCell(row.KeyName)
	}},
	{header: "Provider", kind: exportCellText, get: func(row *usageLogsExportRow) exportCell {
		return exportTextCell(exportOptionalText(row.ProviderName))
	}},
	{header: "Model", kind: exportCellText, get: func(row *usageLogsExportRow) exportCell {
		return exportTextCell(exportOptionalText(row.Model))
	}},
	{header: "Original Model", kind: exportCellText, get: func(row *usageLogsExportRow) exportCell {
		return exportTextCell(exportOptionalText(row.OriginalModel))
	}},
	{header: "Endpoint", kind: exportCellText, get: func(row *usageLogsExportRow) exportCell {
		return exportTextCell(exportOptionalText(row.Endpoint))
	}},
	{header: "Status Code", kind: exportCellNumber, get: func(row *usageLogsExportRow) exportCell {
		return exportIntPtrCell(row.StatusCode)
	}},
	{header: "Input Tokens", kind: exportCellNumber, zeroWhenNull: true,
		get: func(row *usageLogsExportRow) exportCell { return exportIntCell(row.InputTokens) }},
	{header: "Output Tokens", kind: exportCellNumber, zeroWhenNull: true,
		get: func(row *usageLogsExportRow) exportCell { return exportIntCell(row.OutputTokens) }},
	{header: "Cache Write 5m", kind: exportCellNumber, zeroWhenNull: true,
		get: func(row *usageLogsExportRow) exportCell { return exportIntCell(row.CacheCreation5m) }},
	{header: "Cache Write 1h", kind: exportCellNumber, zeroWhenNull: true,
		get: func(row *usageLogsExportRow) exportCell { return exportIntCell(row.CacheCreation1h) }},
	{header: "Cache Read", kind: exportCellNumber, zeroWhenNull: true,
		get: func(row *usageLogsExportRow) exportCell { return exportIntCell(row.CacheRead) }},
	{header: "Total Tokens", kind: exportCellNumber, zeroWhenNull: true,
		get: func(row *usageLogsExportRow) exportCell { return exportIntCell(row.TotalTokens) }},
	{header: "Cost (USD)", kind: exportCellNumber, zeroWhenNull: true, costFormat: true,
		get: func(row *usageLogsExportRow) exportCell { return exportDecimalCell(row.CostUSD) }},
	{header: "Duration (ms)", kind: exportCellNumber, get: func(row *usageLogsExportRow) exportCell {
		return exportIntCell(row.DurationMs)
	}},
	{header: "Prefix ID", kind: exportCellText, get: func(row *usageLogsExportRow) exportCell {
		return exportTextCell(exportPrefixIDOf(row))
	}},
	{header: "Session ID", kind: exportCellText, get: func(row *usageLogsExportRow) exportCell {
		return exportTextCell(exportPhysicalSessionIDOf(row))
	}},
	{header: "Retry Count", kind: exportCellNumber, zeroWhenNull: true,
		get: func(row *usageLogsExportRow) exportCell {
			count := int64(exportRetryCountOf(row))
			return exportIntCell(&count)
		}},
}

// exportBuildDetailHeaders 复刻 buildDetailHeaders：datetime 列的表头带时区。
func exportBuildDetailHeaders(timezone string) []string {
	headers := make([]string, 0, len(exportDetailColumns))
	for _, column := range exportDetailColumns {
		if column.kind == exportCellDatetime {
			headers = append(headers, column.header+" ("+timezone+")")
			continue
		}
		headers = append(headers, column.header)
	}
	return headers
}

// exportIsBlankCell 复刻 isBlankValue：null/undefined 与纯空白文本都算空。
func exportIsBlankCell(cell exportCell) bool {
	switch {
	case cell.number != nil:
		return false
	case cell.date != nil:
		return false
	default:
		return strings.TrimSpace(cell.text) == ""
	}
}

// --- 数值归一（numeric.ts） --------------------------------------------------

// exportToFiniteNumber 复刻 toFiniteNumber：空/非有限一律 nil。
func exportToFiniteNumber(value string) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

// exportNormalizeDecimalForSpreadsheet 复刻 normalizeDecimalForSpreadsheet：
// 至多 15 位有效数字、纯十进制（绝不科学计数）、去尾零；非有限/空/缺失一律 "0"。
func exportNormalizeDecimalForSpreadsheet(cell exportCell) string {
	raw := ""
	if cell.number != nil {
		raw = *cell.number
	}
	value, ok := exportToFiniteNumber(raw)
	if !ok {
		return "0"
	}
	// JS 的 Intl.NumberFormat 对 -0 输出 "-0"（useGrouping:false 不影响符号）。
	if value == 0 && math.Signbit(value) {
		return "-0"
	}
	return exportFormatSignificant(value, 15)
}

// exportFormatSignificant 把 float64 渲染成「至多 significant 位有效数字的纯十进制文本」。
//
// 先按 'e' 格式化取定有效数字（保留位四舍五入，与 Intl 的 decimal rounding 同向），再把指数
// 展开成定点写法——`strconv` 的 'f' 直出会丢掉「先归一到 15 位」这一步，'g'/'e' 又会引入
// 科学计数，两者都不是 Excel 想要的形状。
func exportFormatSignificant(value float64, significant int) string {
	formatted := strconv.FormatFloat(value, 'e', significant-1, 64)
	mantissa, exponentText, found := strings.Cut(formatted, "e")
	if !found {
		return formatted
	}
	exponent, err := strconv.Atoi(exponentText)
	if err != nil {
		return formatted
	}

	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign = "-"
		mantissa = mantissa[1:]
	}
	mantissa = strings.Replace(mantissa, ".", "", 1)
	digits := strings.TrimRight(mantissa, "0")
	if digits == "" {
		return "0"
	}
	// 小数点位于第 exponent+1 位数字之后（0 表示整数部分为空）。
	pointAt := exponent + 1
	switch {
	case pointAt <= 0:
		return sign + "0." + strings.Repeat("0", -pointAt) + digits
	case pointAt >= len(digits):
		return sign + digits + strings.Repeat("0", pointAt-len(digits))
	default:
		return sign + digits[:pointAt] + "." + digits[pointAt:]
	}
}

// --- CSV（csv.ts） ----------------------------------------------------------

// exportCsvBOM 是 Node 的 CSV_BOM（U+FEFF）。写在正文最前，Excel 才不会把中文当 Latin-1。
const exportCsvBOM = "\ufeff"

// exportEscapeCsvField 复刻 escapeCsvField：先中和公式注入（首字符是 = + - @ 时前缀单引号），
// 再按需加引号并把内部引号翻倍。
func exportEscapeCsvField(field string) string {
	trimmed := strings.TrimLeft(field, " \t\r\n")
	safe := field
	if trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		safe = "'" + field
	}
	if strings.ContainsAny(safe, ",\"\n\r") {
		return `"` + strings.ReplaceAll(safe, `"`, `""`) + `"`
	}
	return safe
}

// exportRenderCsvCell 复刻 renderCsvCell（csv.ts:40-55）。
func exportRenderCsvCell(cell exportCell, column exportDetailColumn, location *time.Location) string {
	switch column.kind {
	case exportCellDatetime:
		if cell.date == nil {
			return ""
		}
		return exportFormatTimestamp(*cell.date, location)
	case exportCellNumber:
		if exportIsBlankCell(cell) && !column.zeroWhenNull {
			return ""
		}
		return exportNormalizeDecimalForSpreadsheet(cell)
	default:
		return exportEscapeCsvField(cell.text)
	}
}

// exportBuildCsvHeaderLine 复刻 buildCsvHeaderLine。
func exportBuildCsvHeaderLine(timezone string) string {
	headers := exportBuildDetailHeaders(timezone)
	escaped := make([]string, 0, len(headers))
	for _, header := range headers {
		escaped = append(escaped, exportEscapeCsvField(header))
	}
	return strings.Join(escaped, ",")
}

// exportBuildCsvRows 复刻 buildCsvRows。
func exportBuildCsvRows(rows []usageLogsExportRow, location *time.Location) []string {
	lines := make([]string, 0, len(rows))
	for index := range rows {
		row := &rows[index]
		cells := make([]string, 0, len(exportDetailColumns))
		for _, column := range exportDetailColumns {
			cells = append(cells, exportRenderCsvCell(column.get(row), column, location))
		}
		lines = append(lines, strings.Join(cells, ","))
	}
	return lines
}

// exportFormatTimestamp 复刻 formatExportTimestamp（yyyy-MM-dd HH:mm:ss，按给定时区）。
func exportFormatTimestamp(at time.Time, location *time.Location) string {
	if location == nil {
		location = time.UTC
	}
	return at.In(location).Format("2006-01-02 15:04:05")
}

// exportExcelZonedSerial 复刻 toExcelZonedDate + excelSerial：把瞬时点换算成「该时区的墙钟时间」
// 再求 Excel 序列号。返回 0（false）表示时间无效。
//
// Excel 的日序列原点在 1899-12-30，与 Unix 纪元差 25569 天；时间戳都是整秒，故按秒取整，
// 免得二进制除法的小数误差把秒位显示偏一格（Node xlsx.ts:excelSerial 同此意）。
func exportExcelZonedSerial(at time.Time, location *time.Location) (float64, bool) {
	if at.IsZero() {
		return 0, false
	}
	if location == nil {
		location = time.UTC
	}
	zoned := at.In(location)
	wall := time.Date(zoned.Year(), zoned.Month(), zoned.Day(),
		zoned.Hour(), zoned.Minute(), zoned.Second(), 0, time.UTC)
	serial := float64(wall.Unix())/86400 + 25569
	return math.Round(serial*86400) / 86400, true
}

// --- 重试次数（provider-chain-formatter.ts:144-254） ------------------------

// exportChainItem 是 providerChain JSONB 元素里导出用得到的两列。
type exportChainItem struct {
	Reason     string `json:"reason"`
	StatusCode *int   `json:"statusCode"`
}

// exportRetryCountOf 复刻 getRetryCount：并发竞速（hedge）不算顺序重试，一律 0；
// 否则「实际请求」条数减一。链缺失或解析失败按 0（Node 里 providerChain 为 null 时也是 0）。
func exportRetryCountOf(row *usageLogsExportRow) int {
	if len(row.ProviderChain) == 0 {
		return 0
	}
	var chain []exportChainItem
	if err := json.Unmarshal(row.ProviderChain, &chain); err != nil || chain == nil {
		return 0
	}
	if exportIsHedgeRace(chain) {
		return 0
	}
	actual := 0
	for _, item := range chain {
		if exportIsActualRequest(item) {
			actual++
		}
	}
	if actual-1 < 0 {
		return 0
	}
	return actual - 1
}

// exportIsHedgeRace 复刻 isHedgeRace。
func exportIsHedgeRace(chain []exportChainItem) bool {
	for _, item := range chain {
		switch item.Reason {
		case "hedge_triggered", "hedge_launched", "hedge_winner",
			"hedge_loser_cancelled", "hedge_loser_billed":
			return true
		}
	}
	return false
}

// exportIsActualRequest 复刻 isActualRequest。
func exportIsActualRequest(item exportChainItem) bool {
	switch item.Reason {
	case "concurrent_limit_failed",
		"retry_failed", "response_incomplete", "system_error", "resource_not_found",
		"client_error_non_retryable", "endpoint_pool_exhausted", "vendor_type_all_timeout",
		"client_abort", "client_abort_no_first_byte",
		"hedge_winner", "hedge_loser_cancelled", "hedge_loser_billed",
		"http2_fallback":
		return true
	case "hedge_triggered", "hedge_launched":
		return false
	case "request_success", "retry_success":
		// Node 用真值判断：statusCode 为 0/null/undefined 都不算。
		return item.StatusCode != nil && *item.StatusCode != 0
	default:
		return false
	}
}

// --- 汇总（summary.ts） ------------------------------------------------------

// exportSummaryHeaders 复刻 SUMMARY_HEADERS。
var exportSummaryHeaders = []string{
	"Period", "Requests", "Input Tokens", "Output Tokens",
	"Cache Write 5m", "Cache Write 1h", "Cache Read", "Total Tokens", "Cost (USD)",
}

const exportUnknownPeriod = "Unknown"

// exportSummaryRow 复刻 SummaryRow。
type exportSummaryRow struct {
	Period       string
	Requests     int64
	InputTokens  int64
	OutputTokens int64
	CacheWrite5m int64
	CacheWrite1h int64
	CacheRead    int64
	TotalTokens  int64
	Cost         float64
}

// usageLogsExportSummary 复刻 UsageLogsSummary。
type usageLogsExportSummary struct {
	Granularity string // "daily" | "hourly"
	Rows        []exportSummaryRow
	Total       exportSummaryRow
}

func exportEmptySummaryRow(period string) exportSummaryRow {
	return exportSummaryRow{Period: period}
}

// exportAccumulateSummary 复刻 summary.ts:accumulate。
func exportAccumulateSummary(target *exportSummaryRow, row *usageLogsExportRow) {
	target.Requests++
	target.InputTokens += exportDerefInt64(row.InputTokens)
	target.OutputTokens += exportDerefInt64(row.OutputTokens)
	target.CacheWrite5m += exportDerefInt64(row.CacheCreation5m)
	target.CacheWrite1h += exportDerefInt64(row.CacheCreation1h)
	target.CacheRead += exportDerefInt64(row.CacheRead)
	target.TotalTokens += exportDerefInt64(row.TotalTokens)
	if row.CostUSD != nil {
		if cost, ok := exportToFiniteNumber(*row.CostUSD); ok {
			target.Cost += cost
		}
	}
}

func exportDerefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func exportMergeSummary(target *exportSummaryRow, source exportSummaryRow) {
	target.Requests += source.Requests
	target.InputTokens += source.InputTokens
	target.OutputTokens += source.OutputTokens
	target.CacheWrite5m += source.CacheWrite5m
	target.CacheWrite1h += source.CacheWrite1h
	target.CacheRead += source.CacheRead
	target.TotalTokens += source.TotalTokens
	target.Cost += source.Cost
}

// usageLogsExportSummaryAccumulator 复刻 createSummaryAccumulator：逐行折叠，桶按小时存，
// finalize 时按「出现过的自然日数」决定按小时还是按天输出。
type usageLogsExportSummaryAccumulator struct {
	location    *time.Location
	hourBuckets map[string]*exportSummaryRow
	hourOrder   []string
	days        map[string]bool
	total       exportSummaryRow
	unknown     *exportSummaryRow
}

func newUsageLogsExportSummaryAccumulator(location *time.Location) *usageLogsExportSummaryAccumulator {
	if location == nil {
		location = time.UTC
	}
	return &usageLogsExportSummaryAccumulator{
		location:    location,
		hourBuckets: map[string]*exportSummaryRow{},
		days:        map[string]bool{},
		total:       exportEmptySummaryRow("Total"),
	}
}

func (accumulator *usageLogsExportSummaryAccumulator) Add(row *usageLogsExportRow) {
	exportAccumulateSummary(&accumulator.total, row)
	if row.CreatedAt == nil || row.CreatedAt.IsZero() {
		if accumulator.unknown == nil {
			unknown := exportEmptySummaryRow(exportUnknownPeriod)
			accumulator.unknown = &unknown
		}
		exportAccumulateSummary(accumulator.unknown, row)
		return
	}
	// 小时桶与日桶的键都是零填充 ISO 前缀，故字典序即时间序（finalize 依赖这点）。
	hourKey := row.CreatedAt.In(accumulator.location).Format("2006-01-02 15") + ":00"
	accumulator.days[hourKey[:10]] = true
	bucket, ok := accumulator.hourBuckets[hourKey]
	if !ok {
		created := exportEmptySummaryRow(hourKey)
		bucket = &created
		accumulator.hourBuckets[hourKey] = bucket
		accumulator.hourOrder = append(accumulator.hourOrder, hourKey)
	}
	exportAccumulateSummary(bucket, row)
}

func (accumulator *usageLogsExportSummaryAccumulator) Finalize() usageLogsExportSummary {
	granularity := "daily"
	if len(accumulator.days) <= 1 {
		granularity = "hourly"
	}

	rows := make([]exportSummaryRow, 0, len(accumulator.hourBuckets)+1)
	if granularity == "hourly" {
		for _, key := range accumulator.hourOrder {
			rows = append(rows, *accumulator.hourBuckets[key])
		}
	} else {
		dayBuckets := map[string]*exportSummaryRow{}
		dayOrder := make([]string, 0, len(accumulator.hourOrder))
		for _, key := range accumulator.hourOrder {
			dayKey := key[:10]
			bucket, ok := dayBuckets[dayKey]
			if !ok {
				created := exportEmptySummaryRow(dayKey)
				bucket = &created
				dayBuckets[dayKey] = bucket
				dayOrder = append(dayOrder, dayKey)
			}
			exportMergeSummary(bucket, *accumulator.hourBuckets[key])
		}
		for _, key := range dayOrder {
			rows = append(rows, *dayBuckets[key])
		}
	}
	if accumulator.unknown != nil {
		rows = append(rows, *accumulator.unknown)
	}
	sortExportSummaryRows(rows)
	return usageLogsExportSummary{Granularity: granularity, Rows: rows, Total: accumulator.total}
}

// sortExportSummaryRows 复刻 byPeriod：按 period 字典序（"Unknown" 因此排在日期之后）。
func sortExportSummaryRows(rows []exportSummaryRow) {
	for index := 1; index < len(rows); index++ {
		for cursor := index; cursor > 0 && rows[cursor].Period < rows[cursor-1].Period; cursor-- {
			rows[cursor], rows[cursor-1] = rows[cursor-1], rows[cursor]
		}
	}
}

// --- 数据源适配 --------------------------------------------------------------

// exportRowFromMessage 把列表行归一为导出行。totalTokens 的口径与 Node 一致：
// 输入 + 输出 + 缓存写入 + 缓存读取（不含 5m/1h 这两个细分列，它们是 cacheCreation 的下游）。
func exportRowFromMessage(row *store.UsageLogRow) usageLogsExportRow {
	return usageLogsExportRow{
		CreatedAt:           row.CreatedAt,
		UserName:            exportOptionalText(row.UserName),
		KeyName:             exportOptionalText(row.KeyName),
		ProviderName:        row.ProviderName,
		Model:               row.Model,
		OriginalModel:       row.OriginalModel,
		Endpoint:            row.Endpoint,
		StatusCode:          row.StatusCode,
		InputTokens:         row.InputTokens,
		OutputTokens:        row.OutputTokens,
		CacheCreation5m:     row.CacheCreation5mInputTokens,
		CacheCreation1h:     row.CacheCreation1hInputTokens,
		CacheRead:           row.CacheReadInputTokens,
		TotalTokens:         exportMessageTotalTokens(row),
		CostUSD:             row.CostUSD,
		DurationMs:          row.DurationMs,
		SessionIdentityKind: row.SessionIdentityKind,
		SessionID:           row.SessionID,
		SourceSessionID:     row.SourceSessionID,
		ProviderChain:       row.ProviderChain,
	}
}

func exportMessageTotalTokens(row *store.UsageLogRow) *int64 {
	total := exportDerefInt64(row.InputTokens) + exportDerefInt64(row.OutputTokens) +
		exportDerefInt64(row.CacheCreationInputTokens) + exportDerefInt64(row.CacheReadInputTokens)
	return &total
}

// exportRowFromLedger 归一账本回退行（Node 的 fallbackLogs 映射）。
//
// 三处与列表行的差别照 Node：用户名缺省为 "User #<id>"、密钥名缺省回落到密钥值、
// providerChain 为 null（故重试次数恒 0）。
func exportRowFromLedger(row *store.LedgerUsageLogRow) usageLogsExportRow {
	userName := exportOptionalText(row.UserName)
	if userName == "" && row.UserID != nil {
		userName = "User #" + strconv.FormatInt(*row.UserID, 10)
	}
	keyName := exportOptionalText(row.KeyName)
	if keyName == "" {
		keyName = exportOptionalText(row.Key)
	}
	total := exportDerefInt64(row.InputTokens) + exportDerefInt64(row.OutputTokens) +
		exportDerefInt64(row.CacheCreationInputTokens) + exportDerefInt64(row.CacheReadInputTokens)
	return usageLogsExportRow{
		CreatedAt:           row.CreatedAt,
		UserName:            userName,
		KeyName:             keyName,
		ProviderName:        row.ProviderName,
		Model:               row.Model,
		OriginalModel:       row.OriginalModel,
		Endpoint:            row.Endpoint,
		StatusCode:          row.StatusCode,
		InputTokens:         row.InputTokens,
		OutputTokens:        row.OutputTokens,
		CacheCreation5m:     row.CacheCreation5mInputTokens,
		CacheCreation1h:     row.CacheCreation1hInputTokens,
		CacheRead:           row.CacheReadInputTokens,
		TotalTokens:         &total,
		CostUSD:             row.CostUSD,
		DurationMs:          row.DurationMs,
		SessionIdentityKind: row.SessionIdentityKind,
		SessionID:           row.SessionID,
		SourceSessionID:     row.SourceSessionID,
	}
}
