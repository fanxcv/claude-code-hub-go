package convert

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// probeKey 是流式判定读的那个顶层成员名（forward 侧的实际用法）。
const probeKey = "stream"

// referenceTopLevelBoolTrue 是被测函数的语义基准：**权威实现**（ParseJSON + Get + Bool）。
//
// 快速路径的每一处判据都以它为准，对拍用例是两者等价性的唯一防线。
func referenceTopLevelBoolTrue(data []byte, key string) bool {
	value, err := ParseJSON(data)
	if err != nil {
		return false
	}
	field, ok := value.Get(key)
	if !ok {
		return false
	}
	requested, _ := field.Bool()
	return requested
}

func assertProbeAgrees(t *testing.T, body []byte) {
	t.Helper()
	got := TopLevelBoolTrue(body, probeKey)
	want := referenceTopLevelBoolTrue(body, probeKey)
	if got != want {
		t.Fatalf("快速路径与权威实现分歧：body=%q 快速=%v 权威=%v", body, got, want)
	}
}

// TestTopLevelBoolTrueBoundaryShapes 逐条覆盖快速路径的每个分支与每个「非法」判据。
//
// want 是显式期望值（不只对拍）：若两个实现一起错成同一个值，本表仍能发现。
func TestTopLevelBoolTrueBoundaryShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{name: "顶层真值", body: `{"stream":true}`, want: true},
		{name: "顶层假值", body: `{"stream":false}`, want: false},
		{name: "空对象", body: `{}`, want: false},
		{name: "无该成员", body: `{"other":1}`, want: false},
		{name: "键大小写不符", body: `{"Stream":true}`, want: false},
		{name: "字符串真值不算", body: `{"stream":"true"}`, want: false},
		{name: "数字真值不算", body: `{"stream":1}`, want: false},
		{name: "null 不算", body: `{"stream":null}`, want: false},
		{name: "数组不算", body: `{"stream":[true]}`, want: false},
		{name: "对象不算", body: `{"stream":{"v":true}}`, want: false},
		{name: "嵌套里的同名键不算", body: `{"a":{"stream":true}}`, want: false},
		{name: "数组元素里的同名键不算", body: `{"a":[{"stream":true}]}`, want: false},
		{name: "真值在前", body: `{"stream":true,"a":1}`, want: true},
		{name: "真值在后", body: `{"a":1,"stream":true}`, want: true},
		{name: "重复键后者覆盖为假", body: `{"stream":true,"stream":false}`, want: false},
		{name: "重复键后者覆盖为真", body: `{"stream":false,"stream":true}`, want: true},
		{name: "重复键后者为非布尔", body: `{"stream":true,"stream":"x"}`, want: false},
		{name: "重复键前者为非布尔", body: `{"stream":"x","stream":true}`, want: true},
		{name: "允许空白", body: " \n\t{\"stream\" : true }\r\n ", want: true},
		{name: "转义键需解码后比较", body: `{"\u0073tream":true}`, want: true},
		{name: "转义键不相等", body: `{"\u0074tream":true}`, want: false},
		{name: "代理对键", body: `{"\ud83d\ude00":true}`, want: false},
		{name: "值侧含转义不影响", body: `{"stream":true,"a":"\n\u00e9"}`, want: true},
		{name: "非法转义使整体非法", body: `{"stream":true,"a":"\x"}`, want: false},
		{name: "不完整 u 转义使整体非法", body: `{"stream":true,"a":"\u00"}`, want: false},
		{name: "数字宽松接受加号", body: `{"stream":true,"a":+1}`, want: true},
		{name: "非法数字使整体非法", body: `{"stream":true,"a":1.2.3}`, want: false},
		{name: "前导零数字合法", body: `{"stream":true,"a":01}`, want: true},
		{name: "指数数字合法", body: `{"stream":true,"a":1e5}`, want: true},
		{name: "空输入", body: ``, want: false},
		{name: "只有空白", body: "  ", want: false},
		{name: "未闭合对象", body: `{"stream":true`, want: false},
		{name: "末尾多余内容", body: `{"stream":true}x`, want: false},
		{name: "末尾多余空白可接受", body: `{"stream":true} `, want: true},
		{name: "字面量截断", body: `{"stream":tru}`, want: false},
		{name: "字面量大小写不符", body: `{"stream":TRUE}`, want: false},
		{name: "尾随逗号", body: `{"a":1,}`, want: false},
		{name: "单引号", body: `{'a':1}`, want: false},
		{name: "顶层数组", body: `[1,2]`, want: false},
		{name: "顶层字符串", body: `"x"`, want: false},
		{name: "顶层数字", body: `123`, want: false},
		{name: "顶层 true", body: `true`, want: false},
		{name: "顶层 null", body: `null`, want: false},
		{name: "数组未闭合", body: `{"stream":true,"a":[1,2}`, want: false},
		{name: "数组内期待逗号", body: `{"stream":true,"a":[1 2]}`, want: false},
		{name: "对象内缺冒号", body: `{"stream" true}`, want: false},
		{name: "深层嵌套真值仍为真", body: `{"stream":true,"a":[[[[{"b":[1,2,3]}]]]]}`, want: true},
		{name: "裸控制字符沿用快路径宽松", body: "{\"stream\":true,\"a\":\"裸\n换行\"}", want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(testCase.body)
			got := TopLevelBoolTrue(body, probeKey)
			if got != testCase.want {
				t.Fatalf("TopLevelBoolTrue(%q) = %v，期望 %v", testCase.body, got, testCase.want)
			}
			assertProbeAgrees(t, body)
		})
	}
}

