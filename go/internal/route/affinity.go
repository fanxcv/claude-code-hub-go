package route

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/redis/go-redis/v9"
)

// sep 是 Node 指纹里使用的段分隔符（U+001F，见 fingerprint.ts 的 `const SEP = "\x1f"`）。
const sep = "\x1f"

// FingerprintBoundary 是一个指纹边界，对应 Node 的 FingerprintBoundary。
type FingerprintBoundary struct {
	// Depth 为 0 表示 F_sys；>=1 表示第 depth 条会话消息后的累计边界。
	Depth int `json:"depth"`
	// FP 是 sha256 截 32 位十六进制。
	FP string `json:"fp"`
	// PrefixBytes 是从开头到本边界（含）的规范化累计字节数。
	PrefixBytes int `json:"prefixBytes"`
	// HasCacheControl 表示 anthropic 的显式缓存断点落在本消息上。
	HasCacheControl bool `json:"hasCacheControl,omitempty"`
}

// FingerprintChain 是一条指纹链，对应 Node 的 FingerprintChain。
type FingerprintChain struct {
	// Sys 是 F_sys（系统提示 + 工具集）。
	Sys FingerprintBoundary `json:"sys"`
	// Tail 是浅 -> 深排列的会话消息边界，长度受 window 截断。
	Tail []FingerprintBoundary `json:"tail"`
}

// Tip 返回最深边界（无会话消息时回退 F_sys），对应 Node 的 fingerprintTip。
func (c FingerprintChain) Tip() FingerprintBoundary {
	if len(c.Tail) > 0 {
		return c.Tail[len(c.Tail)-1]
	}
	return c.Sys
}

// DeepestFirst 返回供查找使用的最深 -> 最浅指纹序列（不含 F_sys），
// 对应 Node 的 fingerprintsDeepestFirst：仅系统提示 + 工具相同的跨对话请求不应命中亲和。
func (c FingerprintChain) DeepestFirst() []string {
	out := make([]string, 0, len(c.Tail))
	for i := len(c.Tail) - 1; i >= 0; i-- {
		out = append(out, c.Tail[i].FP)
	}
	return out
}

// BoundaryOf 按指纹找回边界（用于记录命中深度与前缀字节）。
func (c FingerprintChain) BoundaryOf(fp string) (FingerprintBoundary, bool) {
	for _, boundary := range c.Tail {
		if boundary.FP == fp {
			return boundary, true
		}
	}
	return FingerprintBoundary{}, false
}

// AffinityWindow 归一化窗口：非正或非有限回退默认值，上限 MAX_AFFINITY_WINDOW=64。
func AffinityWindow(window int) int {
	if window <= 0 {
		return defaultAffinityWindow
	}
	if window > maxAffinityWindow {
		return maxAffinityWindow
	}
	return window
}

const (
	defaultAffinityWindow = 8
	maxAffinityWindow     = 64
)

// ScopeTag 复刻 buildScopeTag：sha256("{keyId}|{format}|{model}") 截 16 位十六进制。
func ScopeTag(keyID int64, format convert.ClientFormat, model string) string {
	raw := strconv.FormatInt(keyID, 10) + "|" + string(format) + "|" + model
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

// Fingerprint 计算一条指纹链；不支持的格式返回 (nil, false)。
//
// 已实现的线：claude（anthropic messages）、openai（chat completions）、response（OpenAI Responses）、
// gemini（generateContent）与 gemini-cli（正文嵌在 request 里时取内层）——五条分支逐条对照
// Node 的 fingerprint.ts:extractConversation。未知格式返回 false：此时不产生亲和提名，
// 退化为加权随机，不会产生错误命中。
func Fingerprint(body map[string]any, format convert.ClientFormat, window int) (*FingerprintChain, bool) {
	if body == nil {
		return nil, false
	}
	var extracted *extractedConversation
	switch format {
	case convert.FormatClaude:
		extracted = extractClaude(body)
	case convert.FormatOpenAI:
		extracted = extractOpenAIChat(body)
	case convert.FormatResponse:
		extracted = extractResponses(body)
	case convert.FormatGemini:
		extracted = extractGemini(body)
	case convert.FormatGeminiCLI:
		extracted = extractGeminiCLI(body)
	default:
		return nil, false
	}
	if extracted == nil {
		return nil, false
	}
	return buildChain(*extracted, AffinityWindow(window)), true
}

type normalizedMessage struct {
	bytes           string
	hasCacheControl bool
}

type extractedConversation struct {
	sysSegments []string
	messages    []normalizedMessage
}

func buildChain(extracted extractedConversation, window int) *FingerprintChain {
	sysBytes := strings.Join(extracted.sysSegments, "")
	sysFP := hash32(sysBytes)
	cumBytes := len(sysBytes)
	chain := &FingerprintChain{Sys: FingerprintBoundary{Depth: 0, FP: sysFP, PrefixBytes: cumBytes}}

	previous := sysFP
	depth := 0
	for _, message := range extracted.messages {
		if message.bytes == "" {
			continue
		}
		depth++
		previous = hash32(previous + message.bytes)
		cumBytes += len(message.bytes)
		chain.Tail = append(chain.Tail, FingerprintBoundary{
			Depth:           depth,
			FP:              previous,
			PrefixBytes:     cumBytes,
			HasCacheControl: message.hasCacheControl,
		})
	}
	if len(chain.Tail) > window {
		chain.Tail = chain.Tail[len(chain.Tail)-window:]
	}
	return chain
}

func hash32(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:32]
}

// ===== 各协议线的会话抽取（逐条对照 fingerprint.ts） =====

func extractClaude(body map[string]any) *extractedConversation {
	messages, ok := body["messages"].([]any)
	if !ok {
		return nil
	}
	segments := []string{sep}
	switch system := body["system"].(type) {
	case string:
		segments = append(segments, system)
	case []any:
		for _, block := range system {
			segments = append(segments, normalizeContentBlock(block))
		}
	}
	segments = appendTools(segments, body["tools"], func(tool map[string]any) normalizedToolSpec {
		return normalizedToolSpec{
			name:        readString(tool, "name"),
			description: readString(tool, "description"),
			parameters:  readRecord(tool, "input_schema"),
		}
	})

	out := make([]normalizedMessage, 0, len(messages))
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts := []string{sep, readString(msg, "role")}
		hasCacheControl := false
		switch content := msg["content"].(type) {
		case string:
			parts = append(parts, sep, "text:", content)
		case []any:
			for _, block := range content {
				parts = append(parts, normalizeContentBlock(block))
				if record, ok := block.(map[string]any); ok && record["cache_control"] != nil {
					hasCacheControl = true
				}
			}
		}
		out = append(out, finishMessage(parts, hasCacheControl))
	}
	return &extractedConversation{sysSegments: segments, messages: out}
}

