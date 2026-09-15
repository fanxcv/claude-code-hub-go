package forward

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// 本文件实现 Node 的**供应商级参数覆写**与**缓存 TTL 覆写**，它们此前只有列与写侧、
// 数据面零消费（管理面配了不生效）。
//
// 真源：
//   - codex 六件：`src/lib/codex/provider-overrides.ts`（`applyCodexProviderOverridesWithAudit`）；
//   - anthropic 三件：`src/lib/anthropic/provider-overrides.ts`；
//   - gemini googleSearch：`src/lib/gemini/provider-overrides.ts`；
//   - 缓存 TTL：`forwarder.ts:774`（解析）、`3231-3535`（正文 cache_control）、`8829`（beta 头）。
//
// 三条共同约定（Node 各处注释同文）：
//  1. 偏好值 null/undefined/"inherit" **一律表示「遵循客户端」**，不写审计条目；
//  2. 覆写只碰各自列出的字段，其余字段（含同层兄弟字段）原样保留；
//  3. 门控是**供应商类型**（claude/claude-auth、codex、gemini/gemini-cli），不是客户端方言。
//
// 顺序：codex/anthropic/gemini 三族互斥（供应商类型决定），缓存 TTL 在它们之后施加
// （Node 的 cache_control 改写紧跟 anthropic 覆写之后，见 forwarder.ts:3528）。

// inheritPreference 是「遵循客户端」的字面量：它与空串、JSON null 三种形态同义。
const inheritPreference = "inherit"

// 缓存 TTL 的两个取值（`src/lib/cache.ts` 的 CacheTtlOption）。
const (
	cacheTTLShort = "5m"
	cacheTTLLong  = "1h"
)

// ProviderOverrideApplier 实现 OverrideApplier：按供应商类型施加参数覆写。
//
// 它是**每请求一份**的：gemini 的覆写要看客户端路径（生成类端点才生效），而审计条目与
// 缓存 TTL 都是本次请求的产物，故状态跟着请求走，不做全局单例。
type ProviderOverrideApplier struct {
	// clientPath 是客户端请求路径（Node 传的是 `session.requestUrl.pathname`）。
	clientPath string

	mu     sync.Mutex
	audits []map[string]any
}

// NewProviderOverrideApplier 构造一个请求作用域的覆写器。
func NewProviderOverrideApplier(clientPath string) *ProviderOverrideApplier {
	return &ProviderOverrideApplier{clientPath: clientPath}
}

// Apply 按供应商类型改写上游正文；无事可做时**原样返回入参字节**。
//
// 为什么强调「原样返回」：出站正文的字节稳定性直接影响上游前缀缓存命中率（正文变一个字节，
// 缓存段即失效），故只在真的改写了字段时才重新序列化。
//
// protocol 是本次出站正文所属的协议线：gemini 一族只在 gemini 线上施加（Node 的 gemini 覆写
// 只存在于 gemini 原生透传分支里）；codex / anthropic 两族不按它门控——Node 的门控就只是
// 供应商类型，未开转换时正文仍是客户端方言，Node 同样照改（字段名对不上时自会无操作）。
func (a *ProviderOverrideApplier) Apply(
	provider Provider,
	protocol convert.WireProtocol,
	body []byte,
) ([]byte, error) {
	if a == nil || len(body) == 0 {
		return body, nil
	}
	if !hasConfiguredPreference(provider, protocol) && !mayCarryPriorityServiceTier(provider, body) {
		// 零配置早退：不解析、不拷贝。生产上绝大多数供应商没有任何偏好，而本函数在每次尝试
		// 都会跑（重试/竞速各一次）——为了「可能什么都不做」把整份正文建树并深拷贝，
		// 是拿热路径的内存换没发生的改写。
		a.setAudits(nil)
		return body, nil
	}
	tree, err := convert.ParseJSON(body)
	if err != nil || !tree.IsObject() {
		// 非对象正文：三线正文都是对象，出现别的形状说明调用方给错了东西，不改写。
		return body, nil
	}

	out := tree.Clone()
	changed := false
	var entry map[string]any

	switch {
	case provider.Type == convert.ProviderClaude, provider.Type == convert.ProviderClaudeAuth:
		changed, entry = a.applyAnthropic(provider, out)
	case provider.Type == convert.ProviderCodex:
		changed, entry = a.applyCodex(provider, out)
	case IsGeminiProviderType(provider.Type) && protocol == convert.ProtocolGemini:
		changed, entry = a.applyGemini(provider, out)
	}

	// 缓存 TTL 最后施加（Node 把 cache_control 改写放在 anthropic 覆写之后）。
	if a.applyCacheTTL(provider, out) {
		changed = true
	}

	a.setAudits(entry)
	if !changed {
		return body, nil
	}
	return []byte(out.MarshalCompact()), nil
}

