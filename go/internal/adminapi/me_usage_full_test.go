package adminapi

import (
	"encoding/json"
	"testing"
)

// 本测试钉住 me 完整用量行的只读脱敏口径（Node：scrubUsageLogsBatchForReadonly
// 与 scrubProviderChainRequestForReadonly / scrubSpecialSettingsForReadonly）。

func TestScrubMeUsageFullRowBlanksIdentityAndErrorFields(t *testing.T) {
	row := jsonObject{}.
		set("id", int64(7)).
		set("userName", "张三").
		set("keyName", "主密钥").
		set("providerName", "HC_Chat").
		set("errorMessage", "上游 500: 密钥 sk-xxx 无效").
		set("blockedReason", "命中敏感词表").
		set("userAgent", "claude-cli/1.0").
		set("messagesCount", int64(12)).
		set("costMultiplier", "1.5").
		set("groupCostMultiplier", "2").
		set("costBreakdown", json.RawMessage(`{"input":1}`)).
		set("model", "deepseek-v4-flash")

	scrubbed := scrubMeUsageFullRow(row)
	got := map[string]any{}
	for _, field := range scrubbed {
		got[field.Key] = field.Value
	}

	if got["userName"] != "" || got["keyName"] != "" {
		t.Fatalf("身份名应被清空，得到 userName=%v keyName=%v", got["userName"], got["keyName"])
	}
	for _, key := range []string{
		"providerName", "errorMessage", "blockedReason", "userAgent", "messagesCount",
		"costMultiplier", "groupCostMultiplier", "costBreakdown",
	} {
		if got[key] != nil {
			t.Fatalf("%s 应被置 null，得到 %v", key, got[key])
		}
	}
	// 非脱敏字段必须原样保留：否则等于把本人的用量数据也吞掉了。
	if got["model"] != "deepseek-v4-flash" || got["id"] != int64(7) {
		t.Fatalf("非脱敏字段被改动: model=%v id=%v", got["model"], got["id"])
	}
}

func TestScrubMeUsageProviderChainDropsRequestAndStrongScrubFields(t *testing.T) {
	raw := json.RawMessage(`[
	  {"id":1,"reason":"success"},
	  {"id":2,"rawCrossProviderFallbackEnabled":true,"errorDetails":{
	      "request":{"body":"secret prompt"},
	      "clientError":"上游返回 401",
	      "provider":{"upstreamBody":"RAW","upstreamParsed":{"a":1},"name":"p1"}}},
	  {"id":3,"errorDetails":{
	      "request":{"body":"secret2"},
	      "clientError":"普通错误",
	      "provider":{"upstreamBody":"RAW2","name":"p2"}}}
	]`)

	scrubbed, ok := scrubMeUsageProviderChain(raw).(json.RawMessage)
	if !ok {
		t.Fatalf("期望返回 json.RawMessage，得到 %T", scrubMeUsageProviderChain(raw))
	}
	var chain []map[string]any
	if err := json.Unmarshal(scrubbed, &chain); err != nil {
		t.Fatalf("脱敏结果非法 JSON: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("条目数应保持 3，得到 %d", len(chain))
	}
	if _, has := chain[0]["errorDetails"]; has {
		t.Fatalf("无 errorDetails 的条目应原样保留")
	}

	strong := chain[1]["errorDetails"].(map[string]any)
	if _, has := strong["request"]; has {
		t.Fatalf("request（含请求体）必须被剥掉")
	}
	if _, has := strong["clientError"]; has {
		t.Fatalf("强脱敏时 clientError 必须被剥掉")
	}
	strongProvider := strong["provider"].(map[string]any)
	if _, has := strongProvider["upstreamBody"]; has {
		t.Fatalf("强脱敏时 provider.upstreamBody 必须被剥掉")
	}
	if _, has := strongProvider["upstreamParsed"]; has {
		t.Fatalf("强脱敏时 provider.upstreamParsed 必须被剥掉")
	}

	weak := chain[2]["errorDetails"].(map[string]any)
	if _, has := weak["request"]; has {
		t.Fatalf("request 无论强弱都要剥掉")
	}
	if _, has := weak["clientError"]; !has {
		t.Fatalf("非强脱敏时 clientError 应保留（与 Node 一致）")
	}
	weakProvider := weak["provider"].(map[string]any)
	if _, has := weakProvider["upstreamBody"]; !has {
		t.Fatalf("非强脱敏时 provider.upstreamBody 应保留")
	}
}

func TestScrubMeUsageSpecialSettingsOnlyNullsGuardInterceptReason(t *testing.T) {
	settings := []jsonObject{
		jsonObject{}.set("type", "guard_intercept").set("reason", "命中词表 X"),
		jsonObject{}.set("type", "other").set("reason", "保留"),
	}
	scrubbed, ok := scrubMeUsageSpecialSettings(settings).([]jsonObject)
	if !ok {
		t.Fatalf("期望 []jsonObject，得到 %T", scrubMeUsageSpecialSettings(settings))
	}
	for _, field := range scrubbed[0] {
		if field.Key == "reason" && field.Value != nil {
			t.Fatalf("guard_intercept 的 reason 应置 null，得到 %v", field.Value)
		}
	}
	for _, field := range scrubbed[1] {
		if field.Key == "reason" && field.Value != "保留" {
			t.Fatalf("非 guard_intercept 的 reason 应保留，得到 %v", field.Value)
		}
	}
}
