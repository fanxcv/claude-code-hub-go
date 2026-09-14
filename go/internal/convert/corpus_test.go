package convert

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// 语料由 scripts/export-conformance-corpus.ts 从 TS 实现反推生成，见
// tests/load/protocol-conformance/README.md。本文件是 Go 侧逐字节验收入口。

const corpusDir = "../../../tests/load/protocol-conformance/corpus"

type corpusCase struct {
	ID                string            `json:"id"`
	FamilyFrom        string            `json:"familyFrom"`
	FamilyTo          string            `json:"familyTo"`
	Kind              string            `json:"kind"`
	Input             string            `json:"input"`
	Expected          *string           `json:"expected"`
	Passthrough       bool              `json:"passthrough"`
	ExpectError       bool              `json:"expectError"`
	ToolNameRestore   map[string]string `json:"toolNameRestore"`
	ClientPathname    string            `json:"clientPathname"`
	RewrittenPathname *string           `json:"rewrittenPathname"`

	// selection.json / paths.json 专用
	ClientFormat      string          `json:"clientFormat"`
	ProviderType      string          `json:"providerType"`
	ConversionEnabled bool            `json:"conversionEnabled"`
	Compat            ProtocolCompat  `json:"compat"`
	TargetProtocol    string          `json:"targetProtocol"`
	Plan              json.RawMessage `json:"plan"`
}

type corpusFile struct {
	CaseCount int          `json:"caseCount"`
	Cases     []corpusCase `json:"cases"`
}

func loadCorpus(t *testing.T, name string) corpusFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusDir, name))
	if err != nil {
		t.Fatalf("读取语料 %s 失败: %v", name, err)
	}
	var file corpusFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("解析语料 %s 失败: %v", name, err)
	}
	if file.CaseCount != len(file.Cases) {
		t.Fatalf("语料 %s 声明 %d 条，实际 %d 条", name, file.CaseCount, len(file.Cases))
	}
	return file
}

func mustParsePayload(t *testing.T, raw string) *Value {
	t.Helper()
	value, err := ParseJSON([]byte(raw))
	if err != nil {
		t.Fatalf("解析载荷失败: %v", err)
	}
	return value
}

func protocolOfFamily(t *testing.T, family string) WireProtocol {
	t.Helper()
	switch WireProtocol(family) {
	case ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIResponses:
		return WireProtocol(family)
	default:
		t.Fatalf("未知协议线 %q", family)
		return ""
	}
}

// caseModel 复刻语料 harness 的取法：model 取输入体的 model，非字符串则视为空。
func caseModel(source *Value) string {
	model, ok := stringField(source, "model")
	if !ok {
		return ""
	}
	return model
}

func restoreHook(table map[string]string) func(string) string {
	if len(table) == 0 {
		return nil
	}
	return func(name string) string {
		if original, ok := table[name]; ok {
			return original
		}
		return name
	}
}

// byteDiff 定位首个不同字节并给出两侧上下文，避免只报「不一致」。
func byteDiff(got string, want string) string {
	limit := len(got)
	if len(want) < limit {
		limit = len(want)
	}
	offset := 0
	for offset < limit && got[offset] == want[offset] {
		offset++
	}
	window := func(text string) string {
		start := offset - 60
		if start < 0 {
			start = 0
		}
		end := offset + 150
		if end > len(text) {
			end = len(text)
		}
		return text[start:end]
	}
	reason := "字节不一致"
	switch {
	case len(got) < len(want):
		reason = "输出短于期望"
	case len(got) > len(want):
		reason = "输出长于期望"
	}
	return fmt.Sprintf("%s：首个差异在字节 %d\n got: ...%s\nwant: ...%s\n完整输出\n got: %s\nwant: %s",
		reason, offset, window(got), window(want), got, want)
}