// hasConfiguredPreference 报告该供应商在该协议线上有没有任何**已配置**的偏好。
//
// 只做「是否配置」的判定，不做取值合法性——后者由各处 normalize* 处理。
func hasConfiguredPreference(provider Provider, protocol convert.WireProtocol) bool {
	configured := func(value string) bool { return value != "" && value != inheritPreference }

	switch {
	case provider.Type == convert.ProviderClaude, provider.Type == convert.ProviderClaudeAuth:
		return configured(provider.AnthropicMaxTokensPreference) ||
			configured(provider.AnthropicThinkingBudgetPreference) ||
			len(strings.TrimSpace(string(provider.AnthropicAdaptiveThinking))) > 0 ||
			configured(provider.CacheTTLPreference)
	case provider.Type == convert.ProviderCodex:
		return configured(provider.CodexParallelToolCallsPreference) ||
			configured(provider.CodexImageGenerationPreference) ||
			configured(provider.CodexReasoningEffortPreference) ||
			configured(provider.CodexReasoningSummaryPreference) ||
			configured(provider.CodexTextVerbosityPreference) ||
			configured(provider.CodexServiceTierPreference)
	case IsGeminiProviderType(provider.Type) && protocol == convert.ProtocolGemini:
		return configured(provider.GeminiGoogleSearchPreference)
	default:
		return false
	}
}

// mayCarryPriorityServiceTier 是零配置早退的**例外**：codex 线上「请求自带 service_tier=priority」
// 即使一个偏好都没配也要写审计条目（Node 的 `beforeServiceTier === "priority"` 分支），
// 它是 priority 计费口径的取证点。
//
// 为何用字节扫描而不是解析：早退的全部意义就是避开解析。这里只做过近似——正文里没有
// `"priority"` 字面量就一定不会命中（不会漏），有则白白解析一次（不会错）。
func mayCarryPriorityServiceTier(provider Provider, body []byte) bool {
	return provider.Type == convert.ProviderCodex && bytes.Contains(body, []byte(`"priority"`))
}

// OverrideAuditEntries 返回本次 Apply 产生的审计条目（实现 OverrideAuditSource）。
func (a *ProviderOverrideApplier) OverrideAuditEntries() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.audits) == 0 {
		return nil
	}
	out := make([]map[string]any, len(a.audits))
	copy(out, a.audits)
	return out
}

// ResolvedCacheTTL 返回本次请求解析出的缓存 TTL（实现 CacheTTLResolver）。
//
// Node 的解析是**密钥偏好 ?? 供应商偏好**（forwarder.ts:774）。密钥列也在库里
// （`keys.cache_ttl_preference`），但数据面的鉴权态（`pctx.AuthState`）不带它，故本实现
// 只认供应商偏好；两者同时配好时与 Node 有差（详见报告「未尽事项」）。生产两侧当前都是
// `inherit`，故当前无实际差异。
func (a *ProviderOverrideApplier) ResolvedCacheTTL(provider Provider) string {
	if provider.Type != convert.ProviderClaude && provider.Type != convert.ProviderClaudeAuth {
		return ""
	}
	return normalizeCacheTTL(provider.CacheTTLPreference)
}

func (a *ProviderOverrideApplier) setAudits(entry map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if entry == nil {
		a.audits = nil
		return
	}
	a.audits = []map[string]any{entry}
}

// ---------------------------------------------------------------------------
// anthropic 三件（max_tokens / thinking.budget_tokens / adaptive thinking）
// ---------------------------------------------------------------------------

// anthropicAuditSnapshot 取 anthropic 覆写审计要看的四个字段现值。
func anthropicAuditSnapshot(request *convert.Value) map[string]any {
	maxTokens, _ := request.Get("max_tokens")
	return map[string]any{
		"max_tokens":             auditScalar(maxTokens),
		"thinking.type":          nestedAuditScalar(request, "thinking", "type"),
		"thinking.budget_tokens": nestedAuditScalar(request, "thinking", "budget_tokens"),
		"output_config.effort":   nestedAuditScalar(request, "output_config", "effort"),
	}
}

func (a *ProviderOverrideApplier) applyAnthropic(
	provider Provider,
	out *convert.Value,
) (bool, map[string]any) {
	maxTokens, hasMaxTokens := normalizeNumericPreference(provider.AnthropicMaxTokensPreference)
	budgetTokens, hasBudget := normalizeNumericPreference(provider.AnthropicThinkingBudgetPreference)
	adaptive, hasAdaptive := parseAdaptiveThinking(provider.AnthropicAdaptiveThinking)
	if !hasMaxTokens && !hasBudget && !hasAdaptive {
		return false, nil
	}

	before := anthropicAuditSnapshot(out)
	changed := false

	if hasMaxTokens {
		out.Set("max_tokens", convert.NewNumberInt(maxTokens))
		changed = true
	}

	// 第一步：自适应思考**优先于**预算覆写，且命中即返回（Node 的 Step 1/Step 2 结构）：
	// 同一家供应商同时配了 adaptive 与 budget 时，只有模型匹配才用 adaptive，否则退回预算。
	if hasAdaptive {
		model, hasModel := out.StringField("model")
		if adaptiveMatchesModel(adaptive, model, hasModel) {
			out.Set("thinking", convert.NewObject().Set("type", convert.NewString("adaptive")))
			outputConfig := objectFieldOrEmpty(out, "output_config")
			outputConfig.Set("effort", convert.NewString(adaptive.Effort))
			out.Set("output_config", outputConfig)
			after := anthropicAuditSnapshot(out)
			return true, anthropicOverrideEntry(provider, before, after)
		}
	}

	if hasBudget {
		if applyThinkingBudget(out, budgetTokens) {
			changed = true
		}
	}

	after := anthropicAuditSnapshot(out)
	// 命中判定与是否有实际改动无关：Node 在「配了偏好」时就写条目（哪怕值恰好相同或
	// 预算被夹到 1024 以下而整段跳过），前端靠 changed=false 区分。
	return changed, anthropicOverrideEntry(provider, before, after)
}

