package convert

import (
	"strings"
	"testing"
)

// 本文件钉住损失台账的三件事：
//  1. catch-all `unknown_field` 按来源细分（不再是一个格子装下“工具/消息/参数”三种事实）；
//  2. 档位映射（rewrite / degrade / info）——界面“哪些值得一眼看到”的权威口径；
//  3. 同一张逻辑图片在一次转换里只记一次（编码侧不再重复记）。
//
// 为何要钉：这三件事都是**降噪后的可见性契约**。细分错了，排查时仍然看不出丢的是什么；
// 档位错了，真损失会被折叠进详情（或噪声重新占满列表）；图片重复记，计数就是实际值的两倍
// （生产实测：responses→chat 方向的 image 计数为 2×）。

func TestUnknownFieldCapabilityByDetailPrefix(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   string
	}{
		{"工具声明", "tools[].name", LossUnknownFieldTool},
		{"工具选择", "tool_choice.type", LossUnknownFieldTool},
		{"工具调用参数", "tool_call.arguments", LossUnknownFieldTool},
		{"内容部件", "content_part.image_url", LossUnknownFieldContent},
		{"图片 URL 成员", "image_url.missing", LossUnknownFieldContent},
		{"长前缀优先于短前缀", "input_image.image_url", LossUnknownFieldContent},
		{"未知块", "block", LossUnknownFieldContent},
		{"消息级", "messages[3]", LossUnknownFieldMessage},
		{"消息成员", "message.role", LossUnknownFieldMessage},
		{"拒绝块", "refusal", LossUnknownFieldRefusal},
		{"思考类型", "thinking.type", LossUnknownFieldParam},
		{"采样参数", "seed", LossUnknownFieldParam},
		{"线级结构", "input[]", LossUnknownFieldStructure},
		{"输出结构", "output.function_call_output", LossUnknownFieldStructure},
		// 未在规则表内的新来源必须可见地落兜底，而不是悄悄消失。
		{"未归类来源落兜底", "某个新来源", LossUnknownFieldOther},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			collector := &LossCollector{}
			collector.Dropped(LossUnknownField, "request", testCase.detail)
			report := collector.Report()
			if len(report.Entries) != 1 {
				t.Fatalf("应恰好一条损失，实际 %v", report.Entries)
			}
			if got := report.Entries[0].Capability; got != testCase.want {
				t.Fatalf("detail %q 应归到 %s，实际 %s", testCase.detail, testCase.want, got)
			}
			// 前缀是两侧共用的判据（界面按前缀判定“属于 catch-all 家族”），故必须成立。
			if !hasUnknownFieldPrefix(testCase.want) {
				t.Fatalf("%s 必须带 unknown_field. 前缀", testCase.want)
			}
		})
	}
}

func hasUnknownFieldPrefix(capability string) bool {
	return len(capability) > len("unknown_field.") && capability[:len("unknown_field.")] == "unknown_field."
}

// TestUnknownFieldSplitOnRealConversion 走真实编解码链，断言两个不同来源落到两个不同子类别。
//
// 单独钉这一格的理由：上面的表测的是累积入口本身，而细分靠的是「各站点传进来的 detail
// 就是字段路径」这一前提——只有走一遍真解码器才能证明这个前提在真实站点上成立。
func TestUnknownFieldSplitOnRealConversion(t *testing.T) {
	// 一个 user 消息里的 input_image 缺 image_url（站点 detail = input_image.image_url）+
	// 一条 role 无法承载的消息（站点 detail = message.role）。
	const body = `{"model":"gpt-5","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_image"}]},` +
		`{"type":"message","role":"critic","content":"hi"}]}`
	ctx := ConvertCtx{
		ClientFormat:   FormatResponse,
		TargetProto:    ProtocolOpenAIChat,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("responses 线必须能解码")
	}
	got := map[string]string{}
	for _, entry := range decoded.Loss.Entries {
		if entry.Capability == LossUnknownField {
			t.Fatalf("catch-all 不得再出现在台账里（未细分）：%+v", entry)
		}
		got[entry.Detail] = entry.Capability
	}
	if got["input_image.image_url"] != LossUnknownFieldContent {
		t.Errorf("input_image.image_url 应归 %s，实际 %q（全表 %v）",
			LossUnknownFieldContent, got["input_image.image_url"], got)
	}
	if got["message.role"] != LossUnknownFieldMessage {
		t.Errorf("message.role 应归 %s，实际 %q（全表 %v）",
			LossUnknownFieldMessage, got["message.role"], got)
	}
}

