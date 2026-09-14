package adminapi

import (
	"encoding/json"
	"testing"
)

// 本文件钉住 providers 写路径对 `custom_headers` 的校验，逐条对齐 Node 的
// normalizeCustomHeadersRecord（src/lib/custom-headers.ts:33-91）。
//
// 为什么值得单测：这一层的失败**不会**在数据面曝光——数据面只做施加时剥离，
// 于是「写入侧放过脏值」会静默变成库里一条永远生效不了的配置（界面上还显示为已配置）。
// 八种错误码各有独立触发条件，正则/大小写/CRLF 三处最容易写错。

func decodeCustomHeadersField(t *testing.T, raw string) (any, []invalidParam) {
	t.Helper()
	fields := map[string]json.RawMessage{"custom_headers": json.RawMessage(raw)}
	object := adminNewObject(fields, "custom_headers")
	spec := providerCustomHeadersSpec()
	value, present := spec.Decode(object, "custom_headers")
	if issues := object.issues0(); len(issues) > 0 {
		return nil, issues
	}
	if !present {
		return nil, nil
	}
	return value, nil
}

func TestProviderCustomHeadersValidRecordKeepsKeysInOrder(t *testing.T) {
	value, issues := decodeCustomHeadersField(t,
		`{"cf-aig-authorization":"Bearer t","x-opencode-session":"cch-1","X-Trace":"a\u003cb"}`)
	if len(issues) != 0 {
		t.Fatalf("合法对象不得报错：%v", issues)
	}
	// 键序必须保持输入顺序（便于与 Node 落库值逐字节对拍）；且不做 HTML 转义（Node 亦不转）。
	if got := string(value.(json.RawMessage)); got != `{"cf-aig-authorization":"Bearer t","x-opencode-session":"cch-1","X-Trace":"a<b"}` {
		t.Fatalf("归一化结果不符：%s", got)
	}
}

func TestProviderCustomHeadersEmptyObjectNormalizesToClear(t *testing.T) {
	// Node：`names.length === 0 → { ok: true, value: null }`，即「清空」而不是「保留原值」。
	value, issues := decodeCustomHeadersField(t, `{}`)
	if len(issues) != 0 {
		t.Fatalf("空对象应通过：%v", issues)
	}
	// 归一化结果是**带类型的 nil**（json.RawMessage(nil)）：存储层据其写 NULL。
	raw, ok := value.(json.RawMessage)
	if !ok {
		t.Fatalf("归一化结果类型应为 json.RawMessage，实际 %#v", value)
	}
	if len(raw) != 0 {
		t.Fatalf("空对象应归一化为清空（nil RawMessage），实际 %q", raw)
	}
}

func TestProviderCustomHeadersExplicitNullClears(t *testing.T) {
	value, issues := decodeCustomHeadersField(t, `null`)
	if len(issues) != 0 || value != nil {
		t.Fatalf("显式 null 应归一化为 nil：value=%#v issues=%v", value, issues)
	}
}

func TestProviderCustomHeadersMissingFieldStaysAbsent(t *testing.T) {
	// 字段缺失 ≠ 清空：Node 是 `.optional()`，缺失即「不下发」。
	object := adminNewObject(map[string]json.RawMessage{}, "custom_headers")
	if _, present := providerCustomHeadersSpec().Decode(object, "custom_headers"); present {
		t.Fatal("字段缺失时不得下发（否则会把既有配置清空）")
	}
	if issues := object.issues0(); len(issues) != 0 {
		t.Fatalf("字段缺失不是错误：%v", issues)
	}
}