// applyThinkingBudget 写 thinking.type=enabled 与 budget_tokens，并按 Node 的两条护栏夹取。
//
// 护栏（Node 注释原话「Anthropic API requires budget_tokens >= 1024」）：
//   - budget_tokens >= max_tokens 时夹到 max_tokens-1；
//   - 夹完仍 < 1024 则**整段放弃**（宁可不覆写，也不发一个必定被 API 拒的请求）。
func applyThinkingBudget(out *convert.Value, budgetTokens int64) bool {
	thinking := objectFieldOrEmpty(out, "thinking")
	next := budgetTokens
	if maxTokens, ok := numericField(out, "max_tokens"); ok && float64(next) >= maxTokens {
		next = int64(maxTokens) - 1
	}
	if next < 1024 {
		return false
	}
	thinking.Set("type", convert.NewString("enabled"))
	thinking.Set("budget_tokens", convert.NewNumberInt(next))
	out.Set("thinking", thinking)
	return true
}

func anthropicOverrideEntry(provider Provider, before, after map[string]any) map[string]any {
	paths := []string{"max_tokens", "thinking.type", "thinking.budget_tokens", "output_config.effort"}
	changes := make([]specialsettings.OverrideChange, 0, len(paths))
	for _, path := range paths {
		changes = append(changes, specialsettings.OverrideChange{
			Path:    path,
			Before:  before[path],
			After:   after[path],
			Changed: !auditEqual(before[path], after[path]),
		})
	}
	return specialsettings.ProviderParameterOverrideEntry(
		provider.ID, provider.Name, string(provider.Type), changes,
	)
}

// adaptiveThinkingConfig 复刻 Node 的 AnthropicAdaptiveThinkingConfig（types/provider.ts:48-56）。
type adaptiveThinkingConfig struct {
	Effort         string
	ModelMatchMode string
	Models         []string
}

// parseAdaptiveThinking 解析 jsonb 列的原文。
//
// 列在写侧历史上是 jsonb 直通（无 schema 校验），故解析失败一律**视同未配置**——宁可漏一次
// 覆写，也不该让一行脏数据把请求打崩。
func parseAdaptiveThinking(raw json.RawMessage) (adaptiveThinkingConfig, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return adaptiveThinkingConfig{}, false
	}
	var decoded adaptiveThinkingConfig
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return adaptiveThinkingConfig{}, false
	}
	return decoded, true
}

