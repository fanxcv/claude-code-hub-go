package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住写路径里**不依赖真库**的三类判定：厂商域名归一、字段归一、撤销前像。
//
// 为什么不放进集成测试：这三类都是纯函数，而它们的错误后果是「写进库的值与 Node 不同」——
// 那在集成测试里只会表现为「字段值不对」，看不出是哪一步归一错了。这里逐条钉住取值。

func TestProviderVendorDomainKeyMatchesNode(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{
			name: "有 website_url 时取 hostname（忽略路径与 www）",
			payload: map[string]any{
				"website_url": providerDefaultText("https://www.anthropic.com/pricing"),
				"url":         "https://api.anthropic.com/v1",
			},
			want: "anthropic.com",
		},
		{
			name: "website_url 带显式端口时带端口",
			payload: map[string]any{
				"website_url": providerDefaultText("https://relay.example.com:8443"),
				"url":         "https://relay.example.com:8443/v1",
			},
			want: "relay.example.com:8443",
		},
		{
			name: "无 website_url 时取 provider URL 的 host:port（https 默认 443）",
			payload: map[string]any{
				"url": "https://api.example.com/v1",
			},
			want: "api.example.com:443",
		},
		{
			name: "无 website_url 且 http 时取 80",
			payload: map[string]any{
				"url": "http://api.example.com/v1",
			},
			want: "api.example.com:80",
		},
		{
			name: "无 website_url 且无 scheme 时按 https 解析",
			payload: map[string]any{
				"url": "api.example.com",
			},
			want: "api.example.com:443",
		},
		{
			name: "website_url 为空串时回退到 provider URL",
			payload: map[string]any{
				"website_url": providerDefaultText("   "),
				"url":         "https://api.example.com/v1",
			},
			want: "api.example.com:443",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			got, err := providerVendorDomainKey(item.payload)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != item.want {
				t.Fatalf("厂商域名 = %q，期望 %q", got, item.want)
			}
		})
	}
}

func TestProviderNormalizeGroupTag(t *testing.T) {
	cases := []struct {
		in   string
		want string // 空串表示期望 null
	}{
		{"", ""},
		{"   ", ""},
		{"g1", "g1"},
		{"  g1 , g2 ，g1 ", "g1,g2"},
		{"a\nb\rc", "a,b,c"},
		{"，,", ""},
	}
	for _, item := range cases {
		t.Run(item.in, func(t *testing.T) {
			got, _ := providerNormalizeGroupTag(providerDefaultText(item.in)).(*string)
			if item.want == "" {
				if got != nil {
					t.Fatalf("期望 null，实得 %q", *got)
				}
				return
			}
			if got == nil || *got != item.want {
				t.Fatalf("归一 = %v，期望 %q", got, item.want)
			}
		})
	}
}

func TestProviderNormalizeModelRedirects(t *testing.T) {
	// 对象形式（旧 UI）：转成 matchType=exact 的规则列表。
	object := providerNormalizeModelRedirects(json.RawMessage(`{"claude-3":"claude-3.5"," x ":" y "}`))
	// 对象形式按**键的确定序**输出（Node 用 JS 对象的插入序；两者存的都是 jsonb 数组，
	// 且对象形式生成的规则全是 exact，匹配语义与顺序无关）。这条差异登记在白名单里。
	if string(object.(json.RawMessage)) != `[{"matchType":"exact","source":"x","target":"y"},{"matchType":"exact","source":"claude-3","target":"claude-3.5"}]` {
		t.Fatalf("对象形式归一不符: %s", object)
	}
	// 规则列表：逐条 trim。
	list := providerNormalizeModelRedirects(json.RawMessage(
		`[{"matchType":"prefix","source":" claude-","target":" upstream "}]`))
	if string(list.(json.RawMessage)) != `[{"matchType":"prefix","source":"claude-","target":"upstream"}]` {
		t.Fatalf("列表形式归一不符: %s", list)
	}
	// 列表里有一项非法 → 整体 null（Node 的 every 语义）。
	if got := providerNormalizeModelRedirects(json.RawMessage(
		`[{"matchType":"prefix","source":"a","target":"b"},{"matchType":"nope","source":"a","target":"b"}]`)); got != nil {
		t.Fatalf("含非法项应整体 null，实得 %v", got)
	}
	// null / 非法标量 → null。
	for _, raw := range []string{"null", `"str"`, `123`} {
		if got := providerNormalizeModelRedirects(json.RawMessage(raw)); got != nil {
			t.Fatalf("%s 应归一为 null，实得 %v", raw, got)
		}
	}
}

