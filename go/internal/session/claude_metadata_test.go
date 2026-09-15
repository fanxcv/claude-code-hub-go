package session

import (
	"strings"
	"testing"
)

// 用例值取自 Node 的同一批判定（metadata-user-id.ts 的三个纯度函数）。
// 阈值常量 2.1.78 与 Node 的 CLAUDE_CODE_METADATA_USER_ID_JSON_SWITCH_VERSION 同值。

func TestClaudeMetadataUserIDFormat(t *testing.T) {
	cases := []struct {
		name      string
		userAgent string
		want      string
	}{
		{"低于阈值走 legacy", "claude-cli/2.1.77 (external, cli)", claudeMetadataFormatLegacy},
		{"等于阈值走 json", "claude-cli/2.1.78 (external, cli)", claudeMetadataFormatJSON},
		{"高于阈值走 json", "claude-cli/2.1.100 (external, cli)", claudeMetadataFormatJSON},
		{"vscode 低于阈值走 legacy", "claude-cli/2.0.33 (external, claude-vscode)", claudeMetadataFormatLegacy},
		{"非 Claude Code 客户端一律 json", "anthropic-sdk-typescript/1.0.0", claudeMetadataFormatJSON},
		{"UA 缺失一律 json", "", claudeMetadataFormatJSON},
		{"UA 畸形一律 json", "not-a-user-agent", claudeMetadataFormatJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeMetadataUserIDFormat(tc.userAgent); got != tc.want {
				t.Fatalf("format(%q) = %q，期望 %q", tc.userAgent, got, tc.want)
			}
		})
	}
}

func TestBuildClaudeMetadataUserIDShapes(t *testing.T) {
	const keyID = 7
	legacy := BuildClaudeMetadataUserID(keyID, "sess-abc", "claude-cli/2.1.70 (external, cli)")
	if want := "user_" + BuildClaudeMetadataDeviceID(keyID) + "_account__session_sess-abc"; legacy != want {
		t.Fatalf("legacy = %q，期望 %q", legacy, want)
	}

	modern := BuildClaudeMetadataUserID(keyID, "sess-abc", "claude-cli/2.1.78 (external, cli)")
	want := `{"device_id":"` + BuildClaudeMetadataDeviceID(keyID) + `","account_uuid":"","session_id":"sess-abc"}`
	if modern != want {
		t.Fatalf("json = %q，期望 %q", modern, want)
	}
	// device_id 是 sha256 的十六进制（64 字符），与 Node createHash("sha256").digest("hex") 同长同形。
	if len(BuildClaudeMetadataDeviceID(keyID)) != 64 {
		t.Fatalf("device_id 长度 = %d，期望 64", len(BuildClaudeMetadataDeviceID(keyID)))
	}
}

func TestInjectClaudeMetadataUserID(t *testing.T) {
	const keyID = 11
	const sessionID = "sess-inject"
	ua := "claude-cli/2.1.78 (external, cli)"

	t.Run("注入并保留既有 metadata 的其他键", func(t *testing.T) {
		body := map[string]any{"metadata": map[string]any{"trace": "x"}}
		result := InjectClaudeMetadataUserID(body, keyID, sessionID, ua)
		if !result.Applied || result.Reason != "injected" {
			t.Fatalf("结果 = %+v，期望 injected", result)
		}
		metadata, _ := body["metadata"].(map[string]any)
		if metadata["trace"] != "x" {
			t.Fatalf("既有键被丢弃: %#v", metadata)
		}
		if metadata["user_id"] != BuildClaudeMetadataUserID(keyID, sessionID, ua) {
			t.Fatalf("user_id 值不符: %#v", metadata["user_id"])
		}
	})

	t.Run("无 metadata 时新建", func(t *testing.T) {
		body := map[string]any{"model": "m"}
		if result := InjectClaudeMetadataUserID(body, keyID, sessionID, ua); !result.Applied {
			t.Fatalf("未注入: %+v", result)
		}
		if _, ok := body["metadata"].(map[string]any); !ok {
			t.Fatalf("metadata 未建: %#v", body)
		}
	})

	t.Run("已有非空 user_id 时不覆盖", func(t *testing.T) {
		body := map[string]any{"metadata": map[string]any{"user_id": "cli-supplied"}}
		result := InjectClaudeMetadataUserID(body, keyID, sessionID, ua)
		if result.Applied || result.Reason != "already_exists" {
			t.Fatalf("结果 = %+v，期望 already_exists", result)
		}
		metadata, _ := body["metadata"].(map[string]any)
		if metadata["user_id"] != "cli-supplied" {
			t.Fatalf("原值被改写: %#v", metadata["user_id"])
		}
	})

	t.Run("空白 user_id 视为可注入", func(t *testing.T) {
		body := map[string]any{"metadata": map[string]any{"user_id": "   "}}
		if result := InjectClaudeMetadataUserID(body, keyID, sessionID, ua); !result.Applied {
			t.Fatalf("结果 = %+v，期望注入", result)
		}
	})

	t.Run("非字符串 user_id 视为已存在", func(t *testing.T) {
		body := map[string]any{"metadata": map[string]any{"user_id": 42}}
		if result := InjectClaudeMetadataUserID(body, keyID, sessionID, ua); result.Reason != "already_exists" {
			t.Fatalf("结果 = %+v，期望 already_exists", result)
		}
	})

	t.Run("缺 key 或缺会话时不注入", func(t *testing.T) {
		body := map[string]any{}
		if result := InjectClaudeMetadataUserID(body, 0, sessionID, ua); result.Reason != "missing_key_id" {
			t.Fatalf("结果 = %+v，期望 missing_key_id", result)
		}
		body = map[string]any{}
		if result := InjectClaudeMetadataUserID(body, keyID, "", ua); result.Reason != "missing_session_id" {
			t.Fatalf("结果 = %+v，期望 missing_session_id", result)
		}
		if _, exists := body["metadata"]; exists {
			t.Fatal("失败路径不得写入 metadata")
		}
	})

	t.Run("JSON 形制不转义 HTML 字符", func(t *testing.T) {
		// 会话 id 可能来自客户端头；Node 的 JSON.stringify 不转义 < > &，转义会造成身份分叉。
		body := map[string]any{}
		result := InjectClaudeMetadataUserID(body, keyID, "sess-<a>&b", ua)
		if !result.Applied {
			t.Fatalf("未注入: %+v", result)
		}
		if strings.Contains(result.Value, `\u003c`) || strings.Contains(result.Value, `\u0026`) {
			t.Fatalf("发生了 HTML 转义: %q", result.Value)
		}
		if !strings.Contains(result.Value, "sess-<a>&b") {
			t.Fatalf("会话 id 被改写: %q", result.Value)
		}
	})
}

// TestClaudeMetadataRoundTrip 与同包的解析侧对齐：注入出来的值必须能被 ParseClaudeMetadataUserID
// 读回同一个会话 id。两侧同包，这条钉子防的是「形制改了半边」。
func TestClaudeMetadataRoundTrip(t *testing.T) {
	for _, ua := range []string{
		"claude-cli/2.1.70 (external, cli)",
		"claude-cli/2.1.78 (external, cli)",
		"anthropic-sdk-typescript/1.0.0",
	} {
		value := BuildClaudeMetadataUserID(3, "sess-round", ua)
		parsed := ParseClaudeMetadataUserID(value)
		if parsed.SessionID != "sess-round" {
			t.Fatalf("ua=%q 值=%q 解析回 %+v", ua, value, parsed)
		}
	}
}