// normalizeContentBlock 复刻 anthropic 内容块归一化（system 块与消息块共用）。
func normalizeContentBlock(block any) string {
	record, ok := block.(map[string]any)
	if !ok {
		if text, isText := block.(string); isText {
			return sep + "text:" + text
		}
		return ""
	}
	switch blockType := readString(record, "type"); blockType {
	case "text":
		return sep + "text:" + readString(record, "text")
	case "thinking":
		// 思考签名跨轮可能变化，不入指纹。
		return sep + "thinking:" + readString(record, "thinking")
	case "redacted_thinking":
		return sep + "redacted_thinking:" + readString(record, "data")
	case "tool_use":
		// 剥 id，保留工具身份（name + input）。
		return sep + "tool_use:" + readString(record, "name") + ":" + stableStringify(valueOrNil(record["input"]))
	case "tool_result":
		// 剥 tool_use_id，保留结果内容。
		return sep + "tool_result:" + serializeUnknownContent(record["content"])
	case "image", "document":
		return sep + blockType + ":" + digestMediaSource(readRecord(record, "source"))
	default:
		if blockType == "" {
			return ""
		}
		return sep + blockType + ":" + stableStringify(stripVolatileKeys(record))
	}
}

func digestMediaSource(source map[string]any) string {
	if source == nil {
		return ""
	}
	mediaType := readString(source, "media_type")
	if mediaType == "" {
		mediaType = readString(source, "mediaType")
	}
	if data := readString(source, "data"); data != "" {
		// 内容摘要而非长度：同长异图不碰撞；base64 原文绝不进指纹。
		return mediaType + ":" + hash32(data)
	}
	return mediaType + ":" + readString(source, "url")
}

func extractOpenAIChat(body map[string]any) *extractedConversation {
	messages, ok := body["messages"].([]any)
	if !ok {
		return nil
	}
	segments := []string{sep}
	segments = appendTools(segments, body["tools"], func(tool map[string]any) normalizedToolSpec {
		fn := readRecord(tool, "function")
		if fn != nil {
			return normalizedToolSpec{
				name:        readString(fn, "name"),
				description: readString(fn, "description"),
				parameters:  readRecord(fn, "parameters"),
			}
		}
		return normalizedToolSpec{
			name:        readString(tool, "name"),
			description: readString(tool, "description"),
		}
	})

	out := make([]normalizedMessage, 0, len(messages))
	leadingSystem := true
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := readString(msg, "role")
		// 前导 system/developer 消息属于跨对话稳定段，并入 F_sys。
		if leadingSystem && (role == "system" || role == "developer") {
			segments = append(segments, sep, role, ":", serializeUnknownContent(msg["content"]))
			continue
		}
		leadingSystem = false

		parts := []string{sep, role, sep, "content:", serializeUnknownContent(msg["content"])}
		if toolCalls, ok := msg["tool_calls"].([]any); ok {
			for _, call := range toolCalls {
				callRecord, ok := call.(map[string]any)
				if !ok {
					continue
				}
				fn := readRecord(callRecord, "function")
				name, arguments := "", ""
				if fn != nil {
					name = readString(fn, "name")
					arguments = readString(fn, "arguments")
				}
				parts = append(parts, sep, "tool_call:", name, ":", arguments)
			}
		}
		if _, present := msg["tool_call_id"]; present {
			parts = append(parts, sep, "tool_result")
		}
		out = append(out, finishMessage(parts, false))
	}
	return &extractedConversation{sysSegments: segments, messages: out}
}

func extractResponses(body map[string]any) *extractedConversation {
	input := body["input"]
	segments := []string{sep}
	if instructions, ok := body["instructions"].(string); ok {
		segments = append(segments, instructions)
	}
	segments = appendTools(segments, body["tools"], func(tool map[string]any) normalizedToolSpec {
		return normalizedToolSpec{
			name:        readString(tool, "name"),
			description: readString(tool, "description"),
			parameters:  readRecord(tool, "parameters"),
		}
	})

	out := make([]normalizedMessage, 0, 8)
	switch typed := input.(type) {
	case string:
		out = append(out, normalizedMessage{bytes: sep + "user" + sep + "text:" + typed})
	case []any:
		for _, raw := range typed {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			itemType := readString(item, "type")
			if itemType == "" {
				itemType = "message"
			}
			parts := []string{sep}
			switch itemType {
			case "message":
				parts = append(parts, readString(item, "role"), sep, "content:", serializeUnknownContent(item["content"]))
			case "function_call":
				parts = append(parts, "function_call", sep, readString(item, "name"), ":", readString(item, "arguments"))
			case "function_call_output":
				parts = append(parts, "function_call_output", sep, serializeUnknownContent(item["output"]))
			case "reasoning":
				summary := item["summary"]
				if summary == nil {
					summary = item["content"]
				}
				parts = append(parts, "reasoning", sep, serializeUnknownContent(summary))
			default:
				parts = append(parts, itemType, sep, stableStringify(stripVolatileKeys(item)))
			}
			out = append(out, finishMessage(parts, false))
		}
	default:
		return nil
	}
	return &extractedConversation{sysSegments: segments, messages: out}
}

// ===== gemini / gemini-cli =====

