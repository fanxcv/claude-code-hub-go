package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

func claudeBody(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("构造 claude 请求体失败: %v", err)
	}
	return body
}

func TestFingerprintChainIsStableAndOrdered(t *testing.T) {
	body := claudeBody(t, `{
		"system": "you are helpful",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type": "text", "text": "hello"}]}
		]
	}`)
	first, ok := Fingerprint(body, convert.FormatClaude, 8)
	if !ok {
		t.Fatalf("claude 请求体应可指纹化")
	}
	second, _ := Fingerprint(body, convert.FormatClaude, 8)
	if first.Sys.FP != second.Sys.FP || len(first.Tail) != len(second.Tail) {
		t.Fatalf("同一请求体两次指纹化结果不一致")
	}
	if len(first.Tail) != 2 {
		t.Fatalf("tail 长度 = %d，期望 2", len(first.Tail))
	}
	if first.Tail[0].Depth != 1 || first.Tail[1].Depth != 2 {
		t.Errorf("depth 应为 1/2，实际 %d/%d", first.Tail[0].Depth, first.Tail[1].Depth)
	}
	if first.Tail[1].PrefixBytes <= first.Tail[0].PrefixBytes {
		t.Errorf("累计前缀字节应单调递增")
	}
	if first.Tip().FP != first.Tail[1].FP {
		t.Errorf("tip 应为最深边界")
	}
	deepest := first.DeepestFirst()
	if len(deepest) != 2 || deepest[0] != first.Tail[1].FP || deepest[1] != first.Tail[0].FP {
		t.Errorf("deepestFirst 应从深到浅且不含 F_sys: %v", deepest)
	}
	if strings.Contains(strings.Join(deepest, ","), first.Sys.FP) {
		t.Errorf("deepestFirst 不应包含 F_sys")
	}
}

// TestFingerprintPrefixSurvivesAppendedTurn 钉住多轮复用赖以成立的不变量：
// **追加一轮对话后，旧前缀的指纹逐个不变**。
//
// 这就是用户说的「同会话应当继续用上一次那家」能成立的根据：第 N+1 轮的最长未变前缀与第 N 轮
// 的 tip 同指纹，查找自然命回同一供应商（→ 上游缓存可命中）。若归一化里混入任何随轮次变化的东西
// （时间戳、递增 id、消息序号等），这条会红，而链的其余断言看不出来。
func TestFingerprintPrefixSurvivesAppendedTurn(t *testing.T) {
	twoTurns := claudeBody(t, `{
		"system": "you are helpful",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type": "text", "text": "hello"}]}
		]
	}`)
	threeTurns := claudeBody(t, `{
		"system": "you are helpful",
		"messages": [
			{"role": "user", "content": "hi"},
			{"role": "assistant", "content": [{"type": "text", "text": "hello"}]},
			{"role": "user", "content": "继续"}
		]
	}`)

	before, ok := Fingerprint(twoTurns, convert.FormatClaude, 8)
	if !ok {
		t.Fatalf("两轮请求体应可指纹化")
	}
	after, ok := Fingerprint(threeTurns, convert.FormatClaude, 8)
	if !ok {
		t.Fatalf("三轮请求体应可指纹化")
	}

	if before.Sys.FP != after.Sys.FP {
		t.Fatalf("追加一轮不得改 F_sys：%s vs %s", before.Sys.FP, after.Sys.FP)
	}
	if len(after.Tail) != len(before.Tail)+1 {
		t.Fatalf("tail 长度应 +1，实际 %d -> %d", len(before.Tail), len(after.Tail))
	}
	for index, old := range before.Tail {
		if after.Tail[index].FP != old.FP {
			t.Fatalf("第 %d 个旧前缀的指纹被追加轮次改变：%s -> %s",
				index+1, old.FP, after.Tail[index].FP)
		}
	}
	// 上一轮的 tip 必须仍能在新一轮的最深→最浅序列里找到（查找就是靠它命中）。
	if before.Tip().FP != after.Tail[len(after.Tail)-2].FP {
		t.Fatalf("上一轮的 tip 应与新一轮的次深边界同指纹")
	}
}

