package forward

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
)

// rewriteBody 把 rewriteModelField 的分段结果拼成连续字节（与出站正文同字节）。
func rewriteBody(t *testing.T, body []byte, model string) []byte {
	t.Helper()
	parts, err := rewriteModelField(body, model)
	if err != nil {
		t.Fatalf("rewriteModelField 失败: %v", err)
	}
	if parts == nil {
		return body
	}
	return parts.materialize()
}

// outboundBody 读出计划真正发给上游的字节（分段 reader 或单切片）。
func outboundBody(t *testing.T, plan *Plan) []byte {
	t.Helper()
	request := plan.Request()
	if request.Body == nil {
		return nil
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("读取出站正文失败: %v", err)
	}
	return body
}

// TestRewriteModelFieldKeepsBytesVerbatim 钉住「只换顶层 model 的值，其余字节逐字不变」。
//
// 旧实现（json.Unmarshal + json.Marshal）会把正文整体重排：键序变字典序、空白被压缩、
// 转义形态被归一。出站字节的稳定性决定上游前缀缓存命中率，故逐字比对是这条函数的契约，
// 不是风格偏好。
func TestRewriteModelFieldKeepsBytesVerbatim(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		model string
		want  string
	}{
		{"顶层在前", `{"model":"a","x":1}`, "TARGET", `{"model":"TARGET","x":1}`},
		{"顶层在后", `{"x":1,"model":"a"}`, "TARGET", `{"x":1,"model":"TARGET"}`},
		{"唯一键", `{"model":"a"}`, "TARGET", `{"model":"TARGET"}`},
		{
			"新名更短",
			`{"model":"claude-sonnet-4-5-20250929","x":1}`,
			"glm",
			`{"model":"glm","x":1}`,
		},
		{
			"新名等长",
			`{"model":"claude","x":1}`,
			"ABCDEF",
			`{"model":"ABCDEF","x":1}`,
		},
		{
			"新名更长",
			`{"model":"glm","x":1}`,
			"claude-sonnet-4-5-20250929",
			`{"model":"claude-sonnet-4-5-20250929","x":1}`,
		},
		{
			"保留空白与键序",
			"{ \"x\" : 1 ,\n  \"model\" : \"a\" ,\t\"y\":[1, 2] }",
			"TARGET",
			"{ \"x\" : 1 ,\n  \"model\" : \"TARGET\" ,\t\"y\":[1, 2] }",
		},
		{
			"嵌套同名键不碰",
			`{"model":"a","nested":{"model":"b"}}`,
			"TARGET",
			`{"model":"TARGET","nested":{"model":"b"}}`,
		},
		{
			"嵌套在前也不碰",
			`{"nested":{"model":"b"},"model":"a"}`,
			"TARGET",
			`{"nested":{"model":"b"},"model":"TARGET"}`,
		},
		{
			"数组里的同名键不碰",
			`{"tools":[{"model":"b"}],"model":"a"}`,
			"TARGET",
			`{"tools":[{"model":"b"}],"model":"TARGET"}`,
		},
		{
			"值不是字符串时换成字符串",
			`{"model":{"alias":"a"},"x":1}`,
			"TARGET",
			`{"model":"TARGET","x":1}`,
		},
		{
			"值是 null",
			`{"model":null}`,
			"TARGET",
			`{"model":"TARGET"}`,
		},
		{
			"值是数字",
			`{"model":123,"x":1}`,
			"TARGET",
			`{"model":"TARGET","x":1}`,
		},
		{
			"尾部空白照留",
			"{\"model\":\"a\"}\n",
			"TARGET",
			"{\"model\":\"TARGET\"}\n",
		},
		{
			"顶层重复时逐处替换",
			`{"model":"a","model":"b","x":1}`,
			"TARGET",
			`{"model":"TARGET","model":"TARGET","x":1}`,
		},
		{
			"键写成转义形态也算顶层 model",
			`{"\u006dodel":"a","x":1}`,
			"TARGET",
			`{"\u006dodel":"TARGET","x":1}`,
		},
		{
			"其余键里的转义原样保留",
			`{"model":"a","note":"say \"hi\"\n","p":"\u00e9"}`,
			"TARGET",
			`{"model":"TARGET","note":"say \"hi\"\n","p":"\u00e9"}`,
		},
		{
			"容器字符串里的括号不误判结束",
			`{"model":"a","blob":"}]\"}[","x":1}`,
			"TARGET",
			`{"model":"TARGET","blob":"}]\"}[","x":1}`,
		},
		{
			"Unicode 模型名",
			`{"model":"a"}`,
			"模型-甲",
			`{"model":"模型-甲"}`,
		},
		{
			"模型名需要 JSON 转义",
			`{"model":"a"}`,
			"a\"b\\c\nd",
			`{"model":"a\"b\\c\nd"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(tc.body)
			before := string(original)
			rewritten := rewriteBody(t, original, tc.model)
			if string(rewritten) != tc.want {
				t.Fatalf("正文 = %q，期望 %q", rewritten, tc.want)
			}
			// 入参是共享的客户端正文快照（竞速各 attempt 与回放共用一份），不得被就地改动。
			if string(original) != before {
				t.Fatalf("入参被就地改写: %q", original)
			}
			// 结果必须是合法 JSON，且顶层 model 就是目标名（防「字符串形状对但语义错」）。
			var decoded map[string]any
			if err := json.Unmarshal(rewritten, &decoded); err != nil {
				t.Fatalf("改写结果不是合法 JSON: %v (%q)", err, rewritten)
			}
			if decoded["model"] != tc.model {
				t.Fatalf("顶层 model = %v，期望 %q", decoded["model"], tc.model)
			}
		})
	}
}

// TestRewriteModelFieldAbsentModelReturnsNil 钉住「找不到顶层 model 时不产出任何分段」。
//
// 这是常态路径（绝大多数请求没有配模型重定向）的零成本要求：既不复制，也不报错。
func TestRewriteModelFieldAbsentModelReturnsNil(t *testing.T) {
	for _, body := range []string{
		`{"x":1}`,
		`{}`,
		`{"nested" : { "model" : "b" } }`,
		`{"models":["a"],"model_extra":1}`,
		// JSON 的 `null` 解进 map 是合法的「无 model」：旧实现同样原样返回。
		`null`,
	} {
		parts, err := rewriteModelField([]byte(body), "TARGET")
		if err != nil {
			t.Fatalf("%s: rewriteModelField 失败: %v", body, err)
		}
		if parts != nil {
			t.Fatalf("%s: 未命中时不该产出分段，实际 %d 段", body, len(parts.segments))
		}
	}

	absent := []byte(`{"x":1,"nested":{"model":"b"}}`)
	if allocs := testing.AllocsPerRun(100, func() {
		if parts, err := rewriteModelField(absent, "TARGET"); err != nil || parts != nil {
			t.Fatalf("rewriteModelField = %v, %v", parts, err)
		}
	}); allocs != 0 {
		t.Fatalf("未命中路径分配了 %v 次，期望 0（零分配是这条路径的全部意义）", allocs)
	}
}

// TestRewriteModelFieldRejectsNonObjectBody 钉住错误面：正文不是 JSON 对象时报 ErrInvalidBody。
//
// 旧实现经 json.Unmarshal 到 map 同样在这几种形状上报错，调用方（dataplane）据哨兵判类，
// 故哨兵必须仍然可 errors.Is 命中。
func TestRewriteModelFieldRejectsNonObjectBody(t *testing.T) {
	for _, body := range []string{
		``,
		`[1,2]`,
		`"just a string"`,
		`123`,
		`{"model":"a"} trailing`,
		`{"model":"a",}`,
		`{"model":"a" "x":1}`,
		`{"model":}`,
		`{"model":"unterminated}`,
	} {
		if _, err := rewriteModelField([]byte(body), "TARGET"); !errors.Is(err, ErrInvalidBody) {
			t.Fatalf("%q: err = %v，期望 ErrInvalidBody", body, err)
		}
	}
}

// TestBuildPlanModelRedirectKeepsKeyOrder 把上面那条接到真实计划上：
// 客户端原样正文经重定向后，除 model 值外逐字不变（旧实现在这里会重排键序）。
func TestBuildPlanModelRedirectKeepsKeyOrder(t *testing.T) {
	const body = `{"max_tokens":1024,"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`
	const want = `{"max_tokens":1024,"model":"glm-4.6","messages":[{"role":"user","content":"ping"}]}`

	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)

	plan, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: provider}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatalf("本用例不转换（原生直通），实际 conversion=%+v", plan.Conversion)
	}
	// 请求期不得落地：正文仍分段，只引用入参切片。
	if plan.Body != nil || plan.parts == nil {
		t.Fatalf("正文应保持分段（Body = %q，分段 = %v）", plan.Body, plan.parts)
	}
	if got := outboundBody(t, plan); string(got) != want {
		t.Fatalf("正文 = %q，期望 %q", got, want)
	}
}

