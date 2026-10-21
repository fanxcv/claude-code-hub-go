package convert

// 本文件承载「结构化输出约束」在 chat 与 responses 两线之间的互转。
//
// 两线说的是同一件事，只是外壳不同（OpenAPI 两线语义）：
//
//	chat:      response_format = {"type":"json_schema","json_schema":{"name","schema","strict"}}
//	responses: text.format     = {"type":"json_schema","name","schema","strict"}
//
// `type` 为 text / json_object 时两线同形，json_schema 只差一层嵌套。枢纽存的是**源线原始形状**
// （Request.ResponseFormat 直接挂源树指针），编码侧按目标线重排外壳——内层 `schema` 子树全程
// 是同一个 *Value，不复制（零拷贝基线见 schema_sharing_test.go）。
//
// 为何只做这三种 type：两线 OpenAPI 只共有 text / json_object / json_schema；`grammar` 之类
// 目标线没有等价物，猜不出语义就不写出去——它留在源线 passthrough 里，跨线由 stateful.go 的
// 记损清单兜住（丢的那一侧必须留痕，而不是静默改写）。

// isStructuredOutputFormat 报告一个 format 对象是否可跨线等价互转。
func isStructuredOutputFormat(format *Value) bool {
	if !isRecord(format) {
		return false
	}
	switch kind, _ := stringField(format, "type"); kind {
	case "text", "json_object", "json_schema":
		return true
	default:
		return false
	}
}

// supportsStructuredOutput 报告目标线是否有结构化输出的承载位。
//
// anthropic 线只有 tools 的 input schema，没有「约束本次输出」的请求字段，故那一侧仍记损。
func supportsStructuredOutput(wire WireProtocol) bool {
	return wire == ProtocolOpenAIChat || wire == ProtocolOpenAIResponses
}

// takeChatResponseFormat 取 chat 线的 response_format（形状即 format 对象本身）。
func takeChatResponseFormat(body *Value) *Value {
	format := fieldOrNil(body, "response_format")
	if !isStructuredOutputFormat(format) {
		return nil
	}
	return format
}

// takeResponsesTextFormat 取 responses 线的 text.format（text 是容器，format 才是约束）。
func takeResponsesTextFormat(body *Value) *Value {
	format := fieldOrNil(body, "text", "format")
	if !isStructuredOutputFormat(format) {
		return nil
	}
	return format
}

// renderChatResponseFormat 把枢纽里的格式约束渲染成 chat 线的 response_format。
func renderChatResponseFormat(format *Value) *Value {
	if kind, _ := stringField(format, "type"); kind != "json_schema" {
		return format // text / json_object 两线同形，原样挂源树指针
	}
	if isRecord(fieldOrNil(format, "json_schema")) {
		return format // 已是 chat 形（同线直通）
	}
	nested := NewObject()
	for _, member := range format.Members() {
		if member.Key == "type" {
			continue
		}
		nested.Set(member.Key, member.Value)
	}
	out := NewObject().Set("type", NewString("json_schema"))
	if len(nested.Members()) > 0 {
		out.Set("json_schema", nested)
	}
	return out
}

// renderResponsesTextFormat 把枢纽里的格式约束渲染成 responses 的 text.format。
func renderResponsesTextFormat(format *Value) *Value {
	if kind, _ := stringField(format, "type"); kind != "json_schema" {
		return format
	}
	nested := fieldOrNil(format, "json_schema")
	if !isRecord(nested) {
		return format // 已是 responses 形（同线直通）
	}
	out := NewObject().Set("type", NewString("json_schema"))
	for _, member := range nested.Members() {
		out.Set(member.Key, member.Value)
	}
	return out
}

// reportResidualTextMembers 记 text 容器里除 format 之外成员的损失（目前只有 verbosity）。
//
// 为何逐成员而不整键记：format 已经转出去了，整键记会把成功承载的约束也一并算作丢失；而
// verbosity 是显示偏好，两条目标线都没有等价物，仍须留痕。
func reportResidualTextMembers(text *Value, loss *LossCollector, direction string) {
	if !isRecord(text) {
		return
	}
	for _, member := range text.Members() {
		if member.Key == "format" || !isMeaningful(member.Value) {
			continue
		}
		loss.Dropped(LossTextControls, direction, "text."+member.Key)
	}
}
