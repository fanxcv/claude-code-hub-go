package guard

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住「Store 只改被改动的字节」这条纪律。
//
// 为什么值得钉：出站字节决定上游前缀缓存命中率（见 forward/plan.go 的 rewriteModelField 与
// ProviderOverrideApplier.Apply 的同一条理由）。守卫链在 BuildPlan 之前跑，它一旦把整份正文
// 重新塑形，转发层再小心也白费。

// newBodyAccessorForTest 造一个带指定正文的访问器。
func newBodyAccessorForTest(tb testing.TB, payload string) *BodyAccessor {
	tb.Helper()
	header := pctx.Init{Method: "POST", Path: "/v1/messages", Body: io.NopCloser(strings.NewReader(payload))}
	ctx, err := pctx.New(header)
	if err != nil {
		tb.Fatalf("构造上下文失败: %v", err)
	}
	accessor, err := NewBodyAccessor(ctx, BodyAccessOptions{})
	if err != nil {
		tb.Fatalf("构造正文访问器失败: %v", err)
	}
	return accessor
}

// storeAndBytes 取树、按 mutate 改写、Store 后取字节。
func storeAndBytes(t *testing.T, accessor *BodyAccessor, mutate func(tree map[string]any)) []byte {
	t.Helper()
	tree, err := accessor.JSON()
	if err != nil {
		t.Fatalf("取树失败: %v", err)
	}
	mutate(tree)
	if err := accessor.Store(tree); err != nil {
		t.Fatalf("写回失败: %v", err)
	}
	bytes, err := accessor.Bytes()
	if err != nil {
		t.Fatalf("取字节失败: %v", err)
	}
	return bytes
}

// assertValidJSONObject 校验字节仍是合法 JSON 对象。
func assertValidJSONObject(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	var tree map[string]any
	if err := json.Unmarshal(payload, &tree); err != nil {
		t.Fatalf("出站字节不是合法 JSON 对象: %v（%s）", err, payload)
	}
	return tree
}

