package convert

import "strings"

// 本文件是音频、视频与内联文档跨线承载的公共件（#13）。
//
// 三条线的媒体分片形状不同、承载位也不同：chat 用 `input_audio` / `video_url` / `file`，
// responses 用 `input_audio` / `input_video` / `input_file`，anthropic 只有 image / document。
// 枢纽因此只保留三样事实——媒体类型、内联数据、远程地址（BlockAudio / BlockVideo /
// BlockDocument），**不新增 Block 字段**：块必须守在 192 字节这一档 size class。
//
// 原始分片同时留在 Block.Value + Block.Wire 上：同线编码器原样回写它，故同线往返逐字节不变；
// 跨线编码器忽略它，按媒体字段重建目标线形态（形状差异与丢失都记在解码侧）。

// mediaBlock 把解码时的原始分片记进媒体块（见文件头）。
func mediaBlock(block Block, wire WireProtocol, part *Value) Block {
	block.Wire = wire
	block.Value = part
	return block
}

// sameLineMediaPart 取媒体块在同线上的原始分片；跨线或没有原始分片时返回 nil。
func sameLineMediaPart(block Block, wire WireProtocol) *Value {
	if block.Wire != wire {
		return nil
	}
	return block.Value
}

// audioMediaType 把 chat / responses 的音频 `format`（wav / mp3 / …）折成媒体类型。
//
// 枢纽不为音频单开字段：它与图片同形（内联 base64），format 只是媒体类型的子类型后缀。
func audioMediaType(format string) string { return "audio/" + format }

// audioFormat 是 audioMediaType 的反向；非 audio/* 返回空，编码侧据此记损。
func audioFormat(mediaType string) string {
	if !strings.HasPrefix(mediaType, "audio/") {
		return ""
	}
	return mediaType[len("audio/"):]
}

// mediaAudioFields 取音频块送给目标线的 (format, data)；送不出时记损。
//
// 两条 OpenAI 线的音频分片都只有内联 base64 一种形态（没有 URL 形态），故缺 data 即无处可送。
func mediaAudioFields(block Block, loss *LossCollector, direction string) (string, string, bool) {
	format := audioFormat(block.MediaType)
	switch {
	case block.Data == "":
		loss.Dropped(LossMedia, direction, "audio.url_only")
		return "", "", false
	case format == "":
		loss.Dropped(LossMedia, direction, "audio.media_type")
		return "", "", false
	}
	return format, block.Data, true
}

// mediaVideoURL 取视频块的远程地址；没有地址时记损（视频只有 URL 形态，内联视频不走本枢纽）。
func mediaVideoURL(block Block, loss *LossCollector, direction string) (string, bool) {
	if block.URL == "" {
		loss.Dropped(LossMedia, direction, "video.no_url")
		return "", false
	}
	return block.URL, true
}

// mediaDocumentFields 取文档块送给目标线的 (媒体类型, 裸 base64)；没有内联数据时记损。
//
// 远程文档（anthropic 的 source.type=url）在两条 OpenAI 线上没有取证过的承载位，故只送内联数据。
func mediaDocumentFields(block Block, loss *LossCollector, direction string) (string, string, bool) {
	if block.Data == "" {
		loss.Dropped(LossMedia, direction, "document.no_data")
		return "", "", false
	}
	mediaType := block.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return mediaType, block.Data, true
}

// documentInlineData 把内联文档的 data URL 解成 (媒体类型, 裸 base64)。
//
// 只认 data URL：两条 OpenAI 线的 `file_data` 都以 data URL 表达，裸 base64 没有媒体类型
// 就无法在 anthropic 侧写出 document.source，故记损而不猜（猜错换来的是上游 400）。
func documentInlineData(fileData string) (string, string, bool) {
	parsed, ok := parseChatDataURL(fileData)
	if !ok {
		return "", "", false
	}
	return parsed.MediaType, parsed.Data, true
}

// documentExtensions 把内联文档的媒体类型映射为 chat file 分片的展示用扩展名。
var documentExtensions = map[string]string{
	"application/json": "json",
	"application/pdf":  "pdf",
	"text/csv":         "csv",
	"text/markdown":    "md",
	"text/plain":       "txt",
}

// documentFilename 合成 chat file 分片要求的展示用文件名。
//
// 枢纽不承载文件名：它只是展示标签，三条线各自写法不同；同线往返靠原始分片，不经本函数。
func documentFilename(mediaType string) string {
	if extension, ok := documentExtensions[mediaType]; ok {
		return "document." + extension
	}
	return "document.bin"
}

// videoURLOf 取 `{url}` 对象或裸字符串两种写法的视频地址；对象里的其余成员记损。
func videoURLOf(raw *Value, loss *LossCollector, direction string) string {
	switch {
	case raw == nil:
		return ""
	case raw.Kind() == "string":
		url, _ := raw.String()
		return url
	case raw.IsObject():
		url, _ := stringField(raw, "url")
		for _, extra := range meaningfulExtras(raw, "url") {
			loss.Dropped(LossMedia, direction, "video."+extra)
		}
		return url
	}
	return ""
}

// anthropicDocumentMediaTypes 是 anthropic document 块被保证接受的媒体类型。
//
// 外线带进来的文档只在命中本表时才写出：送一个本线不认的 media_type 换来的是上游 400，
// 而 400 计入供应商熔断（形状缺陷被记成供应商失败）。
var anthropicDocumentMediaTypes = map[string]bool{
	"application/pdf": true,
	"text/plain":      true,
}

// anthropicAcceptsForeignDocument 报告文档块能否在本线承载：只闸外线来的块。
//
// anthropic 自己的文档不做闸门——那是客户端的声明，原样透传（与 image 分支同口径）。
func anthropicAcceptsForeignDocument(block Block) bool {
	if block.Wire == anthropicWire || block.Wire == "" || block.Data == "" {
		return true
	}
	return anthropicDocumentMediaTypes[block.MediaType]
}

// mediaUnreachableDetail 是「目标线没有该媒体的承载位」的记损 detail：<kind>.unreachable:<来源线>。
//
// 来源线取自解码时记在块上的 Wire。台账必须能分辨音频是从哪条线来的——chat 的 input_audio 是
// 嵌套对象、responses 的是平铺字段，排查时不能混。
func mediaUnreachableDetail(block Block) string {
	source := string(block.Wire)
	if source == "" {
		source = "unknown"
	}
	return string(block.Kind) + ".unreachable:" + source
}