// modelRewriteBenchBody 造约 size 字节的 claude 形状正文，model 在前、另含嵌套同名键。
func modelRewriteBenchBody(size int) []byte {
	var builder strings.Builder
	builder.WriteString(`{"model":"claude-sonnet-4-5-20250929","max_tokens":1024,"messages":[`)
	first := true
	for builder.Len() < size {
		if !first {
			builder.WriteString(",")
		}
		first = false
		builder.WriteString(`{"role":"user","content":[{"type":"text","text":"`)
		builder.WriteString(strings.Repeat("x", 96))
		builder.WriteString(`"}]}`)
	}
	builder.WriteString(`],"metadata":{"model":"nested-should-stay"}}`)
	return []byte(builder.String())
}

// BenchmarkRewriteModelFieldLargeBody 量「1.9MB 正文改写一次」的耗时与分配。
//
// 改写只产出分段（引用入参切片 + 新模型名），故 B/op 应只剩新模型名那一小份；
// 旧实现（解析整棵树再序列化）在同尺寸正文上实测 8.33ms / 6.14MB 分配（3.1× 放大），
// 逐字替换版曾为 1.95MB/op（整篇副本）。
func BenchmarkRewriteModelFieldLargeBody(b *testing.B) {
	body := modelRewriteBenchBody(1900 << 10)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		rewritten, err := rewriteModelField(body, "glm-4.6")
		if err != nil {
			b.Fatalf("rewriteModelField 失败: %v", err)
		}
		if rewritten == nil || rewritten.length == len(body) {
			b.Fatal("前置不成立：本基准需真的改写（模型名长度应不同）")
		}
	}
}