// TestTopLevelBoolTrueExhaustiveSmallInputs 穷举所有长度 ≤4 的小输入，逐一与权威实现对拍。
//
// 字母表覆盖 JSON 的全部语法元字符（括号、引号、冒号、逗号、字面量首字母、数字、空白），
// 故这一族能抓到「接受域比权威实现宽/严」这类最难自查的分歧。
func TestTopLevelBoolTrueExhaustiveSmallInputs(t *testing.T) {
	alphabet := []byte(`{}[]",:tfn0 `)
	const maxLen = 4
	buffer := make([]byte, 0, maxLen)
	checked := 0
	var walk func(depth int)
	walk = func(depth int) {
		if depth == maxLen {
			return
		}
		for _, symbol := range alphabet {
			buffer = append(buffer, symbol)
			assertProbeAgrees(t, buffer)
			checked++
			walk(depth + 1)
			buffer = buffer[:len(buffer)-1]
		}
	}
	walk(0)
	if checked < 10000 {
		t.Fatalf("穷举例数异常偏少（%d），穷举未真正展开", checked)
	}
	t.Logf("小输入穷举对拍 %d 例", checked)
}

// TestTopLevelBoolTrueKeyShapeFamilies 在「键 + 值 + 尾部」的笛卡尔积上对拍。
//
// 小输入穷举里不会出现 "stream" 这个键名，故单开这一族把真值分支也穷举到。
func TestTopLevelBoolTrueKeyShapeFamilies(t *testing.T) {
	prefixes := []string{`{`, `{ `, `{"stream"`, `{"stream" `, `{"stream":`, ` { "stream" : `}
	values := []string{`true`, `false`, `null`, `1`, `"true"`, `[1]`, `{}`, `tru`, `TRUE`, `+1`, `01`, `1e5`, `1.2.3`}
	suffixes := []string{`}`, ` }`, `} `, `,}`, `}x`, ``, `,"a":1}`, `,"stream":true}`, `,"stream":false}`, `,"stream":"x"}`}
	checked := 0
	for _, prefix := range prefixes {
		for _, value := range values {
			for _, suffix := range suffixes {
				body := []byte(prefix + value + suffix)
				assertProbeAgrees(t, body)
				checked++
			}
		}
	}
	t.Logf("键形态族对拍 %d 例", checked)
}

// TestTopLevelBoolTrueGoldenFiles 用仓库里既有的 JSON 样本做对拍（存在才跑）。
//
// 注：协议一致性语料目录（tests/load/protocol-conformance/corpus）已随 Node 退役删除，
// 故这里取 go/testdata 下的既有样本；样本不是请求体也无妨——对拍只看两实现是否一致。
func TestTopLevelBoolTrueGoldenFiles(t *testing.T) {
	patterns := []string{"../testdata/golden/*.json", "../testdata/*.json"}
	found := 0
	for _, pattern := range patterns {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("展开 %s 失败: %v", pattern, err)
		}
		for _, path := range paths {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读取 %s 失败: %v", path, err)
			}
			assertProbeAgrees(t, raw)
			found++
		}
	}
	t.Logf("既有 JSON 样本对拍 %d 个文件", found)
}

