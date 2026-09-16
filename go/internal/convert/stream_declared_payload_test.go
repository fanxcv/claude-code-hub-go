package convert

import "testing"

// 声明式载荷的对账验收：上游只在 done / 终态 / item 载荷里给全文、或只在块起始帧里给正文，
// 而一个增量都不发时，跨线客户端仍须拿到完整正文——且**只拿到一次**。
//
// 为什么单列一个文件：语料（streams.json）只覆盖「增量齐全」的形态，本面在修之前是静默丢内容
// 或重复内容，语料与既有单测都抓不到。
//
// 两条硬断言贯穿全篇：
//   - 正文在输出里出现且仅出现一次（少即丢内容、多即重复）；
//   - 客户端方言的终止事件齐全（内容对了但流没闭合同样算坏）。

// ---------------------------------------------------------------------------
// 契约：六条跨协议对 × 声明式载荷
// ---------------------------------------------------------------------------

const declaredTextAnthropicStart = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m1","model":"m","usage":{"input_tokens":5,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"起始正文"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

const declaredTextChatSingleDelta = "data: " +
	`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"整段正文"}}]}` + "\n\n" +
	"data: " +
	`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}` + "\n\n" +
	"data: [DONE]\n\n"

const declaredTextResponsesDeclaration = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
	"event: response.content_part.added\n" +
	`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"i1","part":{"type":"output_text","text":"全量正文"}}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"全量正文"}]}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"id":"i1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"全量正文"}]}],"usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n"

func TestStreamDeclaredPayloadReachesClientExactlyOnce(t *testing.T) {
	cases := []struct {
		upstream    WireProtocol
		client      WireProtocol
		input       string
		wantText    string
		wantSuccess string
	}{
		{ProtocolAnthropicMessages, ProtocolOpenAIChat, declaredTextAnthropicStart, "起始正文",
			"起始正文只写在 content_block_start 里（无任何 text_delta）"},
		{ProtocolAnthropicMessages, ProtocolOpenAIResponses, declaredTextAnthropicStart, "起始正文",
			"同上，目标线是 responses"},
		{ProtocolOpenAIChat, ProtocolAnthropicMessages, declaredTextChatSingleDelta, "整段正文",
			"chat 上游把整段正文放在唯一一个 delta 分块里"},
		{ProtocolOpenAIChat, ProtocolOpenAIResponses, declaredTextChatSingleDelta, "整段正文",
			"同上，目标线是 responses"},
		{ProtocolOpenAIResponses, ProtocolAnthropicMessages, declaredTextResponsesDeclaration, "全量正文",
			"responses 上游只在 content_part.added / item / completed 载荷里给全文，无任何 delta"},
		{ProtocolOpenAIResponses, ProtocolOpenAIChat, declaredTextResponsesDeclaration, "全量正文",
			"同上，目标线是 chat"},
	}
	for _, testCase := range cases {
		testCase := testCase
		name := string(testCase.upstream) + ".to." + string(testCase.client)
		t.Run(name, func(t *testing.T) {
			for _, chunkSize := range []int{0, 1, 7} {
				out := runDeclaredPayloadPipe(t, testCase.upstream, testCase.client, testCase.input, chunkSize)
				got := clientDialectText(t, testCase.client, out)
				// 只对「增量通道」做完整比对：声明帧（responses 的 done / part / item / completed）
				// 按协议本就重复携带全文，拿整段输出计次会误判为重复。
				if got != testCase.wantText {
					t.Fatalf("正文对账失败（chunkSize=%d，场景：%s）：got %q want %q\n原文：%q",
						chunkSize, testCase.wantSuccess, got, testCase.wantText, out)
				}
				assertClientTerminator(t, testCase.client, out)
				assertClientUsageConsistent(t, testCase.client, out)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// responses 解码器：done 载荷只补差额
// ---------------------------------------------------------------------------

func TestResponsesDonePayloadAppendsOnlyMissingTail(t *testing.T) {
	prefix := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"message","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"i1","part":{"type":"output_text","text":""}}` + "\n\n"

	cases := []struct {
		name        string
		deltas      []string
		doneText    string
		itemText    string
		completedTX string
		want        string
	}{
		{
			name:     "声明全文长于已发增量",
			deltas:   []string{"你", "好"},
			doneText: "你好世界",
			want:     "你好世界",
		},
		{
			name:     "声明全文与已发增量相等（不得重发）",
			deltas:   []string{"你好"},
			doneText: "你好",
			want:     "你好",
		},
		{
			name:     "声明全文与已发增量不构成前缀（宁可不补，绝不重复）",
			deltas:   []string{"你好"},
			doneText: "世界",
			want:     "你好",
		},
		{
			name:     "一个增量都没发（只剩 item 与终态载荷）",
			itemText: "全量正文",
			want:     "全量正文",
		},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			input := prefix
			for _, delta := range testCase.deltas {
				input += "event: response.output_text.delta\n" +
					`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"i1","delta":"` + delta + `"}` + "\n\n"
			}
			if testCase.doneText != "" {
				input += "event: response.output_text.done\n" +
					`data: {"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"i1","text":"` + testCase.doneText + `"}` + "\n\n"
			}
			itemText := testCase.itemText
			if itemText == "" {
				itemText = testCase.doneText
			}
			if itemText != "" {
				input += "event: response.output_item.done\n" +
					`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"i1","type":"message","status":"completed","content":[{"type":"output_text","text":"` + itemText + `"}]}}` + "\n\n"
			}
			input += "event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n"

			// done 已把差额补齐时，item.done 与终态载荷不得再补一次。
			for _, chunkSize := range []int{0, 1} {
				out := runDeclaredPayloadPipe(t, ProtocolOpenAIResponses, ProtocolOpenAIChat, input, chunkSize)
				got := clientDialectText(t, ProtocolOpenAIChat, out)
				if got != testCase.want {
					t.Fatalf("chunkSize=%d：got %q want %q\n原文：%q", chunkSize, got, testCase.want, out)
				}
				assertClientTerminator(t, ProtocolOpenAIChat, out)
			}
		})
	}
}

