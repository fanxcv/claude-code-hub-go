package specialsettings

// 整流器审计条目（`message_request.special_settings`）。
//
// 形状真源：`src/types/special-settings.ts` 的六个整流器条目 + Node 的 buildAuditSetting
// （`forwarder.ts:1267-1372`）与两个主动型的 addSpecialSetting（`forwarder.ts:3474-3491`、
// `response-input-rectifier.ts:87-96`）。
//
// 两种形状**不可混同**：
//   - 被动型（thinking 三件套 + gemini function id）：带 providerId / providerName / trigger /
//     attemptNumber / retryAttemptNumber，且**命中即写**（hit 取整流是否真的应用）；
//   - 主动型（billing header / responses input）：只有 type/scope/hit + 各自字段，**无 provider、
//     无 trigger、无 attempt**，且仅在真的改写了正文时才写（故 hit 恒为 true）。
const (
	// TypeThinkingEffortConflictRectifier 是 thinking effort 冲突整流器。
	TypeThinkingEffortConflictRectifier = "thinking_effort_conflict_rectifier"
	// TypeThinkingSignatureRectifier 是 thinking signature 整流器。
	TypeThinkingSignatureRectifier = "thinking_signature_rectifier"
	// TypeThinkingBudgetRectifier 是 thinking 预算整流器。
	TypeThinkingBudgetRectifier = "thinking_budget_rectifier"
	// TypeGeminiFunctionIDRectifier 是 gemini function id 整流器。
	TypeGeminiFunctionIDRectifier = "gemini_function_id_rectifier"
	// TypeBillingHeaderRectifier 是 billing header 主动剥离器。
	TypeBillingHeaderRectifier = "billing_header_rectifier"
	// TypeResponseInputRectifier 是 responses input 主动归一条目。
	TypeResponseInputRectifier = "response_input_rectifier"
	// TypeThinkingPlaceholderSignatureRectifier 是占位思考签名的主动剥离条目。
	//
	// 与被动型的 thinking_signature_rectifier 分开命名：那个是上游报错后删 thinking 块，
	// 这个是发送前剥掉我方自己造给客户端的占位签名，两条事实不能混为一条。
	TypeThinkingPlaceholderSignatureRectifier = "thinking_placeholder_signature_rectifier"
)

// ReactiveRectifierEntry 组一条被动整流器的审计条目。
//
// hit 取整流是否真的应用（Node 的 `hit: rectified.applied`）：命中触发词但整流不适用时
// Node 同样写条目（hit=false），故调用方在 Applied 为假时也要写。
func ReactiveRectifierEntry(
	typeName string,
	trigger string,
	providerID int64,
	providerName string,
	attemptNumber int,
	retryAttemptNumber int,
	hit bool,
	fields map[string]any,
) map[string]any {
	entry := map[string]any{
		"type":               typeName,
		"scope":              "request",
		"hit":                hit,
		"providerId":         providerID,
		"providerName":       providerName,
		"trigger":            trigger,
		"attemptNumber":      attemptNumber,
		"retryAttemptNumber": retryAttemptNumber,
	}
	for key, value := range fields {
		entry[key] = value
	}
	return entry
}

// ProactiveRectifierEntry 组一条主动整流器的审计条目（无 provider / trigger / attempt）。
func ProactiveRectifierEntry(typeName string, fields map[string]any) map[string]any {
	entry := map[string]any{
		"type":  typeName,
		"scope": "request",
		"hit":   true,
	}
	for key, value := range fields {
		entry[key] = value
	}
	return entry
}
