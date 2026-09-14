package adminapi

import (
	"math"
	"strconv"
	"strings"
)

// 本文件复刻 src/lib/utils/currency.ts 的 formatCurrency（排行榜的 `*Formatted` 字段全靠它）。
//
// 口径逐条对齐：
//  1. 数值取 **最短往返十进制**（Node 侧 decimal.js 从 JS number 构造时也走 String(n)），
//     再按 HALF_UP 保留两位——不是先乘 100 再 math.Round（那会因二进制表示把 1.005 舍成 1.00）。
//  2. 分组与小数分隔符按 locale（Intl.NumberFormat）：九种币种里只有 de-DE 与其余不同。
//  3. 符号直接前置，负数表现为 `$-1,234.56`（Node 也是这么拼的）。
//
// 一处登记差异：币种不在表内时 Go 回退 USD；Node 会在 `CURRENCY_CONFIG[code].locale` 上抛错
// 变成 500。settings 的白名单已把币种限定在九种之内，故这条只在脏数据下可见。
type leaderboardCurrency struct {
	Symbol string
	Locale string
}

// leaderboardCurrencies 照 CURRENCY_CONFIG（键集与 system_settings.go 的 currencyCodes 同源）。
var leaderboardCurrencies = map[string]leaderboardCurrency{
	"USD": {"$", "en-US"},
	"CNY": {"¥", "zh-CN"},
	"EUR": {"€", "de-DE"},
	"JPY": {"¥", "ja-JP"},
	"GBP": {"£", "en-GB"},
	"HKD": {"HK$", "zh-HK"},
	"TWD": {"NT$", "zh-TW"},
	"KRW": {"₩", "ko-KR"},
	"SGD": {"S$", "en-SG"},
}

// leaderboardGroupSeparators 返回 (千分位, 小数点) 两个分隔符。
//
// 只有 de-DE 是点分组逗号小数；其余八种都是逗号分组点小数（Intl 对 ja-JP/ko-KR/zh-* 同样如此）。
func leaderboardGroupSeparators(locale string) (string, string) {
	if locale == "de-DE" {
		return ".", ","
	}
	return ",", "."
}

// leaderboardFormatCurrency 复刻 formatCurrency(value, currencyCode)。
func leaderboardFormatCurrency(value float64, currencyCode string) string {
	currency, ok := leaderboardCurrencies[currencyCode]
	if !ok {
		currency = leaderboardCurrencies["USD"]
	}
	groupSeparator, decimalSeparator := leaderboardGroupSeparators(currency.Locale)
	negative, digits := leaderboardRoundHalfUpToTwoDigits(value)
	integer, fraction := leaderboardSplitDigits(digits)

	var builder strings.Builder
	if negative {
		builder.WriteString("-")
	}
	builder.WriteString(leaderboardGroupDigits(integer, groupSeparator))
	builder.WriteString(decimalSeparator)
	builder.WriteString(fraction)
	return currency.Symbol + builder.String()
}

// leaderboardRoundHalfUpToTwoDigits 把 float64 按 HALF_UP 保留两位，返回 (是否负, 数字串)。
//
// 数字串形如 "123456" 的最后两位是小数位（保证至少三位，即至少一位整数 + 两位小数）。
func leaderboardRoundHalfUpToTwoDigits(value float64) (bool, string) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		// Node 侧 decimal.js 对 NaN/Infinity 会抛错；这里给 0 面包屑（真实数据不可能到这里）。
		return false, "000"
	}
	// 'f' 与 -1 组合给出最短往返十进制且**不用指数记法**（1e-7 出 "0.0000001"）。
	text := strconv.FormatFloat(value, 'f', -1, 64)

	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	integer, fraction, _ := strings.Cut(text, ".")
	fraction = fraction + "00" // 不足两位补零
	keptFraction := fraction[:2]
	dropped := fraction[2:]

	digits := integer + keptFraction
	// HALF_UP：被丢弃部分的首位 >= 5 就进位（与 Decimal.toDecimalPlaces(2, ROUND_HALF_UP) 同判）。
	if len(dropped) > 0 && dropped[0] >= '5' {
		digits = leaderboardIncrementDigits(digits)
	}
	if digits == "" {
		digits = "0"
	}
	// 补足「至少一位整数 + 两位小数」。
	for len(digits) < 3 {
		digits = "0" + digits
	}
	// 负号照抄（含 `-0.00` 这种零值负号：Node 的 toLocaleString 同样保留）。
	return negative, digits
}

// leaderboardIncrementDigits 对纯数字串做 +1（十进制进位）。
func leaderboardIncrementDigits(digits string) string {
	runes := []byte(digits)
	for index := len(runes) - 1; index >= 0; index-- {
		if runes[index] != '9' {
			runes[index]++
			return string(runes)
		}
		runes[index] = '0'
	}
	return "1" + string(runes)
}

// leaderboardSplitDigits 拆出整数部分与两位小数部分。
func leaderboardSplitDigits(digits string) (string, string) {
	integer := digits[:len(digits)-2]
	fraction := digits[len(digits)-2:]
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	return integer, fraction
}

// leaderboardGroupDigits 按三位分组（locale 决定千分位符号）。
func leaderboardGroupDigits(integer, separator string) string {
	if len(integer) <= 3 {
		return integer
	}
	var builder strings.Builder
	lead := len(integer) % 3
	if lead > 0 {
		builder.WriteString(integer[:lead])
		builder.WriteString(separator)
	}
	for index := lead; index < len(integer); index += 3 {
		builder.WriteString(integer[index : index+3])
		if index+3 < len(integer) {
			builder.WriteString(separator)
		}
	}
	return builder.String()
}

// leaderboardFormatCurrencyPtr 是「可空数值 -> 可空格式化串」的助手（Node 的 `!= null ? format : null`）。
func leaderboardFormatCurrencyPtr(value *float64, currencyCode string) *string {
	if value == nil {
		return nil
	}
	formatted := leaderboardFormatCurrency(*value, currencyCode)
	return &formatted
}