func TestFingerprintIgnoresToolOrderAndVolatileIds(t *testing.T) {
	base := `{
		"system": "sys",
		"tools": [
			{"name": "b", "description": "B", "input_schema": {"type": "object"}},
			{"name": "a", "description": "A", "input_schema": {"type": "object"}}
		],
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_1", "name": "a", "input": {"x": 1}},
				{"type": "text", "text": "done"}
			]}
		]
	}`
	// 工具顺序调换、tool_use 的 id 变化，其余逐字相同。
	reordered := `{
		"system": "sys",
		"tools": [
			{"name": "a", "description": "A", "input_schema": {"type": "object"}},
			{"name": "b", "description": "B", "input_schema": {"type": "object"}}
		],
		"messages": [
			{"role": "assistant", "content": [
				{"type": "tool_use", "id": "toolu_999", "name": "a", "input": {"x": 1}},
				{"type": "text", "text": "done"}
			]}
		]
	}`
	first, _ := Fingerprint(claudeBody(t, base), convert.FormatClaude, 8)
	second, _ := Fingerprint(claudeBody(t, reordered), convert.FormatClaude, 8)

	if first.Sys.FP != second.Sys.FP {
		t.Errorf("工具顺序差异不应改变 F_sys（Node 侧 appendTools 按 name 排序）")
	}
	if first.Tail[0].FP != second.Tail[0].FP {
		t.Errorf("tool_use 的 id 属易变键，不应进入指纹")
	}

	// 内容块顺序是 Node 的语义组成部分：换序即换指纹（此处钉住，避免后人误「优化」成排序）。
	blockReordered := claudeBody(t, `{
		"system": "sys",
		"messages": [
			{"role": "assistant", "content": [
				{"type": "text", "text": "done"},
				{"type": "tool_use", "id": "toolu_1", "name": "a", "input": {"x": 1}}
			]}
		]
	}`)
	third, _ := Fingerprint(blockReordered, convert.FormatClaude, 8)
	if third.Tail[0].FP == first.Tail[0].FP {
		t.Errorf("内容块顺序变化应改变指纹（Node 不做块排序）")
	}
}

func TestFingerprintWindowTruncationKeepsDeepest(t *testing.T) {
	var messages []string
	for index := 0; index < 5; index++ {
		messages = append(messages, `{"role": "user", "content": "m`+string(rune('0'+index))+`"}`)
	}
	body := claudeBody(t, `{"messages": [`+strings.Join(messages, ",")+`]}`)

	full, _ := Fingerprint(body, convert.FormatClaude, 8)
	windowed, _ := Fingerprint(body, convert.FormatClaude, 2)

	if len(full.Tail) != 5 {
		t.Fatalf("未截断时 tail 长度 = %d，期望 5", len(full.Tail))
	}
	if len(windowed.Tail) != 2 {
		t.Fatalf("窗口 2 时 tail 长度 = %d，期望 2", len(windowed.Tail))
	}
	if windowed.Tail[1].FP != full.Tail[4].FP {
		t.Errorf("截断应保留最深边界")
	}
	if windowed.Tail[1].Depth != 5 {
		t.Errorf("截断后 depth 应保留原始深度，实际 %d", windowed.Tail[1].Depth)
	}
}

func TestFingerprintMarksCacheControl(t *testing.T) {
	body := claudeBody(t, `{
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "x", "cache_control": {"type": "ephemeral"}}]}
		]
	}`)
	chain, ok := Fingerprint(body, convert.FormatClaude, 8)
	if !ok || len(chain.Tail) != 1 {
		t.Fatalf("应产出 1 个会话边界")
	}
	if !chain.Tail[0].HasCacheControl {
		t.Errorf("显式缓存断点应被标记")
	}
}

func TestFingerprintSkipsEmptyMessages(t *testing.T) {
	body := claudeBody(t, `{
		"messages": [
			{"role": "user", "content": []},
			{"role": "assistant", "content": "real"}
		]
	}`)
	chain, ok := Fingerprint(body, convert.FormatClaude, 8)
	if !ok {
		t.Fatalf("应可指纹化")
	}
	if len(chain.Tail) != 1 {
		t.Fatalf("空内容消息不应产生边界，实际 %d 个", len(chain.Tail))
	}
}

