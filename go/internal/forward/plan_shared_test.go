package forward

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住 PlanShared（竞速内复用与候选无关的转换产物，见 plan_shared.go）的三件事：
//  1. 不同目标协议线的候选各得**自己那条线**的正文——键里少了协议线就会把一种协议的正文
//     发到另一种协议的上游；
//  2. 同一协议线但模型重定向目标不同的候选各得**自己的模型名**——键里少了目标名就会把甲的
//     模型名发给乙（上游 404，或把甲的用量记到乙账上）。这是本机制第二个真实危险，且与协议
//     线不同——它只看供应商配置，看不出协议差异；
//  3. 逐候选的其余改写（供应商覆写）仍在共享正文之上各算各的，且共享前后逐字节相同。

const sharedPlanClientBody = `{"model":"claude-sonnet-4-5-20250929","max_tokens":1024,"stream":true,` +
	`"system":"你是网关助手","messages":[{"role":"user","content":"ping"}],` +
	`"tools":[{"name":"weather","description":"查天气","input_schema":{"type":"object",` +
	`"properties":{"city":{"type":"string"}}}}]}`

func sharedPlanClient() ClientRequest {
	return ClientRequest{
		Method:  "POST",
		Path:    "/v1/messages",
		Headers: newClientHeaders("content-type", "application/json"),
		Format:  convert.FormatClaude,
		Model:   "claude-sonnet-4-5-20250929",
		Body:    []byte(sharedPlanClientBody),
		HasBody: true,
	}
}

func sharedPlanProvider(id int64, providerType convert.ProviderType) Provider {
	return Provider{
		ID:   id,
		Name: "候选",
		Type: providerType,
		Key:  "sk-up",
		URL:  "https://relay.example.com",
	}
}