func TestLossSeverityOf(t *testing.T) {
	cases := []struct {
		name       string
		capability string
		action     LossAction
		want       LossSeverity
	}{
		// 内容/参数被改写或丢弃 → 改写档：上游看到的东西变了。
		// image 的 rewritten 不在此列：那是 data URL → base64 的表示归一，送达内容不变（见下一条）。
		{"客户端参数被丢", LossTopK, LossDropped, SeverityRewrite},
		{"思考块被丢", LossThinkingBlock, LossDropped, SeverityRewrite},
		{"命中未列出的能力按改写档", LossDocument, LossDropped, SeverityRewrite},
		{"细分后的 catch-all", LossUnknownFieldTool, LossDropped, SeverityRewrite},
		{"未细分的 catch-all 兜底", LossUnknownField, LossDropped, SeverityRewrite},
		// 改写档里「按动作分档」的一对：同一 capability 两种事实必须分开。
		// 真丢图（非 image/* 媒体类型、GIF、data URL 解析失败转 opaque）仍须显眼。
		{"图片被丢", LossImage, LossDropped, SeverityRewrite},
		// 表示归一 → 信息档：客户端用 data URL 表达、送达内容逐字不变，徽章不该计它
		// （生产实证：算进改写档会让「几乎每条带图的转换」都挂徽章，真损失被淹没）。
		{"图片表示归一", LossImage, LossRewritten, SeverityInfo},
		// 保真度弱化 → 降级档：thinking.block 的两种事实必须分档。
		{"思考强度换算", LossThinkingBlock, LossDowngraded, SeverityDegrade},
		{"思考签名丢失", LossThinkingSignature, LossDropped, SeverityDegrade},
		{"预算降级为等级", LossThinkingDerived, LossDowngraded, SeverityDegrade},
		{"缓存提示丢失", LossCacheControl, LossDropped, SeverityDegrade},
		{"思考回传退化", LossReasoningReplay, LossDropped, SeverityDegrade},
		// 不承载约束 → 信息档：丢掉后本次作答完全不变。
		{"落库开关", LossStoreFlag, LossDropped, SeverityInfo},
		{"缓存路由键", LossPromptCacheKey, LossDropped, SeverityInfo},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := LossSeverityOf(testCase.capability, testCase.action); got != testCase.want {
				t.Fatalf("%s/%s 应为 %s，实际 %s",
					testCase.capability, testCase.action, testCase.want, got)
			}
		})
	}
}