// BenchmarkRewriteModelFieldAbsentModel 量「正文里没有顶层 model」的常态路径（应零分配）。
func BenchmarkRewriteModelFieldAbsentModel(b *testing.B) {
	body := bytes.Replace(modelRewriteBenchBody(1900<<10), []byte(`"model":"claude-sonnet-4-5-20250929",`), nil, 1)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := rewriteModelField(body, "glm-4.6"); err != nil {
			b.Fatalf("rewriteModelField 失败: %v", err)
		}
	}
}

// BenchmarkBuildPlanNativeLargeBody 量「原生直通（无转换）一次计划编译」的每 attempt 成本。
//
// 竞速的每个候选都会各编译一次计划；这个数字是「attempt 间复用正文」能省掉的上限。
func BenchmarkBuildPlanNativeLargeBody(b *testing.B) {
	var builder strings.Builder
	builder.WriteString(`{"model":"claude-sonnet-4-5-20250929","stream":true,"max_tokens":1024,"messages":[`)
	for builder.Len() < 1900<<10 {
		builder.WriteString(`{"role":"user","content":"`)
		builder.WriteString(strings.Repeat("x", 96))
		builder.WriteString(`"},`)
	}
	builder.WriteString(`{"role":"user","content":"tail"}]}`)
	client := newClaudeRequest(builder.String())
	input := PlanInput{Client: client, Target: Target{Provider: claudeProvider("https://relay.example.com")}}

	b.ReportAllocs()
	b.SetBytes(int64(len(client.Body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		plan, err := BuildPlan(input)
		if err != nil {
			b.Fatalf("BuildPlan 失败: %v", err)
		}
		if plan.Conversion != nil {
			b.Fatal("前置不成立：本基准是原生直通")
		}
	}
}

// BenchmarkBuildPlanNativeLargeBodyWithRedirect 量「原生直通 + 模型重定向」的每 attempt 计划成本。
//
// 这条是甲项的真实验收面：1.9MB 正文 + 配了 redirects 时，旧实现每次尝试
// 17.0ms / 14.6MB / 202k 次分配；逐字替换版降到 2.6ms / 1.96MB / 52 次（那 1.96MB 是
// 替换后正文的整篇拷贝）；分段版降到约 2.4ms / 5KB / 53 次——**请求期不再有那份整篇副本**。
// 计划本身也不在请求期物化：只有结算期的探针与（有缓存块时的）TTL 解析才会拼出连续正文，
// 那时上游响应已到，不计入本基准。
func BenchmarkBuildPlanNativeLargeBodyWithRedirect(b *testing.B) {
	var builder strings.Builder
	builder.WriteString(`{"model":"claude-sonnet-4-5-20250929","stream":true,"max_tokens":1024,"messages":[`)
	for builder.Len() < 1900<<10 {
		builder.WriteString(`{"role":"user","content":"`)
		builder.WriteString(strings.Repeat("x", 96))
		builder.WriteString(`"},`)
	}
	builder.WriteString(`{"role":"user","content":"tail"}]}`)

	client := newClaudeRequest(builder.String())
	provider := claudeProvider("https://relay.example.com")
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)
	input := PlanInput{Client: client, Target: Target{Provider: provider}}

	b.ReportAllocs()
	b.SetBytes(int64(len(client.Body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		plan, err := BuildPlan(input)
		if err != nil {
			b.Fatalf("BuildPlan 失败: %v", err)
		}
		if plan.Redirect == nil {
			b.Fatal("前置不成立：本基准需命中重定向")
		}
	}
}

// BenchmarkBuildPlanNativeLargeBodyWithRedirectProductionShape 量「生产装配形态」下的同一件事：
// 供应商从库里读回（SQL NULL 的 jsonb 解成字面量 `null`）+ 数据面总会挂上的覆写器。
//
// 为什么要单列：零配置早退的判定若把 `null` 当成已配置，每次计划编译都会白做一次
// ParseJSON + Clone，整篇副本也回来——普通基准（provider 字段为 nil）看不见。
// 2026-10 修前实测 9.27–9.48ms / 16,078,588–16,079,872 B/op / 218,048–218,049 allocs；修后与无偏好路径一致。
func BenchmarkBuildPlanNativeLargeBodyWithRedirectProductionShape(b *testing.B) {
	var builder strings.Builder
	builder.WriteString(`{"model":"claude-sonnet-4-5-20250929","stream":true,"max_tokens":1024,"messages":[`)
	for builder.Len() < 1900<<10 {
		builder.WriteString(`{"role":"user","content":"`)
		builder.WriteString(strings.Repeat("x", 96))
		builder.WriteString(`"},`)
	}
	builder.WriteString(`{"role":"user","content":"tail"}]}`)

	client := newClaudeRequest(builder.String())
	provider := claudeProvider("https://relay.example.com")
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)
	provider.AnthropicAdaptiveThinking = json.RawMessage(`null`)
	input := PlanInput{
		Client:    client,
		Target:    Target{Provider: provider},
		Overrides: NewProviderOverrideApplier("/v1/messages"),
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(client.Body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		plan, err := BuildPlan(input)
		if err != nil {
			b.Fatalf("BuildPlan 失败: %v", err)
		}
		if plan.Redirect == nil {
			b.Fatal("前置不成立：本基准需命中重定向")
		}
		if plan.parts == nil || plan.Body != nil {
			b.Fatal("前置不成立：生产形态（jsonb null 未配置）应保持分段，不得物化")
		}
	}
}

// BenchmarkBuildPlanConversionLargeBody 量「跨线转换一次计划编译」的每 attempt 成本。
func BenchmarkBuildPlanConversionLargeBody(b *testing.B) {
	client := responsesClient(string(planBenchBody(1900 << 10)))
	provider := claudeProvider("https://relay.example.com")
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"gpt-5","target":"claude-sonnet-4-5-20250929"}]`)
	input := PlanInput{Client: client, Target: Target{Provider: provider}, ConversionEnabled: true}

	b.ReportAllocs()
	b.SetBytes(int64(len(client.Body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		plan, err := BuildPlan(input)
		if err != nil {
			b.Fatalf("BuildPlan 失败: %v", err)
		}
		if plan.Conversion == nil || plan.ConversionFallback {
			b.Fatal("前置不成立：本基准需转换成功")
		}
	}
}

// redirectProvider 是 native 路配上「客户端模型 → target」一条 exact 重定向的供应商。
func redirectProvider(target string) Provider {
	provider := claudeProvider("https://relay.example.com")
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"` + target + `"}]`)
	return provider
}

