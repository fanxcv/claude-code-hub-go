package convert

import (
	"slices"
	"strings"
	"testing"
)

// 本文件钉住「请求侧高层字段跨线无承载位」的两条处置：
//   - 状态型字段（previous_response_id / conversation / store:true）：只**判定**冲突，由
//     forward.BuildPlan 据此 fail-closed（见 forward/plan_test.go 的对应用例）；
//   - 约束型字段（response_format / text / store:false）：记损，不阻断；
//   - 缓存路由键（prompt_cache_key）：能带就带（chat/responses 两线原样回写），anthropic 线才记损。
//
// 为什么要钉：这些字段此前落进 passthrough 逃生舱后**没有任何消费者**（request 侧 passthrough
// 只在目标线取用，而跨线时目标线与来源线不同），于是 Node 与本仓都表现为静默丢弃——客户端
// 以为命中了前缀缓存、以为拿到的是 json_schema 约束下的输出，实际都不是，且报告里查不到痕迹。

func TestStatefulConversionConflict(t *testing.T) {
	cases := []struct {
		name string
		wire WireProtocol
		body string
		want string
	}{
		{"previous_response_id", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","previous_response_id":"resp_123"}`, "previous_response_id"},
		{"conversation 字符串", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","conversation":"conv_1"}`, "conversation"},
		{"conversation 对象", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","conversation":{"id":"conv_1"}}`, "conversation"},
		{"store:true", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","store":true}`, "store"},
		{"store:false 不算冲突", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","store":false}`, ""},
		{"无状态字段", ProtocolOpenAIResponses, `{"model":"gpt-5","input":"hi"}`, ""},
		{"空串视为未给出", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","previous_response_id":""}`, ""},
		{"null 视为未给出", ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":"hi","conversation":null}`, ""},
		// 其它方言里出现同名字段只是杂项键：不能因一个杂项键把请求打回（错误体形状也不对）。
		{"chat 线上的同名字段不判冲突", ProtocolOpenAIChat,
			`{"model":"gpt-5","messages":[],"previous_response_id":"resp_123"}`, ""},
		{"anthropic 线上的同名字段不判冲突", ProtocolAnthropicMessages,
			`{"model":"claude","messages":[],"conversation":"conv_1"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StatefulConversionConflict(tc.wire, mustParsePayload(t, tc.body))
			if got != tc.want {
				t.Fatalf("冲突字段应为 %q，收到 %q", tc.want, got)
			}
		})
	}
}

// TestForeignDroppableFieldsRecordedAsLoss 走真实编解码链，断言跨线丢弃的约束型字段都进了损失台账。
func TestForeignDroppableFieldsRecordedAsLoss(t *testing.T) {
	cases := []struct {
		name     string
		source   WireProtocol
		target   WireProtocol
		body     string
		wantLoss []string
	}{
		{
			// 结构化输出不再记损：chat 的 response_format 与 responses 的 text.format 现已互转
			// （见 structured_output.go）；缓存路由键由枢纽原样回写，store:false 与目标线同义。
			// 本格必须为**空**：参数化记过的旧行为（整键 text 丢一条）正是本 lane 要消除的。
			name:   "responses→chat：结构化输出与缓存键都改为转发（不再记损）",
			source: ProtocolOpenAIResponses,
			target: ProtocolOpenAIChat,
			body: `{"model":"gpt-5","input":"hi","prompt_cache_key":"k1",` +
				`"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}},` +
				`"store":false}`,
			wantLoss: []string{},
		},
		{
			// store:false 是多数 Responses 客户端的默认值，与「目标线不落库」等价 ⇒ 不记损。
			// 它曾是每个转换请求恒定多出的一条（生产实测每行 +1），故这一格必须为**空**：
			// wantLoss 为空时下面会跑「不多不少」断言，凭空补条目会直接转红。
			name:     "responses→chat：store:false 不算损失",
			source:   ProtocolOpenAIResponses,
			target:   ProtocolOpenAIChat,
			body:     `{"model":"gpt-5","input":"hi","store":false}`,
			wantLoss: []string{},
		},
		{
			// store:true 是真实要求落库：responses 源线由 fail-closed 拦（不会走到记损），
			// 其它源线仍必须记（目标线不落库，而状态型冲突只对 responses 源线判定）。
			name:     "chat→anthropic：store:true 仍记损",
			source:   ProtocolOpenAIChat,
			target:   ProtocolAnthropicMessages,
			body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"store":true}`,
			wantLoss: []string{LossStoreFlag + "/dropped/store"},
		},
		{
			// 两线互转成立 ⇒ 不再记损（json_object 是两线同形的三种 type 之一）。
			name:     "chat→responses：response_format 改为转发",
			source:   ProtocolOpenAIChat,
			target:   ProtocolOpenAIResponses,
			body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
			wantLoss: []string{},
		},
		{
			// anthropic 线没有「约束本次输出」的请求字段：那一侧仍必须留痕。
			name:   "responses→anthropic：text.controls 仍记损",
			source: ProtocolOpenAIResponses,
			target: ProtocolAnthropicMessages,
			body: `{"model":"gpt-5","input":"hi",` +
				`"text":{"format":{"type":"json_schema","name":"r","schema":{"type":"object"}}}}`,
			wantLoss: []string{LossTextControls + "/dropped/text"},
		},
		{
			name:     "chat→anthropic：response_format 同样记损",
			source:   ProtocolOpenAIChat,
			target:   ProtocolAnthropicMessages,
			body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
			wantLoss: []string{LossResponseFormat + "/dropped/response_format"},
		},
		{
			name:     "无这些字段时不记损（不多不少）",
			source:   ProtocolOpenAIChat,
			target:   ProtocolAnthropicMessages,
			body:     `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`,
			wantLoss: []string{},
		},
		{
			name:   "null / 空值不算给出",
			source: ProtocolOpenAIResponses,
			target: ProtocolOpenAIChat,
			body:   `{"model":"gpt-5","input":"hi","prompt_cache_key":null,"text":{},"store":null}`,
			// text 是空对象，isMeaningful 判为无内容；另外两个是 null。
			wantLoss: []string{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := mustParsePayload(t, tc.body)
			ctx := ConvertCtx{
				ClientFormat:   clientFormatOfProtocol(tc.source),
				TargetProto:    tc.target,
				Model:          "m",
				ToWireToolName: NormalizeToolName,
			}
			decoded, ok := DecodeRequest(tc.source, source, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无解码器", tc.source)
			}
			encoded, ok := EncodeRequest(tc.target, decoded.Value, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无编码器", tc.target)
			}

			got := []string{}
			for _, entry := range encoded.Loss.Entries {
				got = append(got, entry.Capability+"/"+string(entry.Action)+"/"+entry.Detail)
			}
			for _, want := range tc.wantLoss {
				if !slices.Contains(got, want) {
					t.Fatalf("缺少损失条目 %s；实际：%v", want, got)
				}
			}
			// 「不多」也钉住：无这些字段时不得凭空补条目（多余条目会让台账失去可读性）。
			// 只查本文件新增的四类：同一份报告里还有其它来源的损失（如 max_tokens.defaulted）。
			if len(tc.wantLoss) == 0 {
				for _, entry := range encoded.Loss.Entries {
					for _, class := range foreignDroppableClasses {
						if entry.Capability == class {
							t.Fatalf("不该有 %s 类损失，实际：%v", class, got)
						}
					}
				}
			}
		})
	}
}

// TestForeignDroppableRequestKeysRecordedAsLoss 钉住「请求级开关与采样参数跨线整键丢」必须留痕。
//
// 这些键在枢纽里没有承载位，此前落进 passthrough 逃生舱后无人消费，于是跨线即静默消失：客户端
// 以为限定了输出上限、以为是后台任务，实际都不是，台账里也查不到痕迹。本用例对每条键断言
// 「有值恰好记一条、空值/null 不记」——后一半与 store:false 同因：默认值不承载约束，记了就是
// 每个请求恒定一条噪声。
func TestForeignDroppableRequestKeysRecordedAsLoss(t *testing.T) {
	// 源线取 chat 与 responses 两向：这些键在两条源线的 knownKeys 之外，故都落 passthrough；
	// 任一目标线不同即跨线丢。只钉一个源线会漏掉「同一键在另一源线上已被承载」的情况。
	values := []struct {
		key   string
		value string
	}{
		{"metadata", `{"user_id":"u1"}`},
		{"client_metadata", `{"trace":"t1"}`},
		{"include", `["reasoning.encrypted_content"]`},
		{"truncation", `"auto"`},
		{"background", `true`},
		{"top_logprobs", `3`},
		{"presence_penalty", `0.5`},
		{"frequency_penalty", `0.25`},
		{"user", `"u_1"`},
		{"service_tier", `"priority"`},
		{"safety_identifier", `"s_1"`},
	}

	// 源线到 body 的拼装：responses 用 input，chat 用 messages（两者只差正文骨架）。
	bodies := map[WireProtocol]func(item struct {
		key   string
		value string
	}) string{
		ProtocolOpenAIResponses: func(item struct {
			key   string
			value string
		}) string {
			return `{"model":"gpt-5","input":"hi","` + item.key + `":` + item.value + `}`
		},
		ProtocolOpenAIChat: func(item struct {
			key   string
			value string
		}) string {
			return `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"` +
				item.key + `":` + item.value + `}`
		},
	}

	lossesFor := func(t *testing.T, source WireProtocol, target WireProtocol, body string) []string {
		t.Helper()
		ctx := ConvertCtx{
			ClientFormat:   clientFormatOfProtocol(source),
			TargetProto:    target,
			Model:          "m",
			ToWireToolName: NormalizeToolName,
		}
		decoded, ok := DecodeRequest(source, mustParsePayload(t, body), ctx)
		if !ok {
			t.Fatalf("协议线 %s 无解码器", source)
		}
		encoded, ok := EncodeRequest(target, decoded.Value, ctx)
		if !ok {
			t.Fatalf("协议线 %s 无编码器", target)
		}
		got := []string{}
		for _, entry := range encoded.Loss.Entries {
			got = append(got, entry.Capability+"/"+string(entry.Action)+"/"+entry.Detail)
		}
		return got
	}

	countOf := func(entries []string, detail string) int {
		count := 0
		for _, entry := range entries {
			if strings.HasSuffix(entry, "/"+detail) {
				count++
			}
		}
		return count
	}

	for _, item := range values {
		for source, buildBody := range bodies {
			for _, target := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages, ProtocolOpenAIResponses} {
				if source == target {
					continue
				}
				t.Run(string(source)+"→"+string(target)+"/"+item.key, func(t *testing.T) {
					entries := lossesFor(t, source, target, buildBody(item))
					if countOf(entries, item.key) != 1 {
						t.Fatalf("%s 跨线丢必须恰好记一条，实际 %v", item.key, entries)
					}
				})
			}
		}
	}

	// 空值不承载约束：空串 / 空对象 / 空数组 / null 一律不记（与 store:false 同因）。
	empties := []struct {
		key   string
		value string
	}{
		{"metadata", `{}`},
		{"client_metadata", `{}`},
		{"include", `[]`},
		{"truncation", `""`},
		{"background", `false`},
		{"top_logprobs", `null`},
		{"presence_penalty", `null`},
		{"frequency_penalty", `null`},
		{"user", `""`},
		{"service_tier", `""`},
		{"safety_identifier", `""`},
	}
	for _, item := range empties {
		body := `{"model":"gpt-5","input":"hi","` + item.key + `":` + item.value + `}`
		if entries := lossesFor(t, ProtocolOpenAIResponses, ProtocolOpenAIChat, body); countOf(entries, item.key) != 0 {
			t.Fatalf("%s 为空值时不得记损，实际 %v", item.key, entries)
		}
		chatBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"` +
			item.key + `":` + item.value + `}`
		if entries := lossesFor(t, ProtocolOpenAIChat, ProtocolAnthropicMessages, chatBody); countOf(entries, item.key) != 0 {
			t.Fatalf("%s 为空值时不得记损（chat 源线），实际 %v", item.key, entries)
		}
	}
}

// foreignDroppableClasses 是本文件钉住的四类损失；「不多不少」断言按它过滤，避免误伤其它来源的损失。
var foreignDroppableClasses = []string{
	LossPromptCacheKey, LossResponseFormat, LossTextControls, LossStoreFlag,
}

// TestUnknownForeignFieldsRecordedAsLoss 钉住白名单外的外线顶层字段也必须留痕。
//
// 为何要钉：白名单永远是滞后的那一侧——`container` / `max_tool_calls` 这类键落进 passthrough 后
// 无任何消费者，跨线即零痕迹（客户端以为指定了容器，上游根本没收到）。同时必须钉住「不重复」：
// `prompt_cache_key` 已有承载位、`metadata` 已在白名单，兜底再记一遍就是双计。
func TestUnknownForeignFieldsRecordedAsLoss(t *testing.T) {
	lossesFor := func(t *testing.T, source, target WireProtocol, body string) []string {
		t.Helper()
		ctx := ConvertCtx{
			ClientFormat:   clientFormatOfProtocol(source),
			TargetProto:    target,
			Model:          "m",
			ToWireToolName: NormalizeToolName,
		}
		decoded, ok := DecodeRequest(source, mustParsePayload(t, body), ctx)
		if !ok {
			t.Fatalf("协议线 %s 无解码器", source)
		}
		encoded, ok := EncodeRequest(target, decoded.Value, ctx)
		if !ok {
			t.Fatalf("协议线 %s 无编码器", target)
		}
		got := []string{}
		for _, entry := range encoded.Loss.Entries {
			got = append(got, entry.Capability+"/"+string(entry.Action)+"/"+entry.Detail)
		}
		return got
	}
	countOf := func(entries []string, capability, detail string) int {
		count := 0
		for _, entry := range entries {
			if entry == capability+"/dropped/"+detail {
				count++
			}
		}
		return count
	}

	responsesBody := func(extra string) string { return `{"model":"gpt-5","input":"hi",` + extra + `}` }
	chatBody := func(extra string) string {
		return `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],` + extra + `}`
	}

	// 白名单外的未知键：三个方向各恰好一条。
	for _, item := range []struct {
		name   string
		source WireProtocol
		target WireProtocol
		body   string
		key    string
	}{
		{"responses→chat/container", ProtocolOpenAIResponses, ProtocolOpenAIChat,
			responsesBody(`"container":"cntr_1"`), "container"},
		{"responses→chat/max_tool_calls", ProtocolOpenAIResponses, ProtocolOpenAIChat,
			responsesBody(`"max_tool_calls":3`), "max_tool_calls"},
		{"responses→anthropic/container", ProtocolOpenAIResponses, ProtocolAnthropicMessages,
			responsesBody(`"container":"cntr_1"`), "container"},
		{"chat→responses/container", ProtocolOpenAIChat, ProtocolOpenAIResponses,
			chatBody(`"container":"cntr_1"`), "container"},
	} {
		t.Run(item.name, func(t *testing.T) {
			entries := lossesFor(t, item.source, item.target, item.body)
			if count := countOf(entries, LossUnknownFieldParam, item.key); count != 1 {
				t.Fatalf("%s 必须恰好记一条 %s，实际 %d 条：%v", item.key, LossUnknownFieldParam, count, entries)
			}
		})
	}

	// 已消费的键不得被兜底重复记：prompt_cache_key 进枢纽（chat/responses 回写、anthropic
	// 另有专用类别），metadata 已有白名单条目——两者在兜底那一层都必须零条。
	cacheKeyEntries := lossesFor(t, ProtocolOpenAIResponses, ProtocolOpenAIChat,
		responsesBody(`"prompt_cache_key":"k1"`))
	if count := countOf(cacheKeyEntries, LossUnknownFieldParam, "prompt_cache_key"); count != 0 {
		t.Fatalf("prompt_cache_key 已被枢纽承载，不得兜底记损，实际 %v", cacheKeyEntries)
	}
	for _, entries := range [][]string{
		lossesFor(t, ProtocolOpenAIResponses, ProtocolAnthropicMessages, responsesBody(`"prompt_cache_key":"k1"`)),
		lossesFor(t, ProtocolOpenAIChat, ProtocolAnthropicMessages, chatBody(`"prompt_cache_key":"k1"`)),
	} {
		if count := countOf(entries, LossUnknownFieldParam, "prompt_cache_key"); count != 0 {
			t.Fatalf("prompt_cache_key 已有专用类别，不得兜底再记，实际 %v", entries)
		}
		if count := countOf(entries, LossPromptCacheKey, "anthropic_has_no_prompt_cache_key"); count != 1 {
			t.Fatalf("anthropic 线必须恰好记一条专用损失，实际 %v", entries)
		}
	}

	metadataEntries := lossesFor(t, ProtocolOpenAIResponses, ProtocolOpenAIChat,
		responsesBody(`"metadata":{"user_id":"u1"}`))
	// metadata 归身份档（info）：目标线不承载时生成内容一点不变，与 content 类改写不同档。
	if count := countOf(metadataEntries, LossUnknownFieldIdentity, "metadata"); count != 1 {
		t.Fatalf("metadata 应恰好一条身份档损失，实际 %v", metadataEntries)
	}
	if count := countOf(metadataEntries, LossUnknownFieldParam, "metadata"); count != 0 {
		t.Fatalf("metadata 不得再按 unknown_field.param 记一条，实际 %v", metadataEntries)
	}

	// 空值不承载约束：空对象 / null 一律不记（与 store:false 同因）。
	for _, value := range []string{`{}`, `null`} {
		entries := lossesFor(t, ProtocolOpenAIResponses, ProtocolOpenAIChat,
			responsesBody(`"container":`+value))
		if count := countOf(entries, LossUnknownFieldParam, "container"); count != 0 {
			t.Fatalf("container=%s 不得记损，实际 %v", value, entries)
		}
	}
}

// TestPromptCacheKeyCarriedExceptAnthropic 钉住缓存路由键的新处置：能带就带，带不了才记损。
//
// 为何必须钉两面：
//   - chat 线**必须带**：守卫链为了让供应商命中前缀缓存会主动补 prompt_cache_key
//     （guard.completeCodexSession），若编码器把它丢掉，补了等于没补、且每个请求白多一条损失；
//   - anthropic 线**必须记**：它无此概念（缓存由 cache_control 显式标记），静默丢才是缺陷；
//   - 网关注入的字段在 anthropic 线上也不算客户端约束（不记损）。
func TestPromptCacheKeyCarriedExceptAnthropic(t *testing.T) {
	const body = `{"model":"gpt-5","input":"hi","prompt_cache_key":"sess_1"}`
	run := func(t *testing.T, target WireProtocol, injected []string) ([]string, string) {
		t.Helper()
		ctx := ConvertCtx{
			ClientFormat:              FormatResponse,
			TargetProto:               target,
			Model:                     "m",
			ToWireToolName:            NormalizeToolName,
			GatewayInjectedBodyFields: injected,
		}
		decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, body), ctx)
		if !ok {
			t.Fatal("responses 线必须能解码")
		}
		encoded, ok := EncodeRequest(target, decoded.Value, ctx)
		if !ok {
			t.Fatal("目标线必须能编码")
		}
		got := []string{}
		for _, entry := range append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...) {
			if entry.Capability == LossPromptCacheKey {
				got = append(got, entry.Detail)
			}
		}
		key, _ := stringField(encoded.Body, "prompt_cache_key")
		return got, key
	}

	if losses, key := run(t, ProtocolOpenAIChat, nil); key != "sess_1" {
		t.Fatalf("chat 出站必须带上客户端的 prompt_cache_key，实际出站 %q（损失 %v）", key, losses)
	}
	if losses, _ := run(t, ProtocolOpenAIChat, []string{"prompt_cache_key"}); len(losses) != 0 {
		t.Fatalf("chat 线不丢该字段，不得记损，实际 %v", losses)
	}
	if losses, key := run(t, ProtocolOpenAIResponses, nil); key != "sess_1" || len(losses) != 0 {
		t.Fatalf("responses 线原样带回，实际出站 %q、损失 %v", key, losses)
	}
	if losses, _ := run(t, ProtocolAnthropicMessages, nil); len(losses) != 1 {
		t.Fatalf("anthropic 线无承载位，客户端声明的缓存键必须恰好记一条损失，实际 %v", losses)
	}
	if losses, _ := run(t, ProtocolAnthropicMessages, []string{"prompt_cache_key"}); len(losses) != 0 {
		t.Fatalf("网关注入的 prompt_cache_key 不算客户端约束，不得记损，实际 %v", losses)
	}
}

// TestCodexConversionKeepsRewriteTierEmpty 用真实 Codex 形状的请求体跑 responses→chat，
// 钉住「请求级开关重分档后该转换的 rewrite 档计数为 0」。
//
// 为何要钉：Codex 每个请求都带 `include:["reasoning.encrypted_content"]`，它此前落
// unknown_field.param（rewrite 档），于是几乎每条转换请求恒挂一枚「内容改写」徽章——生产实测
// 24h 内 1982/7899 行带徽章，其中 90.8%（2159 项）只由这一格构成。计数口径必须是整份台账按
// LossSeverityOf 累加，而非只数某几个 capability：漏一条就是又让一条噪声点亮徽章。
//
// 对照组（presence_penalty）证明 rewrite 档没被整体改瞎：采样惩罚直接改生成分布，必须仍计入。
func TestCodexConversionKeepsRewriteTierEmpty(t *testing.T) {
	const codexBody = `{"model":"gpt-5-codex","instructions":"you are codex",` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"function","name":"shell","description":"run a command","strict":false,` +
		`"parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}],` +
		`"tool_choice":"auto","parallel_tool_calls":true,"store":false,` +
		`"reasoning":{"effort":"medium","summary":"auto"},` +
		`"include":["reasoning.encrypted_content"],"stream":true}`

	run := func(t *testing.T, body string) (rewriteCount int, entries []string) {
		t.Helper()
		ctx := ConvertCtx{
			ClientFormat:   clientFormatOfProtocol(ProtocolOpenAIResponses),
			TargetProto:    ProtocolOpenAIChat,
			Model:          "m",
			ToWireToolName: NormalizeToolName,
		}
		decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, body), ctx)
		if !ok {
			t.Fatal("responses 线必须能解码")
		}
		encoded, ok := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)
		if !ok {
			t.Fatal("chat 线必须能编码")
		}
		// 与 forward 的 encodeConvertedBody 同口径：两侧损失合并成一份台账。
		for _, entry := range append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...) {
			entries = append(entries, entry.Capability+"/"+string(entry.Action)+"/"+entry.Detail)
			if LossSeverityOf(entry.Capability, entry.Action) == SeverityRewrite {
				rewriteCount++
			}
		}
		return rewriteCount, entries
	}

	rewriteCount, entries := run(t, codexBody)
	if rewriteCount != 0 {
		t.Fatalf("真实 Codex 请求的转换不得有 rewrite 档损失，实际 %d 条：%v", rewriteCount, entries)
	}
	if !slices.Contains(entries, LossRequestOption+"/dropped/include") {
		t.Fatalf("include 必须落 %s/dropped/include，实际台账：%v", LossRequestOption, entries)
	}

	// 对照组：采样惩罚直接改生成分布，丢掉它必然换一种答法 ⇒ 必须仍在 rewrite 档。
	controlBody := strings.Replace(codexBody, `"stream":true`, `"presence_penalty":0.5,"stream":true`, 1)
	controlCount, controlEntries := run(t, controlBody)
	if controlCount < 1 {
		t.Fatalf("presence_penalty 必须计进 rewrite 档（对照组的可证伪点），实际 0 条：%v", controlEntries)
	}
	if !slices.Contains(controlEntries, LossRequestSampler+"/dropped/presence_penalty") {
		t.Fatalf("presence_penalty 必须落 %s/dropped/presence_penalty，实际台账：%v",
			LossRequestSampler, controlEntries)
	}
}