// TestImageLossCountedOncePerLogicalImage 钉住「一张图只记一次」的最小一格。
//
// 缺陷原状：responses→chat 一条转换里，解码侧（data URL → base64）与编码侧（base64 → data URL）
// 各记一次，于是同一张图进两次台账。修法是**解码侧只留事实、编码侧按最终结果记一条**
// （见 Block.FromDataURL）：送达则记 rewritten，被目标线整幅丢掉则只记 dropped。
//
// 本用例只守「一张图一条」这一格（PNG / user / responses→chat）；足量的覆盖在
// image_loss_matrix_test.go 的六向 × role × 媒体形态矩阵里（那里同时断言目标正文里图还在）。
//
// 断言同时覆盖「编码侧真的把图写出去了」：若哪天编码器把图丢了，计数会变成 0 或出现 drop，
// 这条用例会连同下面的 message 形状一起失败，而不是默默通过。
func TestImageLossCountedOncePerLogicalImage(t *testing.T) {
	const body = `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"看这张图"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="}]}]}`
	ctx := ConvertCtx{
		ClientFormat:   FormatResponse,
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
	// 与 forward.convertBody 同口径：解码侧与编码侧的损失合并成一份台账。
	entries := append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...)
	imageEntries := []LossEntry{}
	for _, entry := range entries {
		if entry.Capability == LossImage {
			imageEntries = append(imageEntries, entry)
		}
	}
	if len(imageEntries) != 1 {
		t.Fatalf("一张图只该记一次，实际 %d 次：%+v", len(imageEntries), imageEntries)
	}
	if imageEntries[0].Action != LossRewritten {
		t.Fatalf("图片跨线是表示改写（非丢弃），实际 action=%s", imageEntries[0].Action)
	}
	// 图确实编进了目标正文（data URL 形态），不是被丢掉换来的“只记一次”。
	if marshaled := string(encoded.Body.MarshalCompact()); !strings.Contains(marshaled, "data:image/png;base64,") {
		t.Fatalf("目标正文里应有一张 data URL 图片：%s", marshaled)
	}
}

// TestImageLossSeveritySeparatesNormalizationFromLoss 钉住「表示归一」与「真损失」在档位上分开。
//
// 为何要钉这一对：`image` 这一个 capability 曾同时承载两种事实——(a) 客户端用 data URL 表达、
// 编码侧按目标线原样重表达（送达内容不变），(b) 图被目标线整块丢掉。两者共用一个 capability 时，
// 只把 image/rewritten 降为信息档会把 (b) 一起从徽章上降掉（真损失被降噪吞掉）；而 (a) 留在
// 改写档的代价有生产实证：近 24h 带 rewrite 标记的转换请求里 **61%（253/416）只涉及这一条**，
// 于是「几乎每条带图的转换都挂改写徽章」。
//
// 三格分别对应：① 归一降为信息档；② 真丢仍是改写档；③ 解析不出的 data URL（装箱成 opaque、
// 跨线编码时被整块丢掉）**不借用 image**，故不会被 ① 一起降档。
func TestImageLossSeveritySeparatesNormalizationFromLoss(t *testing.T) {
	// ① 表示归一：data URL → (mediaType, data) → 拼回 data URL，送达内容不变。
	if got := LossSeverityOf(LossImage, LossRewritten); got != SeverityInfo {
		t.Fatalf("图片表示归一无内容变化，应为 %s，实际 %s", SeverityInfo, got)
	}
	// ② 真丢图：上游看不到这张图了，必须仍然显眼。
	if got := LossSeverityOf(LossImage, LossDropped); got != SeverityRewrite {
		t.Fatalf("真丢图必须留在 %s，实际 %s", SeverityRewrite, got)
	}
	// ③ 畸形 data URL（媒体类型不是 image/*）：归因到 unknown_field.content，不占用 image。
	const body = `{"model":"gpt-5","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"看这张图"},` +
		`{"type":"input_image","image_url":"data:application/pdf;base64,AAAA"}]}]}`
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, body), ConvertCtx{
		ClientFormat:   FormatResponse,
		TargetProto:    ProtocolOpenAIChat,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	})
	if !ok {
		t.Fatal("responses 线必须能解码")
	}
	found := -1
	for index, entry := range decoded.Loss.Entries {
		if entry.Capability == LossImage {
			t.Fatalf("解析不出的 data URL 不该记成 image（会把真损失随表示归一一起降档）：%+v", entry)
		}
		if entry.Action == LossRewritten && entry.Detail == "image_url.data_url" {
			found = index
		}
	}
	if found < 0 {
		t.Fatalf("解析不出的 data URL 必须留一条可归因的记录，实际 %+v", decoded.Loss.Entries)
	}
	entry := decoded.Loss.Entries[found]
	if got := LossSeverityOf(entry.Capability, entry.Action); got != SeverityRewrite {
		t.Fatalf("该记录仍须是 %s（上游确实看不到这张图），实际 %s（capability=%s）",
			SeverityRewrite, got, entry.Capability)
	}
}