func TestProviderNormalizeAllowedModels(t *testing.T) {
	// 字符串项 → exact 规则。
	got := providerNormalizeAllowedModels(json.RawMessage(`["gpt-4",{"matchType":"prefix","pattern":" claude-"}]`))
	if string(got.(json.RawMessage)) != `[{"matchType":"exact","pattern":"gpt-4"},{"matchType":"prefix","pattern":"claude-"}]` {
		t.Fatalf("allowed_models 归一不符: %s", got)
	}
	// 空串与非法 matchType 被过滤（不是整体 null：Node 是 map+filter）。
	got = providerNormalizeAllowedModels(json.RawMessage(`["",{"matchType":"nope","pattern":"x"},{"matchType":"exact","pattern":"ok"}]`))
	if string(got.(json.RawMessage)) != `[{"matchType":"exact","pattern":"ok"}]` {
		t.Fatalf("过滤结果不符: %s", got)
	}
	// 非数组 → null。
	if got := providerNormalizeAllowedModels(json.RawMessage(`{"a":"b"}`)); got != nil {
		t.Fatalf("非数组应 null，实得 %v", got)
	}
}

func TestProviderBuildPreimageRecordsOnlyChangedFields(t *testing.T) {
	existing := store.AdminProvider{
		ID:   42,
		Name: "老名字",
		URL:  "https://a.example.com",
		// 库里是 numeric，读出来是 *float64。
		CostMultiplier: floatPtrForTest(1.5),
		Priority:       3,
		AllowedClients: []string{"cli-a"},
	}
	payload := map[string]any{
		"name":            "新名字",    // 变了 → 记
		"priority":        int64(3), // 没变 → 不记
		"cost_multiplier": floatPtrForTest(1.5),
		"key":             "sk-new",                  // 密钥永不进前像
		"group_tag":       providerDefaultText("g1"), // 库里为 nil → 变了
	}
	preimage := providerBuildPreimage(existing, payload)

	if _, ok := preimage["key"]; ok {
		t.Fatal("前像不得含密钥")
	}
	if preimage["name"] != "老名字" {
		t.Fatalf("name 前像 = %v，期望旧值", preimage["name"])
	}
	if _, ok := preimage["priority"]; ok {
		t.Fatal("未变化的字段不应进前像")
	}
	if _, ok := preimage["costMultiplier"]; ok {
		t.Fatal("未变化的 costMultiplier 不应进前像")
	}
	if value, ok := preimage["groupTag"]; !ok || value != nil {
		t.Fatalf("groupTag 前像应为 null（旧值为空），实得 %v", value)
	}
}