// extractGemini 复刻 fingerprint.ts:extractGemini。
//
// 与其它线的两点差别：
//   - gemini 把系统段放在 systemInstruction.parts（REST 也有 system_instruction 变体）；
//   - 工具声明嵌在 tools[].functionDeclarations 里，先摊平再并入 F_sys。
//
// contents 不是数组时不可指纹化（与 Node 的 `!Array.isArray(contents) → null` 同结论）。
func extractGemini(body map[string]any) *extractedConversation {
	contents, ok := body["contents"].([]any)
	if !ok {
		return nil
	}

	segments := []string{sep}
	systemInstruction := readRecord(body, "systemInstruction")
	if systemInstruction == nil {
		systemInstruction = readRecord(body, "system_instruction")
	}
	if systemInstruction != nil {
		if parts, ok := systemInstruction["parts"].([]any); ok {
			for _, part := range parts {
				segments = append(segments, normalizeGeminiPart(part))
			}
		}
	}
	segments = appendTools(segments, flattenGeminiTools(body["tools"]), func(declaration map[string]any) normalizedToolSpec {
		return normalizedToolSpec{
			name:        readString(declaration, "name"),
			description: readString(declaration, "description"),
			parameters:  readRecord(declaration, "parameters"),
		}
	})

	out := make([]normalizedMessage, 0, len(contents))
	for _, raw := range contents {
		content, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts := []string{sep, readString(content, "role")}
		if contentParts, ok := content["parts"].([]any); ok {
			for _, part := range contentParts {
				parts = append(parts, normalizeGeminiPart(part))
			}
		}
		out = append(out, finishMessage(parts, false))
	}
	return &extractedConversation{sysSegments: segments, messages: out}
}

// extractGeminiCLI 复刻 fingerprint.ts 的 gemini-cli 分支：正文嵌在 request 里时取内层。
//
// JS 的判据是 `request && typeof request === "object"`：null/缺失/标量都回落外层正文，
// 而数组同样满足 typeof === "object"，进内层后取不到 contents 即归 null。为保持
// 「同输入同结论」，这里显式跟随该分支。
func extractGeminiCLI(body map[string]any) *extractedConversation {
	if raw, present := body["request"]; present {
		switch request := raw.(type) {
		case map[string]any:
			return extractGemini(request)
		case []any:
			return nil
		}
	}
	return extractGemini(body)
}

// flattenGeminiTools 复刻 fingerprint.ts:flattenGeminiTools：把 tools[].functionDeclarations 摊平，
// 没有该键的条目按声明本身对待（Node 的 else 分支）。
func flattenGeminiTools(raw any) []any {
	tools, ok := raw.([]any)
	if !ok {
		return nil
	}
	declarations := make([]any, 0, len(tools))
	for _, tool := range tools {
		record, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		if nested, ok := record["functionDeclarations"].([]any); ok {
			declarations = append(declarations, nested...)
			continue
		}
		declarations = append(declarations, tool)
	}
	return declarations
}

// normalizeGeminiPart 复刻 fingerprint.ts:normalizeGeminiPart。
//
// 易变 id 一律不入指纹：functionCall 只取 name + args、functionResponse 只取 name + response；
// 内联二进制取内容 sha256 摘要（同长异图不碰撞），文件引用取 uri。无法归类的 part 走兜底，
// 其内部顶层易变键由 stripVolatileKeys 剥离。
func normalizeGeminiPart(part any) string {
	record, ok := part.(map[string]any)
	if !ok {
		return ""
	}
	if text, ok := record["text"].(string); ok {
		return sep + "text:" + text
	}
	if functionCall := readRecord(record, "functionCall"); functionCall != nil {
		return sep + "tool_use:" + readString(functionCall, "name") + ":" +
			stableStringify(valueOrNil(functionCall["args"]))
	}
	if functionResponse := readRecord(record, "functionResponse"); functionResponse != nil {
		return sep + "tool_result:" + readString(functionResponse, "name") + ":" +
			stableStringify(valueOrNil(functionResponse["response"]))
	}
	inlineData := firstRecord(record, "inlineData", "inline_data")
	if inlineData != nil {
		digest := ""
		if data := readString(inlineData, "data"); data != "" {
			digest = hash32(data)
		}
		return sep + "image:" + firstString(inlineData, "mimeType", "mime_type") + ":" + digest
	}
	fileData := firstRecord(record, "fileData", "file_data")
	if fileData != nil {
		return sep + "file:" + firstString(fileData, "mimeType", "mime_type") + ":" +
			firstString(fileData, "fileUri", "file_uri")
	}
	return sep + "part:" + stableStringify(stripVolatileKeys(record))
}

// firstRecord 按给定顺序取第一个存在的对象值（Node 的 `a ?? b` / `a || b` 形态）。
func firstRecord(record map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value := readRecord(record, key); value != nil {
			return value
		}
	}
	return nil
}

// firstString 按给定顺序取第一个非空字符串值。
func firstString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := readString(record, key); value != "" {
			return value
		}
	}
	return ""
}

type normalizedToolSpec struct {
	name        string
	description string
	parameters  map[string]any
}

// appendTools 复刻 appendTools：工具按 name 排序后并入 F_sys，工具顺序差异不产生不同 F_sys。
//
// 返回追加后的切片而**不是**就地 append：Go 的切片按值传递，而 `segments := []string{sep}`
// 容量只有 1，首个工具就触发扩容，调用方拿不到新底层数组——工具段会静默丢失（Node 的
// `segments.push(...)` 是就地变更，两者语义不同）。
func appendTools(segments []string, raw any, project func(map[string]any) normalizedToolSpec) []string {
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return segments
	}
	specs := make([]normalizedToolSpec, 0, len(tools))
	for _, tool := range tools {
		record, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		spec := project(record)
		if spec.name == "" {
			continue
		}
		specs = append(specs, spec)
	}
	sort.SliceStable(specs, func(i, j int) bool { return specs[i].name < specs[j].name })
	for _, spec := range specs {
		parameters := ""
		if spec.parameters != nil {
			parameters = stableStringify(spec.parameters)
		}
		segments = append(segments, sep, spec.name, ":", spec.description, ":", parameters)
	}
	return segments
}

// finishMessage 复刻 finishMessage：只有分隔符 + role 而无任何内容段的消息视为空。
func finishMessage(parts []string, hasCacheControl bool) normalizedMessage {
	bytes := strings.Join(parts, "")
	meaningful := len(parts) > 2
	return normalizedMessage{bytes: valueOrEmpty(meaningful, bytes), hasCacheControl: hasCacheControl}
}

func serializeUnknownContent(content any) string {
	if content == nil {
		return ""
	}
	if text, ok := content.(string); ok {
		return text
	}
	return stableStringify(content)
}

