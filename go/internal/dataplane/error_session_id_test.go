package dataplane

import (
	"encoding/json"
	"strings"
	"testing"
)

// 期望值逐条取自 Node 的 attachSessionIdToErrorMessage / attachSessionIdToErrorResponse 语义。

func TestAttachSessionIDToErrorMessage(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		message   string
		want      string
	}{
		{"无会话 id 原样", "", "boom", "boom"},
		{"追加后缀", "sess-1", "boom", "boom (cch_session_id: sess-1)"},
		{"已带标记不重复", "sess-1", "boom (cch_session_id: sess-9)", "boom (cch_session_id: sess-9)"},
		{"空文案也追加", "sess-1", "", " (cch_session_id: sess-1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := attachSessionIDToErrorMessage(tc.sessionID, tc.message); got != tc.want {
				t.Fatalf("got %q，期望 %q", got, tc.want)
			}
		})
	}
}

func TestAttachSessionIDToErrorBody(t *testing.T) {
	twoKeys := []byte(`{"error":{"type":"rate_limit_error","message":"too many"}}`)
	spliced := attachSessionIDToErrorBody("sess-7", 429, "application/json; charset=utf-8", twoKeys)

	var decoded map[string]any
	if err := json.Unmarshal(spliced, &decoded); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, spliced)
	}
	message := decoded["error"].(map[string]any)["message"]
	if message != "too many (cch_session_id: sess-7)" {
		t.Fatalf("message = %v", message)
	}
	// 键序必须保持：Node 走 parse+stringify（V8 保插入序），本实现按字节切片，两者同结果。
	if !strings.HasPrefix(string(spliced), `{"error":{"type":"rate_limit_error","message":`) {
		t.Fatalf("键序被打乱: %s", spliced)
	}

	t.Run("非 JSON 媒体类型不动", func(t *testing.T) {
		body := []byte(`{"error":{"message":"x"}}`)
		if got := attachSessionIDToErrorBody("s", 500, "text/plain", body); string(got) != string(body) {
			t.Fatalf("被改动了: %s", got)
		}
	})
	t.Run("2xx 不动", func(t *testing.T) {
		body := []byte(`{"error":{"message":"x"}}`)
		if got := attachSessionIDToErrorBody("s", 200, "application/json", body); string(got) != string(body) {
			t.Fatalf("被改动了: %s", got)
		}
	})
	t.Run("无会话 id 不动", func(t *testing.T) {
		body := []byte(`{"error":{"message":"x"}}`)
		if got := attachSessionIDToErrorBody("", 500, "application/json", body); string(got) != string(body) {
			t.Fatalf("被改动了: %s", got)
		}
	})
	t.Run("形状不符不动", func(t *testing.T) {
		unchanged := [][]byte{
			[]byte(`{"error":"plain"}`),           // error 不是对象
			[]byte(`{"error":{"message":42}}`),    // message 不是字符串
			[]byte(`{"other":{"message":"x"}}`),   // 没有 error 键
			[]byte(`[{"error":{"message":"x"}}]`), // 顶层是数组
			[]byte(`{"error":{"message":"x"}`),    // 截断
			[]byte(`{"error":{"message":"x"`),     // 更早截断
			[]byte(`not json at all`),             // 完全不是 JSON
			[]byte(`{"error":null}`),              // error 为 null
		}
		for _, body := range unchanged {
			got := attachSessionIDToErrorBody("s", 500, "application/json", body)
			if string(got) != string(body) {
				t.Fatalf("本应原样却改了: %s -> %s", body, got)
			}
		}
		// 合法且另有顶层键的正文应被改，且其余键不动。
		body := []byte(`{"error":{"message":"x"},"n":2}`)
		got := attachSessionIDToErrorBody("s", 500, "application/json", body)
		if !strings.Contains(string(got), `"n":2`) {
			t.Fatalf("其余键被丢弃: %s", got)
		}
		if !strings.Contains(string(got), `"message":"x (cch_session_id: s)"`) {
			t.Fatalf("message 未挂: %s", got)
		}
	})
	t.Run("嵌套 details 里的同名字段不被误改", func(t *testing.T) {
		body := []byte(`{"error":{"message":"x","details":{"message":"inner"}}}`)
		got := attachSessionIDToErrorBody("s", 500, "application/json", body)
		if strings.Count(string(got), "cch_session_id") != 1 {
			t.Fatalf("改了不止一处: %s", got)
		}
		if !strings.Contains(string(got), `"inner"`) {
			t.Fatalf("details 被改动: %s", got)
		}
	})
	t.Run("已带标记时不重复且返回原切片", func(t *testing.T) {
		body := []byte(`{"error":{"message":"x (cch_session_id: old)"}}`)
		got := attachSessionIDToErrorBody("new", 500, "application/json", body)
		if string(got) != string(body) {
			t.Fatalf("被改动了: %s", got)
		}
	})
	t.Run("转义字符的 message 逐字保留再追加", func(t *testing.T) {
		body := []byte(`{"error":{"message":"a \"quoted\" line\nbreak"}}`)
		got := attachSessionIDToErrorBody("s", 400, "application/json", body)
		var decoded map[string]any
		if err := json.Unmarshal(got, &decoded); err != nil {
			t.Fatalf("结果不是合法 JSON: %v (%s)", err, got)
		}
		want := "a \"quoted\" line\nbreak (cch_session_id: s)"
		if decoded["error"].(map[string]any)["message"] != want {
			t.Fatalf("message = %q，期望 %q", decoded["error"].(map[string]any)["message"], want)
		}
	})
	t.Run("键序与缩进原样保留", func(t *testing.T) {
		body := []byte("{\n  \"error\": {\n    \"message\": \"x\"\n  },\n  \"request_id\": \"r\"\n}")
		got := attachSessionIDToErrorBody("s", 400, "application/json", body)
		if !strings.Contains(string(got), "\n  \"request_id\": \"r\"") {
			t.Fatalf("格式被重整: %s", got)
		}
		if !strings.Contains(string(got), `"message": "x (cch_session_id: s)"`) {
			t.Fatalf("message 未挂: %s", got)
		}
	})
}