// TestResponsesArgumentsDoneAppendsOnlyMissingTail 钉住工具参数通道的对账。
//
// 与文本同一条规则、另一条通道：`function_call_arguments.done` 里的 arguments 是声明式全文，
// 已发增量是它的前缀时只补差额（参数被截断是客户端最常见的一类“工具调用看起来对了但执行失败”）。
func TestResponsesArgumentsDoneAppendsOnlyMissingTail(t *testing.T) {
	const prefix = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":""}}` + "\n\n"

	const terminal = "event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n"

	t.Run("增量后只剩差额", func(t *testing.T) {
		input := prefix +
			"event: response.function_call_arguments.delta\n" +
			`data: {"type":"response.function_call_arguments.delta","output_index":0,"item_id":"fc_1","delta":"{\"a\":"}` + "\n\n" +
			"event: response.function_call_arguments.done\n" +
			`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"a\":1}"}` + "\n\n" +
			terminal
		want := `{"a":1}`
		out := runDeclaredPayloadPipe(t, ProtocolOpenAIResponses, ProtocolAnthropicMessages, input, 0)
		if got := clientDialectToolArgs(t, ProtocolAnthropicMessages, out); got != want {
			t.Fatalf("参数对账失败：got %q want %q\n原文：%q", got, want, out)
		}
	})

	t.Run("一个增量都没发", func(t *testing.T) {
		input := prefix +
			"event: response.function_call_arguments.done\n" +
			`data: {"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_1","arguments":"{\"a\":2}"}` + "\n\n" +
			terminal
		want := `{"a":2}`
		for _, client := range []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIChat} {
			out := runDeclaredPayloadPipe(t, ProtocolOpenAIResponses, client, input, 0)
			if got := clientDialectToolArgs(t, client, out); got != want {
				t.Fatalf("%s 参数对账失败：got %q want %q\n原文：%q", client, got, want, out)
			}
			assertClientTerminator(t, client, out)
		}
	})
}

// ---------------------------------------------------------------------------
// responses 解码器：content_part.added 的初始文本与后续增量并存
// ---------------------------------------------------------------------------