var volatileKeys = map[string]bool{
	"id": true, "call_id": true, "tool_use_id": true, "tool_call_id": true, "cache_control": true,
}

func stripVolatileKeys(record map[string]any) map[string]any {
	out := make(map[string]any, len(record))
	for key, value := range record {
		if volatileKeys[key] {
			continue
		}
		out[key] = value
	}
	return out
}

// stableStringify 复刻 request-identity.ts 的 stableStringify：对象键按字典序，数组保序。
//
// 已知差异：数字用 Go 的最短往返表示，与 JS 的 JSON.stringify 在极端量级（>=1e21 或极小小数）
// 下可能不同形；这类值出现在指纹输入里只会让该条消息的指纹与 Node 不同，不会串到别的会话。
func stableStringify(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	case bool, float64, int, int64, json.Number:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, stableStringify(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			if typed[key] == nil {
				continue
			}
			encoded, _ := json.Marshal(key)
			parts = append(parts, string(encoded)+":"+stableStringify(typed[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "null"
		}
		return string(encoded)
	}
}

func readString(record map[string]any, key string) string {
	value, ok := record[key].(string)
	if !ok {
		return ""
	}
	return value
}

func readRecord(record map[string]any, key string) map[string]any {
	value, ok := record[key].(map[string]any)
	if !ok {
		return nil
	}
	return value
}

func valueOrNil(value any) any {
	if value == nil {
		return nil
	}
	return value
}

func valueOrEmpty(keep bool, value string) string {
	if keep {
		return value
	}
	return ""
}

// ===== 亲和指针读取（最小可用：最长前缀命中即提名） =====

// affinityKeyPrefix 与 Node 的键形制一致：cch:pfx:{<scopeTag>}:fp:<fp>。
// 花括号是 Redis Cluster hash-tag，单机下只是键名的一部分。
const (
	affinityKeyPrefix = "cch:pfx:"
	// affinityTombstoneTTLSeconds 对应 Node 的 TOMBSTONE_TTL_SECONDS。
	affinityTombstoneTTLSeconds = 60
	// affinityGenerationFenceTTLSeconds 对应 Node 的 GENERATION_FENCE_TTL_SECONDS。
	affinityGenerationFenceTTLSeconds = 2 * 24 * 60 * 60
)

// 脚本以包级变量持有，让 go-redis 的 EVALSHA 摘要跨调用复用（每次 NewScript 都会丢掉已加载的摘要）。
var (
	affinityLookupCandidates = redis.NewScript(affinityLookupCandidatesLua)
	affinityValidateHit      = redis.NewScript(affinityValidateHitLua)
	affinityEnsureGeneration = redis.NewScript(affinityEnsureGenerationLua)
	affinityCASWrite         = redis.NewScript(affinityCASWriteLua)
	affinityInvalidate       = redis.NewScript(affinityInvalidateLua)
)

// AffinityStore 读写前缀亲和绑定。
//
// 键形制（与 Node 逐字一致，{scopeTag} 为 Redis Cluster hash tag）：
//
//	cch:pfx:{scope}:fp:<fp>        绑定值，活跃 "1|<providerId>|<identityFp>|<generation>"，墓碑 "0|<reason>|..."
//	cch:pfx:{scope}:gen:<identityFp>  identity generation，写入侧的 CAS 依据
//	cch:pfx:{scope}:desc:<identityFp>  旧版 descendant 集合，仅失效时读取
//	cch:pfx:{scope}:desc-v2:<identityFp> descendant 有序集合，带绑定过期时刻
//	cch:pfx:{scope}:generation       旧版 generation，仅查找侧兼容读
//
// generation fence：查找时若候选携带的 generation 与 gen 键不符即被拒（迟到的旧 generation
// 既不能命中，也不能写回）；写入走 CAS_WRITE，gen 键变了即静默放弃。
type AffinityStore struct {
	redis  redis.UniversalClient
	window int
	// slidingTTLSeconds 对应 PREFIX_AFFINITY_TTL_SECONDS：命中续期与写回共用同一 TTL。
	slidingTTLSeconds int
	// generationToken 生成新的 identity generation，测试可注入以拿到确定值。
	generationToken func() string
}

// AffinityOptions 是 AffinityStore 的构造参数。
type AffinityOptions struct {
	// Redis 为 nil 时 Lookup 一律视为不可用（fail-open，与 Node 的亲和读失败语义一致）。
	Redis redis.UniversalClient
	// Window 对应 PREFIX_AFFINITY_WINDOW。
	Window int
	// SlidingTTLSeconds 对应 PREFIX_AFFINITY_TTL_SECONDS。
	SlidingTTLSeconds int
	// GenerationToken 为 nil 时用默认的 "v3:<uuid4>"（与 Node 的 generationToken 默认值同形）。
	GenerationToken func() string
}

// NewAffinityStore 构造亲和读写器。
func NewAffinityStore(opts AffinityOptions) *AffinityStore {
	token := opts.GenerationToken
	if token == nil {
		token = newAffinityGenerationToken
	}
	return &AffinityStore{
		redis:             opts.Redis,
		window:            AffinityWindow(opts.Window),
		slidingTTLSeconds: opts.SlidingTTLSeconds,
		generationToken:   token,
	}
}

// newAffinityGenerationToken 复刻 Node 的 `v3:${randomUUID()}`。
func newAffinityGenerationToken() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "v3:" + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw[:])
	return "v3:" + encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" +
		encoded[16:20] + "-" + encoded[20:32]
}

// bindingKey 对应 Node 的 buildKey。
func (a *AffinityStore) bindingKey(scopeTag, fp string) string {
	return affinityKeyPrefix + "{" + scopeTag + "}:fp:" + fp
}

// generationKey 对应 Node 的 buildGenerationKey。
func (a *AffinityStore) generationKey(scopeTag, identityFP string) string {
	return affinityKeyPrefix + "{" + scopeTag + "}:gen:" + identityFP
}

// descendantsKey 对应 Node 的 buildDescendantsKey（旧版集合）。
func (a *AffinityStore) descendantsKey(scopeTag, identityFP string) string {
	return affinityKeyPrefix + "{" + scopeTag + "}:desc:" + identityFP
}

