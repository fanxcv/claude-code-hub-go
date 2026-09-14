package adminauth

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

const testAdminToken = "probe-admin-token"

func TestRoundTrip(t *testing.T) {
	now := time.UnixMilli(1_760_000_000_000)
	token, err := CreateSignedToken(testAdminToken, time.Hour, now)
	if err != nil {
		t.Fatalf("CreateSignedToken: %v", err)
	}
	if !IsSignedTokenFormat(token) {
		t.Fatalf("token 缺少前缀: %s", token)
	}
	if err := VerifySignedToken(token, testAdminToken, time.Hour, now); err != nil {
		t.Fatalf("VerifySignedToken 应通过, got %v", err)
	}
}

func TestVerifyRejectsWrongAdminToken(t *testing.T) {
	now := time.UnixMilli(1_760_000_000_000)
	token, err := CreateSignedToken(testAdminToken, time.Hour, now)
	if err != nil {
		t.Fatalf("CreateSignedToken: %v", err)
	}
	if err := VerifySignedToken(token, "other-token", time.Hour, now); !errors.Is(err, ErrSignature) {
		t.Fatalf("换 adminToken 应报签名不符, got %v", err)
	}
}

func TestVerifyRejectsTamperedPayload(t *testing.T) {
	now := time.UnixMilli(1_760_000_000_000)
	token, err := CreateSignedToken(testAdminToken, time.Hour, now)
	if err != nil {
		t.Fatalf("CreateSignedToken: %v", err)
	}
	// 篡改载荷末位字符，签名必须失效。
	parts := strings.Split(token, ".")
	tampered := parts[0] + "." + parts[1][:len(parts[1])-1] + "A" + "." + parts[2]
	if err := VerifySignedToken(tampered, testAdminToken, time.Hour, now); err == nil {
		t.Fatal("篡改载荷后仍验签通过")
	}
}

func TestVerifyRejectsExpiredAndFuture(t *testing.T) {
	issued := time.UnixMilli(1_760_000_000_000)
	token, err := CreateSignedToken(testAdminToken, time.Hour, issued)
	if err != nil {
		t.Fatalf("CreateSignedToken: %v", err)
	}

	if err := VerifySignedToken(token, testAdminToken, time.Hour, issued.Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期令牌应报 ErrExpired, got %v", err)
	}
	if err := VerifySignedToken(token, testAdminToken, time.Hour, issued.Add(-ClockSkew-time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("超出时钟偏移应报 ErrExpired, got %v", err)
	}
	if err := VerifySignedToken(token, testAdminToken, time.Hour, issued.Add(-time.Minute)); err != nil {
		t.Fatalf("偏移一分钟内应通过, got %v", err)
	}
}

func TestVerifyHonorsMaxTTLShrink(t *testing.T) {
	issued := time.UnixMilli(1_760_000_000_000)
	token, err := CreateSignedToken(testAdminToken, 8*time.Hour, issued)
	if err != nil {
		t.Fatalf("CreateSignedToken: %v", err)
	}

	// maxTtl 收紧到 1 小时：签发后 90 分钟即失效，尽管令牌自身 exp 在 8 小时后。
	if err := VerifySignedToken(token, testAdminToken, time.Hour, issued.Add(90*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("收紧 maxTtl 后应失效, got %v", err)
	}
	if err := VerifySignedToken(token, testAdminToken, time.Hour, issued.Add(30*time.Minute)); err != nil {
		t.Fatalf("收紧窗口内应通过, got %v", err)
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	now := time.UnixMilli(1_760_000_000_000)
	cases := map[string]string{
		"空":          "",
		"无前缀":        "abc.def",
		"缺签名":        SignedTokenPrefix + "abc",
		"多段":         SignedTokenPrefix + "a.b.c",
		"签名段为空":      SignedTokenPrefix + "abc.",
		"载荷非 base64": SignedTokenPrefix + "!!!." + "sig",
		"超长":         SignedTokenPrefix + strings.Repeat("a", MaxTokenLength),
	}
	for name, token := range cases {
		if err := VerifySignedToken(token, testAdminToken, time.Hour, now); err == nil {
			t.Errorf("%s: 应报错但通过了", name)
		}
	}
}

// TestVerifyNodeIssuedCookie 用 Node 数据面真实签发的 auth-token 验签。
//
// cookie 由 CCH_ADMIN_COOKIE_FILE 指向的文件提供（一行，不含换行），ADMIN_TOKEN 由
// CCH_ADMIN_TOKEN 提供；两者都来自环境，绝不写进仓库或日志。未设置即跳过。
func TestVerifyNodeIssuedCookie(t *testing.T) {
	cookieFile := os.Getenv("CCH_ADMIN_COOKIE_FILE")
	adminToken := os.Getenv("CCH_ADMIN_TOKEN")
	if cookieFile == "" || adminToken == "" {
		t.Skip("需要 CCH_ADMIN_COOKIE_FILE 与 CCH_ADMIN_TOKEN")
	}

	raw, err := os.ReadFile(cookieFile)
	if err != nil {
		t.Fatalf("读取 cookie 文件: %v", err)
	}
	token := strings.TrimSpace(string(raw))
	if !IsSignedTokenFormat(token) {
		t.Fatalf("Node 签发的 cookie 不是签名令牌形态: 前缀=%q", token[:min(24, len(token))])
	}

	ttl := 7 * 24 * time.Hour
	if err := VerifySignedToken(token, adminToken, ttl, time.Now()); err != nil {
		t.Fatalf("Node 签发的 cookie 在 Go 侧验签失败: %v", err)
	}
}