// TestBuildPlanModelRewriteIsSegmented 钉住 native 路的重定向不在计划期复制整篇正文：
// 出站字节由可 Seek 的分段 reader 给出，ContentLength 是各段之和（不得退化成 -1）。
func TestBuildPlanModelRewriteIsSegmented(t *testing.T) {
	const body = `{"max_tokens":1024,"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`
	want := []byte(`{"max_tokens":1024,"model":"glm-4.6","messages":[{"role":"user","content":"ping"}]}`)

	plan, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: redirectProvider("glm-4.6")}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.parts == nil {
		t.Fatal("native 路 + 重定向应产成分段正文（计划期不得复制整篇）")
	}
	if plan.Body != nil {
		t.Fatalf("分段形态下 Body 必须为 nil，实际分配了 %d 字节", len(plan.Body))
	}
	if plan.ContentLength != int64(len(want)) {
		t.Fatalf("ContentLength = %d，期望 %d", plan.ContentLength, len(want))
	}
	request := plan.Request()
	if request.ContentLength != int64(len(want)) || request.ContentLength == -1 {
		t.Fatalf("拨号 ContentLength = %d，期望精确长度 %d（-1 会退化成 chunked）", request.ContentLength, len(want))
	}
	if _, ok := request.Body.(io.Seeker); !ok {
		t.Fatal("拨号正文必须可 Seek：dial 的 rewindBody 在 h2 失败后要重放正文")
	}
	if got := outboundBody(t, plan); !bytes.Equal(got, want) {
		t.Fatalf("出站正文 = %q，期望 %q", got, want)
	}
}