// intentionalDivergence 描述一处「Go 有意超出 Node」的格位。
//
// 每一种分歧都有**可判定的断言**（比单纯放宽强得多）：
//   - kindAddedField：把派生字段从 got 里摘掉后必须与 Node 期望**逐字节相等**——
//     即「只多了这一个字段，没有顺手改动其它字节」；
//   - kindOmitEmptyContentMessages：把 Node 期望里 `content: []` 的消息摘掉后必须与 got
//     逐字节相等——即「只少了那几条空消息，其余一字未改」。
type intentionalDivergence struct {
	Kind   string
	Path   []string // kindAddedField：派生字段路径
	Value  string   // kindAddedField：该字段期望值（字符串）
	Reason string
}

const (
	kindAddedField               = "added_field"
	kindOmitEmptyContentMessages = "omit_empty_content_messages"
	kindReasoningAsContent       = "reasoning_as_content"
	// kindReasoningMergedIntoFollowingAssistant：Go 把 reasoning 项的思考**并入紧随其后的
	// assistant 消息**（Node 会拆成两条）。该分歧**只为满足上游硬规则**，理由见登记处注释。
	kindReasoningMergedIntoFollowingAssistant = "reasoning_merged_into_following_assistant"
)

// intentionalRequestDivergences 是**逐条登记**的有意分歧。
//
// 背景：语料的 expected 由 `scripts/export-conformance-corpus.ts` 调用 **Node 实现**生成，
// 是 Node 行为的忠实快照——所以**不修改它**（改了就读不出「Node 到底怎么做的」）。
// 以下三处 Go 有意不同，且理由都能落到上游行为上：
//  1. 思考强度载体换算（见 internal/convert/thinking.go）；
//  2. 跨线丢弃后**空 content 的消息不再输出**（Node 会输出 `content: []`，Anthropic /
//     Chat 都不接受空内容数组，上游会直接 400）；两边都记损，差别只在「要不要发一条注定被拒的消息」；
//  3. 仅思考的 assistant 消息**把思考写进 content**（Node 写 `content: ""`）。两类上游的
//     要求互斥，只有两处都写才能同时满足：严格上游把空串也判为缺失，报 `Invalid assistant
//     message: content or tool_calls`；thinking 模式的 chat 上游反向要求历史 assistant 轮次
//     必须带回思考，缺失报 `reasoning_content must be passed back`。
var intentionalRequestDivergences = map[string]intentionalDivergence{
	"request.anthropic-messages.system-image-thinking.to.openai-chat": {
		Kind:   kindAddedField,
		Path:   []string{"reasoning_effort"},
		Value:  "low",
		Reason: "Node 丢弃 thinking.budget_tokens；Go 按阈值反查为等级（1024 → low）",
	},
	"request.anthropic-messages.system-image-thinking.to.openai-responses": {
		Kind:   kindAddedField,
		Path:   []string{"reasoning", "effort"},
		Value:  "low",
		Reason: "同上，落至 responses 的 reasoning.effort",
	},
	"request.openai-responses.mcp-items.to.anthropic-messages": {
		Kind: kindOmitEmptyContentMessages,
		Reason: "responses 的 mcp_call / web_search_call / item_reference 项在 anthropic 线无表示：" +
			"Node 把整项换成一个空 content 消息（`content: []`，上游会拒），Go 省略该消息并记 `mcp.tool` 等专用损失",
	},
	"request.openai-responses.mcp-items.to.openai-chat": {
		Kind:   kindOmitEmptyContentMessages,
		Reason: "同上（chat 线的承载位是 assistant 消息上的 tool_calls / 文本，二者皆无对应表示）",
	},
	"request.openai-responses.reasoning-sources.to.openai-chat": {
		Kind: kindReasoningMergedIntoFollowingAssistant,
		Reason: "chat 线上 reasoning_content 是本条 assistant 消息**自身**的字段：Node 把 reasoning 项单独" +
			"发成一条 `{assistant, content:\"\", reasoning_content:…}`，于是紧随其后的那条真正承载本轮" +
			"输出的消息（正文或 tool_calls）**没有** reasoning_content。上游规则（真上游二分实测，见" +
			"responses-to-chat-conversion-gaps.md §3.1）：带 tools 且开启思考模式时，历史里**每个** assistant" +
			"轮次都必须带 reasoning_content——Missing 即 400 `The reasoning_content in the thinking mode" +
			" must be passed back to the API.`（生产 OpenCode X Chat 实测，该 400 属 CategoryProviderError、" +
			"计熔断器，故我方形状缺陷会把供应商推进熔断）。Go 改为并入：一条 assistant 消息同时带" +
			"reasoning_content 与本轮正文/tool_calls，两侧语义等价而上游不再报错",
	},
}