func TestProviderPreimageRoundTrip(t *testing.T) {
	// 前像（Node 形状的 JSON）→ 写字段袋，类型必须与存储层的绑定类型一致，否则撤销写不进去。
	preimage := map[string]any{
		"name":           "旧名",
		"costMultiplier": 1.5,
		"priority":       float64(3),
		"isEnabled":      true,
		"limit5hUsd":     nil,
		"groupTag":       nil,
		"allowedClients": []any{"cli-a"},
		"tpm":            float64(10),
	}
	payload, issues := providerPreimageToWriteFields(preimage)
	if len(issues) > 0 {
		t.Fatalf("前像回写失败: %+v", issues)
	}
	if payload["name"] != "旧名" {
		t.Fatalf("name = %v", payload["name"])
	}
	if value, ok := payload["cost_multiplier"].(*float64); !ok || *value != 1.5 {
		t.Fatalf("cost_multiplier 类型不符: %#v", payload["cost_multiplier"])
	}
	if value, ok := payload["priority"].(int64); !ok || value != 3 {
		t.Fatalf("priority = %#v", payload["priority"])
	}
	if value, ok := payload["is_enabled"].(bool); !ok || !value {
		t.Fatalf("is_enabled = %#v", payload["is_enabled"])
	}
	if value, ok := payload["limit_5h_usd"].(*float64); !ok || value != nil {
		t.Fatalf("limit_5h_usd 应为 nil *float64: %#v", payload["limit_5h_usd"])
	}
	if value, ok := payload["group_tag"].(*string); !ok || value != nil {
		t.Fatalf("group_tag 应为 nil *string: %#v", payload["group_tag"])
	}
	if _, ok := payload["allowed_clients"].(json.RawMessage); !ok {
		t.Fatalf("allowed_clients 应为 JSON: %#v", payload["allowed_clients"])
	}
	if value, ok := payload["tpm"].(*int64); !ok || *value != 10 {
		t.Fatalf("tpm = %#v", payload["tpm"])
	}

	// 类型不符时不得静默跳过（例如用字符串冒充数字）。
	if _, issues := providerPreimageToWriteFields(map[string]any{"priority": "3"}); len(issues) == 0 {
		t.Fatal("类型不符应给出校验失败")
	}
	// Go 侧未知的 camelCase 名跳过而不是写错列。
	if payload, issues := providerPreimageToWriteFields(map[string]any{"unknownFutureField": 1}); len(issues) != 0 || len(payload) != 0 {
		t.Fatalf("未知字段应被跳过，实得 %v %+v", payload, issues)
	}
}