// TestBuildPlanModelRewriteLeavesSharedBodyUntouched 钉住竞速共享语义：同一份入参正文连编
// 两个目标名不同的候选，各自出站正确、互不串线，且入参切片一个字节未动（禁止就地改写）。
func TestBuildPlanModelRewriteLeavesSharedBodyUntouched(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`
	client := newClaudeRequest(body)
	before := string(client.Body)

	build := func(target string) *Plan {
		plan, err := BuildPlan(PlanInput{Client: client, Target: Target{Provider: redirectProvider(target)}})
		if err != nil {
			t.Fatalf("BuildPlan(%s) 失败: %v", target, err)
		}
		return plan
	}
	first := build("glm-4.6")
	second := build("upstream-b")

	firstBody := outboundBody(t, first)
	secondBody := outboundBody(t, second)
	if !bytes.Contains(firstBody, []byte(`"model":"glm-4.6"`)) || bytes.Contains(firstBody, []byte("upstream-b")) {
		t.Fatalf("候选甲的正文串线或未改写: %s", firstBody)
	}
	if !bytes.Contains(secondBody, []byte(`"model":"upstream-b"`)) || bytes.Contains(secondBody, []byte("glm-4.6")) {
		t.Fatalf("候选乙的正文串线或未改写: %s", secondBody)
	}
	if string(client.Body) != before {
		t.Fatalf("共享的入参正文被就地改写: %q", client.Body)
	}
	// 分段引用入参切片：前缀段与入参同一底层数组，说明这份正文没有被复制。
	if len(first.parts.segments[0]) == 0 || &first.parts.segments[0][0] != &client.Body[0] {
		t.Fatal("分段正文应引用入参切片（前缀段与入参不同源说明发生了复制）")
	}
}

// TestPlanRequestBodySeekable 钉住拨号层的重放契约：分段 reader Seek 回起点后完整重读，
// Seek 到中间偏移读出正确后缀。
func TestPlanRequestBodySeekable(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`
	want := []byte(`{"model":"glm-4.6","messages":[{"role":"user","content":"ping"}]}`)
	plan, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: redirectProvider("glm-4.6")}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}

	request := plan.Request()
	seeker, ok := request.Body.(io.Seeker)
	if !ok {
		t.Fatal("拨号正文必须可 Seek")
	}
	first, err := io.ReadAll(request.Body)
	if err != nil || !bytes.Equal(first, want) {
		t.Fatalf("首读 = %q, %v，期望 %q", first, err, want)
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("Seek 回起点失败: %v", err)
	}
	again, err := io.ReadAll(request.Body)
	if err != nil || !bytes.Equal(again, first) {
		t.Fatalf("回起点重读 = %q, %v，期望 %q（h2→h1 回退要重放同一份正文）", again, err, first)
	}
	offset := int64(len(want)/2 + 1)
	if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
		t.Fatalf("Seek 到中间偏移失败: %v", err)
	}
	suffix, err := io.ReadAll(request.Body)
	if err != nil || !bytes.Equal(suffix, want[offset:]) {
		t.Fatalf("中间偏移读出的后缀 = %q, %v，期望 %q", suffix, err, want[offset:])
	}
	if got, err := seeker.Seek(0, io.SeekEnd); err != nil || got != int64(len(want)) {
		t.Fatalf("Seek 到末尾 = %d, %v，期望 %d", got, err, len(want))
	}
	if rest, err := io.ReadAll(request.Body); err != nil || len(rest) != 0 {
		t.Fatalf("末尾之后应读到 EOF，实际 %q, %v", rest, err)
	}
	if _, err := seeker.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("负偏移应报错（与 bytes.Reader 同语义）")
	}
}