// TestResponsesContentPartSeedAndFollowingDeltaReachesClient 钉住「块起始帧给了初始文本、
// 之后又来了增量，且初始文本是最终正文的前缀」这一形态。
//
// 与 TestStreamDeclaredPayloadReachesClientExactlyOnce 的区别：那条覆盖的是「只给初始文本、
// 一个增量都不发」，初始文本是唯一来源；本条覆盖的是初始文本 + 续写增量。此形态下「关闭块时才补
// seed」的兜底永远不会触发（emitted 已非空），而 done / item 的对账也补不回来——emitted 不是
// 声明全文的前缀，按「宁可不补、绝不重复」的保守策略算不出差额，于是整段前缀静默消失。
func TestResponsesContentPartSeedAndFollowingDeltaReachesClient(t *testing.T) {
	const input = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"i1","part":{"type":"output_text","text":"起始"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"i1","delta":"正文"}` + "\n\n" +
		"event: response.output_text.done\n" +
		`data: {"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"i1","text":"起始正文"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"起始正文"}]}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"id":"i1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"起始正文"}]}],"usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n"

	const want = "起始正文"
	for _, client := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages} {
		client := client
		t.Run(string(client), func(t *testing.T) {
			for _, chunkSize := range []int{0, 1} {
				out := runDeclaredPayloadPipe(t, ProtocolOpenAIResponses, client, input, chunkSize)
				if got := clientDialectText(t, client, out); got != want {
					t.Fatalf("chunkSize=%d：got %q want %q（初始文本丢失或被重复）\n原文：%q",
						chunkSize, got, want, out)
				}
				assertClientTerminator(t, client, out)
			}
		})
	}
}

// TestResponsesSeedRepeatedAsFirstDeltaReachesClientOnce 钉住「content_part.added 的 part.text 与
// 首个增量逐字相同」这一歧义形态。
//
// 它有两种互斥解释：上游把整段正文又作为首片增量重发了一遍（重发型），或初始文本恰好与首段续写
// 同字（合法续写型，如 seed「好」+ delta「好」=「好好」）。两种解释在增量到达时是同一串字节，本地
// 无从区分，只有 done / item / 终态给出的声明式全文能消歧——故这段必须悬置到声明到达再按声明交付。
// 按「相同即去重」的启发式就地猜，会直接把合法续写型的首字吞掉。
func TestResponsesSeedRepeatedAsFirstDeltaReachesClientOnce(t *testing.T) {
	build := func(seed string, deltas []string, declared string) string {
		input := "event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n" +
			"event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
			"event: response.content_part.added\n" +
			`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"i1","part":{"type":"output_text","text":"` + seed + `"}}` + "\n\n"
		for _, delta := range deltas {
			input += "event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"i1","delta":"` + delta + `"}` + "\n\n"
		}
		return input +
			"event: response.output_text.done\n" +
			`data: {"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"i1","text":"` + declared + `"}` + "\n\n" +
			"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + declared + `"}]}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"id":"i1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"` + declared + `"}]}],"usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n"
	}

	cases := []struct {
		name     string
		seed     string
		deltas   []string
		declared string
		want     string
	}{
		{"重发型（声明全文与首片同内容）", "正文", []string{"正文"}, "正文", "正文"},
		{"重发首片后继续续写", "正文", []string{"正文", "更多"}, "正文更多", "正文更多"},
		{"累计型上游（后续增量再带一遍全文）", "正文", []string{"正文", "正文更多"}, "正文更多", "正文更多"},
		{"合法续写型（声明全文是两段之和）", "好", []string{"好"}, "好好", "好好"},
	}
	for _, testCase := range cases {
		testCase := testCase
		for _, client := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages} {
			client := client
			t.Run(testCase.name+"/"+string(client), func(t *testing.T) {
				for _, chunkSize := range []int{0, 1} {
					out := runDeclaredPayloadPipe(t, ProtocolOpenAIResponses, client,
						build(testCase.seed, testCase.deltas, testCase.declared), chunkSize)
					if got := clientDialectText(t, client, out); got != testCase.want {
						t.Fatalf("chunkSize=%d：got %q want %q（重发首片被重复交付，或合法续写的首字被吞）\n原文：%q",
							chunkSize, got, testCase.want, out)
					}
					assertClientTerminator(t, client, out)
					assertClientUsageConsistent(t, client, out)
				}
			})
		}
	}
}