func TestProviderCustomHeadersRejections(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantCode string
		wantName string
	}{
		{"数组不是对象", `["x","y"]`, "not_object", ""},
		{"标量不是对象", `"x"`, "not_object", ""},
		{"数字不是对象", `7`, "not_object", ""},
		{"空键", `{"":"v"}`, "empty_name", ""},
		{"纯空白键", `{"   ":"v"}`, "empty_name", "   "},
		{"键含换行", `{"x-a\nb":"v"}`, "crlf", "x-a\nb"},
		{"键非 HTTP token", `{"x a":"v"}`, "invalid_name", "x a"},
		{"键含冒号", `{"x:y":"v"}`, "invalid_name", "x:y"},
		{"鉴权头（小写）", `{"authorization":"Bearer evil"}`, "protected_name", "authorization"},
		{"鉴权头（大小写混合）", `{"X-Api-Key":"evil"}`, "protected_name", "X-Api-Key"},
		{"google 鉴权头", `{"x-goog-api-key":"evil"}`, "protected_name", "x-goog-api-key"},
		{"大小写重复键", `{"X-Dup":"a","x-dup":"b"}`, "duplicate_name", "x-dup"},
		{"值非字符串（数字）", `{"x-n":1}`, "invalid_value", "x-n"},
		{"值非字符串（数组）", `{"x-a":["a"]}`, "invalid_value", "x-a"},
		{"值非字符串（null）", `{"x-z":null}`, "invalid_value", "x-z"},
		{"值含换行", `{"x-crlf":"a\r\nb"}`, "crlf", "x-crlf"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, issues := decodeCustomHeadersField(t, testCase.raw)
			if len(issues) != 1 {
				t.Fatalf("期望恰好 1 条 issue，实际 %v", issues)
			}
			issue := issues[0]
			if issue.Code != testCase.wantCode {
				t.Fatalf("code = %q，期望 %q", issue.Code, testCase.wantCode)
			}
			if len(issue.Path) != 1 || issue.Path[0] != "custom_headers" {
				t.Fatalf("path 应指向字段本身：%v", issue.Path)
			}
			// message 前缀与 Node 一致（界面按 `custom_headers_<code>` 映射五语种文案）。
			prefix := "custom_headers_" + testCase.wantCode
			if len(issue.Message) < len(prefix) || issue.Message[:len(prefix)] != prefix {
				t.Fatalf("message 前缀不符：%q", issue.Message)
			}
			if testCase.wantName != "" && issue.Message != prefix+` (`+testCase.wantName+`)` {
				t.Fatalf("message 未带命中键名：%q", issue.Message)
			}
		})
	}
}

func TestProviderCustomHeadersDoesNotRejectApplyTimeReservedNames(t *testing.T) {
	// 与 Node 的分工：validator 只管形状，`host`/`content-length`/内部标记头由**施加点**剥离。
	// 若这里改成拒绝，就与 Node 分叉（Node 接受入库、出站静默剥离）。
	value, issues := decodeCustomHeadersField(t,
		`{"host":"evil.example","content-length":"9","x-cch-internal-secret":"s","x-ok":"v"}`)
	if len(issues) != 0 {
		t.Fatalf("保留名不属写入侧校验范围：%v", issues)
	}
	if got := string(value.(json.RawMessage)); got != `{"host":"evil.example","content-length":"9","x-cch-internal-secret":"s","x-ok":"v"}` {
		t.Fatalf("原样下发即可（剥离在施加点）：%s", got)
	}
}

// 钉住「写路径真的用了这个 spec」：把 spec 换回 providerJSONFieldSpec 时本用例转红。
func TestProviderWriteSpecsBindCustomHeadersSpec(t *testing.T) {
	specs := providerUpdateWriteSpecs()
	spec, ok := specs["custom_headers"]
	if !ok {
		t.Fatal("更新字段表里没有 custom_headers")
	}
	fields := map[string]json.RawMessage{"custom_headers": json.RawMessage(`{"authorization":"evil"}`)}
	object := adminNewObject(fields, "custom_headers")
	if _, present := spec.Decode(object, "custom_headers"); present {
		t.Fatal("更新路径必须走带校验的 spec（此值应被拒）")
	}
	if issues := object.issues0(); len(issues) != 1 || issues[0].Code != "protected_name" {
		t.Fatalf("更新路径未走校验 spec：%v", issues)
	}
}