// TestPlanMaterializedBodyIsIdempotent 钉住物化入口：首调拼接并缓存，之后零分配，
// 且与分段 reader 读出的字节一致。
func TestPlanMaterializedBodyIsIdempotent(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","messages":[]}`
	const want = `{"model":"glm-4.6","messages":[]}`
	plan, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: redirectProvider("glm-4.6")}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}

	segmented := outboundBody(t, plan)
	materialized := plan.MaterializedBody()
	if !bytes.Equal(materialized, segmented) {
		t.Fatalf("物化结果 = %q，分段读出的 = %q", materialized, segmented)
	}
	if string(materialized) != want {
		t.Fatalf("正文 = %q，期望 %q", materialized, want)
	}
	if plan.parts != nil {
		t.Fatal("物化后不应再持有分段")
	}
	if allocs := testing.AllocsPerRun(100, func() { _ = plan.MaterializedBody() }); allocs != 0 {
		t.Fatalf("物化后再取正文分配了 %v 次，期望 0", allocs)
	}
}

// TestPlanAppliedCacheTTLAcrossSegments 钉住缓存 TTL 的读取不漏分段：`"cache_control"`
// 恰好横跨两段时仍须命中；正文里没有缓存块时不得物化。
func TestPlanAppliedCacheTTLAcrossSegments(t *testing.T) {
	t.Run("恰好横跨两段", func(t *testing.T) {
		body := []byte(`{"model":"x","system":[{"type":"text","text":"s",` +
			`"cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[]}`)
		cut := bytes.Index(body, []byte(`"cache_control"`)) + 5
		plan := &Plan{parts: &bodyParts{segments: [][]byte{body[:cut], body[cut:]}, length: len(body)}}
		if got := plan.AppliedCacheTTL(); got != cacheTTLLong {
			t.Fatalf("AppliedCacheTTL = %q，期望 %q（跨段边界也必须命中）", got, cacheTTLLong)
		}
	})
	t.Run("没有缓存块时不物化", func(t *testing.T) {
		body := []byte(`{"model":"x","messages":[]}`)
		plan := &Plan{parts: &bodyParts{segments: [][]byte{body[:5], body[5:]}, length: len(body)}}
		if got := plan.AppliedCacheTTL(); got != "" {
			t.Fatalf("AppliedCacheTTL = %q，期望空串", got)
		}
		if plan.Body != nil || plan.parts == nil {
			t.Fatal("没有缓存块的正文不该被物化成连续副本")
		}
	})
}

