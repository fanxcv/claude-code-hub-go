package terminal

import (
	"bytes"
	"encoding/json"
)

// State 是 message_request 一行的结算状态。
type State string

const (
	// StateOpen 表示该行尚无终态：账本行会随每次监视列写入被重建，计费与投影不得视为完成。
	StateOpen State = "open"
	// StateSettled 表示该行已有终态：账本行的 is_success 与 success_rate_outcome 已定。
	StateSettled State = "settled"
)

// FinalizationFacts 是终态判据的四个输入，与库内函数 fn_is_message_request_finalized
// 的签名逐参对应（见 drizzle/0104_watery_thunderbird.sql）。
type FinalizationFacts struct {
	BlockedBy     *string
	StatusCode    *int
	ProviderChain []byte
	ErrorMessage  *string
}

// finalizedChainReasons 复刻函数里的 reason 白名单：链路最后一段是这些原因时，
// 即使没有状态码也判定终态。
var finalizedChainReasons = map[string]struct{}{
	"request_success":            {},
	"retry_success":              {},
	"retry_failed":               {},
	"system_error":               {},
	"resource_not_found":         {},
	"client_error_non_retryable": {},
	"concurrent_limit_failed":    {},
	"hedge_winner":               {},
	"hedge_loser_cancelled":      {},
	"hedge_loser_billed":         {},
	"client_abort":               {},
}

// IsFinalized 复刻 fn_is_message_request_finalized。Go 侧需要在**不读库**的前提下判断
// 一行是否已终态（决定是否还能发终态写、是否该放弃副作用），因此这份镜像是必要的；
// 它与库内函数的一致性由集成测试 TestIsFinalizedMatchesDatabaseFunction 用同一组
// 输入逐例对库内函数比对。
func IsFinalized(facts FinalizationFacts) bool {
	if facts.BlockedBy != nil {
		return true
	}
	if facts.StatusCode != nil {
		return true
	}
	if facts.ErrorMessage != nil && *facts.ErrorMessage != "" {
		return true
	}
	reason, statusCode, errorMessage, ok := lastChainEntry(facts.ProviderChain)
	if !ok {
		return false
	}
	if _, whitelisted := finalizedChainReasons[reason]; whitelisted {
		return true
	}
	if statusCode {
		return true
	}
	return errorMessage != ""
}

// lastChainEntry 取出 provider_chain 的最后一段，返回其中的 reason、是否带数值型
// statusCode、errorMessage。非数组、空数组或末段不是对象时 ok=false，
// 与 SQL 里 jsonb_typeof/jsonb_array_length 的守卫等价。
func lastChainEntry(chain []byte) (reason string, hasStatusCode bool, errorMessage string, ok bool) {
	if len(bytes.TrimSpace(chain)) == 0 {
		return "", false, "", false
	}
	decoder := json.NewDecoder(bytes.NewReader(chain))
	decoder.UseNumber()
	var entries []any
	if err := decoder.Decode(&entries); err != nil || len(entries) == 0 {
		return "", false, "", false
	}
	last, isObject := entries[len(entries)-1].(map[string]any)
	if !isObject {
		return "", false, "", false
	}
	if raw, exists := last["reason"]; exists {
		if value, isString := raw.(string); isString {
			reason = value
		}
	}
	if raw, exists := last["statusCode"]; exists {
		if _, isNumber := raw.(json.Number); isNumber {
			hasStatusCode = true
		}
	}
	if raw, exists := last["errorMessage"]; exists {
		if value, isString := raw.(string); isString {
			errorMessage = value
		}
	}
	return reason, hasStatusCode, errorMessage, true
}

// Classify 把一行的事实映射成状态。
func Classify(facts FinalizationFacts) State {
	if IsFinalized(facts) {
		return StateSettled
	}
	return StateOpen
}