// TestBodyAccessorUnstoredBytesAreOriginal 钉住正确性基线：未 Store 时 Bytes 逐字节等于入站原文。
func TestBodyAccessorUnstoredBytesAreOriginal(t *testing.T) {
	// 刻意带上 marshal 会失真的三类形态：HTML 敏感字符、超 float64 精度的大整数、非字典序键。
	payload := `{"zeta":"a < b && c > d","messages":[{"role":"user","content":"<div>&amp;</div>"}],` +
		`"big":12345678901234567890,"alpha":1.0,"beta":1e21}`
	accessor := newBodyAccessorForTest(t, payload)

	got, err := accessor.Bytes()
	if err != nil {
		t.Fatalf("取字节失败: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("未 Store 时必须逐字返回原文\n得到 %q\n期望 %q", got, payload)
	}

	// 就地改树但不 Store：出站仍是原文（与旧实现同语义，改动未写回即不可见）。
	tree, err := accessor.JSON()
	if err != nil {
		t.Fatalf("取树失败: %v", err)
	}
	tree["alpha"] = 2.0
	again, err := accessor.Bytes()
	if err != nil {
		t.Fatalf("取字节失败: %v", err)
	}
	if string(again) != payload {
		t.Fatalf("未 Store 的改动不该影响出站字节\n得到 %q", again)
	}
}

// TestBodyAccessorPatchesOnlyChangedTopLevelKey 钉住核心纪律：Store 一个顶层键后，只有那个键的值变。
func TestBodyAccessorPatchesOnlyChangedTopLevelKey(t *testing.T) {
	const head = `{"zeta":"a < b && c > d",`
	const messages = `"messages":[{"role":"user","content":"if a < b && c > d then 1 <= 100"}],`
	const tail = `"big":12345678901234567890,"alpha":1.0,"beta":1e21}`
	accessor := newBodyAccessorForTest(t, head+messages+tail)

	got := storeAndBytes(t, accessor, func(tree map[string]any) {
		tree["prompt_cache_key"] = "sess-abc"
	})

	// 追加键落在末尾，别的键连字节一起原样保留。
	want := head + messages + tail[:len(tail)-1] + `,"prompt_cache_key":"sess-abc"}`
	if string(got) != want {
		t.Fatalf("只有被改动的键该变\n得到 %q\n期望 %q", got, want)
	}
	// 三类失真都不许出现。
	for _, forbidden := range []string{`\u003c`, `\u003e`, `\u0026`, `12345678901234567000`} {
		if bytes.Contains(got, []byte(forbidden)) {
			t.Fatalf("出站字节含失真形态 %q: %s", forbidden, got)
		}
	}
	if !bytes.Contains(got, []byte(`"big":12345678901234567890`)) {
		t.Fatalf("大整数丢了原写法: %s", got)
	}
	if !bytes.Contains(got, []byte(`"alpha":1.0`)) || !bytes.Contains(got, []byte(`"beta":1e21`)) {
		t.Fatalf("数字写法被重排: %s", got)
	}
	if bytes.Index(got, []byte(`"zeta"`)) > bytes.Index(got, []byte(`"messages"`)) {
		t.Fatalf("键序被重排: %s", got)
	}
}

// TestBodyAccessorPatchesValueInPlace 钉住替换（而非追加）：改动一个已存在的顶层标量键。
func TestBodyAccessorPatchesValueInPlace(t *testing.T) {
	payload := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"a<b && c>d"}],"big":12345678901234567890}`
	accessor := newBodyAccessorForTest(t, payload)

	got := storeAndBytes(t, accessor, func(tree map[string]any) {
		tree["model"] = "claude-opus-4-1"
	})

	want := `{"model":"claude-opus-4-1","messages":[{"role":"user","content":"a<b && c>d"}],"big":12345678901234567890}`
	if string(got) != want {
		t.Fatalf("替换应只动该键的值\n得到 %q\n期望 %q", got, want)
	}
}

// TestBodyAccessorPatchesNestedPath 钉住嵌套改动的定位：只重写被改的那个成员里的那个子键。
func TestBodyAccessorPatchesNestedPath(t *testing.T) {
	payload := `{"model":"m","metadata":{"user_id":"","client":"cli <x>","n":12345678901234567890},` +
		`"messages":[{"role":"user","content":"a<b"}],"zeta":1}`
	accessor := newBodyAccessorForTest(t, payload)

	got := storeAndBytes(t, accessor, func(tree map[string]any) {
		metadata, _ := tree["metadata"].(map[string]any)
		metadata["user_id"] = "user_x_session_y"
	})
	tree := assertValidJSONObject(t, got)

	metadata, _ := tree["metadata"].(map[string]any)
	if metadata["user_id"] != "user_x_session_y" {
		t.Fatalf("嵌套改动没生效: %v", metadata)
	}
	// 兄弟子键的值重编码但键名与键序照原文；其它顶层成员逐字不动。
	if !bytes.Contains(got, []byte(`"metadata":{"user_id":"user_x_session_y","client":"cli <x>","n":12345678901234567890}`)) {
		t.Fatalf("嵌套改动定位不对或波及了兄弟子键: %s", got)
	}
	if !bytes.HasSuffix(got, []byte(`"messages":[{"role":"user","content":"a<b"}],"zeta":1}`)) {
		t.Fatalf("改动一个成员不该波及别的顶层成员: %s", got)
	}
}

// TestBodyAccessorPatchAddsNestedSubkey 是生产现场的形态：metadata 已在、只缺 user_id。
//
// 这一步是 P0-2 的原形：注入成功后 metadata 多一个子键，而 messages 里的整段正文必须逐字不变，
// 否则同一会话里「客户端自带 user_id」与「网关补 user_id」的请求前缀不同，上游缓存段作废。
func TestBodyAccessorPatchAddsNestedSubkey(t *testing.T) {
	payload := `{"model":"m","metadata":{"client":"cli"},"messages":[{"content":"a<b && c>d","n":12345678901234567890}],"zeta":1}`
	accessor := newBodyAccessorForTest(t, payload)

	got := storeAndBytes(t, accessor, func(tree map[string]any) {
		metadata, _ := tree["metadata"].(map[string]any)
		metadata["user_id"] = "user_x_session_y"
	})

	want := `{"model":"m","metadata":{"client":"cli","user_id":"user_x_session_y"},` +
		`"messages":[{"content":"a<b && c>d","n":12345678901234567890}],"zeta":1}`
	if string(got) != want {
		t.Fatalf("嵌套加子键不该波及别的字节\n得到 %q\n期望 %q", got, want)
	}
}

// TestBodyAccessorPatchPreservesWhitespaceAndOrder 钉住原文的空白与键序原样保留。
func TestBodyAccessorPatchPreservesWhitespaceAndOrder(t *testing.T) {
	payload := "{\n  \"zeta\" : \"<x>\",\n  \"messages\" : [ {\"role\":\"user\"} ],\n  \"alpha\" : 12345678901234567890\n}"
	accessor := newBodyAccessorForTest(t, payload)

	// 只加一个键：原有三个成员连空白一起照抄。
	got := storeAndBytes(t, accessor, func(tree map[string]any) {
		tree["prompt_cache_key"] = "k"
	})
	want := "{\n  \"zeta\" : \"<x>\",\n  \"messages\" : [ {\"role\":\"user\"} ],\n  \"alpha\" : 12345678901234567890,\"prompt_cache_key\":\"k\"\n}"
	if string(got) != want {
		t.Fatalf("空白与键序应原样保留\n得到 %q\n期望 %q", got, want)
	}
	assertValidJSONObject(t, got)
}

// TestBodyAccessorPatchDropsDeletedKey 钉住删除：被删成员整段消失，其余成员与分隔符合法。
func TestBodyAccessorPatchDropsDeletedKey(t *testing.T) {
	accessor := newBodyAccessorForTest(t, `{"a":1,"b":"a<b","c":3}`)

	got := storeAndBytes(t, accessor, func(tree map[string]any) {
		delete(tree, "b")
	})
	if string(got) != `{"a":1,"c":3}` {
		t.Fatalf("删除未生效或分隔符错了: %q", got)
	}
	assertValidJSONObject(t, got)

	// 删首个与末个成员时也不许留下悬空逗号。
	first := newBodyAccessorForTest(t, `{"a":1,"b":2,"c":3}`)
	if got := storeAndBytes(t, first, func(tree map[string]any) { delete(tree, "a") }); string(got) != `{"b":2,"c":3}` {
		t.Fatalf("删首个成员出错: %q", got)
	}
	last := newBodyAccessorForTest(t, `{"a":1,"b":2,"c":3}`)
	if got := storeAndBytes(t, last, func(tree map[string]any) { delete(tree, "c") }); string(got) != `{"a":1,"b":2}` {
		t.Fatalf("删末个成员出错: %q", got)
	}
	all := newBodyAccessorForTest(t, `{"a":1}`)
	if got := storeAndBytes(t, all, func(tree map[string]any) { delete(tree, "a") }); string(got) != `{}` {
		t.Fatalf("删空成员出错: %q", got)
	}
}

// TestBodyAccessorPatchNoOpStore 钉住「改了又改回」与「什么都没改」：Store 之后仍是原字节。
func TestBodyAccessorPatchNoOpStore(t *testing.T) {
	payload := `{"zeta":"a < b && c > d","n":12345678901234567890,"messages":[{"content":"<x>&y"}]}`
	accessor := newBodyAccessorForTest(t, payload)

	// 过滤规则命中但正文没变（例如替换前后相同）：出站字节必须逐字仍是原文。
	got := storeAndBytes(t, accessor, func(map[string]any) {})
	if string(got) != payload {
		t.Fatalf("空改动的 Store 不该改动字节\n得到 %q\n期望 %q", got, payload)
	}

	// 改了又改回：同样逐字回到原文。
	back := newBodyAccessorForTest(t, payload)
	storeAndBytes(t, back, func(tree map[string]any) { tree["zeta"] = "changed" })
	tree, err := back.JSON()
	if err != nil {
		t.Fatalf("取树失败: %v", err)
	}
	tree["zeta"] = "a < b && c > d"
	if err := back.Store(tree); err != nil {
		t.Fatalf("二次写回失败: %v", err)
	}
	got, err = back.Bytes()
	if err != nil {
		t.Fatalf("取字节失败: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("改回原值后应回到原字节\n得到 %q\n期望 %q", got, payload)
	}
}

// TestBodyAccessorPatchSwapsKey 钉住「删一个 + 加一个」：成员数与原文相同时也必须补上新键。
func TestBodyAccessorPatchSwapsKey(t *testing.T) {
	accessor := newBodyAccessorForTest(t, `{"a":1,"b":"a<b","c":3}`)

	got := storeAndBytes(t, accessor, func(tree map[string]any) {
		delete(tree, "b")
		tree["d"] = "<d>"
	})
	if string(got) != `{"a":1,"c":3,"d":"<d>"}` {
		t.Fatalf("换键出错: %q", got)
	}
	assertValidJSONObject(t, got)
}

// TestBodyAccessorPatchEmptyObject 钉住空对象：往里加键时不留悬空逗号。
func TestBodyAccessorPatchEmptyObject(t *testing.T) {
	accessor := newBodyAccessorForTest(t, `{}`)
	got := storeAndBytes(t, accessor, func(tree map[string]any) { tree["a"] = 1 })
	if string(got) != `{"a":1}` {
		t.Fatalf("空对象加键出错: %q", got)
	}
	assertValidJSONObject(t, got)
}

// TestBodyAccessorPatchFallsBackOnNonObject 钉住兜底：原文不是 JSON 对象时退回整份序列化。
func TestBodyAccessorPatchFallsBackOnNonObject(t *testing.T) {
	// 空正文：解析成空对象，Store 之后没有「未改动的字节」可保，走整份序列化。
	header := pctx.Init{Method: "POST", Path: "/v1/messages", Body: io.NopCloser(strings.NewReader(""))}
	ctx, err := pctx.New(header)
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	accessor, err := NewBodyAccessor(ctx, BodyAccessOptions{})
	if err != nil {
		t.Fatalf("构造正文访问器失败: %v", err)
	}
	got := storeAndBytes(t, accessor, func(tree map[string]any) { tree["a"] = 1 })
	if string(got) != `{"a":1}` {
		t.Fatalf("空正文兜底序列化出错: %q", got)
	}
}

// TestBodyAccessorPatchSkipsUnchangedMembers 钉住「语义没变就不重写」：改 A 不该让 B 重新编码。
func TestBodyAccessorPatchSkipsUnchangedMembers(t *testing.T) {
	// messages 里有 < > & 与大整数：只要没动它，就必须逐字保留。
	payload := `{"messages":[{"content":"a<b && c>d","n":12345678901234567890}],"metadata":{"user_id":""},"tail":"<x>"}`
	accessor := newBodyAccessorForTest(t, payload)

	got := storeAndBytes(t, accessor, func(tree map[string]any) {
		metadata, _ := tree["metadata"].(map[string]any)
		metadata["user_id"] = "injected"
	})
	if !bytes.HasPrefix(got, []byte(`{"messages":[{"content":"a<b && c>d","n":12345678901234567890}],`)) {
		t.Fatalf("未改动的成员被重写了: %s", got)
	}
	if !bytes.HasSuffix(got, []byte(`"tail":"<x>"}`)) {
		t.Fatalf("未改动的成员被重写了: %s", got)
	}
	assertValidJSONObject(t, got)
}

// TestBodyAccessorPatchRepeatedStore 钉住多次 Store：每次都以入站原文为底本重算，结果幂等。
func TestBodyAccessorPatchRepeatedStore(t *testing.T) {
	payload := `{"a":"<x>","b":1}`
	accessor := newBodyAccessorForTest(t, payload)

	storeAndBytes(t, accessor, func(tree map[string]any) { tree["b"] = 2 })
	tree, err := accessor.JSON()
	if err != nil {
		t.Fatalf("取树失败: %v", err)
	}
	tree["b"] = 3
	if err := accessor.Store(tree); err != nil {
		t.Fatalf("二次写回失败: %v", err)
	}
	got, err := accessor.Bytes()
	if err != nil {
		t.Fatalf("取字节失败: %v", err)
	}
	if string(got) != `{"a":"<x>","b":3}` {
		t.Fatalf("多次 Store 后出错: %q", got)
	}
}

// largePatchBody 造一份 400 条消息、含 < > & 与比较运算符的正文（约 35 KB）。
func largePatchBody() string {
	var payload strings.Builder
	payload.WriteString(`{"model":"m","system":"a < b && c > d","messages":[`)
	for i := 0; i < 400; i++ {
		if i > 0 {
			payload.WriteString(",")
		}
		payload.WriteString(`{"role":"user","content":[{"type":"text","text":"if a < b && c > d then 1 <= 100"}]}`)
	}
	payload.WriteString(`],"big":12345678901234567890}`)
	return payload.String()
}

// patchOnce 模拟一次「注入一个顶层键并取字节」。
func patchOnce(tb testing.TB, accessor *BodyAccessor) {
	tb.Helper()
	tree, err := accessor.JSON()
	if err != nil {
		tb.Fatalf("取树失败: %v", err)
	}
	tree["prompt_cache_key"] = "sess-abc"
	if err := accessor.Store(tree); err != nil {
		tb.Fatalf("写回失败: %v", err)
	}
	if _, err := accessor.Bytes(); err != nil {
		tb.Fatalf("取字节失败: %v", err)
	}
}

// TestBodyAccessorPatchNoWholeMarshal 用分配次数钉住「没有再整份 marshal」。
//
// 整份 json.Marshal 一份 400 条消息的正文要数千次分配；定点改写只分配出站缓冲与少量小对象。
func TestBodyAccessorPatchNoWholeMarshal(t *testing.T) {
	payload := largePatchBody()
	accessor := newBodyAccessorForTest(t, payload)

	tree, err := accessor.JSON()
	if err != nil {
		t.Fatalf("取树失败: %v", err)
	}
	whole := testing.AllocsPerRun(20, func() {
		if _, err := json.Marshal(tree); err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
	})
	patched := testing.AllocsPerRun(20, func() { patchOnce(t, accessor) })
	if patched >= whole/4 {
		t.Fatalf("定点改写的分配（%.0f）不该逼近整份 marshal（%.0f）", patched, whole)
	}
	t.Logf("分配次数：整份 marshal=%.0f，定点改写=%.0f", whole, patched)
}

// BenchmarkBodyAccessorPatchLargeBody 对比两条序列化路径的分配与耗时（正文端点不变）。
func BenchmarkBodyAccessorPatchLargeBody(b *testing.B) {
	accessor := newBodyAccessorForTest(b, largePatchBody())

	b.Run("patched", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			patchOnce(b, accessor)
		}
	})
	b.Run("whole-marshal", func(b *testing.B) {
		b.ReportAllocs()
		tree, err := accessor.JSON()
		if err != nil {
			b.Fatalf("取树失败: %v", err)
		}
		for i := 0; i < b.N; i++ {
			if _, err := json.Marshal(tree); err != nil {
				b.Fatalf("序列化失败: %v", err)
			}
		}
	})
}

// TestScanBodyLayoutSpans 钉住区间扫描：值的起止要落在原文里，嵌套与转义都不许算错。
func TestScanBodyLayoutSpans(t *testing.T) {
	raw := []byte(`{"b" : {"x":"<}","y": [1, {"z":2}]} ,"a":12345678901234567890, "c":"\u0041"}`)
	layout, err := scanBodyLayout(raw)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if len(layout.members) != 3 {
		t.Fatalf("成员数 = %d，期望 3", len(layout.members))
	}
	for index, want := range []string{
		`{"x":"<}","y": [1, {"z":2}]}`,
		`12345678901234567890`,
		`"\u0041"`,
	} {
		span := layout.members[index]
		if got := string(raw[span.valueStart:span.valueEnd]); got != want {
			t.Fatalf("成员 %d 的区间 = %q，期望 %q", index, got, want)
		}
	}
	if raw[layout.closeStart] != '}' {
		t.Fatalf("结尾位置不是 '}': %q", raw[layout.closeStart])
	}
}