// valueAtPath 按路径取嵌套成员。
func valueAtPath(root *Value, path []string) (*Value, bool) {
	current := root
	for _, key := range path {
		if current == nil {
			return nil, false
		}
		next, present := current.Get(key)
		if !present {
			return nil, false
		}
		current = next
	}
	return current, true
}

// removePathPruningEmpty 删掉嵌套成员；若删除后某层父对象变为空，则**一并剪掉**。
//
// 为何要剪空：Node 在没有任何思考字段时**根本不输出该容器**。Go 因为多了派生字段而创建了
// `reasoning`（或 `thinking`）容器；摘掉派生字段后必须把空容器也摘掉，才能与 Node 的期望逐字节对齐
// （否则断言会因 `"reasoning":{}` 这种残留而失败，而那不是真正的分歧）。
func removePathPruningEmpty(root *Value, path []string) {
	if len(path) == 0 {
		return
	}
	parent, ok := valueAtPath(root, path[:len(path)-1])
	if !ok {
		return
	}
	parent.Delete(path[len(path)-1])
	// 自底向上剪空容器（只处理对象；数组留给调用方判定）。
	for depth := len(path) - 1; depth >= 1; depth-- {
		container, found := valueAtPath(root, path[:depth-1])
		if !found {
			return
		}
		child, found := container.Get(path[depth-1])
		if !found || child == nil || !child.IsObject() || len(child.Members()) > 0 {
			return
		}
		container.Delete(path[depth-1])
	}
}

// assertIntentionalDivergence 按登记的类别断言「只差登记的那一处」。
func assertIntentionalDivergence(t *testing.T, got, want string, divergence intentionalDivergence) {
	t.Helper()
	switch divergence.Kind {
	case kindAddedField:
		assertAddedFieldDivergence(t, got, want, divergence)
	case kindOmitEmptyContentMessages:
		assertOmittedEmptyContentMessages(t, got, want, divergence)
	case kindReasoningAsContent:
		assertReasoningAsContentDivergence(t, got, want, divergence)
	case kindReasoningMergedIntoFollowingAssistant:
		assertReasoningMergedDivergence(t, got, want, divergence)
	default:
		t.Fatalf("有意分歧：未登记的类别 %q（原因：%s）", divergence.Kind, divergence.Reason)
	}
}

// assertAddedFieldDivergence 断言「恰好只多了这一个派生字段」。
func assertAddedFieldDivergence(t *testing.T, got, want string, divergence intentionalDivergence) {
	t.Helper()
	value := mustParsePayload(t, got)
	node, present := valueAtPath(value, divergence.Path)
	if !present {
		t.Fatalf("有意分歧：got 里没有派生字段 %v（原因：%s）\n got: %s",
			divergence.Path, divergence.Reason, got)
	}
	actual, isString := node.String()
	if !isString || actual != divergence.Value {
		t.Fatalf("有意分歧：派生字段 %v 的值 = %q（是字符串：%v），期望 %q（原因：%s）",
			divergence.Path, actual, isString, divergence.Value, divergence.Reason)
	}
	removePathPruningEmpty(value, divergence.Path)
	stripped := value.MarshalCompact()
	if stripped != want {
		t.Fatalf("有意分歧：摘掉派生字段后仍与 Node 期望不一致（说明还顺带改了其它字节）\n 摘掉后: %s\n want: %s",
			stripped, want)
	}
}

