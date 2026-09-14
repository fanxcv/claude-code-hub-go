package limit

import "strings"

// 本文件复刻限流拦截用到的九条 i18n 文案（messages/<locale>/errors.json 的 RATE_LIMIT_*），
// 逐字取自各语种 JSON，不做模板化改写。
//
// 为什么不做通用 i18n 层：与 internal/guard/messages.go 同样的理由——数据面热路径只用到这九条，
// 而且必须能逐字断言「与 Node 输出一致」。注意模板里的 ${current} 是「美元符号 + 占位符」，
// 渲染后形如 $0.1234；这一点按 next-intl 的 ICU 解析行为复刻。
const (
	// MessageRPMExceeded 对应 errors.RATE_LIMIT_RPM_EXCEEDED。
	MessageRPMExceeded = "RATE_LIMIT_RPM_EXCEEDED"
	// Message5hExceeded 对应 errors.RATE_LIMIT_5H_EXCEEDED（5h 固定窗口）。
	Message5hExceeded = "RATE_LIMIT_5H_EXCEEDED"
	// Message5hRollingExceeded 对应 errors.RATE_LIMIT_5H_ROLLING_EXCEEDED。
	Message5hRollingExceeded = "RATE_LIMIT_5H_ROLLING_EXCEEDED"
	// MessageDailyQuotaExceeded 对应 errors.RATE_LIMIT_DAILY_QUOTA_EXCEEDED（daily 固定窗口）。
	MessageDailyQuotaExceeded = "RATE_LIMIT_DAILY_QUOTA_EXCEEDED"
	// MessageDailyRollingExceeded 对应 errors.RATE_LIMIT_DAILY_ROLLING_EXCEEDED。
	MessageDailyRollingExceeded = "RATE_LIMIT_DAILY_ROLLING_EXCEEDED"
	// MessageWeeklyExceeded 对应 errors.RATE_LIMIT_WEEKLY_EXCEEDED。
	MessageWeeklyExceeded = "RATE_LIMIT_WEEKLY_EXCEEDED"
	// MessageMonthlyExceeded 对应 errors.RATE_LIMIT_MONTHLY_EXCEEDED。
	MessageMonthlyExceeded = "RATE_LIMIT_MONTHLY_EXCEEDED"
	// MessageTotalExceeded 对应 errors.RATE_LIMIT_TOTAL_EXCEEDED。
	MessageTotalExceeded = "RATE_LIMIT_TOTAL_EXCEEDED"
	// MessageConcurrentSessionsExceeded 对应 errors.RATE_LIMIT_CONCURRENT_SESSIONS_EXCEEDED。
	MessageConcurrentSessionsExceeded = "RATE_LIMIT_CONCURRENT_SESSIONS_EXCEEDED"
)

// DefaultLocale 与 internal/guard 的默认语种一致（i18n/config.ts 的 defaultLocale）。
const DefaultLocale = "zh-CN"

