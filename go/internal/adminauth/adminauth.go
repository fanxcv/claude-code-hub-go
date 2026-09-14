// Package adminauth 复刻 Node 侧管理会话令牌的签发与验签，供 Go 数据面与 Go 管理面
// 直接校验浏览器已持有的 auth-token cookie。
//
// 唯一真源：src/lib/auth-admin-session-token.ts（SIGNED_ADMIN_SESSION_TOKEN_PREFIX、
// HMAC-SHA256 密钥派生、base64url 无填充、CLOCK_SKEW_MS、maxTtlSeconds 收紧语义）。
// 任何与本包不一致的改动都必须以该 TS 文件为准。
package adminauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SignedTokenPrefix 与 TS 的 SIGNED_ADMIN_SESSION_TOKEN_PREFIX 逐字一致。
const SignedTokenPrefix = "cch_admin_session_v1."

// signingSecretPrefix 与 TS 的 `cch-admin-session-token-v1:${adminToken}` 逐字一致。
const signingSecretPrefix = "cch-admin-session-token-v1:"

// MaxTokenLength 与 TS 的 MAX_TOKEN_LENGTH 一致。
const MaxTokenLength = 4096

// ClockSkew 与 TS 的 CLOCK_SKEW_MS 一致：签发时间晚于当前时间超过该值即判无效。
const ClockSkew = 5 * time.Minute

// ErrMalformed 表示令牌结构不合法（前缀、分段、base64 或载荷形状）。
var ErrMalformed = errors.New("adminauth: malformed admin session token")

// ErrSignature 表示签名不匹配。
var ErrSignature = errors.New("adminauth: admin session token signature mismatch")

// ErrExpired 表示令牌已过期，或签发时间超出允许的时钟偏移。
var ErrExpired = errors.New("adminauth: admin session token expired")

type payload struct {
	Version int     `json:"v"`
	Type    string  `json:"typ"`
	Issued  float64 `json:"iat"`
	Expires float64 `json:"exp"`
	Nonce   string  `json:"nonce"`
}

// IsSignedTokenFormat 对应 TS 的 isSignedAdminSessionTokenFormat。
func IsSignedTokenFormat(token string) bool {
	return strings.HasPrefix(token, SignedTokenPrefix)
}

func signingKey(adminToken string) []byte {
	return []byte(signingSecretPrefix + adminToken)
}

func encodeBase64URL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func sign(value, adminToken string) string {
	mac := hmac.New(sha256.New, signingKey(adminToken))
	mac.Write([]byte(value))
	return encodeBase64URL(mac.Sum(nil))
}

// CreateSignedToken 对应 TS 的 createSignedAdminSessionToken。
func CreateSignedToken(adminToken string, ttl time.Duration, now time.Time) (string, error) {
	ttlSeconds := int64(ttl / time.Second)
	if adminToken == "" || ttlSeconds <= 0 {
		return "", fmt.Errorf("adminauth: invalid admin session token input")
	}

	nonce, err := randomUUID()
	if err != nil {
		return "", err
	}

	body, err := json.Marshal(payload{
		Version: 1,
		Type:    "admin-session",
		Issued:  float64(now.UnixMilli()),
		Expires: float64(now.Add(time.Duration(ttlSeconds) * time.Second).UnixMilli()),
		Nonce:   nonce,
	})
	if err != nil {
		return "", err
	}

	signedValue := SignedTokenPrefix + encodeBase64URL(body)
	return signedValue + "." + sign(signedValue, adminToken), nil
}

// VerifySignedToken 对应 TS 的 verifySignedAdminSessionToken，返回 nil 表示有效。
func VerifySignedToken(token, adminToken string, maxTTL time.Duration, now time.Time) error {
	if adminToken == "" || len(token) > MaxTokenLength || !IsSignedTokenFormat(token) {
		return ErrMalformed
	}

	withoutPrefix := strings.TrimPrefix(token, SignedTokenPrefix)
	separator := strings.Index(withoutPrefix, ".")
	if separator <= 0 || separator != strings.LastIndex(withoutPrefix, ".") {
		return ErrMalformed
	}

	payloadPart := withoutPrefix[:separator]
	signature := withoutPrefix[separator+1:]
	if payloadPart == "" || signature == "" {
		return ErrMalformed
	}

	signedValue := SignedTokenPrefix + payloadPart
	expected := sign(signedValue, adminToken)
	if subtle.ConstantTimeCompare([]byte(signature), []byte(expected)) != 1 {
		return ErrSignature
	}

	raw, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		return ErrMalformed
	}

	var parsed payload
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ErrMalformed
	}
	if parsed.Version != 1 || parsed.Type != "admin-session" || parsed.Nonce == "" {
		return ErrMalformed
	}

	maxTTLMillis := float64(int64(maxTTL/time.Second) * 1000)
	if maxTTLMillis <= 0 {
		return ErrMalformed
	}
	if parsed.Expires <= parsed.Issued {
		return ErrMalformed
	}

	// 与 TS 一致：配置的 TTL 是「自签发起算」的上限，下调配置只收紧旧 cookie 的剩余寿命。
	effectiveExpiresAt := parsed.Expires
	if issuedPlusMax := parsed.Issued + maxTTLMillis; issuedPlusMax < effectiveExpiresAt {
		effectiveExpiresAt = issuedPlusMax
	}

	nowMillis := float64(now.UnixMilli())
	if parsed.Issued > nowMillis+float64(ClockSkew/time.Millisecond) {
		return ErrExpired
	}
	if effectiveExpiresAt <= nowMillis {
		return ErrExpired
	}

	return nil
}

// randomUUID 产出与 crypto.randomUUID() 同形状的 v4 UUID（TS 用 randomUUID 填充 nonce）。
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
