package adminapi

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/xuri/excelize/v2"
)

// 本文件用 excelize 组装与 Node 同形的 XLSX（两张表：明细 "Usage Logs" + 日/小时汇总）。
//
// **与 Node 的已知差异（登记）**：Node 是手写 zip + 三份 XML 的自研写出器
// （src/lib/usage-logs/export/xlsx.ts，282 行），因此**字节级不可对齐**——压缩参数、XML
// 内的属性顺序、共享字符串表的有无都会不同。对齐面是**语义面**：表名、表头（含时区后缀）、
// 单元格类型（数字真是数字、时间是真 Excel 日期）、数字格式串、汇总表的行与合计行。
// 消费者是 Excel/WPS，读的是语义而不是字节，故这个差异是允许差异（A2 对拍时按语义断言）。
//
// **流式写法**：明细表用 excelize 的 StreamWriter，行写一行丢一行——Node 的用意也是「不把
// 整表留在内存」（xlsx.ts 文件头），而 excelize 的普通 SetCell 写法会让每个单元格留在内存里
// （50 万格 ≈ 数百 MiB）。汇总表行数极少，用普通写法。

const (
	// 导出明细表的表名（Node xlsx.ts:workbookXml）。
	exportXlsxDetailSheet = "Usage Logs"
	// 导出汇总表名（Node summarySheetName）。
	exportXlsxDailySheet  = "Daily Summary"
	exportXlsxHourlySheet = "Hourly Summary"
)

// 导出上限：Node 侧没有任何上限，导出是一路把所有匹配行读进内存再拼文档；在本进程的内存预算
// （700 MiB，且数据面流式请求要占其中一部分）下，两个并发导出各自无界会直接把进程拖崩。
// 因此设「行数 + 内容字节」双上限，超限**显式失败**（作业状态里带可判别错误），而不是 OOM。
//
// 取值依据：单行明细分 19 列、约 300 字节渲染后文本 → 20 万行 ≈ 60 MiB 内容，200k 行是
// 「一次导出一年日志」的量级；内容上限 32 MiB 与之相当。并发上限为 worker 数（2），
// 故导出侧最坏瞬时内存 ≈ 2 × (内容 + 组装副本) ≈ 2 × ~100 MiB ≈ 200 MiB，留在预算内。
const (
	exportMaxRows    = 200000
	exportMaxContent = 32 << 20
)

// errUsageLogsExportRowLimit / errUsageLogsExportSizeLimit 是两类超限错误。
//
// 用哨兵错误而不是字符串子串：作业状态里的 error 文案要给用户看（含两个上限值），
// 而调用方要能判别是哪一类（同步 CSV 路径据此选错误码）。
var (
	errUsageLogsExportRowLimit  = errors.New("usage_logs.export_row_limit_exceeded")
	errUsageLogsExportSizeLimit = errors.New("usage_logs.export_size_limit_exceeded")
)

func exportRowLimitError() error {
	return fmt.Errorf("%w: 导出上限 %d 行", errUsageLogsExportRowLimit, exportMaxRows)
}

func exportSizeLimitError() error {
	return fmt.Errorf("%w: 导出内容上限 %d MiB", errUsageLogsExportSizeLimit, exportMaxContent>>20)
}

// usageLogsExportXlsxWriter 是流式 XLSX 写出器。
type usageLogsExportXlsxWriter struct {
	file       *excelize.File
	stream     *excelize.StreamWriter
	summary    *usageLogsExportSummaryAccumulator
	location   *time.Location
	textStyle  int
	intStyle   int
	costStyle  int
	dateStyle  int
	headerFont int
	rows       int
	finished   bool
}

