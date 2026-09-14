package guard

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 路径读写是过滤器的地基：危险键必须整条作废，数组下标要能穿越。
func TestParseFilterPath(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		wantCount int
	}{
		{name: "点号路径", path: "a.b.c", wantCount: 3},
		{name: "数组下标", path: "messages[0].content", wantCount: 3},
		{name: "空路径", path: "   ", wantCount: 0},
		{name: "原型污染键整条作废", path: "__proto__.polluted", wantCount: 0},
		{name: "constructor 键作废", path: "a.constructor.b", wantCount: 0},
		{name: "prototype 键作废", path: "prototype", wantCount: 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := len(parseFilterPath(testCase.path)); got != testCase.wantCount {
				t.Fatalf("路径 %q 应解析出 %d 段，收到 %d", testCase.path, testCase.wantCount, got)
			}
		})
	}
}

func TestSetGetDeleteByPath(t *testing.T) {
	body := map[string]any{}

	setValueByPath(body, "a.b.c", "v")
	value, ok := getValueByPath(body, "a.b.c")
	if !ok || value != "v" {
		t.Fatalf("设置后应能读到 v，收到 %v (ok=%v)", value, ok)
	}

	setValueByPath(body, "arr[0].name", "first")
	value, ok = getValueByPath(body, "arr[0].name")
	if !ok || value != "first" {
		t.Fatalf("数组路径写入失败: %v (ok=%v)", value, ok)
	}

	deleteByPath(body, "a.b.c")
	if _, ok := getValueByPath(body, "a.b.c"); ok {
		t.Fatal("删除后不应再读到该路径")
	}
}

// 深度相等是去重与断言的基础。
func TestDeepEqualValues(t *testing.T) {
	cases := []struct {
		name  string
		left  any
		right any
		want  bool
	}{
		{name: "标量相等", left: "a", right: "a", want: true},
		{name: "标量不等", left: "a", right: "b"},
		{name: "数字相等", left: float64(1), right: float64(1), want: true},
		{name: "数组长度不同", left: []any{float64(1)}, right: []any{float64(1), float64(2)}},
		{
			name:  "对象键顺序无关",
			left:  map[string]any{"a": float64(1), "b": float64(2)},
			right: map[string]any{"b": float64(2), "a": float64(1)},
			want:  true,
		},
		{name: "对象多键不等", left: map[string]any{"a": float64(1)}, right: map[string]any{}},
		{name: "nil 与 nil", left: nil, right: nil, want: true},
		{name: "nil 与值", left: nil, right: float64(1)},
		{name: "跨类型不等", left: "1", right: float64(1)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := deepEqualValues(testCase.left, testCase.right); got != testCase.want {
				t.Fatalf("应为 %v，收到 %v", testCase.want, got)
			}
		})
	}
}

// 深合并的 null 语义是删除键，这是配置里「用 null 关掉上游默认值」的落点。
func TestDeepMergeValues(t *testing.T) {
	target := map[string]any{
		"keep":   "v",
		"remove": "x",
		"nested": map[string]any{"inner": "1", "stay": "2"},
	}
	deepMergeValues(target, map[string]any{
		"remove": nil,
		"nested": map[string]any{"inner": "9"},
		"new":    "n",
	})

	if _, exists := target["remove"]; exists {
		t.Fatal("null 应删除键")
	}
	if target["keep"] != "v" {
		t.Fatal("未提及的键应保持")
	}
	nested := target["nested"].(map[string]any)
	if nested["inner"] != "9" || nested["stay"] != "2" {
		t.Fatalf("嵌套应递归合并且保留未提及的键: %v", nested)
	}
	if target["new"] != "n" {
		t.Fatal("新键应写入")
	}
}

// 原型污染是防护点：危险键不得被合并进目标。
func TestDeepMergeBlocksUnsafeKeys(t *testing.T) {
	target := map[string]any{}
	source := map[string]any{"safe": "1"}
	source["__proto__"] = map[string]any{"polluted": true}
	deepMergeValues(target, source)

	if target["safe"] != "1" {
		t.Fatal("正常键应写入")
	}
	if _, exists := target["__proto__"]; exists {
		t.Fatal("__proto__ 必须被拒绝")
	}
}