func buildSharedPlan(t *testing.T, client ClientRequest, provider Provider, shared *PlanShared) *Plan {
	t.Helper()
	plan, err := BuildPlan(PlanInput{
		Client:            client,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
		Shared:            shared,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	return plan
}

// planBody 把出站正文解成树，供结构性断言。
func planBody(t *testing.T, plan *Plan) *convert.Value {
	t.Helper()
	value, err := convert.ParseJSON(plan.Body)
	if err != nil {
		t.Fatalf("出站正文不是合法 JSON: %v (%s)", err, plan.Body)
	}
	return value
}

// TestPlanSharedGivesEachTargetProtocolItsOwnBody 用同一请求跑多个候选（chat x2、responses、
// 原生 claude），断言各候选拿到的是**自己目标协议线**的正文形状，且共享前后逐字节相同。
func TestPlanSharedGivesEachTargetProtocolItsOwnBody(t *testing.T) {
	client := sharedPlanClient()
	shared := NewPlanShared()
	providers := []Provider{
		sharedPlanProvider(1, convert.ProviderOpenAICompatible),
		sharedPlanProvider(2, convert.ProviderOpenAICompatible),
		sharedPlanProvider(3, convert.ProviderCodex),
		sharedPlanProvider(4, convert.ProviderClaude),
	}
	plans := make([]*Plan, len(providers))
	for index, provider := range providers {
		plans[index] = buildSharedPlan(t, client, provider, shared)
	}
	chatA, chatB, responses, native := plans[0], plans[1], plans[2], plans[3]

	// chat 线：system 折进 messages[0]，正文里没有顶层 system。
	if chatA.Protocol != convert.ProtocolOpenAIChat {
		t.Fatalf("chat 候选协议线 = %q", chatA.Protocol)
	}
	chatValue := planBody(t, chatA)
	messages, ok := chatValue.Get("messages")
	if !ok || !messages.IsArray() || len(messages.Items()) == 0 {
		t.Fatalf("chat 正文缺 messages[]: %s", chatA.Body)
	}
	role, _ := messages.Items()[0].StringField("role")
	if role != "system" {
		t.Fatalf("chat 正文首条消息应为 system（system 已折进 messages）: %s", chatA.Body)
	}
	if _, present := chatValue.Get("system"); present {
		t.Fatalf("chat 正文不该有顶层 system: %s", chatA.Body)
	}
	if _, present := chatValue.Get("input"); present {
		t.Fatalf("chat 正文混进了 responses 的 input: %s", chatA.Body)
	}

	// responses 线：system 走 instructions、消息走 input，没有 messages。
	if responses.Protocol != convert.ProtocolOpenAIResponses {
		t.Fatalf("responses 候选协议线 = %q", responses.Protocol)
	}
	responsesValue := planBody(t, responses)
	if _, present := responsesValue.Get("instructions"); !present {
		t.Fatalf("responses 正文缺 instructions: %s", responses.Body)
	}
	if _, present := responsesValue.Get("input"); !present {
		t.Fatalf("responses 正文缺 input: %s", responses.Body)
	}
	if _, present := responsesValue.Get("messages"); present {
		t.Fatalf("responses 正文混进了 chat 的 messages: %s", responses.Body)
	}

	// 原生同线候选不转换：正文就是客户端快照那一片字节，不复制。
	if native.Conversion != nil {
		t.Fatal("claude 候选不该有转换计划")
	}
	if !bytes.Equal(native.Body, client.Body) {
		t.Fatalf("原生同线候选的正文被改写: %s", native.Body)
	}

	// 共享生效：同协议线的两路候选拿到的是**同一片**字节。
	if len(chatA.Body) == 0 || &chatA.Body[0] != &chatB.Body[0] {
		t.Fatal("同目标协议线的两个候选未共用同一份正文")
	}
	if &chatA.Body[0] == &responses.Body[0] {
		t.Fatal("不同目标协议线的候选共用同一份正文——会把一种协议的正文发到另一种协议的上游")
	}

	// 共享不得改变任何一个字节：逐个与「不共享」的编译结果对拍。
	for index, provider := range providers {
		fresh := buildSharedPlan(t, client, provider, nil)
		if !bytes.Equal(plans[index].Body, fresh.Body) {
			t.Fatalf("候选 %d 共享后的正文与不共享时不同:\n shared = %s\n   fresh = %s",
				provider.ID, plans[index].Body, fresh.Body)
		}
	}
}

// TestPlanSharedKeepsPerCandidateRewrites 钉住「共享正文之上的逐候选改写照旧」：
// 同目标协议线、不同模型重定向的两个候选，各自拿到自己的 model 名；而目标名相同的第三个
// 候选与甲共用同一份字节（说明模型名进了编码键，而解码产物仍跨候选共用）。
func TestPlanSharedKeepsPerCandidateRewrites(t *testing.T) {
	client := sharedPlanClient()
	shared := NewPlanShared()
	withRedirect := func(id int64, target string) Provider {
		provider := sharedPlanProvider(id, convert.ProviderOpenAICompatible)
		provider.ModelRedirects = []byte(`{"claude-sonnet-4-5-20250929":"` + target + `"}`)
		return provider
	}

	upstreamA := buildSharedPlan(t, client, withRedirect(1, "upstream-a"), shared)
	upstreamB := buildSharedPlan(t, client, withRedirect(2, "upstream-b"), shared)
	upstreamC := buildSharedPlan(t, client, withRedirect(3, "upstream-a"), shared)

	modelOf := func(plan *Plan) string {
		model, _ := planBody(t, plan).StringField("model")
		return model
	}
	if got := modelOf(upstreamA); got != "upstream-a" {
		t.Fatalf("候选甲 model = %q", got)
	}
	if got := modelOf(upstreamB); got != "upstream-b" {
		t.Fatalf("候选乙 model = %q", got)
	}
	if bytes.Equal(upstreamA.Body, upstreamB.Body) {
		t.Fatal("重定向目标不同的两个候选拿到了同一份正文")
	}
	if bytes.Contains(upstreamA.Body, []byte("upstream-b")) {
		t.Fatalf("发给甲的正文里出现了乙的模型名: %s", upstreamA.Body)
	}
	if bytes.Contains(upstreamB.Body, []byte("upstream-a")) {
		t.Fatalf("发给乙的正文里出现了甲的模型名: %s", upstreamB.Body)
	}

	// 同协议同目标名的候选仍要共用字节：共享没有被模型名拆成「逐候选各算一遍」。
	if &upstreamA.Body[0] != &upstreamC.Body[0] {
		t.Fatal("同协议线且同重定向目标的两个候选未共用同一份正文（共享失效）")
	}
}

// TestPlanSharedDropsStaleBody 钉住正文快照被换掉时旧产物作废（串行路径的整流器会改写快照）。
func TestPlanSharedDropsStaleBody(t *testing.T) {
	shared := NewPlanShared()
	conversion := &convert.ConversionPlan{
		ClientProtocol: convert.ProtocolAnthropicMessages,
		TargetProtocol: convert.ProtocolOpenAIChat,
	}
	first := sharedPlanClient()
	before := shared.conversion(first, conversion, "")
	if before.failure != nil {
		t.Fatalf("首次转换失败: %v", before.failure)
	}

	rectified := first
	rectified.Body = []byte(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"整流后"}]}`)
	after := shared.conversion(rectified, conversion, "")
	if after.failure != nil {
		t.Fatalf("整流后转换失败: %v", after.failure)
	}
	if bytes.Equal(after.body, before.body) {
		t.Fatal("正文换了却复用了旧产物——整流前的正文会被发给上游")
	}
	if !bytes.Contains(after.body, []byte("整流后")) {
		t.Fatalf("正文未反映整流后的快照: %s", after.body)
	}
}

// TestHedgeCandidatesShareConvertedBodyWithoutCrossingProtocols 是端到端版本：竞速两路候选都开
// 转换（claude 客户端 → chat 上游），两台上游各自收到的正文必须都是 chat 形状，且逐字节相同
// ——同协议线共用一份产物，协议线本身不变。
func TestHedgeCandidatesShareConvertedBodyWithoutCrossingProtocols(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	recorder := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("读上游请求正文失败: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer recorder.Close()

	candidate := func(id int64) *Candidate {
		return &Candidate{
			Provider: Provider{
				ID: id, Name: "候选", Type: convert.ProviderOpenAICompatible,
				Key: "sk-up", URL: recorder.URL, MaxRetryAttempts: intPointer(1),
			},
			ConversionEnabled: true,
		}
	}
	harness := newHedgeHarness(t)
	harness.setSelect([]*Candidate{candidate(2)})
	if _, err := ForwardStreamHedge(context.Background(), harness.pc,
		candidate(1), harness.deps, harness.options, harness.cfg); err == nil {
		t.Fatal("两台上游都 500，竞速必须返回错误")
	}

	mu.Lock()
	received := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("应有两路上游收到请求，实际 %d 路", len(received))
	}
	for index, body := range received {
		value, err := convert.ParseJSON(body)
		if err != nil {
			t.Fatalf("第 %d 路上游收到的正文不是合法 JSON: %v (%s)", index+1, err, body)
		}
		messages, present := value.Get("messages")
		if !present || !messages.IsArray() {
			t.Fatalf("第 %d 路上游收到的不是 chat 正文: %s", index+1, body)
		}
		if _, present := value.Get("system"); present {
			t.Fatalf("第 %d 路上游收到了 anthropic 形状的顶层 system: %s", index+1, body)
		}
	}
	if !bytes.Equal(received[0], received[1]) {
		t.Fatalf("同协议线的两路候选收到的正文不同:\n 甲 = %s\n 乙 = %s", received[0], received[1])
	}
}

// TestHedgeCandidatesKeepOwnRedirectAcrossSameProtocol 是竞速端到端的第二条钉子：两个候选走
// **同一条**目标协议线（chat），但模型重定向目标不同。共享产物的键少了目标名时它们会串用同一份
// 正文，于是有一路把别人的模型名发给上游——这里逐台记录器核对各自收到的 model。
func TestHedgeCandidatesKeepOwnRedirectAcrossSameProtocol(t *testing.T) {
	record := func() (*httptest.Server, func() []string) {
		var mu sync.Mutex
		models := []string{}
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			body, err := io.ReadAll(request.Body)
			if err != nil {
				t.Errorf("读上游请求正文失败: %v", err)
			}
			value, err := convert.ParseJSON(body)
			if err != nil {
				t.Errorf("上游收到的正文不是合法 JSON: %v (%s)", err, body)
			} else {
				model, _ := value.StringField("model")
				mu.Lock()
				models = append(models, model)
				mu.Unlock()
			}
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = writer.Write([]byte(`{"error":{"message":"boom"}}`))
		}))
		return server, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), models...)
		}
	}
	serverA, modelsA := record()
	defer serverA.Close()
	serverB, modelsB := record()
	defer serverB.Close()

	candidate := func(id int64, url string, target string) *Candidate {
		return &Candidate{
			Provider: Provider{
				ID: id, Name: "候选", Type: convert.ProviderOpenAICompatible,
				Key: "sk-up", URL: url, MaxRetryAttempts: intPointer(1),
				// 同一份规则源、不同目标名：这正是「同协议不同重定向」的真实形态。
				ModelRedirects: []byte(`{"claude-sonnet-4-5-20250929":"` + target + `"}`),
			},
			ConversionEnabled: true,
		}
	}
	harness := newHedgeHarness(t)
	harness.setSelect([]*Candidate{candidate(2, serverB.URL, "upstream-b")})
	if _, err := ForwardStreamHedge(context.Background(), harness.pc,
		candidate(1, serverA.URL, "upstream-a"), harness.deps, harness.options, harness.cfg); err == nil {
		t.Fatal("两台上游都 500，竞速必须返回错误")
	}

	if got := modelsA(); len(got) != 1 || got[0] != "upstream-a" {
		t.Fatalf("候选甲收到的 model = %v，期望 [upstream-a]（同协议不同重定向不得串用）", got)
	}
	if got := modelsB(); len(got) != 1 || got[0] != "upstream-b" {
		t.Fatalf("候选乙收到的 model = %v，期望 [upstream-b]（同协议不同重定向不得串用）", got)
	}
}