// descendantsV2Key 对应 Node 的 buildDescendantsV2Key。
func (a *AffinityStore) descendantsV2Key(scopeTag, identityFP string) string {
	return affinityKeyPrefix + "{" + scopeTag + "}:desc-v2:" + identityFP
}

// legacyGenerationKey 对应 Node 的 buildLegacyGenerationKey。
func (a *AffinityStore) legacyGenerationKey(scopeTag string) string {
	return affinityKeyPrefix + "{" + scopeTag + "}:generation"
}

// conversationActivityKey 是「本次对话最后活跃时刻」键：cch:pfx:{<scopeTag>}:seen:<sessionId>。
//
// **键的选型是本轮被实证推翻过一版的地方，记在这里免得后人重走**：
//   - **绑定指纹**：绑定只在**命中**时被续期（affinity_validate_hit_v5 的 EXPIRE），
//     而活跃记录也只在同一次命中里写 ⇒ 两者刷新时刻恒等 ⇒「绑定仍活 ∧ 空闲 > 阈值」
//     不可达，闸门永不触发。
//   - **identity root**：它取自绑定值携带的 root，fork / 续跑会话命中同一绑定时 root 相同
//     ⇒ 区分不出「哪个会话闲置了」，而那正是要治的情形。
//
// 只有客户端会话身份是本会话自己的：别的会话再多命中同一绑定，也不会刷新这个键。
// 它取自既有会话缝合道（`guard.Deps.SessionLookup` → `SessionResult.SessionID`，
// 与落库列 session_id 同源），不自造第三套 id 口径。
//
// 值与 TTL：值是上次活跃的 Unix 秒（判「超过多久」必须存时刻，不能存布尔）；
// TTL 是阈值的 conversationActivityTTLFactor 倍——**必须严格大于阈值**，
// 否则记录与判定同时到期，「无记录」既可能是首条请求也可能是久未活跃。
//
// 它落在 `{<scopeTag>}` hash tag 内：与绑定同槽，且既有集成夹具的
// `cch:pfx:{<scope>:*` 清扫模式能一并覆盖。
func (a *AffinityStore) conversationActivityKey(scopeTag, sessionID string) string {
	return affinityKeyPrefix + "{" + scopeTag + "}:seen:" + sessionID
}

// conversationActivityTTLFactor 是活跃记录 TTL 相对判定阈值的倍数。
//
// 取 4 的根据：判定语为「空闲 > 阈值」，而记录要活到能被读到。倍数为 1 时记录与阈值同时
// 到期（不可判定）；倍数越大，可判定的空闲区间越长，代价只是键多活一阵。
//
// **登记在案的上限**：空闲超过 4 倍阈值时记录也过期，此时按「无记录」fail-open（不撤销亲和）。
// 即本闸门只覆盖 `(阈值, 4×阈值]` 这段空闲，不是任意长的闲置。
const conversationActivityTTLFactor = 4

// AffinityHint 是一次命中提示，对应 Node 的 AffinityHint。
type AffinityHint struct {
	ProviderID int64
	MatchedFP  string
	// MatchedIndex 为 0-based：0 = 最深（tip），越大越浅。
	MatchedIndex int
}

// AffinityLookup 是一次查找的结果，对应 Node 的 AffinityLookupResult。
//
// 未命中时 Hint 为 nil，但 IdentityFP 与 Generation 仍然有效——终态写回需要它们
// （对话的第一条请求就是「未命中但必须写回」的情形）。
// AffinityIdentity 是本次请求的亲和身份事实，对应 Node 的 session.affinity 存在时
// provider-selector 写入 `session_identity_kind = "prefix_affinity"` 的那一支：
// `identity = "pfx:" + scopeTag + ":" + (identityFp ?? matchedFp ?? tipFp)`。
//
// 它与 AffinityLookup / AffinityWriteback **不是同一个条件**：那两者要求查找真跑通
// （Redis 可用），而 Node 的 `session.affinity` 在指纹链可算时就已经存在，与 Redis 是否
// 可达无关。故 Redis 故障时本字段仍非 nil，该请求的日志依旧是「前缀亲和身份」。
type AffinityIdentity struct {
	// ScopeTag 是亲和作用域标签（KeyID + 方言 + 模型）。
	ScopeTag string
	// Fingerprint 是身份指纹：命中用绑定的 identityFp，否则用链尾指纹。
	Fingerprint string
}

type AffinityLookup struct {
	// Hint 为 nil 表示未命中活跃绑定。
	Hint *AffinityHint
	// IdentityFP 是本次请求所属的 identity root：命中时为绑定携带的 root，未命中时为最深指纹。
	IdentityFP string
	// Generation 是查找时捕获的 identity generation；写回必须以它做 CAS，否则会被 fence 拒。
	Generation string
}