func TestFingerprintSupportsOpenAIAndResponses(t *testing.T) {
	chat := `{
		"messages": [
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "hi"}
		],
		"tools": [{"type": "function", "function": {"name": "f", "description": "F"}}]
	}`
	var chatBody map[string]any
	if err := json.Unmarshal([]byte(chat), &chatBody); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	chain, ok := Fingerprint(chatBody, convert.FormatOpenAI, 8)
	if !ok || len(chain.Tail) != 1 {
		t.Fatalf("openai chat：前导 system 并入 F_sys，只剩 1 条会话消息，实际 %d", len(chain.Tail))
	}

	responses := `{"instructions": "sys", "input": [{"type": "message", "role": "user", "content": "hi"}]}`
	var responsesBody map[string]any
	if err := json.Unmarshal([]byte(responses), &responsesBody); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	chain, ok = Fingerprint(responsesBody, convert.FormatResponse, 8)
	if !ok || len(chain.Tail) != 1 {
		t.Fatalf("responses：应产出 1 条会话消息")
	}
}

func TestFingerprintUnsupportedFormatsAndBodies(t *testing.T) {
	// gemini 的 contents 为空数组时**可**指纹化（只有 F_sys，无会话边界），与 Node 同结论。
	chain, ok := Fingerprint(map[string]any{"contents": []any{}}, convert.FormatGemini, 8)
	if !ok || len(chain.Tail) != 0 {
		t.Errorf("空 contents 应产出只有 F_sys 的链，实际 ok=%v 边界=%d", ok, len(chain.Tail))
	}
	if _, ok := Fingerprint(map[string]any{"contents": "nope"}, convert.FormatGemini, 8); ok {
		t.Errorf("contents 非数组应不可指纹化")
	}
	if _, ok := Fingerprint(map[string]any{}, convert.FormatClaude, 8); ok {
		t.Errorf("缺少 messages 的请求体应不可指纹化")
	}
	if _, ok := Fingerprint(nil, convert.FormatClaude, 8); ok {
		t.Errorf("空请求体应不可指纹化")
	}
	if _, ok := Fingerprint(map[string]any{"contents": []any{}}, convert.ClientFormat("bogus"), 8); ok {
		t.Errorf("未知客户端格式应不可指纹化")
	}
}

func TestAffinityWindowNormalization(t *testing.T) {
	cases := map[int]int{0: 8, -3: 8, 4: 4, 100: 64}
	for input, want := range cases {
		if got := AffinityWindow(input); got != want {
			t.Errorf("AffinityWindow(%d) = %d，期望 %d", input, got, want)
		}
	}
}

func TestScopeTagShapeAndSensitivity(t *testing.T) {
	tag := ScopeTag(42, convert.FormatClaude, "claude-3-5-sonnet")
	if len(tag) != 16 {
		t.Errorf("scope tag 长度 = %d，期望 16", len(tag))
	}
	if tag == ScopeTag(43, convert.FormatClaude, "claude-3-5-sonnet") {
		t.Errorf("不同 key 不应共用 scope tag")
	}
	if tag == ScopeTag(42, convert.FormatClaude, "claude-3-5-haiku") {
		t.Errorf("不同模型不应共用 scope tag")
	}
	if tag != ScopeTag(42, convert.FormatClaude, "claude-3-5-sonnet") {
		t.Errorf("同输入应稳定同值")
	}
}

func TestAffinityValueProvider(t *testing.T) {
	if id, ok := affinityValueProvider("1|7|fpxyz|v3:abc"); !ok || id != 7 {
		t.Errorf("四段活跃绑定应解析出 providerId，实际 %d ok=%v", id, ok)
	}
	if id, ok := affinityValueProvider("1|9"); !ok || id != 9 {
		t.Errorf("两段旧格式应解析出 providerId")
	}
	if _, ok := affinityValueProvider("garbage"); ok {
		t.Errorf("非法值不应命中")
	}
	if _, ok := affinityValueProvider("1|0|x|y"); ok {
		t.Errorf("非正 providerId 不应命中")
	}
}

func TestAffinityGenerationTokenIsUUIDShaped(t *testing.T) {
	token := newAffinityGenerationToken()
	if !strings.HasPrefix(token, "v3:") {
		t.Errorf("generation token 应以 v3: 开头，实际 %q", token)
	}
	if got, want := len(token), len("v3:")+36; got != want {
		t.Errorf("generation token 长度 = %d，期望 %d（uuid4）", got, want)
	}
	if newAffinityGenerationToken() == token {
		t.Errorf("generation token 必须每次不同：相同 token 会让 fence 失去意义")
	}
}

