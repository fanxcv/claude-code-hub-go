package forward

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

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
			rewritten, err := rewriteModelField(original, tc.model)
			if err != nil {
				t.Fatalf("rewriteModelField 失败: %v", err)
			}
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

// TestRewriteModelFieldAbsentModelReturnsInput 钉住「找不到顶层 model 时原样返回同一份切片」。
//
// 这是常态路径（绝大多数请求没有配模型重定向）的零成本要求：既不得复制，也不得报错。
func TestRewriteModelFieldAbsentModelReturnsInput(t *testing.T) {
	for _, body := range []string{
		`{"x":1}`,
		`{}`,
		`{"nested" : { "model" : "b" } }`,
		`{"models":["a"],"model_extra":1}`,
		// JSON 的 `null` 解进 map 是合法的「无 model」：旧实现同样原样返回。
		`null`,
	} {
		original := []byte(body)
		rewritten, err := rewriteModelField(original, "TARGET")
		if err != nil {
			t.Fatalf("%s: rewriteModelField 失败: %v", body, err)
		}
		if len(rewritten) != len(original) || &rewritten[0] != &original[0] {
			t.Fatalf("%s: 未命中时应原样返回同一份切片，实际 %q", body, rewritten)
		}
	}

	absent := []byte(`{"x":1,"nested":{"model":"b"}}`)
	if allocs := testing.AllocsPerRun(100, func() {
		if _, err := rewriteModelField(absent, "TARGET"); err != nil {
			t.Fatalf("rewriteModelField 失败: %v", err)
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
	if string(plan.Body) != want {
		t.Fatalf("正文 = %q，期望 %q", plan.Body, want)
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
// 旧实现（解析整棵树再序列化）在同尺寸正文上实测 8.33ms / 6.14MB 分配（3.1× 放大）。
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
		if len(rewritten) == len(body) {
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
// 17.0ms / 14.6MB / 202k 次分配，现在 2.6ms / 1.96MB / 52 次（剩下的 1.96MB 是替换后
// 那一份正文的**唯一**拷贝，字节级替换绕不过）。竞速时每个候选都付一次这个账。
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