// Lookup 按最深 -> 最浅查找活跃绑定：候选选择、generation 校验与命中续期在 Redis 内原子完成。
//
// 第二个返回值为 false 表示「本次查找不可用」（未配置 Redis、参数不足或 Redis 故障），
// 调用方应回落加权随机；与 Node 的 fail-open 语义一致，绝不因亲和失败影响主路径。
func (a *AffinityStore) Lookup(
	ctx context.Context,
	scopeTag string,
	fpsDeepestFirst []string,
) (*AffinityLookup, bool) {
	if a == nil || a.redis == nil || scopeTag == "" {
		return nil, false
	}
	fps := make([]string, 0, len(fpsDeepestFirst))
	for _, fp := range fpsDeepestFirst {
		if fp != "" {
			fps = append(fps, fp)
		}
	}
	if len(fps) == 0 {
		return nil, false
	}

	keys := make([]string, 0, len(fps)+1)
	for _, fp := range fps {
		keys = append(keys, a.bindingKey(scopeTag, fp))
	}
	keys = append(keys, a.legacyGenerationKey(scopeTag))

	raw, err := affinityLookupCandidates.Run(ctx, a.redis, keys).Result()
	if err != nil {
		return nil, false
	}
	candidates, ok := raw.([]any)
	if !ok {
		return nil, false
	}

	ttl := int64(a.slidingTTLSeconds)
	if ttl < 0 {
		ttl = 0
	}
	now := time.Now().Unix()
	for offset := 0; offset+1 < len(candidates); offset += 2 {
		position, ok := toInt64(candidates[offset])
		if !ok {
			continue
		}
		matchedIndex := int(position) - 1
		value, ok := candidates[offset+1].(string)
		if !ok || matchedIndex < 0 || matchedIndex >= len(fps) {
			continue
		}
		providerID, ok := affinityValueProvider(value)
		if !ok {
			continue
		}

		matchedFP := fps[matchedIndex]
		identityFP, generation, migrated := matchedFP, a.generationToken(), ""
		if parts := strings.Split(value, "|"); len(parts) == 4 && parts[2] != "" && parts[3] != "" {
			// 四段是新格式：沿用绑定自带的 identity 与 generation，不做迁移。
			identityFP, generation = parts[2], parts[3]
		} else {
			// 旧格式（两段/三段）：本次升级为四段，identity root 取命中指纹。
			migrated = "1|" + strconv.FormatInt(providerID, 10) + "|" + identityFP + "|" + generation
		}

		validated, err := affinityValidateHit.Run(ctx, a.redis, []string{
			keys[matchedIndex],
			a.generationKey(scopeTag, identityFP),
			a.descendantsV2Key(scopeTag, identityFP),
		}, []any{value, generation, migrated, ttl, affinityGenerationFenceTTLSeconds, now, now + ttl}).Int64()
		if err != nil {
			return nil, false
		}
		if validated != 1 {
			// generation 不符（invalidate 已推进）：本候选作废，继续向浅找。
			continue
		}
		return &AffinityLookup{
			Hint:       &AffinityHint{ProviderID: providerID, MatchedFP: matchedFP, MatchedIndex: matchedIndex},
			IdentityFP: identityFP,
			Generation: generation,
		}, true
	}

	// 未命中：确保 identity root 已有 generation，供终态写回做 CAS。
	missIdentityFP := fps[0]
	ensured, err := affinityEnsureGeneration.Run(ctx, a.redis,
		[]string{a.generationKey(scopeTag, missIdentityFP)},
		[]any{a.generationToken(), affinityGenerationFenceTTLSeconds}).Result()
	if err != nil {
		return nil, false
	}
	generation, ok := ensured.(string)
	if !ok || generation == "" {
		return nil, false
	}
	return &AffinityLookup{IdentityFP: missIdentityFP, Generation: generation}, true
}

// ConversationIdle 返回该会话距上次活跃的秒数、判定阈值，以及该记录是否可信。
//
// known 为 false 的四种情形，一律 fail-open（**调用方不得据此撤销亲和**）：
// 会话身份为空（客户端未带 session id）、Redis 未配置或往返失败、记录不存在、值不可解析。
// 「记录不存在」既可能是本次对话的首条请求，也可能是空闲已超过记录 TTL——两者不可区分，
// 故只能放行（宁可保留粘性，不可无故打断）。
//
// 第二个返回值是阈值，取自与绑定同源的滑动 TTL，避免调用方与 store 各写一个数。
// 空闲量按秒计算，负值（对端实例时钟略快）归 0。
func (a *AffinityStore) ConversationIdle(
	ctx context.Context,
	scopeTag string,
	sessionID string,
	now int64,
) (idleSeconds int64, thresholdSeconds int64, known bool) {
	if a == nil || a.redis == nil || scopeTag == "" || sessionID == "" || a.slidingTTLSeconds <= 0 {
		return 0, 0, false
	}
	threshold := int64(a.slidingTTLSeconds)
	raw, err := a.redis.Get(ctx, a.conversationActivityKey(scopeTag, sessionID)).Result()
	if err != nil {
		// 未命中（redis.Nil）与故障一视同仁：都是「读不到可信历史」。
		return 0, threshold, false
	}
	last, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, threshold, false
	}
	if idle := now - last; idle > 0 {
		return idle, threshold, true
	}
	return 0, threshold, true
}

// NoteConversationActivity 记下该会话在本刻活跃。
//
// 写失败只影响下一次判定（退化为 fail-open），不在请求的关键语义路径上，故不返回值、
// 也不在 store 层打日志（本包无 logger；接线层已在撤销时留痕）。
// TTL 取阈值的 conversationActivityTTLFactor 倍（见那里的上限说明）。
func (a *AffinityStore) NoteConversationActivity(
	ctx context.Context,
	scopeTag string,
	sessionID string,
	now int64,
) {
	if a == nil || a.redis == nil || scopeTag == "" || sessionID == "" || a.slidingTTLSeconds <= 0 {
		return
	}
	ttl := time.Duration(a.slidingTTLSeconds*conversationActivityTTLFactor) * time.Second
	_ = a.redis.Set(ctx, a.conversationActivityKey(scopeTag, sessionID),
		strconv.FormatInt(now, 10), ttl).Err()
}

// Put 是成功终态的写回：只写 tip 一键（对话推进天然累积链条，无需写全窗口），
// 不写 F_sys 键（仅系统提示词相同的跨对话请求不应互相粘连）。
//
// 返回值 false 表示未写入，原因只有两类：参数不足/未配置 Redis，或 generation CAS 失败
// （在途旧请求带着旧 generation 回来写回）——后者正是 fence 要挡住的情形，不报错、只放弃。
func (a *AffinityStore) Put(
	ctx context.Context,
	scopeTag string,
	tipFP string,
	providerID int64,
	identityFP string,
	expectedGeneration string,
) bool {
	// nil 接收者**必须**在解引用之前判掉：本函数 godoc 承诺「false 的原因只有参数不足/未配置
	// Redis 与 generation CAS 失败两类」，而 slidingTTLSeconds 是解引用；同文件 Lookup 与
	// Invalidate 也是这个次序，本处是全文件唯一的例外（SA5011）。
	if a == nil {
		return false
	}
	ttl := int64(a.slidingTTLSeconds)
	if a.redis == nil || scopeTag == "" || tipFP == "" || providerID <= 0 ||
		identityFP == "" || expectedGeneration == "" || ttl <= 0 {
		return false
	}
	now := time.Now().Unix()
	value := "1|" + strconv.FormatInt(providerID, 10) + "|" + identityFP + "|" + expectedGeneration
	return a.casWrite(ctx, scopeTag, tipFP, identityFP, expectedGeneration, value, ttl, now)
}