// TestBuildPlanModelRewriteStaysSegmentedForZeroConfigOverrides 钉住生产形态：装配总给一个
// 覆写器，而绝大多数供应商没有配置偏好（见 overrides.go 的零配置早退）——此时不得为它物化。
//
// adaptive thinking 必须设成 `null` 字面量：那是 SQL NULL 经 row_to_json 读回的形态，
// 不是 nil。按「非空串」判会把「未配置」误判成「已配置」，本用例即红。
func TestBuildPlanModelRewriteStaysSegmentedForZeroConfigOverrides(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[]}`
	provider := redirectProvider("glm-4.6")
	provider.AnthropicAdaptiveThinking = json.RawMessage(`null`)
	plan, err := BuildPlan(PlanInput{
		Client:    newClaudeRequest(body),
		Target:    Target{Provider: provider},
		Overrides: NewProviderOverrideApplier("/v1/messages"),
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Body != nil || plan.parts == nil {
		t.Fatal("零配置覆写把分段正文拼回了整篇副本")
	}
	if got := outboundBody(t, plan); !bytes.Contains(got, []byte(`"model":"glm-4.6"`)) {
		t.Fatalf("模型名未改写: %s", got)
	}
}

// TestBuildPlanModelRewriteMaterializesForConfiguredOverrides 钉住另一头：覆写真的会改写正文时，
// 模型名改写必须先就位（覆写会重新序列化整棵树），出站同时带新模型名与覆写值。
func TestBuildPlanModelRewriteMaterializesForConfiguredOverrides(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[]}`
	provider := redirectProvider("glm-4.6")
	provider.AnthropicMaxTokensPreference = "2048"
	plan, err := BuildPlan(PlanInput{
		Client:    newClaudeRequest(body),
		Target:    Target{Provider: provider},
		Overrides: NewProviderOverrideApplier("/v1/messages"),
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(outboundBody(t, plan), &decoded); err != nil {
		t.Fatalf("出站正文不是 JSON: %v", err)
	}
	if decoded["model"] != "glm-4.6" {
		t.Fatalf("覆写后模型名被改回了: %v", decoded["model"])
	}
	if decoded["max_tokens"] != float64(2048) {
		t.Fatalf("覆写未生效: %v", decoded["max_tokens"])
	}
}

// TestBuildPlanModelRewriteWithChatUsageOption 钉住 chat 线的 include_usage 补齐与重定向同时生效
// （该步会重新序列化正文，故模型名改写必须先落地）。
func TestBuildPlanModelRewriteWithChatUsageOption(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	provider := openAICompatibleProvider("https://relay.example.com")
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)
	plan, err := BuildPlan(PlanInput{Client: chatClient(body), Target: Target{Provider: provider}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(outboundBody(t, plan), &decoded); err != nil {
		t.Fatalf("出站正文不是 JSON: %v", err)
	}
	if decoded["model"] != "glm-4.6" {
		t.Fatalf("模型名未改写: %v", decoded["model"])
	}
	options, ok := decoded["stream_options"].(map[string]any)
	if !ok || options["include_usage"] != true {
		t.Fatalf("include_usage 未补齐: %v", decoded["stream_options"])
	}
}

// TestBuildPlanModelRewriteCommutesWithEmptyToolArgs 钉住 native 路的空参数归一与模型改写
// 可交换：两者都只替换一段互不重叠的字节，先后顺序不改变出站字节。
func TestBuildPlanModelRewriteCommutesWithEmptyToolArgs(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":""}}]}]}`
	want := `{"model":"glm-4.6","messages":[{"role":"user","content":"ping"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":"{}"}}]}]}`

	provider := openAICompatibleProvider("https://relay.example.com")
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)
	plan, err := BuildPlan(PlanInput{Client: chatClient(body), Target: Target{Provider: provider}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if got := outboundBody(t, plan); string(got) != want {
		t.Fatalf("出站正文 = %q，期望 %q", got, want)
	}
}

// TestPlanRequestGetBodyReplaysSegmentedBody 钉住 307/308 的重放契约：分段计划的拨号请求必须
// 给出 GetBody，且每次调用都交出一份**独立的、从头开始**的分段 reader（零拷贝）；非分段计划
// 不碰 GetBody（bytes.NewReader 那个由 http.NewRequestWithContext 自动设置）。
func TestPlanRequestGetBodyReplaysSegmentedBody(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`
	want := []byte(`{"model":"glm-4.6","messages":[{"role":"user","content":"ping"}]}`)

	plan, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: redirectProvider("glm-4.6")}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.parts == nil {
		t.Fatal("前置不成立：本用例需要分段正文")
	}
	request := plan.Request()
	if request.GetBody == nil {
		t.Fatal("分段正文必须给出 GetBody，否则上游回 307/308 时不会带体重放")
	}
	first, err := request.GetBody()
	if err != nil {
		t.Fatalf("GetBody 失败: %v", err)
	}
	defer func() { _ = first.Close() }()
	firstBody, err := io.ReadAll(first)
	if err != nil {
		t.Fatalf("读第一份重放正文失败: %v", err)
	}
	if !bytes.Equal(firstBody, want) {
		t.Fatalf("第一份重放正文 = %q，期望 %q", firstBody, want)
	}
	second, err := request.GetBody()
	if err != nil {
		t.Fatalf("第二次 GetBody 失败: %v", err)
	}
	defer func() { _ = second.Close() }()
	if first == second {
		t.Fatal("重放必须交出独立 reader 实例：旧 reader 在重定向时已被读完")
	}
	secondBody, err := io.ReadAll(second)
	if err != nil {
		t.Fatalf("读第二份重放正文失败: %v", err)
	}
	if !bytes.Equal(secondBody, want) {
		t.Fatalf("第二份重放正文 = %q，期望 %q", secondBody, want)
	}

	// 非分段路径不设 GetBody：交由 http.NewRequestWithContext 对 bytes.Reader 自动设置。
	plain, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: claudeProvider("https://relay.example.com")}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plain.parts != nil {
		t.Fatal("前置不成立：无重定向时不应产成分段正文")
	}
	if plainRequest := plain.Request(); plainRequest.GetBody != nil {
		t.Fatal("非分段路径不得设置 GetBody（bytes.Reader 的已由 Go 自动设好，重复设置只是噪声）")
	}
}

// TestPlanRequestRedirectCarriesSegmentedBody 是本 lane 的回归钉子：上游回 307 时，分段正文
// 必须像旧实现的 bytes.Reader 一样被带体重放。两个子测试成对，证明用例能捕获回退而不是恒过：
// 设 GetBody 时跟随到目标拿到 200，不设时空手把 307 原样交回调用方（修复前就是这个行为）。
func TestPlanRequestRedirectCarriesSegmentedBody(t *testing.T) {
	const body = `{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`
	want := []byte(`{"model":"glm-4.6","messages":[{"role":"user","content":"ping"}]}`)

	// 目标服务器把收到的正文送进通道（通道同时给出 happens-before 边，-race 下也干净）。
	received := make(chan string, 4)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			payload = []byte("读正文失败: " + err.Error())
		}
		received <- string(payload)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(gateway.Close)

	post := func(t *testing.T, request dial.Request) *dial.Response {
		t.Helper()
		client, err := dial.New(dial.Options{})
		if err != nil {
			t.Fatalf("dial.New 失败: %v", err)
		}
		t.Cleanup(client.Close)
		response, err := client.RoundTrip(t.Context(), request)
		if err != nil {
			t.Fatalf("RoundTrip 失败: %v", err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		return response
	}
	receive := func(t *testing.T) (string, bool) {
		t.Helper()
		select {
		case payload := <-received:
			return payload, true
		default:
			return "", false
		}
	}

	provider := redirectProvider("glm-4.6")
	provider.URL = gateway.URL
	plan, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: provider}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.parts == nil {
		t.Fatal("前置不成立：本用例需要分段正文")
	}

	t.Run("设 GetBody 时带体重放", func(t *testing.T) {
		response := post(t, plan.Request())
		if response.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（307 被原样交回说明正文没被重放）", response.StatusCode)
		}
		payload, ok := receive(t)
		if !ok {
			t.Fatal("重定向目标没收到请求")
		}
		if payload != string(want) {
			t.Fatalf("重定向目标收到的正文 = %q，期望 %q", payload, want)
		}
	})

	t.Run("不设 GetBody 时原样返回 307", func(t *testing.T) {
		request := plan.Request()
		request.GetBody = nil // 复刻修复前的拨号请求形态
		response := post(t, request)
		if response.StatusCode != http.StatusTemporaryRedirect {
			t.Fatalf("状态码 = %d，期望 307（本子用例是回退的可证伪点）", response.StatusCode)
		}
		if payload, ok := receive(t); ok {
			t.Fatalf("不设 GetBody 时不该有请求到达重定向目标，却收到了 %q", payload)
		}
	})

	t.Run("非分段计划仍由 Go 自动设的 GetBody 重放", func(t *testing.T) {
		plain, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: claudeProvider(gateway.URL)}})
		if err != nil {
			t.Fatalf("BuildPlan 失败: %v", err)
		}
		if plain.parts != nil {
			t.Fatal("前置不成立：无重定向时不应产成分段正文")
		}
		expected := outboundBody(t, plain)
		response := post(t, plain.Request())
		if response.StatusCode != http.StatusOK {
			t.Fatalf("状态码 = %d，期望 200（Go 对 bytes.Reader 自动设置的 GetBody 被覆盖了）", response.StatusCode)
		}
		payload, ok := receive(t)
		if !ok {
			t.Fatal("重定向目标没收到请求")
		}
		if payload != string(expected) {
			t.Fatalf("重定向目标收到的正文 = %q，期望 %q", payload, expected)
		}
	})
}
