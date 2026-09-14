package adminapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件钉住 parseSessionSequenceQuery 与 Node SessionSequenceQuerySchema 的等价性。
//
// 为何值得单独钉：静态分析把 normalizeSessionRequestSequence 报成零调用（属实），
// 但顺着它会引出一个**看起来像缺陷、其实不是**的疑问——「requestSequence=0 是否漏了归一」。
// 真相是 0 在解析层就被挡下（Node 的 `.positive()`），走不到 action 层的那个 util。
// 这条不变量此前没有任何用例覆盖，而它恰好是「参数语义会不会悄悄放宽」的闸门。

func parseSequenceQueryFrom(t *testing.T, target string) (sessionSequenceQuery, []InvalidParam) {
	t.Helper()
	parsed, issues := parseSessionSequenceQuery(httptest.NewRequest(http.MethodGet, target, nil))
	return parsed, issues
}

// TestParseSessionSequenceQueryRejectsNonPositive 钉住 `z.coerce.number().int().positive()`：
// 0 与负数都是 400（too_small），**不是**「与没传同判」。
func TestParseSessionSequenceQueryRejectsNonPositive(t *testing.T) {
	for _, raw := range []string{"0", "-1"} {
		t.Run("requestSequence="+raw, func(t *testing.T) {
			_, issues := parseSequenceQueryFrom(t, "/api/v1/sessions/detail?requestSequence="+raw)
			if len(issues) == 0 {
				t.Fatal("非正数应产生校验问题（Node 的 .positive() 亦拒绝）")
			}
			if issues[0].Code != "too_small" {
				t.Fatalf("问题码应为 too_small，得到 %q", issues[0].Code)
			}
		})
	}
}

// TestParseSessionSequenceQueryAbsentMeansUnspecified 钉住「没传」：不报错、且不认为指定了序号。
func TestParseSessionSequenceQueryAbsentMeansUnspecified(t *testing.T) {
	parsed, issues := parseSequenceQueryFrom(t, "/api/v1/sessions/detail")
	if len(issues) != 0 {
		t.Fatalf("缺省不应报错，得到 %v", issues)
	}
	if parsed.HasRequestSequence {
		t.Fatal("没传时不应认为指定了序号")
	}
	if parsed.RequestSequence != 0 {
		t.Fatalf("没传时序号应为零值，得到 %d", parsed.RequestSequence)
	}
}

// TestParseSessionSequenceQueryAcceptsPositive 钉住合法正数：置 present 且保留原值。
// 这条与上面两条合起来，正是定位器里 `hasRequestID || query.HasRequestSequence`（:529）
// 与 Node `normalizeRequestSequence(requestSequence) !== null` 的等价依据。
func TestParseSessionSequenceQueryAcceptsPositive(t *testing.T) {
	parsed, issues := parseSequenceQueryFrom(t, "/api/v1/sessions/detail?requestSequence=3")
	if len(issues) != 0 {
		t.Fatalf("合法正数不应报错，得到 %v", issues)
	}
	if !parsed.HasRequestSequence {
		t.Fatal("合法正数应认为指定了序号")
	}
	if parsed.RequestSequence != 3 {
		t.Fatalf("序号应为 3，得到 %d", parsed.RequestSequence)
	}
}