func TestProviderCreateFieldSpecsCoverNodeSchema(t *testing.T) {
	// 创建字段表与路由注册用的白名单必须一致：不一致时会出现「白名单放行但解码拒绝」
	// （表现为 400 unrecognized_keys 却看不出原因）。
	specs := providerCreateWriteSpecs()
	for _, name := range providerCreateFieldNames {
		if _, ok := specs[name]; !ok {
			t.Fatalf("创建白名单里的 %q 没有对应的解码规格", name)
		}
	}
	for name := range specs {
		found := false
		for _, allowed := range providerCreateFieldNames {
			if allowed == name {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("解码规格里的 %q 不在创建白名单里", name)
		}
	}
}

func TestProviderRedactedPlaceholderDetection(t *testing.T) {
	// 命中：key 里带脱敏标记。
	fields := map[string]json.RawMessage{"key": json.RawMessage(`"sk-[REDACTED]"`)}
	if name, ok := providerFindRedactedWriteField(fields); !ok || name != "key" {
		t.Fatalf("应命中 key，实得 %q %v", name, ok)
	}
	// 不命中：普通值。
	if _, ok := providerFindRedactedWriteField(map[string]json.RawMessage{
		"key": json.RawMessage(`"sk-real"`),
	}); ok {
		t.Fatal("普通值不应命中")
	}
	// 命中：custom_headers 嵌套值里带标记。
	if _, ok := providerFindRedactedWriteField(map[string]json.RawMessage{
		"custom_headers": json.RawMessage(`{"x-api-key":"REDACTED:REDACTED@"}`),
	}); !ok {
		t.Fatal("custom_headers 里的脱敏标记应命中")
	}
}

func TestProviderReadOnlyHelperDoesNotLeakSecrets(t *testing.T) {
	// 响应体不得回显 key：providerSummaryPayload 的既有断言在 providers_test.go，
	// 这里只钉住写路径新用到的 URL 脱敏不改变无凭据的 URL。
	if got := providerRedactURLCredentials("https://a.example.com/v1"); got != "https://a.example.com/v1" {
		t.Fatalf("无凭据 URL 不应被改动: %q", got)
	}
}

func floatPtrForTest(value float64) *float64 { return &value }

var _ = httptest.NewRecorder
var _ = strings.TrimSpace
var _ = http.MethodPost

// TestProviderGroupPrioritiesWriteValidation 钉住分组优先级覆盖的写入口径。
//
// 为什么非钉不可：列是 jsonb 且写入侧原本**原样透传**，于是字符串数字、数组、标量都能落库；
// 而选路侧按整数读（map[string]int），落进去就读不出来——用户只会看到「配了却不生效」。
// 口径对齐 Node 的 zod（src/lib/api/v1/schemas/providers.ts:36：
// groupPriorities = z.record(z.string(), z.number()).nullable()），并有两处**有意更严**：
// 值必须是整数、值不得为 null（JSON null 在既有读取语义里等价于 0 = 最高优先级）。
func TestProviderGroupPrioritiesWriteValidation(t *testing.T) {
	names := []string{"group_priorities"}
	cases := []struct {
		name string
		body string
		// wantStored 是期望落库的 JSON 原文；为空且 wantCleared 为假表示该列不写。
		wantStored  string
		wantCleared bool
		wantIssue   bool
	}{
		{name: "整数记录放行", body: `{"group_priorities":{"fan":0,"vip":2}}`, wantStored: `{"fan":0,"vip":2}`},
		{name: "空对象放行", body: `{"group_priorities":{}}`, wantStored: `{}`},
		{name: "负数放行", body: `{"group_priorities":{"fan":-1}}`, wantStored: `{"fan":-1}`},
		{name: "顶层 null 表示清空", body: `{"group_priorities":null}`, wantCleared: true},
		{name: "缺省不写该列", body: `{}`},
		{name: "顶层数组拒绝", body: `{"group_priorities":[1]}`, wantIssue: true},
		{name: "顶层字符串拒绝", body: `{"group_priorities":"0"}`, wantIssue: true},
		{name: "顶层数字拒绝", body: `{"group_priorities":0}`, wantIssue: true},
		{name: "值字符串拒绝", body: `{"group_priorities":{"fan":"0"}}`, wantIssue: true},
		{name: "值浮点拒绝", body: `{"group_priorities":{"fan":0.5}}`, wantIssue: true},
		{name: "值 2.0 拒绝：Go 的 int 解码只接整数字面量，落库会读不回", body: `{"group_priorities":{"fan":2.0}}`, wantIssue: true},
		{name: "值 null 拒绝：null 在读取语义里等于 0", body: `{"group_priorities":{"fan":null}}`, wantIssue: true},
		{name: "值布尔拒绝", body: `{"group_priorities":{"fan":true}}`, wantIssue: true},
		{name: "值数组拒绝", body: `{"group_priorities":{"fan":[1]}}`, wantIssue: true},
	}
	// 创建与更新两张表都必须走校验型 spec：更新表目前从创建表派生，这条钉子拦住
	// 「将来给 update 单独换成原样透传」的回归。
	tables := []struct {
		name  string
		specs map[string]providerDecodeSpec
	}{
		{name: "create", specs: providerCreateWriteSpecs()},
		{name: "update", specs: providerUpdateWriteSpecs()},
	}
	for _, table := range tables {
		for _, testCase := range cases {
			t.Run(table.name+"/"+testCase.name, func(t *testing.T) {
				fields := map[string]json.RawMessage{}
				if err := json.Unmarshal([]byte(testCase.body), &fields); err != nil {
					t.Fatalf("构造请求体失败: %v", err)
				}
				object := adminNewObject(fields)
				payload, issues := providerDecodeWriteFields(object, names, table.specs)
				if testCase.wantIssue {
					if len(issues) == 0 {
						t.Fatalf("应拒绝并给出 400 形状的 issue，实际 payload=%v", payload)
					}
					return
				}
				if len(issues) != 0 {
					t.Fatalf("不该有 issue：%+v", issues)
				}
				stored, present := payload["group_priorities"]
				if !present {
					if testCase.wantStored != "" || testCase.wantCleared {
						t.Fatalf("该列应写入，实际缺省：%v", payload)
					}
					return
				}
				if testCase.wantCleared {
					// 「清空」的判据是**最终写 NULL**，而不是接口里是哪种 nil：store 的 providerJSONKind
					// 对 nil 接口与空 RawMessage 都写 NULL（admin_providers_write.go:235-245），
					// 两者与旧 spec（providerJSONFieldSpec）等价。
					raw, ok := stored.(json.RawMessage)
					if stored != nil && (!ok || len(raw) != 0) {
						t.Fatalf("顶层 null 应落成 NULL，实际 %#v", stored)
					}
					return
				}
				raw, ok := stored.(json.RawMessage)
				if !ok {
					t.Fatalf("该列应为 JSON 原文，实际 %#v", stored)
				}
				if string(raw) != testCase.wantStored {
					t.Fatalf("落库原文 = %s，期望 %s", raw, testCase.wantStored)
				}
			})
		}
	}
}
