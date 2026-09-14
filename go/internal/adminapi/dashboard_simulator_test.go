package adminapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住 `POST /dashboard/dispatch-simulator:simulate` 的**入参校验**与缺省值，
// 对齐 Node 的 DispatchSimulatorInputSchema（src/actions/dispatch-simulator.ts:40-44）。
//
// 成功路径与九步语义由 internal/route 的 simulate_test.go 覆盖（那里不依赖 DB）；
// 本文件只覆盖管理面自己那份责任：解析、缺省、报错。

func parseSimulatorBody(t *testing.T, payload string) (dispatchSimulatorBody, []InvalidParam) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/dashboard/dispatch-simulator:simulate", strings.NewReader(payload))
	return parseDispatchSimulatorBody(request)
}

func TestParseDispatchSimulatorBodyDefaults(t *testing.T) {
	body, problems := parseSimulatorBody(t, `{"clientFormat":"claude"}`)
	if len(problems) != 0 {
		t.Fatalf("不该报错：%+v", problems)
	}
	if body.ClientFormat == nil || *body.ClientFormat != convert.FormatClaude {
		t.Fatalf("clientFormat 解析错误：%v", body.ClientFormat)
	}
	// zod 的 .default("") / .default([])：省略即取缺省值。
	if body.ModelName != nil {
		t.Errorf("modelName 省略时应为 nil（引擎按空串处理）")
	}
	if body.GroupTags != nil {
		t.Errorf("groupTags 省略时应为 nil（引擎退化到 default 分组）")
	}
}

func TestParseDispatchSimulatorBodyAcceptsAllFormats(t *testing.T) {
	for _, format := range []string{"claude", "openai", "response", "gemini", "gemini-cli"} {
		body, problems := parseSimulatorBody(t, `{"clientFormat":"`+format+`"}`)
		if len(problems) != 0 {
			t.Errorf("格式 %s 应被接受，却报错 %+v", format, problems)
			continue
		}
		if body.ClientFormat == nil || string(*body.ClientFormat) != format {
			t.Errorf("格式 %s 解析错误", format)
		}
	}
}

func TestParseDispatchSimulatorBodyRejects(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		code    string
	}{
		{"未知格式", `{"clientFormat":"anthropic"}`, "invalid_enum_value"},
		{"格式类型错", `{"clientFormat":123}`, "invalid_type"},
		{"缺格式", `{}`, "invalid_type"},
		{"空体", ``, "invalid_type"},
		{"模型名超长", `{"clientFormat":"claude","modelName":"` + strings.Repeat("m", 256) + `"}`, "too_big"},
		{"分组标签为空", `{"clientFormat":"claude","groupTags":[""]}`, "too_small"},
		{"分组标签超长", `{"clientFormat":"claude","groupTags":["` + strings.Repeat("g", 256) + `"]}`, "too_big"},
		{"分组标签过多", `{"clientFormat":"claude","groupTags":["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u"]}`, "too_big"},
		{"分组标签类型错", `{"clientFormat":"claude","groupTags":"x"}`, "invalid_type"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, problems := parseSimulatorBody(t, testCase.payload)
			if len(problems) == 0 {
				t.Fatalf("应报校验错误（期望 %s）", testCase.code)
			}
			found := false
			for _, problem := range problems {
				if problem.Code == testCase.code {
					found = true
				}
			}
			if !found {
				t.Errorf("期望错误码 %s，实际 %+v", testCase.code, problems)
			}
		})
	}
}

func TestParseDispatchSimulatorBodyTrimsValues(t *testing.T) {
	body, problems := parseSimulatorBody(t,
		`{"clientFormat":"claude","modelName":"  claude-sonnet-4-5  ","groupTags":["  g1  ","g2"]}`)
	if len(problems) != 0 {
		t.Fatalf("不该报错：%+v", problems)
	}
	if body.ModelName == nil || *body.ModelName != "claude-sonnet-4-5" {
		t.Errorf("modelName 应被 trim，实际 %v", body.ModelName)
	}
	if body.GroupTags == nil || len(*body.GroupTags) != 2 {
		t.Fatalf("groupTags 解析错误：%v", body.GroupTags)
	}
	if (*body.GroupTags)[0] != "g1" {
		t.Errorf("分组标签应被 trim，实际 %q", (*body.GroupTags)[0])
	}
}