// Tombstone 写 failover 墓碑：短 TTL 覆盖旧绑定，阻止后续请求羊群式撞向同一故障绑定，
// 同时让查找跳过墓碑继续向浅回落。同样受 generation CAS 约束。
func (a *AffinityStore) Tombstone(
	ctx context.Context,
	scopeTag string,
	fp string,
	reason string,
	identityFP string,
	expectedGeneration string,
) bool {
	if a == nil || a.redis == nil || scopeTag == "" || fp == "" || identityFP == "" || expectedGeneration == "" {
		return false
	}
	if len(reason) > 32 {
		reason = reason[:32]
	}
	now := time.Now().Unix()
	value := "0|" + reason + "|" + identityFP + "|" + expectedGeneration
	return a.casWrite(ctx, scopeTag, fp, identityFP, expectedGeneration, value, affinityTombstoneTTLSeconds, now)
}

// casWrite 执行 CAS_WRITE_LUA：generation 相符才写绑定、登记 descendant 并续期两个键。
func (a *AffinityStore) casWrite(
	ctx context.Context,
	scopeTag string,
	fp string,
	identityFP string,
	expectedGeneration string,
	value string,
	ttlSeconds int64,
	now int64,
) bool {
	raw, err := affinityCASWrite.Run(ctx, a.redis, []string{
		a.generationKey(scopeTag, identityFP),
		a.bindingKey(scopeTag, fp),
		a.descendantsV2Key(scopeTag, identityFP),
	}, []any{expectedGeneration, value, ttlSeconds, affinityGenerationFenceTTLSeconds, now, now + ttlSeconds}).Result()
	if err != nil {
		return false
	}
	written, ok := toInt64(raw)
	return ok && written == 1
}

// Invalidate 原子推进 identity generation，再删除已登记 descendant 与调用方已知的绑定键。
//
// 这是 generation 递增的唯一入口（管理面终止前缀 Session 时调用）：在途旧请求仍携带旧
// generation，其后续 Put 会被 CAS 拒绝，因此无法把已终止的绑定复活。
func (a *AffinityStore) Invalidate(
	ctx context.Context,
	scopeTag string,
	identityFP string,
	fingerprints []string,
) bool {
	if a == nil || a.redis == nil || scopeTag == "" || identityFP == "" {
		return false
	}
	known := make(map[string]bool, len(fingerprints)+1)
	keys := make([]string, 0, len(fingerprints)+1)
	for _, fp := range append([]string{identityFP}, fingerprints...) {
		if fp == "" || known[fp] {
			continue
		}
		known[fp] = true
		keys = append(keys, a.bindingKey(scopeTag, fp))
	}

	descendantsKey := a.descendantsKey(scopeTag, identityFP)
	descendantsV2Key := a.descendantsV2Key(scopeTag, identityFP)
	now := time.Now().Unix()
	if err := a.redis.ZRemRangeByScore(ctx, descendantsV2Key, "-inf", strconv.FormatInt(now, 10)).Err(); err != nil {
		return false
	}
	legacy, err := a.redis.SMembers(ctx, descendantsKey).Result()
	if err != nil {
		return false
	}
	v2, err := a.redis.ZRange(ctx, descendantsV2Key, 0, -1).Result()
	if err != nil {
		return false
	}
	for _, key := range append(legacy, v2...) {
		if key == "" || known[key] {
			continue
		}
		known[key] = true
		keys = append(keys, key)
	}

	scriptKeys := make([]string, 0, len(keys)+3)
	scriptKeys = append(scriptKeys, a.generationKey(scopeTag, identityFP), descendantsKey, descendantsV2Key)
	scriptKeys = append(scriptKeys, keys...)
	if _, err := affinityInvalidate.Run(ctx, a.redis, scriptKeys,
		[]any{a.generationToken(), affinityGenerationFenceTTLSeconds}).Result(); err != nil {
		return false
	}
	return true
}

