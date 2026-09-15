package rectify

import (
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 占位签名剥离：客户端把上一轮我们补的占位签名带回来时，必须在**发送前**拿掉——
// 真实 Anthropic 上游会校验签名，回传上去只会换来一次 400（还要白烧一个熔断失败计数）。
// 用例形状按「客户端多轮请求」造：assistant 轮里带着我们上一轮发的 thinking 块。

// TestStripPlaceholderSignatureRemovesOwnPlaceholderBlocks 钉住主路径：自家占位签名的思考块整块
// 丢弃（无签名的 thinking 块对 Anthropic 上游同样非法，与被动整流器最终做的事一致）。
func TestStripPlaceholderSignatureRemovesOwnPlaceholderBlocks(t *testing.T) {
	body := mustParse(t, `{"model":"claude","messages":[`+
		`{"role":"assistant","content":[`+
		`{"type":"thinking","thinking":"上一轮的思考","signature":"`+convert.PlaceholderThinkingSignature()+`"},`+
		`{"type":"text","text":"上一轮的回答"}]},`+
		`{"role":"user","content":[{"type":"text","text":"继续"}]}]}`)

	fields, applied := StripPlaceholderSignature(body)

	if !applied {
		t.Fatal("带自家占位签名时应判为已剥离")
	}
	if got := intField(t, fields, "removedPlaceholderThinkingBlocks"); got != 1 {
		t.Fatalf("removedPlaceholderThinkingBlocks 期望 1，收到 %d", got)
	}
	if got := compact(t, body); strings.Contains(got, convert.PlaceholderThinkingSignature()) {
		t.Fatalf("出站正文仍含占位签名：%s", got)
	}
	want := `{"model":"claude","messages":[` +
		`{"role":"assistant","content":[{"type":"text","text":"上一轮的回答"}]},` +
		`{"role":"user","content":[{"type":"text","text":"继续"}]}]}`
	if got := compact(t, body); got != want {
		t.Fatalf("剥离结果不对:\n got %s\nwant %s", got, want)
	}
}

// TestStripPlaceholderSignatureKeepsRealSignatures 钉住反向边界：上游真实签名一个都不许动。
func TestStripPlaceholderSignatureKeepsRealSignatures(t *testing.T) {
	source := `{"model":"claude","messages":[{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"真实思考","signature":"Ereal-upstream-signature"},` +
		`{"type":"redacted_thinking","data":"Ereal-redacted-data"}]}]}`
	body := mustParse(t, source)

	fields, applied := StripPlaceholderSignature(body)

	if applied {
		t.Fatal("真实签名不应被剥离")
	}
	if got := intField(t, fields, "removedPlaceholderThinkingBlocks"); got != 0 {
		t.Fatalf("removedPlaceholderThinkingBlocks 期望 0，收到 %d", got)
	}
	if got := compact(t, body); got != source {
		t.Fatalf("正文被改动:\n got %s\nwant %s", got, source)
	}
}

// TestStripPlaceholderSignatureIgnoresNonAnthropicShapes 钉住形状边界：非 Anthropic 形状
// （content 是字符串、块不是对象、没有 messages）一律不动，也不误报已剥离。
func TestStripPlaceholderSignatureIgnoresNonAnthropicShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"content 为字符串", `{"messages":[{"role":"assistant","content":"thinking"}]}`},
		{"块不是对象", `{"messages":[{"role":"assistant","content":["thinking"]}]}`},
		{"没有 messages", `{"input":"x"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := mustParse(t, testCase.body)
			_, applied := StripPlaceholderSignature(body)
			if applied {
				t.Fatalf("不该判为已剥离：%s", testCase.body)
			}
			if got := compact(t, body); got != testCase.body {
				t.Fatalf("正文被改动:\n got %s\nwant %s", got, testCase.body)
			}
		})
	}
}

// TestStripPlaceholderSignatureRoundTripCoversEmittedSignature 是**端到端**断言：把转换层真正
// 产出的形状（含占位签名的 thinking 块）喂进剥离器，出站字节里不得再出现该签名。
//
// 为什么单列一条：前面两条用的是手写夹具，这条用的是 convert 的常量，保证「产出的」与
// 「剥离的」是同一个值——两处若各写一份，漏剥会在生产表现为「偶发 400」而不是测试红。
func TestStripPlaceholderSignatureRoundTripCoversEmittedSignature(t *testing.T) {
	signature := convert.PlaceholderThinkingSignature()
	body := mustParse(t, `{"model":"deepseek-v4-flash","messages":[`+
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"`+signature+`"}]}]}`)

	if _, applied := StripPlaceholderSignature(body); !applied {
		t.Fatal("转换层产出的占位签名必须能被剥离")
	}
	if strings.Contains(compact(t, body), signature) {
		t.Fatal("出站正文仍含占位签名")
	}
}