// adaptiveMatchesModel 复刻 Node 的匹配判定：mode=all 恒命中；否则要求模型名等于某个模式，
// 或以 `模式-` 开头（Node：`modelId === m || modelId.startsWith(`${m}-`)`）。
func adaptiveMatchesModel(config adaptiveThinkingConfig, model string, hasModel bool) bool {
	if config.ModelMatchMode == "all" {
		return true
	}
	if !hasModel {
		return false
	}
	for _, candidate := range config.Models {
		if model == candidate || strings.HasPrefix(model, candidate+"-") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// codex 六件
// ---------------------------------------------------------------------------

// codexAuditSnapshot 取 codex 覆写审计要看的八项现值（Node 的 before*/after* 集合）。
func codexAuditSnapshot(request *convert.Value) map[string]any {
	return map[string]any{
		"parallel_tool_calls":                     fieldAuditScalar(request, "parallel_tool_calls"),
		"tools.image_generation":                  hasImageGenerationTool(memberValue(request, "tools")),
		"input.additional_tools.image_generation": hasInputImageGenerationTool(memberValue(request, "input")),
		"reasoning.effort":                        nestedAuditScalar(request, "reasoning", "effort"),
		"reasoning.summary":                       nestedAuditScalar(request, "reasoning", "summary"),
		"service_tier":                            fieldAuditScalar(request, "service_tier"),
		"text.verbosity":                          nestedAuditScalar(request, "text", "verbosity"),
		"tool_choice":                             summarizeImageGenerationToolChoice(fieldValue(request, "tool_choice")),
	}
}

func (a *ProviderOverrideApplier) applyCodex(
	provider Provider,
	out *convert.Value,
) (bool, map[string]any) {
	parallelToolCalls, hasParallelToolCalls := normalizeBoolPreference(provider.CodexParallelToolCallsPreference)
	imageGeneration, hasImageGeneration := normalizeBoolPreference(provider.CodexImageGenerationPreference)
	reasoningEffort, hasReasoningEffort := normalizeStringPreference(provider.CodexReasoningEffortPreference)
	reasoningSummary, hasReasoningSummary := normalizeStringPreference(provider.CodexReasoningSummaryPreference)
	textVerbosity, hasTextVerbosity := normalizeStringPreference(provider.CodexTextVerbosityPreference)
	serviceTier, hasServiceTier := normalizeStringPreference(provider.CodexServiceTierPreference)

	// hit 的一个特例：请求本身带 service_tier=priority 时**即使一个偏好都没配**也写条目
	// （Node 的 `beforeServiceTier === "priority"`）——那是 priority 计费口径的取证点。
	serviceTierNow, _ := out.StringField("service_tier")
	if !hasParallelToolCalls && !hasImageGeneration && !hasReasoningEffort && !hasReasoningSummary &&
		!hasTextVerbosity && !hasServiceTier && serviceTierNow != "priority" {
		return false, nil
	}

	before := codexAuditSnapshot(out)
	changed := false

	if hasParallelToolCalls {
		out.Set("parallel_tool_calls", convert.NewBool(parallelToolCalls))
		changed = true
	}

	if hasImageGeneration {
		if applyImageGenerationTools(out, imageGeneration) {
			changed = true
		}
		if applyInputImageGenerationTools(out, imageGeneration) {
			changed = true
		}
		// tool_choice 的改写要看**改写后**的现状：上下文取自 out（Node 传的也是 output）。
		if applyImageGenerationToolChoice(
			out,
			imageGeneration,
			findImageGenerationToolReference(out),
			hasAvailableTool(out),
		) {
			changed = true
		}
	}

	if hasReasoningEffort || hasReasoningSummary {
		reasoning := objectFieldOrEmpty(out, "reasoning")
		if hasReasoningEffort {
			reasoning.Set("effort", convert.NewString(reasoningEffort))
		}
		if hasReasoningSummary {
			reasoning.Set("summary", convert.NewString(reasoningSummary))
		}
		out.Set("reasoning", reasoning)
		changed = true
	}

	if hasTextVerbosity {
		text := objectFieldOrEmpty(out, "text")
		text.Set("verbosity", convert.NewString(textVerbosity))
		out.Set("text", text)
		changed = true
	}

	if hasServiceTier {
		out.Set("service_tier", convert.NewString(serviceTier))
		changed = true
	}

	after := codexAuditSnapshot(out)
	paths := []string{
		"parallel_tool_calls",
		"tools.image_generation",
		"input.additional_tools.image_generation",
		"reasoning.effort",
		"reasoning.summary",
		"service_tier",
		"text.verbosity",
		"tool_choice",
	}
	changes := make([]specialsettings.OverrideChange, 0, len(paths))
	for _, path := range paths {
		changes = append(changes, specialsettings.OverrideChange{
			Path:    path,
			Before:  before[path],
			After:   after[path],
			Changed: !auditEqual(before[path], after[path]),
		})
	}
	return changed, specialsettings.ProviderParameterOverrideEntry(
		provider.ID, provider.Name, string(provider.Type), changes,
	)
}

// imageGenerationToolReference 复刻 Node 的 toImageGenerationToolReference：
// 只认三种形态（`type=image_generation` 与 namespace 名为 image_gen 的两种写法），
// 其余（含 `image_gen` 之外的工具）一律不是。
func imageGenerationToolReference(tool *convert.Value) *convert.Value {
	if tool == nil || !tool.IsObject() {
		return nil
	}
	typeName, _ := tool.StringField("type")
	switch typeName {
	case "image_generation":
		return convert.NewObject().Set("type", convert.NewString("image_generation"))
	case "namespace":
		if name, _ := tool.StringField("name"); name == "image_gen" {
			return convert.NewObject().
				Set("type", convert.NewString("namespace")).
				Set("name", convert.NewString("image_gen"))
		}
		if namespace, _ := tool.StringField("namespace"); namespace == "image_gen" {
			return convert.NewObject().
				Set("type", convert.NewString("namespace")).
				Set("namespace", convert.NewString("image_gen"))
		}
	}
	return nil
}

func isImageGenerationTool(tool *convert.Value) bool {
	return imageGenerationToolReference(tool) != nil
}

func hasImageGenerationTool(tools *convert.Value) bool {
	if tools == nil || !tools.IsArray() {
		return false
	}
	for _, tool := range tools.Items() {
		if isImageGenerationTool(tool) {
			return true
		}
	}
	return false
}

// hasInputImageGenerationTool 复刻 Node：只看 input 里 `type=additional_tools` 条目的 tools。
func hasInputImageGenerationTool(input *convert.Value) bool {
	if input == nil || !input.IsArray() {
		return false
	}
	for _, item := range input.Items() {
		if item == nil || !item.IsObject() {
			continue
		}
		if typeName, _ := item.StringField("type"); typeName != "additional_tools" {
			continue
		}
		if hasImageGenerationTool(memberValue(item, "tools")) {
			return true
		}
	}
	return false
}

// findImageGenerationToolReference 找正文里第一处图像工具引用（tool_choice 补进白名单时用它
// 保持原写法，如 namespace 形态）。
func findImageGenerationToolReference(request *convert.Value) *convert.Value {
	if tools := memberValue(request, "tools"); tools != nil && tools.IsArray() {
		for _, tool := range tools.Items() {
			if reference := imageGenerationToolReference(tool); reference != nil {
				return reference
			}
		}
	}
	input := memberValue(request, "input")
	if input == nil || !input.IsArray() {
		return nil
	}
	for _, item := range input.Items() {
		if item == nil || !item.IsObject() {
			continue
		}
		if typeName, _ := item.StringField("type"); typeName != "additional_tools" {
			continue
		}
		tools := memberValue(item, "tools")
		if tools == nil || !tools.IsArray() {
			continue
		}
		for _, tool := range tools.Items() {
			if reference := imageGenerationToolReference(tool); reference != nil {
				return reference
			}
		}
	}
	return nil
}

// hasAvailableTool 复刻 Node：顶层 tools 非空，或 input 里存在非空的 additional_tools 条目。
func hasAvailableTool(request *convert.Value) bool {
	if tools := memberValue(request, "tools"); tools != nil && tools.IsArray() && tools.Len() > 0 {
		return true
	}
	input := memberValue(request, "input")
	if input == nil || !input.IsArray() {
		return false
	}
	for _, item := range input.Items() {
		if item == nil || !item.IsObject() {
			continue
		}
		if typeName, _ := item.StringField("type"); typeName != "additional_tools" {
			continue
		}
		tools := memberValue(item, "tools")
		if tools != nil && tools.IsArray() && tools.Len() > 0 {
			return true
		}
	}
	return false
}

// applyImageGenerationTools 按偏好注入/移除顶层 tools 里的图像工具。
func applyImageGenerationTools(out *convert.Value, enabled bool) bool {
	tools := memberValue(out, "tools")
	isArray := tools != nil && tools.IsArray()

	if enabled {
		// 已有（顶层或 input 里）即不重复注入：Node 的两个「已存在」判定是并列的。
		if isArray && hasImageGenerationTool(tools) {
			return false
		}
		if hasInputImageGenerationTool(memberValue(out, "input")) {
			return false
		}
		next := convert.NewArray()
		if isArray {
			for _, tool := range tools.Items() {
				next.Append(tool)
			}
		}
		next.Append(convert.NewObject().Set("type", convert.NewString("image_generation")))
		out.Set("tools", next)
		return true
	}

	if !isArray {
		return false
	}
	next := convert.NewArray()
	removed := false
	for _, tool := range tools.Items() {
		if isImageGenerationTool(tool) {
			removed = true
			continue
		}
		next.Append(tool)
	}
	if !removed {
		return false
	}
	if next.Len() > 0 {
		out.Set("tools", next)
	} else {
		// 滤空即整键删除（Node：`delete target.tools`）。
		out.Delete("tools")
	}
	return true
}

// applyInputImageGenerationTools 只在**关闭**图像生成时清理 input 里的 additional_tools 条目。
//
// 被滤空的那个条目整条丢弃（Node 不把它推入新数组），而不是留下一个空的 tools 数组。
func applyInputImageGenerationTools(out *convert.Value, enabled bool) bool {
	if enabled {
		return false
	}
	input := memberValue(out, "input")
	if input == nil || !input.IsArray() {
		return false
	}
	next := convert.NewArray()
	changed := false
	for _, item := range input.Items() {
		if item == nil || !item.IsObject() {
			next.Append(item)
			continue
		}
		if typeName, _ := item.StringField("type"); typeName != "additional_tools" {
			next.Append(item)
			continue
		}
		tools := memberValue(item, "tools")
		if tools == nil || !tools.IsArray() {
			next.Append(item)
			continue
		}
		filtered := convert.NewArray()
		removed := false
		for _, tool := range tools.Items() {
			if isImageGenerationTool(tool) {
				removed = true
				continue
			}
			filtered.Append(tool)
		}
		if !removed {
			next.Append(item)
			continue
		}
		changed = true
		if filtered.Len() > 0 {
			copied := item.Clone()
			copied.Set("tools", filtered)
			next.Append(copied)
		}
	}
	if !changed {
		return false
	}
	out.Set("input", next)
	return true
}

// isImageGenerationToolChoice 复刻 Node 的同名判定（字符串形态、工具对象形态、`{tool: …}` 形态）。
func isImageGenerationToolChoice(value *convert.Value) bool {
	if value == nil {
		return false
	}
	if text, ok := value.String(); ok {
		return text == "image_generation"
	}
	if !value.IsObject() {
		return false
	}
	if isImageGenerationTool(value) {
		return true
	}
	inner, _ := value.Get("tool")
	return isImageGenerationTool(inner)
}

// summarizeImageGenerationToolChoice 产出审计里 tool_choice 的**摘要串**（Node 同名函数）。
//
// 记摘要而不是原文：tool_choice 可能是任意深度的 allowed_tools 结构，审计条目要能一眼看出
// 「白名单里有没有图像工具」，故 Node 把它压成一组固定字符串。
func summarizeImageGenerationToolChoice(value *convert.Value) any {
	if value == nil {
		return nil
	}
	if text, ok := value.String(); ok {
		return text
	}
	if !value.IsObject() {
		return nil
	}
	if inner, ok := value.Get("tool"); ok && isImageGenerationTool(inner) {
		return "tool:image_generation"
	}
	typeName, hasType := value.StringField("type")
	if !hasType {
		return nil
	}
	switch typeName {
	case "image_generation":
		return "image_generation"
	case "namespace":
		name, _ := value.StringField("name")
		namespace, _ := value.StringField("namespace")
		if name == "image_gen" || namespace == "image_gen" {
			return "namespace:image_gen"
		}
	case "allowed_tools":
		tools := memberValue(value, "tools")
		total := 0
		imageTools := 0
		if tools != nil && tools.IsArray() {
			for _, tool := range tools.Items() {
				total++
				if isImageGenerationTool(tool) {
					imageTools++
				}
			}
		}
		switch {
		case imageTools == 0:
			return "allowed_tools"
		case imageTools == total:
			return "allowed_tools:image_generation_only"
		default:
			return "allowed_tools:includes_image_generation"
		}
	}
	return typeName
}

// applyImageGenerationToolChoice 按偏好调整 tool_choice 对图像工具的可达性。
//
// 两个方向都按 Node 的硬约束走：
//   - 开启：白名单形态才补进图像工具（auto/none 等形态不动，避免放宽客户端的限制）；
//   - 关闭：白名单被清空或工具限制只剩图像工具时改成 `none`——**不得回退成 auto**，
//     否则剩余工具会重新暴露给模型（Node 注释原话）。
func applyImageGenerationToolChoice(
	out *convert.Value,
	enabled bool,
	toolReference *convert.Value,
	hasAvailableTools bool,
) bool {
	toolChoice, hasToolChoice := out.Get("tool_choice")

	if enabled {
		if !hasToolChoice || toolChoice == nil || !toolChoice.IsObject() {
			return false
		}
		if typeName, _ := toolChoice.StringField("type"); typeName != "allowed_tools" {
			return false
		}
		tools := memberValue(toolChoice, "tools")
		if tools == nil || !tools.IsArray() {
			return false
		}
		if hasImageGenerationTool(tools) {
			return false
		}
		next := convert.NewArray()
		for _, tool := range tools.Items() {
			next.Append(tool)
		}
		if toolReference != nil {
			next.Append(toolReference)
		} else {
			next.Append(convert.NewObject().Set("type", convert.NewString("image_generation")))
		}
		updated := toolChoice.Clone()
		updated.Set("tools", next)
		out.Set("tool_choice", updated)
		return true
	}

	if !hasAvailableTools {
		if !hasToolChoice {
			return false
		}
		out.Delete("tool_choice")
		return true
	}

	if isImageGenerationToolChoice(toolChoice) {
		out.Set("tool_choice", convert.NewString("none"))
		return true
	}

	if !hasToolChoice || toolChoice == nil || !toolChoice.IsObject() {
		return false
	}
	if typeName, _ := toolChoice.StringField("type"); typeName != "allowed_tools" {
		return false
	}
	tools := memberValue(toolChoice, "tools")
	if tools == nil || !tools.IsArray() {
		return false
	}
	filtered := convert.NewArray()
	for _, tool := range tools.Items() {
		if isImageGenerationTool(tool) {
			continue
		}
		filtered.Append(tool)
	}
	if filtered.Len() == tools.Len() {
		return false
	}
	if filtered.Len() > 0 {
		updated := toolChoice.Clone()
		updated.Set("tools", filtered)
		out.Set("tool_choice", updated)
		return true
	}
	out.Set("tool_choice", convert.NewString("none"))
	return true
}

// ---------------------------------------------------------------------------
// gemini googleSearch
// ---------------------------------------------------------------------------

func (a *ProviderOverrideApplier) applyGemini(
	provider Provider,
	out *convert.Value,
) (bool, map[string]any) {
	// Node：`if (endpointPathname && !isGeminiGenerationEndpointPath(...)) return request`。
	// 路径为空时不判（Node 传的是 requestUrl.pathname，正常情况下恒非空）。
	if a.clientPath != "" && !isGeminiGenerationEndpointPath(a.clientPath) {
		return false, nil
	}

	preference := provider.GeminiGoogleSearchPreference
	if preference == "" || preference == inheritPreference {
		return false, nil
	}
	if preference != "enabled" && preference != "disabled" {
		// 未知取值 Node 只记一条 warn 后原样返回（无审计条目）；写侧本轮已加枚举校验，
		// 这里是脏数据的兜底。
		return false, nil
	}

	tools := memberValue(out, "tools")
	isArray := tools != nil && tools.IsArray()
	hadGoogleSearch := false
	if isArray {
		for _, tool := range tools.Items() {
			if tool != nil && tool.IsObject() && tool.Has("googleSearch") {
				hadGoogleSearch = true
				break
			}
		}
	}

	action := "passthrough"
	if preference == "enabled" && !hadGoogleSearch {
		action = "inject"
	} else if preference == "disabled" && hadGoogleSearch {
		action = "remove"
	}

	changed := false
	switch action {
	case "inject":
		next := convert.NewArray()
		if isArray {
			for _, tool := range tools.Items() {
				next.Append(tool)
			}
		}
		next.Append(convert.NewObject().Set("googleSearch", convert.NewObject()))
		out.Set("tools", next)
		changed = true
	case "remove":
		next := convert.NewArray()
		for _, tool := range tools.Items() {
			if tool != nil && tool.IsObject() && tool.Has("googleSearch") {
				continue
			}
			next.Append(tool)
		}
		if next.Len() > 0 {
			out.Set("tools", next)
		} else {
			out.Delete("tools")
		}
		changed = true
	}

	entry := specialsettings.GeminiGoogleSearchOverrideEntry(
		provider.ID, provider.Name, action, preference, hadGoogleSearch,
	)
	return changed, entry
}

// isGeminiGenerationEndpointPath 复刻 Node 的 endpoint-family-catalog.ts:466：
// 路径归一（去查询串、去尾斜杠、小写）后取最后一个 `:动作` 段，只认生成类动作。
func isGeminiGenerationEndpointPath(pathname string) bool {
	normalized := normalizeEndpointPath(pathname)
	index := strings.LastIndex(normalized, ":")
	if index < 0 {
		return false
	}
	action := normalized[index+1:]
	if strings.Contains(action, "/") {
		return false
	}
	return action == "generatecontent" || action == "streamgeneratecontent"
}

// normalizeEndpointPath 复刻 Node 的 endpoint-paths.ts:34。
func normalizeEndpointPath(pathname string) string {
	if index := strings.Index(pathname, "?"); index >= 0 {
		pathname = pathname[:index]
	}
	if len(pathname) > 1 && strings.HasSuffix(pathname, "/") {
		pathname = pathname[:len(pathname)-1]
	}
	return strings.ToLower(pathname)
}

// ---------------------------------------------------------------------------
// 缓存 TTL
// ---------------------------------------------------------------------------

// applyCacheTTL 把解析出的 TTL 写进 anthropic 消息块（含顶层 system）。
func (a *ProviderOverrideApplier) applyCacheTTL(provider Provider, out *convert.Value) bool {
	ttl := a.ResolvedCacheTTL(provider)
	if ttl == "" {
		return false
	}
	return applyCacheTTLToMessage(out, ttl)
}

// applyCacheTTLToMessage 复刻 forwarder.ts:805 的 applyCacheTtlOverrideToMessage。
//
// 只改**已有** `cache_control.type === "ephemeral"` 的块（即客户端已声明要缓存的那些），
// 不主动新增缓存标记——否则等于把所有请求都变成缓存写。
func applyCacheTTLToMessage(message *convert.Value, ttl string) bool {
	applied := false

	if system := memberValue(message, "system"); system != nil && system.IsArray() {
		if next, hit := applyCacheTTLToBlocks(system, ttl); hit {
			message.Set("system", next)
			applied = true
		}
	}

	messages := memberValue(message, "messages")
	if messages == nil || !messages.IsArray() {
		return applied
	}
	nextMessages := convert.NewArray()
	changedAny := false
	for _, item := range messages.Items() {
		if item == nil || !item.IsObject() {
			nextMessages.Append(item)
			continue
		}
		content := memberValue(item, "content")
		if content == nil || !content.IsArray() {
			nextMessages.Append(item)
			continue
		}
		next, hit := applyCacheTTLToBlocks(content, ttl)
		if !hit {
			nextMessages.Append(item)
			continue
		}
		copied := item.Clone()
		copied.Set("content", next)
		nextMessages.Append(copied)
		changedAny = true
	}
	if changedAny {
		message.Set("messages", nextMessages)
		applied = true
	}
	return applied
}

// applyCacheTTLToBlocks 改写一组内容块的 cache_control.ttl；无命中时返回原数组与 false。
//
// 取值口径照 Node：只有恰好 `"1h"` 才是 1h，其余（含脏值）一律 5m——上游对 ttl 只认这两个值。
func applyCacheTTLToBlocks(blocks *convert.Value, ttl string) (*convert.Value, bool) {
	next := convert.NewArray()
	applied := false
	for _, item := range blocks.Items() {
		if item == nil || !item.IsObject() {
			next.Append(item)
			continue
		}
		cacheControl := memberValue(item, "cache_control")
		if cacheControl == nil || !cacheControl.IsObject() {
			next.Append(item)
			continue
		}
		kind, _ := cacheControl.StringField("type")
		if kind != "ephemeral" {
			next.Append(item)
			continue
		}
		applied = true
		updatedControl := cacheControl.Clone()
		if ttl == cacheTTLLong {
			updatedControl.Set("ttl", convert.NewString(cacheTTLLong))
		} else {
			updatedControl.Set("ttl", convert.NewString(cacheTTLShort))
		}
		copied := item.Clone()
		copied.Set("cache_control", updatedControl)
		next.Append(copied)
	}
	if !applied {
		return blocks, false
	}
	return next, true
}

// normalizeCacheTTL 复刻 Node：空与 inherit 视为未配置，其余原样透传（取值收敛在写入处）。
func normalizeCacheTTL(value string) string {
	if value == "" || value == inheritPreference {
		return ""
	}
	return value
}

// ---------------------------------------------------------------------------
// 取值与比较小工具
// ---------------------------------------------------------------------------

// objectFieldOrEmpty 取对象字段；不存在或不是对象时返回空对象（Node 的 isPlainObject 分支）。
//
// 返回的是**新对象**而非原位改写：调用方随后把它 Set 回宿主，故原值不受影响。
func objectFieldOrEmpty(host *convert.Value, key string) *convert.Value {
	if field := memberValue(host, key); field != nil && field.IsObject() {
		return field
	}
	return convert.NewObject()
}

// memberValue 取字段值，不限类型（缺失或宿主非对象时返回 nil）。
//
// 为何不用 ObjectField：它**只**返回对象，而本文件要取的字段大多是数组
// （tools / input / system / messages）——用 ObjectField 会拿到 nil，表现为「覆写写了但没生效」
// （单测里那一批 fail 的根因就是这个）。
func memberValue(host *convert.Value, key string) *convert.Value {
	if host == nil {
		return nil
	}
	value, ok := host.Get(key)
	if !ok {
		return nil
	}
	return value
}

func fieldValue(host *convert.Value, key string) *convert.Value {
	return memberValue(host, key)
}

// numericField 取数字字段的值；缺失或非数字返回 (0,false)。
func numericField(host *convert.Value, key string) (float64, bool) {
	value, ok := host.Get(key)
	if !ok || value == nil {
		return 0, false
	}
	return value.Float64()
}

// auditScalar 复刻 Node 的 toAuditValue：只留 string/number/boolean，其余（对象/数组/null）记 null。
func auditScalar(value *convert.Value) any {
	if value == nil || value.IsNull() {
		return nil
	}
	if text, ok := value.String(); ok {
		return text
	}
	if boolean, ok := value.Bool(); ok {
		return boolean
	}
	if literal, ok := value.NumberLiteral(); ok {
		// 用 json.Number 保留原字面量，避免 float64 往返改写整数形态（1e21、超长整数）。
		return json.Number(literal)
	}
	return nil
}

func fieldAuditScalar(host *convert.Value, key string) any {
	return auditScalar(fieldValue(host, key))
}

// nestedAuditScalar 取 `宿主.外层.内层` 的审计值；外层不是对象时记 null（Node 同）。
func nestedAuditScalar(host *convert.Value, outer, inner string) any {
	object := host.ObjectField(outer)
	if object == nil || !object.IsObject() {
		return nil
	}
	return fieldAuditScalar(object, inner)
}

// auditEqual 判定两个审计值是否相同（Node 用 `Object.is`）。
//
// json.Number 与 float64/int 混用时按数值比较：审计值一侧来自 `json.Number`（正文原字面量），
// 另一侧可能来自偏好解析出的整数，字面量不同但数值相同应当算「没变」。
func auditEqual(before, after any) bool {
	if before == nil || after == nil {
		return before == nil && after == nil
	}
	beforeNumber, beforeIsNumber := asNumber(before)
	afterNumber, afterIsNumber := asNumber(after)
	if beforeIsNumber || afterIsNumber {
		return beforeIsNumber && afterIsNumber && beforeNumber == afterNumber
	}
	return before == after
}

func asNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	case float64:
		return typed, true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	default:
		return 0, false
	}
}