// affinityValueProvider 从管道串值里取出 provider id，对应 Node 的 parts[1] 解析。
func affinityValueProvider(value string) (int64, bool) {
	fields := strings.Split(value, "|")
	if len(fields) < 2 {
		return 0, false
	}
	id, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// toInt64 归一化 Lua 的数值返回（Redis 一律给整数，但经 go-redis 泛化后可能是多种整型）。
func toInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

// AffinityNomination 是一次被接受的亲和提名。
type AffinityNomination struct {
	Provider Provider
	Hint     AffinityHint
	// MatchedDepth 与 MatchedPrefixBytes 来自指纹链，用于落链展示「匹配到哪个前缀」。
	MatchedDepth      int
	MatchedPrefixByte int
	// 以下四项供终态写回（affinity.go 的 Put）使用，避免调用方自行重建 scope 与指纹。
	ScopeTag   string
	TipFP      string
	IdentityFP string
	Generation string
}

// nominateByAffinity 复刻 tryPrefixAffinityNomination 的软提名：命中后仍须过全套硬校验，
// 任一不过即静默回落加权随机，绝不绕过任何硬性约束。
//
// 第二个返回值是本次查找的结果（未命中时也非 nil），第三个返回值是终态写回事实
// （未命中时同样非 nil：对话第一条请求就是「未命中但必须写回」的情形）；
// 亲和未参与或 Redis 不可用时两者为 nil。
func (s *Selector) nominateByAffinity(
	ctx context.Context,
	req Request,
	filtered []Filtered,
	excluded map[int64]bool,
) (AffinityNomination, *AffinityLookup, *AffinityWriteback, *AffinityIdentity, bool) {
	if s.opts.Affinity == nil || req.KeyID == 0 || req.AffinityBody == nil || req.Format == "" {
		return AffinityNomination{}, nil, nil, nil, false
	}
	chain, ok := Fingerprint(req.AffinityBody, req.Format, s.opts.Affinity.window)
	if !ok {
		return AffinityNomination{}, nil, nil, nil, false
	}
	scopeTag := ScopeTag(req.KeyID, req.Format, req.Model)
	tip := chain.Tip()
	// 调用方预先算过的查找结果优先：Node 的 session.affinity.lookup 就是同一节流手段，
	// 目的是让守卫阶段做过的 Redis 往返不在选路阶段重做一遍。
	lookup := req.AffinityLookup
	if lookup == nil {
		lookup, ok = s.opts.Affinity.Lookup(ctx, scopeTag, chain.DeepestFirst())
		if !ok {
			// 查找不可用（典型是 Redis 故障）：提名与写回都做不了，但身份事实照给
			// （指纹退到链尾）——Node 的 session.affinity 此时同样存在。
			return AffinityNomination{}, nil, nil, affinityIdentityOf(scopeTag, tip, nil), false
		}
	}
	identity := affinityIdentityOf(scopeTag, tip, lookup)
	// 写回事实在选路时就固化：scope 与 tip 是本次请求的事实，终态层无从重建。
	writeback := &AffinityWriteback{
		store:    s.opts.Affinity,
		ScopeTag: scopeTag,
		TipFP:    tip.FP,
		TipDepth: tip.Depth,
		// tip 的前缀字节数供 F3b 缓存模拟列粗估理论可命中缓存量（Node gate.ts 同源）。
		TipPrefixBytes: tip.PrefixBytes,
		IdentityFP:     lookup.IdentityFP,
		Generation:     lookup.Generation,
	}
	if lookup.Hint == nil {
		// 未命中：不带提名，但仍把 lookup 与写回事实交回调用方——
		// 终态写回需要它的 identity 与 generation。
		return AffinityNomination{}, lookup, writeback, identity, false
	}
	hint := lookup.Hint

	provider, err := s.opts.Source.Provider(ctx, hint.ProviderID)
	if err != nil || provider == nil {
		// 读该行失败与「该行真的不存在」处置相反：前者是**临时**原因（DB 抖动/超时/快照装载
		// 失败），tip 绑定必须保留、本次成功终态不得改写；后者（ErrProviderNotFound，或源里
		// 没有这一家且不报错）是结构性失效，旧绑定已死，允许改写。
		//
		// 为何必须分开：terminal 成功侧对非 nil 的 writeback 无条件调 RecordWinner（写 tip
		// 绑定），压成一支的后果是一次瞬时读错就把会话永久改粘到备用，直到 TTL。
		lookupFailed := err != nil && !errors.Is(err, ErrProviderNotFound)
		writeback.Bypass = affinityBypass(filtered, hint.ProviderID, lookupFailed)
		return AffinityNomination{}, lookup, writeback, identity, false
	}
	if !s.validateAffinityCandidate(ctx, *provider, req, excluded) {
		// 校验拒绝同样要分流：临时类（熔断、会话冷却、活动时段、限额、本次已试过）按设计稿
		// §4「跳过该 provider…不清空绑定；待恢复后仍粘回去」须保绑定；结构性失效（停用、
		// 模型/端点不兼容、名单）允许改写。分界表只有一处（transientRejection）。
		writeback.Bypass = affinityBypass(filtered, hint.ProviderID, false)
		return AffinityNomination{}, lookup, writeback, identity, false
	}
	// 提名被接受才登记命中指纹与提名者：墓碑只对提名者写。
	writeback.MatchedFP = hint.MatchedFP
	writeback.NominatedProviderID = provider.ID

	nomination := AffinityNomination{
		Provider:   *provider,
		Hint:       *hint,
		ScopeTag:   scopeTag,
		TipFP:      chain.Tip().FP,
		IdentityFP: lookup.IdentityFP,
		Generation: lookup.Generation,
	}
	if boundary, found := chain.BoundaryOf(hint.MatchedFP); found {
		nomination.MatchedDepth = boundary.Depth
		nomination.MatchedPrefixByte = boundary.PrefixBytes
	}
	return nomination, lookup, writeback, identity, true
}

// affinityIdentityOf 按 Node 的 `identityFp ?? matchedFp ?? tipFp` 取身份指纹。
func affinityIdentityOf(scopeTag string, tip FingerprintBoundary, lookup *AffinityLookup) *AffinityIdentity {
	fingerprint := tip.FP
	if lookup != nil {
		if lookup.IdentityFP != "" {
			fingerprint = lookup.IdentityFP
		} else if lookup.Hint != nil && lookup.Hint.MatchedFP != "" {
			fingerprint = lookup.Hint.MatchedFP
		}
	}
	return &AffinityIdentity{ScopeTag: scopeTag, Fingerprint: fingerprint}
}

// validateAffinityCandidate 复刻 validateAffinityCandidate 的硬校验面。
// 与 Node 的差异：调度窗口、客户端限制、金额限额三项走 Gates（nil 即不判定），
// 理由同 filter.go 的说明。
func (s *Selector) validateAffinityCandidate(
	ctx context.Context,
	p Provider,
	req Request,
	excluded map[int64]bool,
) bool {
	if !p.IsEnabled || p.DisableSessionReuse || excluded[p.ID] {
		return false
	}
	// 尊重供应商的会话粘性 opt-out：亲和与会话复用同属粘性机制。
	if req.Group != "" && !checkProviderGroupMatch(p.GroupTag, req.Group) {
		return false
	}
	if blocked, _ := s.basicFilterRejection(ctx, p, filterInput{
		requestedModel: req.Model,
		format:         req.Format,
		group:          req.Group,
		excluded:       map[int64]bool{},
		endpoints:      req.Endpoint,
		endpointGate:   s.opts.EndpointGate,
		gates:          s.opts.Gates,
	}); blocked {
		return false
	}
	if blocked, _ := s.healthRejection(ctx, p); blocked {
		return false
	}
	// 会话级低速冷却同样要拦住**亲和提名**。
	//
	// 为何不能只靠过滤阶段：亲和一旦命中就短路整场选路（`resolve` 在过滤之后直接返回提名者），
	// 过滤只会从候选人集里去掉它，而提名者并不需要留在集里就能被选中（validateAffinityCandidate
	// 自己逐条重做硬校验）。若这里不判，被粘住的会话会一次次绕过冷却撞回同一家慢渠道——
	// 而「已粘会话如何逃脱」正是本机制要解决的问题。
	if req.SessionID != "" && req.KeyID != 0 && s.opts.SlowRate != nil {
		if s.opts.SlowRate.InCooldown(ctx, req.SessionID, req.KeyID, []Provider{p})[p.ID] {
			return false
		}
	}
	return true
}

// nowOr returns the injected clock or time.Now.
func (s *Selector) nowOr() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}