// newUsageLogsExportXlsxWriter 建工作簿：重命名默认表为明细表并写入表头行。
func newUsageLogsExportXlsxWriter(location *time.Location, timezone string) (*usageLogsExportXlsxWriter, error) {
	if location == nil {
		location = time.UTC
	}
	file := excelize.NewFile()
	if err := file.SetSheetName("Sheet1", exportXlsxDetailSheet); err != nil {
		_ = file.Close()
		return nil, err
	}

	writer := &usageLogsExportXlsxWriter{
		file:     file,
		summary:  newUsageLogsExportSummaryAccumulator(location),
		location: location,
	}

	// 样式：三档自定义数字格式对 Node 的三个 numFmt 常量，表头加粗对 Node 的 STYLE.header。
	styles := []struct {
		target *int
		format string
		bold   bool
	}{
		{&writer.textStyle, "", false},
		{&writer.intStyle, exportIntNumFmt, false},
		{&writer.costStyle, exportCostNumFmt, false},
		{&writer.dateStyle, exportDatetimeNumFmt, false},
		{&writer.headerFont, "", true},
	}
	for _, style := range styles {
		spec := &excelize.Style{}
		if style.format != "" {
			format := style.format
			spec.CustomNumFmt = &format
		}
		if style.bold {
			spec.Font = &excelize.Font{Bold: true}
		}
		id, err := file.NewStyle(spec)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		*style.target = id
	}

	stream, err := file.NewStreamWriter(exportXlsxDetailSheet)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	writer.stream = stream

	// 冻结首行（Node worksheetXml 的 pane ySplit=1/topLeftCell=A2）。必须在第一行 SetRow 之前：
	// StreamWriter 在首次写行时就把 sheetViews 头字段冲刷出去，之后再改 pane 不会生效。
	if err := file.SetPanes(exportXlsxDetailSheet, &excelize.Panes{
		Freeze:      true,
		YSplit:      1,
		TopLeftCell: "A2",
		ActivePane:  "bottomLeft",
		Selection: []excelize.Selection{
			{SQRef: "A2", ActiveCell: "A2", Pane: "bottomLeft"},
		},
	}); err != nil {
		_ = file.Close()
		return nil, err
	}

	headers := exportBuildDetailHeaders(timezone)
	cells := make([]any, 0, len(headers))
	for _, header := range headers {
		cells = append(cells, excelize.Cell{StyleID: writer.headerFont, Value: header})
	}
	if err := stream.SetRow("A1", cells); err != nil {
		_ = file.Close()
		return nil, err
	}
	return writer, nil
}

// addBatch 折进汇总并写出一批明细行。超过行数上限即报错（错误在作业状态里可见）。
func (writer *usageLogsExportXlsxWriter) addBatch(rows []usageLogsExportRow) error {
	if writer.finished {
		return errors.New("adminapi: XLSX 写出器已收尾")
	}
	// 先按批判上限再逐行写：超限时立即失败，不把半张表写出去。
	if writer.rows+len(rows) > exportMaxRows {
		return exportRowLimitError()
	}
	for index := range rows {
		row := &rows[index]
		writer.rows++
		writer.summary.Add(row)
		if err := writer.writeRow(row, writer.rows+1); err != nil {
			return err
		}
	}
	return nil
}

func (writer *usageLogsExportXlsxWriter) writeRow(row *usageLogsExportRow, rowNumber int) error {
	cells := make([]any, 0, len(exportDetailColumns))
	for _, column := range exportDetailColumns {
		cells = append(cells, writer.cellValue(column, row))
	}
	return writer.stream.SetRow("A"+strconv.Itoa(rowNumber), cells)
}

// cellValue 复刻 Node 的 detailCell：空值写「带样式的空格子」，与 Node 的
// `<c r=".." s=".." />` 同义。
func (writer *usageLogsExportXlsxWriter) cellValue(
	column exportDetailColumn,
	row *usageLogsExportRow,
) excelize.Cell {
	cell := column.get(row)
	switch column.kind {
	case exportCellDatetime:
		if cell.date == nil {
			return excelize.Cell{}
		}
		serial, ok := exportExcelZonedSerial(*cell.date, writer.location)
		if !ok {
			return excelize.Cell{}
		}
		return excelize.Cell{StyleID: writer.dateStyle, Value: serial}
	case exportCellNumber:
		if exportIsBlankCell(cell) && !column.zeroWhenNull {
			return excelize.Cell{StyleID: writer.intStyle}
		}
		normalized := exportNormalizeDecimalForSpreadsheet(cell)
		style := writer.intStyle
		if column.costFormat {
			style = writer.costStyle
		}
		value, err := strconv.ParseFloat(normalized, 64)
		if err != nil {
			return excelize.Cell{StyleID: style}
		}
		return excelize.Cell{StyleID: style, Value: value}
	default:
		if cell.text == "" {
			return excelize.Cell{StyleID: writer.textStyle}
		}
		return excelize.Cell{StyleID: writer.textStyle, Value: cell.text}
	}
}

// Finish 收尾：冲刷明细表、写汇总表、导字节。
func (writer *usageLogsExportXlsxWriter) Finish() ([]byte, error) {
	if writer.finished {
		return nil, errors.New("adminapi: XLSX 写出器已收尾")
	}
	writer.finished = true
	if err := writer.stream.Flush(); err != nil {
		_ = writer.file.Close()
		return nil, err
	}
	if err := writer.writeSummarySheet(); err != nil {
		_ = writer.file.Close()
		return nil, err
	}

	buffer, err := writer.file.WriteToBuffer()
	if err != nil {
		_ = writer.file.Close()
		return nil, err
	}
	// Close 只负责回收临时文件与流缓冲，失败不影响已经成形的字节（内容已完整落在 buffer 里），
	// 故这里不让它把一次成功的导出变成失败。
	_ = writer.file.Close()
	if buffer.Len() > exportMaxContent {
		return nil, exportSizeLimitError()
	}
	return buffer.Bytes(), nil
}