// TestResponsesSuspendedSeedWithoutDeclarationStillDeliversContent 钉住悬置内容在「上游连一个
// 声明都不给」（无 done 文本、无 item、终态 output 为空）时的兜底。
//
// 此形态下已无从消歧，只能二选一：赌一种解释（赌错就静默丢字）或不赌（两种都交付，可能重复）。
// 本实现选后者——内容丢失是公认的硬失败，重复不是；故正文在此形态下会出现两次，这是可接受的
// 代价，而不是把内容丢掉的借口。正常上游一定给声明，故这条兜底只在协议残缺时走到。
func TestResponsesSuspendedSeedWithoutDeclarationStillDeliversContent(t *testing.T) {
	const input = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"i1","part":{"type":"output_text","text":"正文"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"i1","delta":"正文"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n"

	for _, client := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages} {
		client := client
		t.Run(string(client), func(t *testing.T) {
			out := runDeclaredPayloadPipe(t, ProtocolOpenAIResponses, client, input, 0)
			got := clientDialectText(t, client, out)
			// 刻意钉住「两个来源各交付一次」：若日后改成「无声明时一律不重复」，改的就是这一条断言，
			// 而不是默默把内容丢掉。上面两种形态（重发型 / 合法续写型）都必须给出 2。
			if occurrences := countOccurrences(got, "正文"); occurrences != 2 {
				t.Fatalf("无声明时悬置内容的交付次数 %d != 2（0=静默丢字，1=赌了一种解释并丢了另一来源）"+
					"：got %q\n原文：%q", occurrences, got, out)
			}
			assertClientTerminator(t, client, out)
		})
	}
}

// TestResponsesSuspendedSeedWithTextlessDoneKeepsTrailingContent 钉住另一处易漏的角落：done 帧存在
// 但不带 text 时，旧路径会拿 seed 顶替声明去裁决悬置增量——多片悬置下 seed 只是首段，据此交付会把
// 后面的内容静默吃掉。此时必须退回「两种来源都交付」的兜底，因为根本没有真声明可依。
func TestResponsesSuspendedSeedWithTextlessDoneKeepsTrailingContent(t *testing.T) {
	const input = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"i1","part":{"type":"output_text","text":"正文"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"i1","delta":"正文"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"i1","delta":"正文更多"}` + "\n\n" +
		"event: response.output_text.done\n" +
		`data: {"type":"response.output_text.done","output_index":0,"content_index":0,"item_id":"i1"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n"

	for _, client := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages} {
		client := client
		t.Run(string(client), func(t *testing.T) {
			out := runDeclaredPayloadPipe(t, ProtocolOpenAIResponses, client, input, 0)
			got := clientDialectText(t, client, out)
			if countOccurrences(got, "更多") == 0 {
				t.Fatalf("done 帧不带 text 时，悬置增量里的末段内容被静默吃掉：got %q\n原文：%q", got, out)
			}
			if countOccurrences(got, "正文") == 0 {
				t.Fatalf("done 帧不带 text 时，初始文本被吞：got %q\n原文：%q", got, out)
			}
			assertClientTerminator(t, client, out)
		})
	}
}

// ---------------------------------------------------------------------------
// anthropic 块起始帧携带的正文（含 thinking）不得跨线消失
// ---------------------------------------------------------------------------

func TestAnthropicStartPayloadReachesCrossLineClient(t *testing.T) {
	const thinkingInput = "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"m1","model":"m","usage":{"input_tokens":5,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"起始推理"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	cases := []struct {
		client WireProtocol
		input  string
		want   string
		event  string
	}{
		{ProtocolOpenAIChat, declaredTextAnthropicStart, "起始正文", `"content":"起始正文"`},
		{ProtocolOpenAIResponses, declaredTextAnthropicStart, "起始正文", `"delta":"起始正文"`},
		{ProtocolOpenAIChat, thinkingInput, "起始推理", `"reasoning_content":"起始推理"`},
		{ProtocolOpenAIResponses, thinkingInput, "起始推理", `"delta":"起始推理"`},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(string(testCase.client)+"."+testCase.event, func(t *testing.T) {
			out := runDeclaredPayloadPipe(t, ProtocolAnthropicMessages, testCase.client, testCase.input, 0)
			if countOccurrences(out, testCase.event) != 1 {
				t.Fatalf("块起始载荷未被转成增量帧或出现多次，want 1 × %s\n原文：%q", testCase.event, out)
			}
			if got := clientDialectText(t, testCase.client, out); got != testCase.want {
				t.Fatalf("got %q want %q\n原文：%q", got, testCase.want, out)
			}
			assertClientTerminator(t, testCase.client, out)
		})
	}
}

