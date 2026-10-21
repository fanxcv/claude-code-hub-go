package convert

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// compactFixture 造一份覆盖序列化各分支的 Value：嵌套对象/数组、转义字符、非 ASCII、
// 数字边界值（超长整数 / 指数 / 负小数）、空对象与空数组、null 与布尔。
func compactFixture() *Value {
	return NewObject().
		Set("model", NewString("gpt-5.6")).
		Set("big", NewNumberInt(9223372036854775807)).
		Set("exp", NewNumber("1e21")).
		Set("frac", NewNumber("-0.1")).
		Set("escaped", NewString("引号\"反斜杠\\制表\t换行\n回车\r退格\b换页\f")).
		Set("ascii", NewString("中文 ñ ü")).
		Set("none", NewNull()).
		Set("yes", NewBool(true)).
		Set("emptyArray", NewArray()).
		Set("items", NewArray(
			NewString("a"),
			NewObject().Set("nested", NewArray(NewNumberInt(0), NewBool(false))),
			NewArray(NewNull()),
		)).
		Set("emptyObject", NewObject())
}

// compactFixtureJSON 是 compactFixture 的独立手写期望值：它是本用例的判据，
// 而不是由两条序列化路径互证（否则两条路径一起错也照样绿）。
const compactFixtureJSON = `{"model":"gpt-5.6","big":9223372036854775807,"exp":1e21,"frac":-0.1,` +
	`"escaped":"引号\"反斜杠\\制表\t换行\n回车\r退格\b换页\f","ascii":"中文 ñ ü",` +
	`"none":null,"yes":true,"emptyArray":[],` +
	`"items":["a",{"nested":[0,false]},[null]],"emptyObject":{}}`

// TestMarshalCompactBytesEquivalence 钉住 MarshalCompact（返回 string）与 AppendCompact
// （直接返回 []byte）逐字节同源。
//
// 出站正文改走 AppendCompact 后少了两份全量拷贝，但字节内容不得有丝毫变化：
// 上游按字节比对，多一个空格就是另一份正文。
func TestMarshalCompactBytesEquivalence(t *testing.T) {
	value := compactFixture()

	if got := []byte(value.MarshalCompact()); !bytes.Equal(got, []byte(compactFixtureJSON)) {
		t.Fatalf("MarshalCompact 与期望值不符\n实际 %s\n期望 %s", got, compactFixtureJSON)
	}
	if got := value.AppendCompact(nil); !bytes.Equal(got, []byte(compactFixtureJSON)) {
		t.Fatalf("AppendCompact 与期望值不符\n实际 %s\n期望 %s", got, compactFixtureJSON)
	}
	if got, want := value.AppendCompact(nil), []byte(value.MarshalCompact()); !bytes.Equal(got, want) {
		t.Fatalf("AppendCompact 与 []byte(MarshalCompact()) 不一致\n实际 %s\n期望 %s", got, want)
	}

	// 前缀追加语义：已有内容原样保留，只在末尾接上正文（流式编码依赖它）。
	prefixed := value.AppendCompact([]byte("data: "))
	if want := "data: " + compactFixtureJSON; string(prefixed) != want {
		t.Fatalf("AppendCompact 前缀追加语义不符\n实际 %s\n期望 %s", prefixed, want)
	}
}

// compactBenchValue 造一个约 size 字节的出站正文形状（长字符串为大宗，夹少量嵌套与数字）。
func compactBenchValue(size int) *Value {
	blob := NewString(strings.Repeat("x", 96))
	items := make([]*Value, 0, size/140+1)
	for wrote := 0; wrote < size; wrote += 140 {
		items = append(items, NewObject().
			Set("role", NewString("assistant")).
			Set("content", blob).
			Set("index", NewNumberInt(int64(len(items)))))
	}
	return NewObject().Set("messages", NewArray(items...))
}

// benchCompactSink 防止编译器把序列化结果判为无用值而整段优化掉。
var benchCompactSink []byte

// BenchmarkMarshalCompact 量出站正文的每请求分配：string_roundtrip 是改造前的路径
// （AppendCompact 造 []byte → string → []byte），append_direct 是改造后的路径。
func BenchmarkMarshalCompact(b *testing.B) {
	for _, size := range []int{1 << 20, 5 << 20} {
		value := compactBenchValue(size)
		b.Run(fmt.Sprintf("%dMiB/string_roundtrip", size>>20), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for index := 0; index < b.N; index++ {
				benchCompactSink = []byte(value.MarshalCompact())
			}
		})
		b.Run(fmt.Sprintf("%dMiB/append_direct", size>>20), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(size))
			for index := 0; index < b.N; index++ {
				benchCompactSink = value.AppendCompact(nil)
			}
		})
	}
}