// writeSummarySheet 复刻 buildSummarySheet：表头 + 各期行 + 合计行（合计行的期列用表头样式）。
func (writer *usageLogsExportXlsxWriter) writeSummarySheet() error {
	summary := writer.summary.Finalize()
	name := exportXlsxDailySheet
	if summary.Granularity == "hourly" {
		name = exportXlsxHourlySheet
	}
	if _, err := writer.file.NewSheet(name); err != nil {
		return err
	}

	for index, header := range exportSummaryHeaders {
		ref, err := excelize.CoordinatesToCellName(index+1, 1)
		if err != nil {
			return err
		}
		if err := writer.file.SetCellValue(name, ref, header); err != nil {
			return err
		}
		if err := writer.file.SetCellStyle(name, ref, ref, writer.headerFont); err != nil {
			return err
		}
	}

	for index, row := range summary.Rows {
		if err := writer.writeSummaryRow(name, row, index+2, writer.textStyle); err != nil {
			return err
		}
	}
	totalRow := len(summary.Rows) + 2
	return writer.writeSummaryRow(name, summary.Total, totalRow, writer.headerFont)
}

func (writer *usageLogsExportXlsxWriter) writeSummaryRow(
	sheet string,
	row exportSummaryRow,
	rowNumber int,
	periodStyle int,
) error {
	values := []struct {
		text  string
		value float64
		style int
	}{
		{text: row.Period, style: periodStyle},
		{value: float64(row.Requests), style: writer.intStyle},
		{value: float64(row.InputTokens), style: writer.intStyle},
		{value: float64(row.OutputTokens), style: writer.intStyle},
		{value: float64(row.CacheWrite5m), style: writer.intStyle},
		{value: float64(row.CacheWrite1h), style: writer.intStyle},
		{value: float64(row.CacheRead), style: writer.intStyle},
		{value: float64(row.TotalTokens), style: writer.intStyle},
		{value: exportSummaryCostValue(row.Cost), style: writer.costStyle},
	}
	for index, entry := range values {
		ref, err := excelize.CoordinatesToCellName(index+1, rowNumber)
		if err != nil {
			return err
		}
		if entry.text != "" {
			if err := writer.file.SetCellStr(sheet, ref, entry.text); err != nil {
				return err
			}
		} else if err := writer.file.SetCellFloat(sheet, ref, entry.value, -1, 64); err != nil {
			return err
		}
		if err := writer.file.SetCellStyle(sheet, ref, ref, entry.style); err != nil {
			return err
		}
	}
	return nil
}

// exportSummaryCostValue 把累计成本按 spreadsheet 归一后转成数值。
//
// Node 走的是「先归一成 15 位有效数字的文本，再把文本写进 <v>」；这里转成 float64 交给
// excelize（它按 shortest round-trip 写），两者在 15 位有效数字以内同形。
func exportSummaryCostValue(cost float64) float64 {
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return 0
	}
	normalized := exportNormalizeDecimalForSpreadsheet(exportCell{number: exportStringPtr(costText(cost))})
	value, err := strconv.ParseFloat(normalized, 64)
	if err != nil {
		return 0
	}
	return value
}

// costText 把累计成本转成归一函数能吃的十进制文本。
func costText(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// usageLogsExportCsvWriter 累积 CSV 行（Node 的 csvLines）。
//
// 与 XLSX 写出器同形（addBatch + Finish）：两条格式路径共用「批入 + 收尾」的骨架，
// 上限与 BOM 的处置就只有一处。
type usageLogsExportCsvWriter struct {
	location *time.Location
	lines    []string
	rows     int
}

func newUsageLogsExportCsvWriter(timezone string) *usageLogsExportCsvWriter {
	return &usageLogsExportCsvWriter{
		lines: []string{exportBuildCsvHeaderLine(timezone)},
	}
}

func (writer *usageLogsExportCsvWriter) addBatch(rows []usageLogsExportRow) error {
	if writer.rows+len(rows) > exportMaxRows {
		return exportRowLimitError()
	}
	writer.lines = append(writer.lines, exportBuildCsvRows(rows, writer.location)...)
	writer.rows += len(rows)
	return nil
}

// Finish 拼出正文：BOM + 各行（以 \n 连接，行尾无换行）。
func (writer *usageLogsExportCsvWriter) Finish() (string, error) {
	content := exportCsvBOM + joinLines(writer.lines)
	if len(content) > exportMaxContent {
		return "", exportSizeLimitError()
	}
	return content, nil
}

func joinLines(lines []string) string {
	var builder bytes.Buffer
	for index, line := range lines {
		if index > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(line)
	}
	return builder.String()
}