func TestReplaceFilterText(t *testing.T) {
	if got := replaceFilterText("a-b-c", "-", "_", MatchContains); got != "a_b_c" {
		t.Fatalf("包含匹配替换失败: %q", got)
	}
	if got := replaceFilterText("exact", "exact", "hit", MatchExact); got != "hit" {
		t.Fatalf("精确替换失败: %q", got)
	}
	if got := replaceFilterText("exact", "exa", "hit", MatchExact); got != "exact" {
		t.Fatalf("精确匹配要求整串相等: %q", got)
	}
	if got := replaceFilterText("v1.2.3", `\d+`, "N", MatchRegex); got != "vN.N.N" {
		t.Fatalf("正则替换失败: %q", got)
	}
	// 非法正则不得改变输入（RE2 不支持的语法）。
	if got := replaceFilterText("abc", "(?<=a)b", "X", MatchRegex); got != "abc" {
		t.Fatalf("非法正则应原样返回: %q", got)
	}
	if got := replaceFilterText("abc", "", "X", MatchContains); got != "abc" {
		t.Fatalf("空目标不应替换: %q", got)
	}
}

// 简单模式的 body 过滤器：json_path 写入与 text_replace 递归替换。
func TestApplyBodyFilterSimple(t *testing.T) {
	deps := Deps{}

	body := map[string]any{"messages": []any{map[string]any{"content": "secret"}}}
	deps.applyBodyFilter(RequestFilter{
		ID:          1,
		Scope:       "body",
		Action:      "text_replace",
		Target:      "secret",
		Replacement: json.RawMessage(`"***"`),
		MatchType:   MatchContains,
	}, body)

	content := body["messages"].([]any)[0].(map[string]any)["content"]
	if content != "***" {
		t.Fatalf("文本替换失败: %v", content)
	}

	deps.applyBodyFilter(RequestFilter{
		ID:          2,
		Scope:       "body",
		Action:      "json_path",
		Target:      "temperature",
		Replacement: json.RawMessage(`0.5`),
	}, body)

	value, ok := getValueByPath(body, "temperature")
	if !ok || value != 0.5 {
		t.Fatalf("json_path 写入失败: %v (ok=%v)", value, ok)
	}
}

// 高级模式：set（含 if_missing）、remove（含元素匹配）、merge、insert（含去重与锚点）。
func TestApplyAdvancedOperations(t *testing.T) {
	operations := json.RawMessage(`[
		{"op":"set","scope":"body","path":"metadata.user_id","value":"u-1"},
		{"op":"set","scope":"body","path":"metadata.user_id","value":"u-2","writeMode":"if_missing"},
		{"op":"merge","scope":"body","path":"metadata","value":{"tag":"a","drop":null}},
		{"op":"insert","scope":"body","path":"messages","position":"start","value":{"role":"system"}},
		{"op":"insert","scope":"body","path":"messages","position":"end","value":{"role":"system"}},
		{"op":"remove","scope":"body","path":"messages","matcher":{"field":"role","value":"system","matchType":"exact"}}
	]`)

	body := map[string]any{
		"messages": []any{map[string]any{"role": "user"}},
		"metadata": map[string]any{"drop": "x"},
		"keep":     "v",
	}
	applyBodyOperation(filterOperation{
		Op:    "merge",
		Path:  "metadata",
		Value: map[string]any{},
	}, body)

	var parsed []filterOperation
	if err := json.Unmarshal(operations, &parsed); err != nil {
		t.Fatalf("解析操作失败: %v", err)
	}
	for _, operation := range parsed {
		applyBodyOperation(operation, body)
	}

	metadata := body["metadata"].(map[string]any)
	if metadata["user_id"] != "u-1" {
		t.Fatalf("if_missing 不应覆盖已有值，收到 %v", metadata["user_id"])
	}
	if metadata["tag"] != "a" {
		t.Fatalf("merge 应写入 tag，收到 %v", metadata["tag"])
	}
	if _, exists := metadata["drop"]; exists {
		t.Fatal("merge 的 null 应删除键")
	}
	messages := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("insert 的去重与 remove 的匹配后应只剩 1 条，收到 %d", len(messages))
	}
	if messages[0].(map[string]any)["role"] != "user" {
		t.Fatalf("剩余消息应为 user: %v", messages[0])
	}
}

