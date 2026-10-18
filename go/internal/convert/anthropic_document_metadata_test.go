package convert

import "testing"

// 本文件钉住 anthropic 源的三处解码行为：
//  1. `document` 的 source=base64 / url 原样承载为 BlockDocument，source=text 降级为 BlockText；
//  2. `image` / `document` 的 source=file（Files API 引用）记一条「媒体不可达」，且只记一条；
//  3. 顶层 `cache_control` 与 `metadata` 不再静默消失。
//
// 为何要钉：这三处此前都表现为「零台账」——请求里的内容或约束凭空不见，事后无法从 LossReport 判读。

func decodeAnthropicOnly(t *testing.T, body string) DecodeResult[*Request] {
	t.Helper()
	decoded, ok := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, body), ConvertCtx{})
	if !ok {
		t.Fatal("anthropic 正文必须能解码")
	}
	return decoded
}

// blocksOf 取第一条 user 消息里的块序列。
func blocksOf(t *testing.T, request *Request) []Block {
	t.Helper()
	for _, item := range request.Items {
		if item.Kind == ItemMessage && item.Role == "user" {
			return item.Blocks
		}
	}
	t.Fatal("未找到 user 消息")
	return nil
}

func TestAnthropicDocumentSourceForms(t *testing.T) {
	cases := []struct {
		name     string
		block    string
		assertOn func(t *testing.T, block Block)
	}{
		{
			name:  "base64",
			block: `{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0="}}`,
			assertOn: func(t *testing.T, block Block) {
				if block.Kind != BlockDocument || block.Data != "JVBERi0=" || block.MediaType != "application/pdf" {
					t.Fatalf("base64 文档必须原样承载：%+v", block)
				}
			},
		},
		{
			name:  "url",
			block: `{"type":"document","source":{"type":"url","url":"https://example.com/doc.pdf"}}`,
			assertOn: func(t *testing.T, block Block) {
				if block.Kind != BlockDocument || block.URL != "https://example.com/doc.pdf" {
					t.Fatalf("url 文档必须原样承载：%+v", block)
				}
			},
		},
		{
			name:  "text",
			block: `{"type":"document","source":{"type":"text","media_type":"text/plain","data":"plain body"}}`,
			assertOn: func(t *testing.T, block Block) {
				// 纯文本文档无媒体载荷：降级为文本块，而不是记为 document 丢失。
				if block.Kind != BlockText || block.Text != "plain body" {
					t.Fatalf("text 文档必须降级为文本块：%+v", block)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decoded := decodeAnthropicOnly(t, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[`+testCase.block+`]}]}`)
			blocks := blocksOf(t, decoded.Value)
			if len(blocks) != 1 {
				t.Fatalf("应恰好一个块，实际 %+v", blocks)
			}
			testCase.assertOn(t, blocks[0])
		})
	}
}

func countLoss(report LossReport, capability string) int {
	count := 0
	for _, entry := range report.Entries {
		if entry.Capability == capability {
			count++
		}
	}
	return count
}

func TestAnthropicMediaSourceFileIsUnreachable(t *testing.T) {
	cases := []struct {
		name       string
		block      string
		capability string
	}{
		{
			name:       "image",
			block:      `{"type":"image","source":{"type":"file","file_id":"file_1"}}`,
			capability: LossImage,
		},
		{
			name:       "document",
			block:      `{"type":"document","source":{"type":"file","file_id":"file_1"}}`,
			capability: LossDocument,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			decoded := decodeAnthropicOnly(t, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[`+testCase.block+`]}]}`)
			if got := countLoss(decoded.Loss, testCase.capability); got != 1 {
				t.Fatalf("%s 源为 file 时必须恰好一条 %s 损失，实得 %d 条：%+v",
					testCase.name, testCase.capability, got, decoded.Loss.Entries)
			}
			// 不装箱：装箱会让编码器再记一条 opaque dropped，同一件事两笔台账。
			// 全部块被丢时 user 消息本身也不产出，故直接扫全部 item。
			for _, item := range decoded.Value.Items {
				for _, block := range item.Blocks {
					if block.Kind != BlockText {
						t.Fatalf("不可达的媒体必须直接丢弃，不得装箱：%+v", block)
					}
				}
			}
		})
	}
}

func TestAnthropicTopLevelCacheControlRecorded(t *testing.T) {
	decoded := decodeAnthropicOnly(t, `{"model":"m","max_tokens":64,"cache_control":{"type":"ephemeral"},`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	entries := []LossEntry{}
	for _, entry := range decoded.Loss.Entries {
		if entry.Capability == LossCacheControl {
			entries = append(entries, entry)
		}
	}
	if len(entries) != 1 || entries[0].Detail != "cache_control" {
		t.Fatalf("顶层 cache_control 必须恰好一条 LossCacheControl detail=cache_control，实际：%+v",
			decoded.Loss.Entries)
	}

	// 无该键时不得凭空记损。
	plain := decodeAnthropicOnly(t, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if got := countLoss(plain.Loss, LossCacheControl); got != 0 {
		t.Fatalf("未给顶层 cache_control 不得记损，实得 %d 条：%+v", got, plain.Loss.Entries)
	}
}

func TestAnthropicMetadataUserIDDecoded(t *testing.T) {
	decoded := decodeAnthropicOnly(t, `{"model":"m","max_tokens":64,"metadata":{"user_id":"u-42"},`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	userID, ok := decoded.Value.Metadata.StringField("user_id")
	if !ok || userID != "u-42" {
		t.Fatalf("metadata.user_id 必须解进枢纽，实际 %+v", decoded.Value.Metadata)
	}

	plain := decodeAnthropicOnly(t, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if plain.Value.Metadata != nil {
		t.Fatalf("未给 metadata 时枢纽字段必须为 nil，实际 %+v", plain.Value.Metadata)
	}
}
