package rectify

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// mustParse 解析夹具正文；解析失败即测试失败（夹具本身写错时不该静默降级）。
func mustParse(t *testing.T, body string) *convert.Value {
	t.Helper()
	parsed, err := convert.ParseJSON([]byte(body))
	if err != nil {
		t.Fatalf("夹具正文不是合法 JSON: %v\n%s", err, body)
	}
	return parsed
}

// compact 返回正文的紧凑序列化（键序保留），用于「整流后正文」的逐字节断言。
func compact(t *testing.T, body *convert.Value) string {
	t.Helper()
	return body.MarshalCompact()
}

// boolField 读审计字段里的布尔值。
func boolField(t *testing.T, fields map[string]any, key string) bool {
	t.Helper()
	value, ok := fields[key].(bool)
	if !ok {
		t.Fatalf("审计字段 %s 不是布尔: %#v", key, fields[key])
	}
	return value
}

// intField 读审计字段里的整数（int 与 int64 都接受）。
func intField(t *testing.T, fields map[string]any, key string) int64 {
	t.Helper()
	switch value := fields[key].(type) {
	case int:
		return int64(value)
	case int64:
		return value
	default:
		t.Fatalf("审计字段 %s 不是整数: %#v", key, fields[key])
		return 0
	}
}