// insert 的锚点语义：找不到锚点时按 onAnchorMissing 回退。
func TestInsertAnchorFallback(t *testing.T) {
	body := map[string]any{"items": []any{map[string]any{"id": "a"}}}

	applyBodyOperation(filterOperation{
		Op:              "insert",
		Path:            "items",
		Value:           map[string]any{"id": "b"},
		Position:        "before",
		Anchor:          &filterMatcher{Field: "id", Value: "zzz", MatchType: "exact"},
		OnAnchorMissing: "skip",
	}, body)
	if len(body["items"].([]any)) != 1 {
		t.Fatal("onAnchorMissing=skip 时不应插入")
	}

	applyBodyOperation(filterOperation{
		Op:              "insert",
		Path:            "items",
		Value:           map[string]any{"id": "c"},
		Position:        "after",
		Anchor:          &filterMatcher{Field: "id", Value: "a", MatchType: "exact"},
		OnAnchorMissing: "skip",
	}, body)
	items := body["items"].([]any)
	if len(items) != 2 || items[1].(map[string]any)["id"] != "c" {
		t.Fatalf("锚点命中时应插在锚点之后: %v", items)
	}
}

// 过滤器步骤：guard 相、全局绑定生效，provider 绑定只在选路后生效。
func TestRequestFilterSteps(t *testing.T) {
	global := RequestFilter{
		ID: 1, Scope: "header", Action: "remove", Target: "x-internal", ExecutionPhase: "guard", BindingType: "global",
	}
	providerBound := RequestFilter{
		ID: 2, Scope: "body", Action: "json_path", Target: "touched",
		Replacement: json.RawMessage(`true`), BindingType: "providers", ProviderIDs: []int64{11},
	}
	finalPhase := RequestFilter{
		ID: 3, Scope: "header", Action: "remove", Target: "x-final", ExecutionPhase: "final", BindingType: "global",
	}

	body := map[string]any{"messages": []any{}}
	factory, access := bodyFactory(t, body)
	deps := Deps{
		Filters:  fakeFilters{filters: []RequestFilter{global, providerBound, finalPhase}},
		Body:     factory,
		Provider: fakeProvider{selection: pctxSelection(11)},
	}

	ctx := newContext(t, map[string]string{"x-internal": "1", "x-final": "1"}, body)
	if _, err := deps.requestFilterStep()(ctx); err != nil {
		t.Fatalf("全局过滤器不应报错: %v", err)
	}
	if ctx.Headers().Has("x-internal") {
		t.Fatal("全局 header 过滤器应删除头部")
	}
	if !ctx.Headers().Has("x-final") {
		t.Fatal("final 相规则不应在守卫相执行")
	}
	if access.stores != 0 {
		t.Fatal("没有 body 规则命中时不应写回正文")
	}

	// 供应商相：先选路再应用。
	if _, err := deps.providerStep()(ctx); err != nil {
		t.Fatalf("选路不应报错: %v", err)
	}
	if _, err := deps.providerRequestFilterStep()(ctx); err != nil {
		t.Fatalf("供应商过滤器不应报错: %v", err)
	}
	if value, ok := getValueByPath(access.current, "touched"); !ok || value != true {
		t.Fatalf("供应商绑定的规则应生效: %v (ok=%v)", value, ok)
	}
	if access.stores == 0 {
		t.Fatal("正文被改动后必须写回")
	}
}

// 未选路时供应商相过滤器跳过。
func TestProviderFilterWithoutSelection(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	factory, _ := bodyFactory(t, body)
	deps := Deps{
		Filters: fakeFilters{filters: []RequestFilter{{
			ID: 2, Scope: "body", Action: "json_path", Target: "touched",
			Replacement: json.RawMessage(`true`), BindingType: "providers", ProviderIDs: []int64{11},
		}}},
		Body: factory,
	}
	ctx := newContext(t, nil, body)
	response, err := deps.providerRequestFilterStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("不应抢答，收到 %d", response.Status)
	}
	if value, ok := getValueByPath(body, "touched"); ok {
		t.Fatalf("未选路时不应应用供应商规则: %v", value)
	}
}

