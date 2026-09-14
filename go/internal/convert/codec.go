package convert

// 三线编解码的统一入口。与 TS 侧 index.ts 的 getCodec(protocol) 一一对应。

// DecodeRequest 把出站请求体解为枢纽请求。
func DecodeRequest(protocol WireProtocol, body *Value, ctx ConvertCtx) (DecodeResult[*Request], bool) {
	switch protocol {
	case ProtocolAnthropicMessages:
		return decodeAnthropicRequest(body, ctx), true
	case ProtocolOpenAIChat:
		return decodeChatRequest(body, ctx), true
	case ProtocolOpenAIResponses:
		return decodeResponsesRequest(body, ctx), true
	default:
		return DecodeResult[*Request]{}, false
	}
}

// EncodeRequest 把枢纽请求编码为入站请求体。
func EncodeRequest(protocol WireProtocol, request *Request, ctx ConvertCtx) (EncodeResult, bool) {
	switch protocol {
	case ProtocolAnthropicMessages:
		return encodeAnthropicRequest(request, ctx), true
	case ProtocolOpenAIChat:
		return encodeChatRequest(request, ctx), true
	case ProtocolOpenAIResponses:
		return encodeResponsesRequest(request, ctx), true
	default:
		return EncodeResult{}, false
	}
}

// DecodeResponse 把上游响应体解为枢纽响应。
//
// Gemini 只在此处被认识（取用量与上游模型名），不在 DecodeRequest / Encode* 里：
// 它不进转换矩阵，只借同一条只读路径给记账与「上游实际模型」用。理由见 codec_gemini.go。
func DecodeResponse(protocol WireProtocol, body *Value, ctx ConvertCtx) (DecodeResult[*Response], bool) {
	switch protocol {
	case ProtocolAnthropicMessages:
		return decodeAnthropicResponse(body, ctx), true
	case ProtocolOpenAIChat:
		return decodeChatResponse(body, ctx), true
	case ProtocolOpenAIResponses:
		return decodeResponsesResponse(body, ctx), true
	case ProtocolGemini:
		return decodeGeminiResponse(body, ctx), true
	default:
		return DecodeResult[*Response]{}, false
	}
}

// EncodeResponse 把枢纽响应编码为客户端响应体。
func EncodeResponse(protocol WireProtocol, response *Response, ctx ConvertCtx) (EncodeResult, bool) {
	switch protocol {
	case ProtocolAnthropicMessages:
		return encodeAnthropicResponse(response, ctx), true
	case ProtocolOpenAIChat:
		return encodeChatResponse(response, ctx), true
	case ProtocolOpenAIResponses:
		return encodeResponsesResponse(response, ctx), true
	default:
		return EncodeResult{}, false
	}
}