// assertOmittedEmptyContentMessages 断言：Node 期望里那些 `content: []` 的消息在 Go 输出里被省略，
// 且**摘掉它们之后两侧逐字节相等**（证明除此之外没有任何其它差异）。
// revertReasoningAsContent 是 kindReasoningAsContent 的归一化：把 Go 填进 content 的思考文本
// 还原成角串（仅限「无 tool_calls 且带非空 reasoning_content 的 assistant 消息」），
// 返回还原条数。
func revertReasoningAsContent(root *Value) (*Value, int) {
	messages, present := root.Get("messages")
	if !present || messages == nil || !messages.IsArray() {
		return root, 0
	}
	reverted := 0
	for _, message := range messages.Items() {
		if role, _ := stringField(message, "role"); role != "assistant" {
			continue
		}
		if _, hasTools := message.Get("tool_calls"); hasTools {
			continue
		}
		reasoning, hasReasoning := message.Get("reasoning_content")
		if !hasReasoning || reasoning == nil {
			continue
		}
		if reasoningText, isString := reasoning.String(); !isString || reasoningText == "" {
			continue
		}
		content, hasContent := message.Get("content")
		if !hasContent || content == nil {
			continue
		}
		if contentText, isString := content.String(); !isString || contentText == "" {
			continue
		}
		message.Set("content", NewString(""))
		reverted++
	}
	return root, reverted
}

// assertReasoningAsContentDivergence 断言「只把空 content 换成了思考文本，其余一字未改」。
func assertReasoningAsContentDivergence(t *testing.T, got, want string, divergence intentionalDivergence) {
	t.Helper()
	normalized, reverted := revertReasoningAsContent(mustParsePayload(t, got))
	if reverted == 0 {
		t.Fatalf("有意分歧：Go 输出里没有「仅思考的 assistant 且 content 非空」的消息，该登记已失效（原因：%s）\n got: %s",
			divergence.Reason, got)
	}
	if revertedBytes := normalized.MarshalCompact(); revertedBytes != want {
		t.Fatalf("有意分歧：把 %d 条消息的 content 还原为空串后仍与 Node 期望不一致（说明还顺带改了别的字节）\n 还原后: %s\n want: %s",
			reverted, revertedBytes, want)
	}
}

// revertReasoningMergedIntoFollowingAssistant 是 kindReasoningMergedIntoFollowingAssistant 的归一化：
//
// 把 Go 合并成一条的「思考 + 本轮输出」拆回 Node 的两条（先拆出 content 为角串的仅思考消息，
// 再留下摘掉 reasoning_content 的本轮输出），返回拆出的条数。
//
// 判据收得很紧：只拆「带非空 reasoning_content，且另有正文或 tool_calls」的 assistant 消息；
// 「思考文本被写进 content」（仅思考、无正文无工具）那类**不在这里拆**——它由另一条登记负责。
// 于是断言恰好等价于「两侧只差这一处合并」。
func revertReasoningMergedIntoFollowingAssistant(root *Value) (*Value, int) {
	messages, present := root.Get("messages")
	if !present || messages == nil || !messages.IsArray() {
		return root, 0
	}
	rebuilt := []*Value{}
	merged := 0
	for _, message := range messages.Items() {
		if role, _ := stringField(message, "role"); role != "assistant" {
			rebuilt = append(rebuilt, message)
			continue
		}
		reasoning, hasReasoning := message.Get("reasoning_content")
		if !hasReasoning || reasoning == nil {
			rebuilt = append(rebuilt, message)
			continue
		}
		reasoningText, isString := reasoning.String()
		if !isString || reasoningText == "" {
			rebuilt = append(rebuilt, message)
			continue
		}
		_, hasTools := message.Get("tool_calls")
		contentText := ""
		contentIsString := false
		if content, hasContent := message.Get("content"); hasContent && content != nil {
			contentText, contentIsString = content.String()
		}
		// 「思考被写进 content」的仅思考消息：不属合并，交给 kindReasoningAsContent 那条登记。
		if !hasTools && contentText == reasoningText {
			rebuilt = append(rebuilt, message)
			continue
		}
		if !hasTools && !(contentIsString && contentText != "") {
			rebuilt = append(rebuilt, message)
			continue
		}
		rebuilt = append(rebuilt, NewObject().
			Set("role", NewString("assistant")).
			Set("content", NewString("")).
			Set("reasoning_content", NewString(reasoningText)))
		output := message
		output.Delete("reasoning_content")
		rebuilt = append(rebuilt, output)
		merged++
	}
	if merged == 0 {
		return root, 0
	}
	root.Set("messages", NewArray(rebuilt...))
	return root, merged
}