// rateLimitMessages 是九条文案的全部语种取值。
var rateLimitMessages = map[string]map[string]string{
	"RATE_LIMIT_RPM_EXCEEDED": {
		"zh-CN": `请求频率超限：当前 {current} 次/分钟（限制：{limit} 次/分钟）。将于 {resetTime} 重置`,
		"zh-TW": `請求頻率超限：當前 {current} 次/分鐘（限制：{limit} 次/分鐘）。將於 {resetTime} 重置`,
		"en":    `Rate limit exceeded: {current} requests per minute (limit: {limit}). Resets at {resetTime}`,
		"ru":    `Превышен лимит запросов: {current} запросов в минуту (лимит: {limit}). Сброс в {resetTime}`,
		"ja":    `リクエストレート制限を超過しました：現在 {current} 回/分（制限：{limit} 回/分）。{resetTime} にリセットされます`,
	},
	"RATE_LIMIT_5H_EXCEEDED": {
		"zh-CN": `5小时消费超限：当前 ${current} USD（限制：${limit} USD）。将于 {resetTime} 重置`,
		"zh-TW": `5小時消費超限：當前 ${current} USD（限制：${limit} USD）。將於 {resetTime} 重置`,
		"en":    `5-hour cost limit exceeded: ${current} USD (limit: ${limit} USD). Resets at {resetTime}`,
		"ru":    `Превышен 5-часовой лимит расходов: ${current} USD (лимит: ${limit} USD). Сброс в {resetTime}`,
		"ja":    `5時間コスト制限を超過しました：${current} USD（制限：${limit} USD）。{resetTime} にリセットされます`,
	},
	"RATE_LIMIT_5H_ROLLING_EXCEEDED": {
		"zh-CN": `5小时滚动窗口消费超限：当前 ${current} USD（限制：${limit} USD）。消费将在过去5小时内逐渐释放`,
		"zh-TW": `5小時滾動視窗消費超限：當前 ${current} USD（限制：${limit} USD）。消費將在過去5小時內逐漸釋放`,
		"en":    `5-hour rolling window cost limit exceeded: ${current} USD (limit: ${limit} USD). Usage gradually expires over the past 5 hours`,
		"ru":    `Превышен лимит скользящего окна 5 часов: ${current} USD (лимит: ${limit} USD). Использование постепенно освобождается за последние 5 часов`,
		"ja":    `5時間ローリングウィンドウのコスト制限を超過しました：${current} USD（制限：${limit} USD）。使用量は過去5時間で徐々に解放されます`,
	},
	"RATE_LIMIT_DAILY_QUOTA_EXCEEDED": {
		"zh-CN": `每日额度超限：当前 ${current} USD（限制：${limit} USD）。将于 {resetTime} 重置`,
		"zh-TW": `每日額度超限：當前 ${current} USD（限制：${limit} USD）。將於 {resetTime} 重置`,
		"en":    `Daily quota exceeded: ${current} USD (limit: ${limit} USD). Resets at {resetTime}`,
		"ru":    `Превышена дневная квота: ${current} USD (лимит: ${limit} USD). Сброс в {resetTime}`,
		"ja":    `日次クォータを超過しました：${current} USD（制限：${limit} USD）。{resetTime} にリセットされます`,
	},
	"RATE_LIMIT_DAILY_ROLLING_EXCEEDED": {
		"zh-CN": `24小时滚动窗口消费超限：当前 ${current} USD（限制：${limit} USD）。消费将在过去24小时内逐渐释放`,
		"zh-TW": `24小時滾動視窗消費超限：當前 ${current} USD（限制：${limit} USD）。消費將在過去24小時內逐漸釋放`,
		"en":    `24-hour rolling window cost limit exceeded: ${current} USD (limit: ${limit} USD). Usage gradually expires over the past 24 hours`,
		"ru":    `Превышен лимит скользящего окна 24 часа: ${current} USD (лимит: ${limit} USD). Использование постепенно освобождается за последние 24 часа`,
		"ja":    `24時間ローリングウィンドウのコスト制限を超過しました：${current} USD（制限：${limit} USD）。使用量は過去24時間で徐々に解放されます`,
	},
	"RATE_LIMIT_WEEKLY_EXCEEDED": {
		"zh-CN": `周消费超限：当前 ${current} USD（限制：${limit} USD）。将于 {resetTime} 重置`,
		"zh-TW": `週消費超限：當前 ${current} USD（限制：${limit} USD）。將於 {resetTime} 重置`,
		"en":    `Weekly cost limit exceeded: ${current} USD (limit: ${limit} USD). Resets at {resetTime}`,
		"ru":    `Превышен недельный лимит расходов: ${current} USD (лимит: ${limit} USD). Сброс в {resetTime}`,
		"ja":    `週次コスト制限を超過しました：${current} USD（制限：${limit} USD）。{resetTime} にリセットされます`,
	},
	"RATE_LIMIT_MONTHLY_EXCEEDED": {
		"zh-CN": `月消费超限：当前 ${current} USD（限制：${limit} USD）。将于 {resetTime} 重置`,
		"zh-TW": `月消費超限：當前 ${current} USD（限制：${limit} USD）。將於 {resetTime} 重置`,
		"en":    `Monthly cost limit exceeded: ${current} USD (limit: ${limit} USD). Resets at {resetTime}`,
		"ru":    `Превышен месячный лимит расходов: ${current} USD (лимит: ${limit} USD). Сброс в {resetTime}`,
		"ja":    `月次コスト制限を超過しました：${current} USD（制限：${limit} USD）。{resetTime} にリセットされます`,
	},
	"RATE_LIMIT_TOTAL_EXCEEDED": {
		"zh-CN": `总消费上限已达到：${current} / ${limit} USD`,
		"zh-TW": `總消費上限已達到：${current} / ${limit} USD`,
		"en":    `Total spending limit exceeded: ${current} USD (limit: ${limit} USD)`,
		"ru":    `Превышен общий лимит расходов: ${current} / ${limit} USD`,
		"ja":    `総支出制限を超過しました：${current} / ${limit} USD`,
	},
	"RATE_LIMIT_CONCURRENT_SESSIONS_EXCEEDED": {
		"zh-CN": `并发 Session 超限：当前 {current} 个（限制：{limit} 个）。请等待活跃 Session 完成`,
		"zh-TW": `並發 Session 超限：當前 {current} 個（限制：{limit} 個）。請等待活躍 Session 完成`,
		"en":    `Concurrent sessions limit exceeded: {current} sessions (limit: {limit}). Please wait for active sessions to complete`,
		"ru":    `Превышен лимит одновременных сессий: {current} сессий (лимит: {limit}). Пожалуйста, дождитесь завершения активных сессий`,
		"ja":    `同時セッション制限を超過しました：現在 {current} セッション（制限：{limit}）。アクティブなセッションが完了するまでお待ちください`,
	},
}

// Message 取一条文案；语种不支持时退回默认语种（对应 next-intl 的 fallback 行为）。
func Message(locale string, code string) string {
	byLocale, ok := rateLimitMessages[code]
	if !ok {
		return ""
	}
	if text, ok := byLocale[locale]; ok {
		return text
	}
	return byLocale[DefaultLocale]
}

// RenderMessage 取文案并替换占位符。
//
// 只替换 ICU 风格的花括号占位符（current/limit/resetTime）。模板里出现的 ${current} 会被替换成
// "$" + 数值，这与 next-intl 的解析结果一致：`$` 是字面量，`{current}` 是参数。
func RenderMessage(locale string, code string, params map[string]string) string {
	text := Message(locale, code)
	if text == "" {
		return text
	}
	replacer := strings.NewReplacer(
		"{current}", params["current"],
		"{limit}", params["limit"],
		"{resetTime}", params["resetTime"],
	)
	return replacer.Replace(text)
}
