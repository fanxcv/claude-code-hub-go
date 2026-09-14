package session

import (
	"strings"
	"testing"
)

// TestBuildBindingKeysMatchesNode 钉住绑定键形制。
//
// 期望值里的 hash tag 是 sha256(keyId + "\x00" + sessionId) 的十六进制，硬编码而不用被测
// 函数自证：切换期间 Node 与 Go 必须命中同一组键，自证会让「两边一起改错」也通过测试。
func TestBuildBindingKeysMatchesNode(t *testing.T) {
	cases := []struct {
		name      string
		sessionID string
		keyID     int64
		tag       string
	}{
		{"普通会话", "sess_abc", 42, "58e50bf5e06aca1558e7d960ade42bb3dadd374f7f190f89cac6328455e7e189"},
		{
			"UUID 形式会话", "a1b2c3d4-e5f6-7890-abcd-ef1234567890", 7,
			"25e2cdaadeb190989a6042166d8b2f89fc95cd25653825d88723f9036f418cf1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BuildBindingKeys(tc.sessionID, tc.keyID)
			want := BindingKeys{
				Canonical:      "session-binding:v1:{" + tc.tag + "}:binding",
				LegacyProvider: "session:" + tc.sessionID + ":provider",
				LegacyOwner:    "session:" + tc.sessionID + ":key",
			}
			if got != want {
				t.Fatalf("绑定键不符\n got=%+v\nwant=%+v", got, want)
			}

			// canonical / cooldown / lease 必须共享同一 hash tag：绑定 Lua 一次操作前三键，
			// 不同 slot 会被 Redis Cluster 直接判 CROSSSLOT。
			for _, key := range []string{
				got.Canonical,
				ProviderCooldownKey(tc.sessionID, tc.keyID, 9),
				DiscoveryLeaseKey(tc.sessionID, tc.keyID),
			} {
				if !strings.Contains(key, "{"+tc.tag+"}") {
					t.Fatalf("键 %q 未携带绑定 hash tag %q", key, tc.tag)
				}
			}
		})
	}
}

// TestDerivedKeyShapes 钉住派生键形制（逐字与 Node 一致，切换期两侧共用）。
func TestDerivedKeyShapes(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"供应商冷却", ProviderCooldownKey("sess_abc", 42, 9),
			"session-binding:v1:{58e50bf5e06aca1558e7d960ade42bb3dadd374f7f190f89cac6328455e7e189}:provider:9:cooldown"},
		{"discovery 租约", DiscoveryLeaseKey("sess_abc", 42),
			"session-binding:v1:{58e50bf5e06aca1558e7d960ade42bb3dadd374f7f190f89cac6328455e7e189}:discovery-lease"},
		{"会话序号", SeqKey("sess_abc"), "session:sess_abc:seq"},
		{"响应体代际", ResponseBodyGenerationKey("sess_abc"), "session:sess_abc:response-body-generation:v1"},
		{"请求工件 owner", RequestOwnerKey("sess_abc", 3), "session:sess_abc:req:3:owner"},
		{"最后活动", LastSeenKey("sess_abc"), "session:sess_abc:last_seen"},
		{"会话详情", InfoKey("sess_abc"), "session:sess_abc:info"},
		{"正文哈希映射", TenantContentHashSessionKey(42, "deadbeef"), "hash:42:deadbeef:session"},
		{"展示用全局活跃集", ObservedGlobalActiveSessionsKey(), "{observed_sessions}:global:active_sessions"},
		{"展示用并发计数", ObservedConcurrentCountKey("sess_abc"), "observed_session:sess_abc:concurrent_count"},
		{"全局活跃集", ActiveSessionsGlobalKey(), "{active_sessions}:global:active_sessions"},
		{"密钥维度活跃集", KeyActiveSessionsKey(42), "{active_sessions}:key:42:active_sessions"},
		{"用户维度活跃集", UserActiveSessionsKey(7), "{active_sessions}:user:7:active_sessions"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("键形制不符\n got=%q\nwant=%q", tc.got, tc.want)
			}
		})
	}
}