// TestParseJSONDoesNotPanicOnTruncatedInput 钉住「截断输入返回错误，而不是 panic」。
//
// 这是写快速路径对拍时抓到的既有缺陷：parseString 是 parseObject 读成员名的入口，
// 而对象在读到成员名之前只做过空白检查，于是 `{` 这类输入会让 p.pos 等于长度、
// `p.input[p.pos]` 越界。正文直接来自网络，故这条路径可达。
func TestParseJSONDoesNotPanicOnTruncatedInput(t *testing.T) {
	inputs := []string{`{`, `{ `, `{"a"`, `{"a":`, `{"a":1,`, `{"a":1,"b"`, `[`, `[1,`, `{"a":{"b"`}
	for _, input := range inputs {
		t.Run(input, func(t *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Fatalf("ParseJSON(%q) panic 了：%v", input, recovered)
				}
			}()
			if _, err := ParseJSON([]byte(input)); err == nil {
				t.Fatalf("ParseJSON(%q) 应返回错误", input)
			}
			if TopLevelBoolTrue([]byte(input), probeKey) {
				t.Fatalf("TopLevelBoolTrue(%q) 应判否", input)
			}
		})
	}
}

// sampleStreamingBody 造一个近似真实的流式请求体（几十 KiB）：
// 体积来自工具表与长消息，形态与生产里那些触发该判定的请求同类。
func sampleStreamingBody() []byte {
	var builder strings.Builder
	builder.WriteString(`{"model":"gpt-4o","stream":true,"tools":[`)
	for index := 0; index < 40; index++ {
		if index > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"type":"function","function":{"name":"tool_` + strconv.Itoa(index) + `","parameters":{"type":"object","properties":{`)
		for property := 0; property < 20; property++ {
			if property > 0 {
				builder.WriteString(",")
			}
			builder.WriteString(`"p` + strconv.Itoa(property) + `":{"type":"string","description":"这是一个较长的中文说明文字，用来撑大正文体积。"}`)
		}
		builder.WriteString(`}}}}`)
	}
	builder.WriteString(`],"messages":[`)
	for index := 0; index < 40; index++ {
		if index > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"role":"user","content":"` + strings.Repeat("内容", 100) + `"}`)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func TestSampleStreamingBodyIsStreaming(t *testing.T) {
	body := sampleStreamingBody()
	if len(body) < 32*1024 {
		t.Fatalf("样本体积偏小（%d 字节），不足以体现分配差异", len(body))
	}
	if !TopLevelBoolTrue(body, probeKey) {
		t.Fatal("样本应判为流式")
	}
	if fast, decided := probeTopLevelBoolTrue(body, probeKey); !decided || !fast {
		t.Fatalf("样本本应走快速路径并判真（decided=%v result=%v）", decided, fast)
	}
}

// sampleEscapedBody 造一个以转义字符串为主的正文：多行文本、制表符与引号在真实请求里
// 最常见，而它们都要走 parseStringEscaped 的慢路径（含反斜杠即不再走快路径）。
func sampleEscapedBody() []byte {
	var builder strings.Builder
	builder.WriteString(`{"model":"gpt-4o","messages":[`)
	for index := 0; index < 200; index++ {
		if index > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(`{"role":"user","content":"第一行\n第二行\t含制表符\"引号\"与反斜杠\\号收尾"}`)
	}
	builder.WriteString(`]}`)
	return []byte(builder.String())
}

func TestSampleEscapedBodyActuallyEscapes(t *testing.T) {
	body := sampleEscapedBody()
	value, err := ParseJSON(body)
	if err != nil {
		t.Fatalf("样本应可解析: %v", err)
	}
	messages, ok := value.Get("messages")
	if !ok {
		t.Fatal("样本应含 messages")
	}
	items := messages.Items()
	if len(items) == 0 {
		t.Fatal("样本应含首条消息")
	}
	content, ok := items[0].StringField("content")
	if !ok {
		t.Fatal("样本首条消息应含 content")
	}
	// 转义被解开才算真的走了慢路径（快路径会把 \n 原样留下）。
	if !strings.Contains(content, "\n") || !strings.Contains(content, "\t") {
		t.Fatalf("样本内容未见转义展开：%q", content)
	}
}

// BenchmarkParseEscapedStrings 量 parseStringEscaped 的分配：改前从 0 几何增长，
// 改后一次预分配到位。
func BenchmarkParseEscapedStrings(b *testing.B) {
	body := sampleEscapedBody()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for index := 0; index < b.N; index++ {
		if _, err := ParseJSON(body); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkProbeParseJSON 是优化前口径：为读一个布尔建整棵树。
func BenchmarkProbeParseJSON(b *testing.B) {
	body := sampleStreamingBody()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for index := 0; index < b.N; index++ {
		if !referenceTopLevelBoolTrue(body, probeKey) {
			b.Fatal("样本应判为流式")
		}
	}
}

// BenchmarkProbeFastPath 是优化后口径：零分配扫描。
func BenchmarkProbeFastPath(b *testing.B) {
	body := sampleStreamingBody()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for index := 0; index < b.N; index++ {
		if !TopLevelBoolTrue(body, probeKey) {
			b.Fatal("样本应判为流式")
		}
	}
}