// ---------------------------------------------------------------------------
// 局部工具
// ---------------------------------------------------------------------------

// runDeclaredPayloadPipe 按给定块大小喂入上游字节，返回客户端字节。
// chunkSize<=0 表示整段一次喂入。
func runDeclaredPayloadPipe(t *testing.T, upstream WireProtocol, client WireProtocol, input string, chunkSize int) string {
	t.Helper()
	pipe, ok := NewStreamPipe(upstream, client, ConvertCtx{
		ClientFormat: clientFormatOfProtocol(client),
		TargetProto:  upstream,
		Stream:       true,
	})
	if !ok {
		t.Fatalf("建管道失败：%s -> %s", upstream, client)
	}
	if chunkSize <= 0 {
		chunkSize = len(input)
	}
	var out []byte
	for i := 0; i < len(input); i += chunkSize {
		end := i + chunkSize
		if end > len(input) {
			end = len(input)
		}
		out = append(out, pipe.Push([]byte(input[i:end]))...)
	}
	out = append(out, pipe.Flush()...)
	return string(out)
}

// clientDialectText 汇总客户端收到的正文（按客户端方言取，避免误读上游方言字段）。
func clientDialectText(t *testing.T, client WireProtocol, out string) string {
	t.Helper()
	frames, _ := ParseSSEFrames(out)
	var builder []byte
	for _, frame := range frames {
		payload := parseFrameJSON(frame)
		if payload == nil {
			continue
		}
		event, _ := stringField(payload, "type")
		switch client {
		case ProtocolOpenAIChat:
			for _, choice := range payload.ArrayField("choices") {
				delta := choice.ObjectField("delta")
				if delta == nil {
					continue
				}
				builder = append(builder, chatDeltaText(delta)...)
			}
		case ProtocolAnthropicMessages:
			if event != "content_block_delta" {
				continue
			}
			delta := payload.ObjectField("delta")
			if text, ok := stringField(delta, "text"); ok {
				builder = append(builder, text...)
			}
			if thinking, ok := stringField(delta, "thinking"); ok {
				builder = append(builder, thinking...)
			}
		case ProtocolOpenAIResponses:
			if event != "response.output_text.delta" && event != "response.reasoning_summary_text.delta" {
				continue
			}
			if delta, ok := stringField(payload, "delta"); ok {
				builder = append(builder, delta...)
			}
		}
	}
	return string(builder)
}

func chatDeltaText(delta *Value) string {
	// 必须用 Get：content 既可为字符串也可为数组，ObjectField 只认对象会恒返回 nil。
	content, _ := delta.Get("content")
	var builder []byte
	if text, ok := content.String(); ok {
		builder = append(builder, text...)
	} else if content.IsArray() {
		for _, part := range content.Items() {
			if text, ok := part.StringField("text"); ok {
				builder = append(builder, text...)
			}
		}
	}
	if reasoning, ok := stringField(delta, "reasoning_content"); ok {
		builder = append(builder, reasoning...)
	}
	return string(builder)
}

// assertClientTerminator 断言客户端方言的终止事件齐全。
func assertClientTerminator(t *testing.T, client WireProtocol, out string) {
	t.Helper()
	switch client {
	case ProtocolOpenAIChat:
		if !contains(out, "data: [DONE]") {
			t.Fatalf("缺 Chat 终止哨兵 [DONE]：%q", lastBytes(out, 200))
		}
	case ProtocolAnthropicMessages:
		if !contains(out, "message_stop") {
			t.Fatalf("缺 Anthropic 终止事件 message_stop：%q", lastBytes(out, 200))
		}
	case ProtocolOpenAIResponses:
		if !contains(out, "response.completed") {
			t.Fatalf("缺 Responses 终止事件 response.completed：%q", lastBytes(out, 200))
		}
	default:
		t.Fatalf("未覆盖的客户端线 %s", client)
	}
}

