package store

import (
	"math"
	"regexp"
	"strings"

	"github.com/shopspring/decimal"
)

// CostScale 复刻 src/lib/utils/currency.ts 的 COST_SCALE。
const CostScale = 15

var costLiteralPattern = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)([eE][+-]?\d+)?$`)

// FormatCostForStorage 复刻 TS 的 formatCostForStorage：把入参规整为 COST_SCALE 位小数的
// 定点字符串；无效入参返回 ok=false，调用方据此跳过整次写入（TS 侧同样提前 return）。
//
// TS 的 toDecimal 接受 string | number | Decimal，非法值（空串、NaN、null、非数字）返回 null。
func FormatCostForStorage(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || !costLiteralPattern.MatchString(trimmed) {
		return "", false
	}
	parsed, err := decimal.NewFromString(trimmed)
	if err != nil {
		return "", false
	}
	return parsed.Round(CostScale).StringFixed(CostScale), true
}

// FormatCostFloat 是 FormatCostForStorage 的浮点入参版本。NaN 与 ±Inf 视为非法，
// 与 TS 侧 Number.isFinite 的判定一致。
func FormatCostFloat(value float64) (string, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", false
	}
	return decimal.NewFromFloat(value).Round(CostScale).StringFixed(CostScale), true
}

// AddCostStrings 复刻 cost_usd 的加性语义（hedge 输家累加语句里的
// COALESCE(cost_usd, 0) + delta）：空串按 0 处理，任一侧是非数字则返回 ok=false。
func AddCostStrings(left string, right string) (string, bool) {
	parsedLeft, err := parseCostOrZero(left)
	if err != nil {
		return "", false
	}
	parsedRight, err := parseCostOrZero(right)
	if err != nil {
		return "", false
	}
	return parsedLeft.Add(parsedRight).Round(CostScale).StringFixed(CostScale), true
}

func parseCostOrZero(value string) (decimal.Decimal, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return decimal.Zero, nil
	}
	return decimal.NewFromString(trimmed)
}