// 未注入 token 时必须走默认实现（生产路径），否则 generation 会是空串、写回全被拒。
func TestAffinityStoreDefaultsGenerationToken(t *testing.T) {
	store := NewAffinityStore(AffinityOptions{Window: 8})
	first := store.generationToken()
	if !strings.HasPrefix(first, "v3:") {
		t.Errorf("默认 generation 应为 v3:<uuid4>，实际 %q", first)
	}
	if store.generationToken() == first {
		t.Errorf("默认 generation 每次调用都应不同")
	}
	injected := NewAffinityStore(AffinityOptions{GenerationToken: func() string { return "v3:fixed" }})
	if injected.generationToken() != "v3:fixed" {
		t.Errorf("注入的 token 应被采用")
	}
}

// Lua 数值经 go-redis 泛化后可能是多种整型；归一化错了会让 fence 的 0/1 判断静默失效。
func TestToInt64NormalizesLuaNumbers(t *testing.T) {
	for name, testCase := range map[string]struct {
		value any
		want  int64
		ok    bool
	}{
		"int64":   {int64(1), 1, true},
		"int":     {int(0), 0, true},
		"float64": {float64(1), 1, true},
		"string":  {"2", 2, true},
		"坏字符串":    {"x", 0, false},
		"nil":     {nil, 0, false},
		"布尔":      {true, 0, false},
	} {
		got, ok := toInt64(testCase.value)
		if ok != testCase.ok || (ok && got != testCase.want) {
			t.Errorf("%s: toInt64(%#v) = (%d, %v)，期望 (%d, %v)", name, testCase.value, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestAffinityKeyShapes(t *testing.T) {
	store := NewAffinityStore(AffinityOptions{Window: 8})
	cases := map[string]string{
		store.bindingKey("scope", "fp"):       "cch:pfx:{scope}:fp:fp",
		store.generationKey("scope", "id"):    "cch:pfx:{scope}:gen:id",
		store.descendantsKey("scope", "id"):   "cch:pfx:{scope}:desc:id",
		store.descendantsV2Key("scope", "id"): "cch:pfx:{scope}:desc-v2:id",
		store.legacyGenerationKey("scope"):    "cch:pfx:{scope}:generation",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("键形制不符：got %q want %q", got, want)
		}
	}
}

// 候选选择与墓碑跳过由 LOOKUP_CANDIDATES_LUA 完成，故须真实 Redis 才可验证：
// 见 affinity_integration_test.go 的 TestIntegrationAffinityLookupSkipsTombstoneAndRenewsTTL。

func TestLookupFailsOpenWithoutRedis(t *testing.T) {
	store := NewAffinityStore(AffinityOptions{})
	if _, ok := store.Lookup(context.Background(), "scope", []string{"fp"}); ok {
		t.Errorf("无 Redis 时应视为不可用")
	}
	if _, ok := store.Lookup(context.Background(), "", []string{"fp"}); ok {
		t.Errorf("缺 scope tag 时应视为不可用")
	}
	if _, ok := store.Lookup(context.Background(), "scope", nil); ok {
		t.Errorf("无候选指纹时应视为不可用")
	}
	if _, ok := store.Lookup(context.Background(), "scope", []string{"", ""}); ok {
		t.Errorf("候选全是空串时应视为不可用")
	}
	if store.Put(context.Background(), "scope", "fp", 7, "id", "gen") {
		t.Errorf("无 Redis 时写回应失败")
	}
	if store.Tombstone(context.Background(), "scope", "fp", "failover", "id", "gen") {
		t.Errorf("无 Redis 时墓碑应失败")
	}
	if store.Invalidate(context.Background(), "scope", "id", []string{"fp"}) {
		t.Errorf("无 Redis 时失效应失败")
	}
}

// Put 的 TTL 与参数不足的拒绝路径（Node put/tombstone 的前置校验）。
func TestAffinityWriteRejectsInsufficientArguments(t *testing.T) {
	store := NewAffinityStore(AffinityOptions{Redis: &fakeRedis{values: map[string]string{}}, Window: 8, SlidingTTLSeconds: 60})
	cases := map[string]bool{
		"缺 scope":      store.Put(context.Background(), "", "fp", 7, "id", "gen"),
		"缺 tip":        store.Put(context.Background(), "scope", "", 7, "id", "gen"),
		"非正供应":         store.Put(context.Background(), "scope", "fp", 0, "id", "gen"),
		"缺 identity":   store.Put(context.Background(), "scope", "fp", 7, "", "gen"),
		"缺 generation": store.Put(context.Background(), "scope", "fp", 7, "id", ""),
		"缺 fp":         store.Tombstone(context.Background(), "scope", "", "failover", "id", "gen"),
	}
	for name, wrote := range cases {
		if wrote {
			t.Errorf("%s：应拒绝写入", name)
		}
	}
	// TTL 未配置时 Put 必须在触达 Redis 之前就放弃（Node 的 ttlSeconds <= 0 分支）：
	// 这里刻意不接 Redis，若实现先访问 Redis 本用例会 panic 而不是报错。
	noTTL := NewAffinityStore(AffinityOptions{Window: 8})
	if noTTL.Put(context.Background(), "scope", "fp", 7, "id", "gen") {
		t.Errorf("未配置 TTL 时写回应失败（Node 的 ttlSeconds <= 0 分支）")
	}

	// nil 接收者：Put 的 godoc 承诺「参数不足/未配置 Redis 即返回 false」，而实现曾先读
	// a.slidingTTLSeconds 再判空（守卫是死代码）⇒ nil 接收者会 panic 而不是返回 false。
	// 同文件 Lookup/Tombstone/Invalidate 都是先判空，这里钉住 Put 与之一致。
	var nilStore *AffinityStore
	if nilStore.Put(context.Background(), "scope", "fp", 7, "id", "gen") {
		t.Errorf("nil 接收者应返回 false，不得 panic")
	}
}

// 命中路径的注入式验证：亲和查找的真实语义由 Redis 集成测试覆盖，
// 此处只钉「命中后的硬校验 + 提名携带的写回信息」。
func TestNominateByAffinityRequiresHardValidation(t *testing.T) {
	target := baseProvider(7, convert.ProviderClaude)
	other := baseProvider(8, convert.ProviderClaude)
	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)

	// Redis 刻意留空：命中来自注入的 lookup，证明选路阶段不会重复访问 Redis。
	affinity := NewAffinityStore(AffinityOptions{Window: 8})

	chain, _ := Fingerprint(body, convert.FormatClaude, 8)
	lookup := &AffinityLookup{
		Hint:       &AffinityHint{ProviderID: 7, MatchedFP: chain.Tip().FP, MatchedIndex: 0},
		IdentityFP: "idfp",
		Generation: "v3:gen",
	}

	source := &stubSource{providers: []Provider{target, other}, byID: map[int64]Provider{7: target, 8: other}}
	selector := NewSelector(Options{
		Source:   source,
		Affinity: affinity,
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	result, err := selector.Select(context.Background(), Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42, AffinityBody: body,
		AffinityLookup: lookup,
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 7 {
		t.Fatalf("亲和应提名供应商 7，实际 %+v", result.Provider)
	}
	if result.Method != MethodPrefixAffinity || result.Reason != ReasonSelectedAffinity {
		t.Errorf("方法与原因不符: %s / %s", result.Method, result.Reason)
	}
	if result.Affinity == nil || result.Affinity.Hint.MatchedFP != chain.Tip().FP {
		t.Errorf("应记录命中指纹")
	}
	item := result.ChainItem()
	if item.Affinity == nil || item.Affinity.MatchedFP != chain.Tip().FP {
		t.Errorf("落链项应带亲和详情: %+v", item.Affinity)
	}
	if item.Affinity.MatchedDepth == nil || *item.Affinity.MatchedDepth != 1 {
		t.Errorf("应记录命中深度")
	}
	// 终态写回所需的四项必须随提名一并交出，否则调用方只能自行重建 scope 与指纹。
	if result.Affinity.ScopeTag != ScopeTag(42, convert.FormatClaude, "m") {
		t.Errorf("提名应携带 scope tag，实际 %q", result.Affinity.ScopeTag)
	}
	if result.Affinity.TipFP != chain.Tip().FP {
		t.Errorf("提名应携带 tip 指纹，实际 %q", result.Affinity.TipFP)
	}
	if result.Affinity.IdentityFP != "idfp" || result.Affinity.Generation != "v3:gen" {
		t.Errorf("提名应携带 identity/generation，实际 %q / %q",
			result.Affinity.IdentityFP, result.Affinity.Generation)
	}
	if result.AffinityLookup != lookup {
		t.Errorf("结果应原样带回本次 lookup（写回需要其 generation）")
	}
}

func TestNominateByAffinityFallsBackWhenCandidateDisabled(t *testing.T) {
	target := baseProvider(7, convert.ProviderClaude)
	target.IsEnabled = false
	other := baseProvider(8, convert.ProviderClaude)
	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)

	affinity := NewAffinityStore(AffinityOptions{Window: 8})
	chain, _ := Fingerprint(body, convert.FormatClaude, 8)
	lookup := &AffinityLookup{
		Hint:       &AffinityHint{ProviderID: 7, MatchedFP: chain.Tip().FP, MatchedIndex: 0},
		IdentityFP: "idfp",
		Generation: "v3:gen",
	}

	source := &stubSource{providers: []Provider{target, other}, byID: map[int64]Provider{7: target, 8: other}}
	selector := NewSelector(Options{
		Source:   source,
		Affinity: affinity,
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42, AffinityBody: body,
		AffinityLookup: lookup,
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 8 {
		t.Fatalf("亲和候选被硬校验拒绝时应静默回落加权随机，实际 %+v", result.Provider)
	}
	if result.Method != MethodWeightedRandom {
		t.Errorf("回落后的 selectionMethod = %q，期望 weighted_random", result.Method)
	}
	// 硬校验拒绝只否掉「提名」，不否掉写回信息：这条请求若最终成功，仍应更新 tip 绑定。
	if result.Affinity != nil {
		t.Errorf("被拒绝的候选不应产生提名: %+v", result.Affinity)
	}
	if result.AffinityLookup != lookup {
		t.Errorf("被拒绝的候选仍应带回 lookup")
	}
}

// 未命中也必须交回 lookup：对话的首条请求正是「未命中但终态要写回」的情形。
func TestSelectReturnsLookupOnMiss(t *testing.T) {
	provider := baseProvider(7, convert.ProviderClaude)
	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)
	affinity := NewAffinityStore(AffinityOptions{Window: 8})

	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{provider}, byID: map[int64]Provider{7: provider}},
		Affinity: affinity,
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42, AffinityBody: body,
		AffinityLookup: &AffinityLookup{IdentityFP: "idfp", Generation: "v3:gen"},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Affinity != nil {
		t.Errorf("未命中不应产生提名")
	}
	if result.AffinityLookup == nil || result.AffinityLookup.Generation != "v3:gen" {
		t.Errorf("未命中应带回 lookup 供终态写回，实际 %+v", result.AffinityLookup)
	}
}

// 亲和存储不可用（Redis 故障/未配置）时整体 fail-open：不提名，也不留 lookup。
func TestAffinityUnavailableFailsOpenWithoutLookup(t *testing.T) {
	target := baseProvider(7, convert.ProviderClaude)
	body := claudeBody(t, `{"messages": [{"role": "user", "content": "hi"}]}`)

	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{target}, byID: map[int64]Provider{7: target}},
		Affinity: NewAffinityStore(AffinityOptions{Window: 8}),
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})
	result, err := selector.Select(context.Background(), Request{
		Model: "m", Format: convert.FormatClaude, KeyID: 42, AffinityBody: body,
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Affinity != nil || result.AffinityLookup != nil {
		t.Errorf("亲和不可用时应静默回落且不留状态: %+v / %+v", result.Affinity, result.AffinityLookup)
	}
	if result.Provider == nil || result.Provider.ID != 7 {
		t.Errorf("应正常回落加权随机，实际 %+v", result.Provider)
	}
}

func TestNominateByAffinityRequiresKeyAndBody(t *testing.T) {
	provider := baseProvider(1, convert.ProviderClaude)
	affinity := NewAffinityStore(AffinityOptions{Window: 8})

	selector := NewSelector(Options{
		Source:   &stubSource{providers: []Provider{provider}, byID: map[int64]Provider{1: provider}},
		Affinity: affinity,
		Rand:     (&scriptedRand{values: []float64{0}}).next,
	})

	for name, req := range map[string]Request{
		"缺 key": {Model: "m", Format: convert.FormatClaude, AffinityBody: map[string]any{"messages": []any{}}},
		"缺正文":   {Model: "m", Format: convert.FormatClaude, KeyID: 1},
		"缺格式":   {Model: "m", KeyID: 1, AffinityBody: map[string]any{"messages": []any{}}},
	} {
		result, err := selector.Select(context.Background(), req)
		if err != nil {
			t.Fatalf("%s：选路失败: %v", name, err)
		}
		if result.Method == MethodPrefixAffinity {
			t.Errorf("%s：不应走亲和提名", name)
		}
	}
}