// normalizeStringPreference 复刻 Node 的 normalizeStringPreference：
// 空与 inherit 表示未配置，其余原样返回。
func normalizeStringPreference(value string) (string, bool) {
	if value == "" || value == inheritPreference {
		return "", false
	}
	return value, true
}

// normalizeBoolPreference 复刻 Node 的 normalizeParallelToolCallsPreference /
// normalizeImageGenerationPreference：只有字面量 "true" 为真，其余非继承值一律假。
func normalizeBoolPreference(value string) (bool, bool) {
	if value == "" || value == inheritPreference {
		return false, false
	}
	return value == "true", true
}

// normalizeNumericPreference 复刻 Node 的 normalizeNumericPreference（`Number.parseInt(value, 10)`）：
// 非法即视为未配置；允许前后空白与正负号，遇到非数字字符即止。
func normalizeNumericPreference(value string) (int64, bool) {
	if value == "" || value == inheritPreference {
		return 0, false
	}
	return parseIntPrefix(value)
}

func parseIntPrefix(value string) (int64, bool) {
	trimmed := strings.TrimLeft(value, " \t\n\r\f\v")
	index := 0
	if index < len(trimmed) && (trimmed[index] == '+' || trimmed[index] == '-') {
		index++
	}
	start := index
	for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
		index++
	}
	if index == start {
		return 0, false
	}
	parsed, err := strconv.ParseInt(trimmed[:index], 10, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}