// assertReasoningMergedDivergence 断言「只把思考与紧随的本轮输出合并成了一条，其余一字未改」。
func assertReasoningMergedDivergence(t *testing.T, got, want string, divergence intentionalDivergence) {
	t.Helper()
	normalized, merged := revertReasoningMergedIntoFollowingAssistant(mustParsePayload(t, got))
	if merged == 0 {
		t.Fatalf("有意分歧：Go 输出里没有「思考与本轮输出合为一条」的消息，该登记已失效（原因：%s）\n got: %s",
			divergence.Reason, got)
	}
	if reverted := normalized.MarshalCompact(); reverted != want {
		t.Fatalf("有意分歧：把 %d 条合并消息拆回 Node 的两条后仍与 Node 期望不一致（说明还顺带改了别的字节）\n 拆后: %s\n want: %s",
			merged, reverted, want)
	}
}

func assertOmittedEmptyContentMessages(t *testing.T, got, want string, divergence intentionalDivergence) {
	t.Helper()
	normalized, dropped := dropEmptyContentMessages(mustParsePayload(t, want))
	if dropped == 0 {
		t.Fatalf("有意分歧：Node 期望里没有 content 为空的消息，该登记已失效（原因：%s）\n want: %s",
			divergence.Reason, want)
	}
	if stripped := normalized.MarshalCompact(); stripped != got {
		t.Fatalf("有意分歧：摘掉 %d 条空内容消息后仍与 Go 输出不一致（说明还顺带改了别的字节）\n 摘后: %s\n got: %s",
			dropped, stripped, got)
	}
}

// dropEmptyContentMessages 把顶层 `messages` 里 content 为空数组的消息摘掉，返回新树与摘掉的条数。
func dropEmptyContentMessages(root *Value) (*Value, int) {
	messages, present := root.Get("messages")
	if !present || messages == nil || !messages.IsArray() {
		return root, 0
	}
	kept := []*Value{}
	dropped := 0
	for _, message := range messages.Items() {
		if content, hasContent := message.Get("content"); hasContent &&
			content != nil && content.IsArray() && len(content.Items()) == 0 {
			dropped++
			continue
		}
		kept = append(kept, message)
	}
	if dropped == 0 {
		return root, 0
	}
	root.Set("messages", NewArray(kept...))
	return root, dropped
}

func TestCorpusRequests(t *testing.T) {
	file := loadCorpus(t, "requests.json")
	converted := 0
	passthrough := 0
	for _, testCase := range file.Cases {
		if testCase.Passthrough {
			// native 直通由路由层绕过转换层，本层不参与；只统计条数，防止语料悄悄丢掉该路径。
			passthrough++
			continue
		}
		t.Run(testCase.ID, func(t *testing.T) {
			source := mustParsePayload(t, testCase.Input)
			sourceProtocol := protocolOfFamily(t, testCase.FamilyFrom)
			target := protocolOfFamily(t, testCase.FamilyTo)
			ctx := ConvertCtx{
				ClientFormat:   clientFormatOfProtocol(target),
				TargetProto:    target,
				Model:          caseModel(source),
				Stream:         responsesIsStream(source),
				ToWireToolName: NormalizeToolName,
			}
			decoded, ok := DecodeRequest(sourceProtocol, source, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无解码器", sourceProtocol)
			}
			encoded, ok := EncodeRequest(target, decoded.Value, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无编码器", target)
			}
			got := encoded.Body.MarshalCompact()
			if testCase.Expected == nil {
				t.Fatalf("用例缺少 expected")
			}
			if got != *testCase.Expected {
				if divergence, isRegistered := intentionalRequestDivergences[testCase.ID]; isRegistered {
					assertIntentionalDivergence(t, got, *testCase.Expected, divergence)
					return
				}
				t.Fatal(byteDiff(got, *testCase.Expected))
			}
		})
		converted++
	}
	if converted == 0 {
		t.Fatal("请求侧转换用例为空")
	}
	if passthrough == 0 {
		t.Fatal("请求侧直通用例为空")
	}
}