// 端点策略要求跳过过滤器时，两个过滤器步骤都必须直通。
func TestFilterBypass(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	factory, access := bodyFactory(t, body)
	deps := Deps{
		Filters: fakeFilters{filters: []RequestFilter{{
			ID: 1, Scope: "body", Action: "json_path", Target: "touched",
			Replacement: json.RawMessage(`true`), BindingType: "global",
		}}},
		Body:                 factory,
		BypassRequestFilters: func(*pctx.Context) bool { return true },
	}
	ctx := newContext(t, nil, body)
	if _, err := deps.requestFilterStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if _, err := deps.providerRequestFilterStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if access.stores != 0 {
		t.Fatal("跳过时不应改动正文")
	}
}

// 规则源读失败是 fail-open：过滤失败不阻塞主流程。
func TestFilterSourceFailureIsFailOpen(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	factory, _ := bodyFactory(t, body)
	deps := Deps{Filters: failingFilters{}, Body: factory}
	ctx := newContext(t, nil, body)

	response, err := deps.requestFilterStep()(ctx)
	if err != nil {
		t.Fatalf("规则源失败不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("规则源失败不应抢答，收到 %d", response.Status)
	}
}

// 头部过滤器：set 的取值规则与 advanced remove。
func TestApplyHeaderFilter(t *testing.T) {
	deps := Deps{}
	ctx := newContext(t, map[string]string{"x-drop": "1", "x-keep": "1"}, nil)

	deps.applyHeaderFilter(RequestFilter{ID: 1, Scope: "header", Action: "remove", Target: "x-drop"}, ctx)
	if ctx.Headers().Has("x-drop") {
		t.Fatal("remove 应删除头部")
	}

	deps.applyHeaderFilter(RequestFilter{ID: 2, Scope: "header", Action: "set", Target: "x-add", Replacement: json.RawMessage(`"v"`)}, ctx)
	if got := ctx.Headers().Get("x-add"); got != "v" {
		t.Fatalf("set 应写入字符串值，收到 %q", got)
	}

	deps.applyHeaderFilter(RequestFilter{ID: 3, Scope: "header", Action: "set", Target: "x-json", Replacement: json.RawMessage(`{"a":1}`)}, ctx)
	if got := ctx.Headers().Get("x-json"); !strings.Contains(got, `"a":1`) {
		t.Fatalf("非字符串 replacement 应 JSON 序列化，收到 %q", got)
	}

	deps.applyHeaderFilter(RequestFilter{ID: 4, Scope: "header", Action: "set", Target: "x-nil", Replacement: json.RawMessage(`null`)}, ctx)
	if got := ctx.Headers().Get("x-nil"); got != "" {
		t.Fatalf("null replacement 应为空串，收到 %q", got)
	}
}

// 分组绑定需要分组标签；标签取不到时规则不生效。
func TestProviderBoundFilters(t *testing.T) {
	filters := []RequestFilter{
		{ID: 1, BindingType: "providers", ProviderIDs: []int64{11}},
		{ID: 2, BindingType: "groups", GroupTags: []string{"paid"}},
		{ID: 3, BindingType: "providers", ProviderIDs: []int64{99}},
	}
	selection := pctxSelection(11)

	selected := providerBoundFilters(filters, selection, nil)
	if len(selected) != 1 || selected[0].ID != 1 {
		t.Fatalf("无分组取值函数时应只选中 provider 绑定: %+v", selected)
	}

	selected = providerBoundFilters(filters, selection, func(pctx.ProviderSelection) string { return "paid" })
	if len(selected) != 2 {
		t.Fatalf("分组命中时应选中两条，收到 %d", len(selected))
	}
}

// failingFilters 是必然失败的规则源。
type failingFilters struct{}

func (failingFilters) RequestFilters(context.Context) ([]RequestFilter, error) {
	return nil, errFilterSource
}

var errFilterSource = errors.New("规则源不可达")