// assertClientUsageConsistent 断言客户端 usage 自洽：cached 计入总输入且不超过它，
// total = 总输入 + 输出。缺 usage 的行跳过（不是所有线在所有场景都回报）。
func assertClientUsageConsistent(t *testing.T, client WireProtocol, out string) {
	t.Helper()
	frames, _ := ParseSSEFrames(out)
	checked := 0
	for _, frame := range frames {
		payload := parseFrameJSON(frame)
		if payload == nil {
			continue
		}
		var usage *Value
		event, _ := stringField(payload, "type")
		switch client {
		case ProtocolOpenAIChat:
			usage = payload.ObjectField("usage")
		case ProtocolAnthropicMessages:
			if event != "message_delta" {
				continue
			}
			usage = payload.ObjectField("usage")
		case ProtocolOpenAIResponses:
			if event != "response.completed" && event != "response.incomplete" {
				continue
			}
			if response := payload.ObjectField("response"); response != nil {
				usage = response.ObjectField("usage")
			}
		}
		if usage == nil || usage.IsNull() || !usage.IsObject() {
			continue
		}
		switch client {
		case ProtocolOpenAIChat:
			prompt, hasPrompt := numberField(usage, "prompt_tokens")
			completion, hasCompletion := numberField(usage, "completion_tokens")
			total, hasTotal := numberField(usage, "total_tokens")
			if hasPrompt && hasCompletion && hasTotal {
				if *total != *prompt+*completion {
					t.Fatalf("chat usage 不自洽：total %v != prompt %v + completion %v", *total, *prompt, *completion)
				}
				checked++
			}
			if cached, ok := numberOrNil(nestedField(usage, "prompt_tokens_details", "cached_tokens")); ok && hasPrompt {
				if *cached > *prompt {
					t.Fatalf("chat usage 不自洽：cached %v > 总输入 prompt %v", *cached, *prompt)
				}
			}
		case ProtocolOpenAIResponses:
			input, hasInput := numberField(usage, "input_tokens")
			output, hasOutput := numberField(usage, "output_tokens")
			total, hasTotal := numberField(usage, "total_tokens")
			if hasInput && hasOutput && hasTotal {
				if *total != *input+*output {
					t.Fatalf("responses usage 不自洽：total %v != input %v + output %v", *total, *input, *output)
				}
				checked++
			}
			if cached, ok := numberOrNil(nestedField(usage, "input_tokens_details", "cached_tokens")); ok && hasInput {
				if *cached > *input {
					t.Fatalf("responses usage 不自洽：cached %v > 总输入 input %v", *cached, *input)
				}
			}
		case ProtocolAnthropicMessages:
			// Anthropic 的 input_tokens 不含 cache_read，两者相加才是总输入：只能断言非负。
			if cacheRead, ok := numberField(usage, "cache_read_input_tokens"); ok && *cacheRead < 0 {
				t.Fatalf("anthropic usage 不自洽：cache_read_input_tokens %v < 0", *cacheRead)
			}
			if output, ok := numberField(usage, "output_tokens"); ok {
				if *output < 0 {
					t.Fatalf("anthropic usage 不自洽：output_tokens %v < 0", *output)
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatalf("%s 客户端未回报任何 usage，无法判定自洽：%q", client, lastBytes(out, 200))
	}
}

// clientDialectToolArgs 汇总客户端收到的工具参数分片（按客户端方言的各线形状取）。
func clientDialectToolArgs(t *testing.T, client WireProtocol, out string) string {
	t.Helper()
	frames, _ := ParseSSEFrames(out)
	var builder []byte
	for _, frame := range frames {
		payload := parseFrameJSON(frame)
		if payload == nil {
			continue
		}
		event, _ := stringField(payload, "type")
		switch client {
		case ProtocolAnthropicMessages:
			if event != "content_block_delta" {
				continue
			}
			if args, ok := stringField(payload.ObjectField("delta"), "partial_json"); ok {
				builder = append(builder, args...)
			}
		case ProtocolOpenAIChat:
			for _, choice := range payload.ArrayField("choices") {
				delta, _ := choice.ObjectField("delta").Get("tool_calls")
				for _, call := range delta.Items() {
					if args, ok := call.ObjectField("function").StringField("arguments"); ok {
						builder = append(builder, args...)
					}
				}
			}
		case ProtocolOpenAIResponses:
			if event != "response.function_call_arguments.delta" {
				continue
			}
			if args, ok := stringField(payload, "delta"); ok {
				builder = append(builder, args...)
			}
		}
	}
	return string(builder)
}

func countOccurrences(text string, needle string) int {
	if len(needle) == 0 {
		return 0
	}
	count := 0
	for i := 0; i+len(needle) <= len(text); i++ {
		if text[i:i+len(needle)] == needle {
			count++
		}
	}
	return count
}