func TestCorpusResponses(t *testing.T) {
	file := loadCorpus(t, "responses.json")
	converted := 0
	passthrough := 0
	for _, testCase := range file.Cases {
		if testCase.Passthrough {
			passthrough++
			continue
		}
		t.Run(testCase.ID, func(t *testing.T) {
			source := mustParsePayload(t, testCase.Input)
			sourceProtocol := protocolOfFamily(t, testCase.FamilyTo)
			target := protocolOfFamily(t, testCase.FamilyFrom)
			ctx := ConvertCtx{
				ClientFormat:     clientFormatOfProtocol(target),
				TargetProto:      target,
				FromWireToolName: restoreHook(testCase.ToolNameRestore),
			}
			decoded, ok := DecodeResponse(sourceProtocol, source, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无响应解码器", sourceProtocol)
			}
			encoded, ok := EncodeResponse(target, decoded.Value, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无响应编码器", target)
			}
			got := encoded.Body.MarshalCompact()
			if testCase.Expected == nil {
				t.Fatalf("用例缺少 expected")
			}
			if got != *testCase.Expected {
				t.Fatal(byteDiff(got, *testCase.Expected))
			}
		})
		converted++
	}
	if converted == 0 {
		t.Fatal("响应侧转换用例为空")
	}
	if passthrough == 0 {
		t.Fatal("响应侧直通用例为空")
	}
}

func TestCorpusSelection(t *testing.T) {
	file := loadCorpus(t, "selection.json")
	for _, testCase := range file.Cases {
		t.Run(testCase.ID, func(t *testing.T) {
			clientFormat := ClientFormat(testCase.ClientFormat)
			providerType := ProviderType(testCase.ProviderType)
			got := ResolveProtocolCompat(clientFormat, providerType, testCase.ConversionEnabled)
			if got != testCase.Compat {
				t.Fatalf("compat 不一致: got %s want %s", got, testCase.Compat)
			}
			plan := PlanConversion(clientFormat, providerType, testCase.ConversionEnabled)
			if string(testCase.Plan) == "null" || len(testCase.Plan) == 0 {
				if plan != nil {
					t.Fatalf("expected plan=null, got %+v", plan)
				}
				return
			}
			if plan == nil {
				t.Fatalf("expected plan=%s, got null", testCase.Plan)
			}
			var expected struct {
				ClientProtocol string `json:"clientProtocol"`
				TargetProtocol string `json:"targetProtocol"`
			}
			if err := json.Unmarshal(testCase.Plan, &expected); err != nil {
				t.Fatalf("解析 plan 失败: %v", err)
			}
			if string(plan.ClientProtocol) != expected.ClientProtocol ||
				string(plan.TargetProtocol) != expected.TargetProtocol {
				t.Fatalf("plan 不一致: got %+v want %s", plan, testCase.Plan)
			}
		})
	}
}

func TestCorpusPaths(t *testing.T) {
	file := loadCorpus(t, "paths.json")
	for _, testCase := range file.Cases {
		t.Run(testCase.ID, func(t *testing.T) {
			got, ok := ResolveUpstreamPath(WireProtocol(testCase.TargetProtocol), testCase.ClientPathname)
			if testCase.Expected == nil {
				if ok {
					t.Fatalf("expected 无映射, got %q", got)
				}
				return
			}
			if !ok {
				t.Fatalf("expected %q, got 无映射", *testCase.Expected)
			}
			if got != *testCase.Expected {
				t.Fatalf("路径不一致: got %q want %q", got, *testCase.Expected)
			}
		})
	}
}

// clientFormatOfProtocol 把协议线映射回路由层客户端格式，仅用于填充 ctx。
func clientFormatOfProtocol(protocol WireProtocol) ClientFormat {
	switch protocol {
	case ProtocolAnthropicMessages:
		return FormatClaude
	case ProtocolOpenAIResponses:
		return FormatResponse
	default:
		return FormatOpenAI
	}
}
