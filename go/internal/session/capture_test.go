package session

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// 本文件钉住**有界捕获**的三条不变量：窗口语义、驻留上界、截断标记。
//
// 为什么值得单独钉：捕获是热路径上唯一触碰正文的地方，它的失败模式是「内存随正文增长」——
// 这类回归不会让任何请求失败，只会让进程在流量上来时被 OOM 杀掉。

// TestResponseCaptureKeepsEverythingWhenWithinWindow 小于窗口时原样保留，且**不**出现标记。
func TestResponseCaptureKeepsEverythingWhenWithinWindow(t *testing.T) {
	capture := NewResponseCapture(1024)
	payload := []byte(strings.Repeat("a", 1000))
	capture.Write(payload)

	if capture.Truncated() {
		t.Fatalf("1000 字节小于 1024 窗口，不应判为截断")
	}
	if got := string(capture.Bytes()); got != string(payload) {
		t.Fatalf("正文被改动：len=%d want=%d", len(got), len(payload))
	}
	if capture.Total() != 1000 {
		t.Fatalf("Total=%d want=1000", capture.Total())
	}
}

// TestResponseCaptureKeepsHeadAndTailWindow 超限时保留头尾两个窗口，并给出省略字节数。
func TestResponseCaptureKeepsHeadAndTailWindow(t *testing.T) {
	capture := NewResponseCapture(64)
	head := strings.Repeat("H", 200)
	middle := strings.Repeat("M", 500)
	tail := strings.Repeat("T", 200)
	capture.Write([]byte(head + middle + tail))

	if !capture.Truncated() {
		t.Fatal("900 字节远超 64 窗口，应判为截断")
	}
	value := string(capture.Bytes())
	if !strings.HasPrefix(value, strings.Repeat("H", 64)) {
		t.Fatalf("头窗口不是前 64 字节：%q", value[:80])
	}
	if !strings.HasSuffix(value, strings.Repeat("T", 64)) {
		t.Fatalf("尾窗口不是后 64 字节：%q", value[len(value)-80:])
	}
	// 省略量 = 总字节 - 保留量（头 64 + 尾 64）。
	if want := strconv.FormatInt(900-128, 10); !strings.Contains(value, "[TRUNCATED: "+want+" bytes omitted]") {
		t.Fatalf("截断标记里的省略量不对：%q", value)
	}
	if capture.Total() != 900 {
		t.Fatalf("Total=%d want=900（必须是看到过的总量而不是保留量）", capture.Total())
	}
}

// TestResponseCaptureMemoryStaysBounded 驻留与正文总大小无关：8 MiB 流下保留量有界。
//
// 这条是协调者要求的「流式 8 MiB 流 + 捕获开启」驻留证明的单元版：按 32 KiB 分块喂进去
// （与 dataplane 的 pumpChunkBytes 同尺寸），保留量必须留在 192 KiB 以内。
func TestResponseCaptureMemoryStaysBounded(t *testing.T) {
	capture := NewResponseCapture(ResponseCaptureWindowBytes)
	chunk := bytes.Repeat([]byte("x"), 32*1024)
	const totalChunks = 256 // 256 × 32 KiB = 8 MiB
	for index := 0; index < totalChunks; index++ {
		capture.Write(chunk)
	}

	resident := len(capture.head) + len(capture.tail)
	if resident > 3*ResponseCaptureWindowBytes {
		t.Fatalf("驻留 %d 字节超出上界 %d（头 %d + 尾 %d）",
			resident, 3*ResponseCaptureWindowBytes, len(capture.head), len(capture.tail))
	}
	if capture.Total() != int64(totalChunks*32*1024) {
		t.Fatalf("Total=%d want=%d", capture.Total(), totalChunks*32*1024)
	}
}

// TestResponseCaptureTailSlidesForwards 尾窗口只留**最后**一段，不留第一段。
func TestResponseCaptureTailSlidesForwards(t *testing.T) {
	capture := NewResponseCapture(8)
	// 三次写入把尾窗口推着走：每次 8 字节，总 24 字节。
	capture.Write([]byte("aaaaaaaa"))
	capture.Write([]byte("bbbbbbbb"))
	capture.Write([]byte("cccccccc"))

	value := string(capture.Bytes())
	if !strings.HasSuffix(value, "cccccccc") {
		t.Fatalf("尾窗口应停在最后一次写入：%q", value)
	}
	if strings.Contains(value, "bbbbbbbb") {
		t.Fatalf("尾窗口应已滑走上一段：%q", value)
	}
}

// TestSanitizeHeadersMasksSensitiveValues 敏感头遮罩 + 保留内部头剔除，与 Node 同判。
func TestSanitizeHeadersMasksSensitiveValues(t *testing.T) {
	headers := map[string][]string{
		"Authorization":          {"Bearer sk-ant-1234567890"},
		"X-Api-Key":              {"short"},
		"X-Cch-Client-Transport": {"websocket"},
		"Anthropic-Version":      {"2023-06-01"},
	}
	result := SanitizeHeaders(headers)

	// Bearer 保留前缀，token 长于 8 字符 → 保留前后 4。
	if got := result["Authorization"]; got != "Bearer sk-a******7890" {
		t.Fatalf("Authorization=%q", got)
	}
	// 短值全遮。
	if got := result["X-Api-Key"]; got != redactedMarker {
		t.Fatalf("X-Api-Key=%q", got)
	}
	if _, leaked := result["X-Cch-Client-Transport"]; leaked {
		t.Fatal("保留内部头不应落盘")
	}
	if got := result["Anthropic-Version"]; got != "2023-06-01" {
		t.Fatalf("非敏感头应原样保留：%q", got)
	}
}

// TestSanitizeHeadersEmptyIsEmptyObject 无头时返回空对象而不是 nil（Node 返回 `{}`）。
func TestSanitizeHeadersEmptyIsEmptyObject(t *testing.T) {
	result := SanitizeHeaders(map[string][]string{})
	if result == nil {
		t.Fatal("空头表应返回空对象而不是 nil")
	}
	if len(result) != 0 {
		t.Fatalf("空头表不应有键：%v", result)
	}
}

// TestSanitizeURLRedactsSensitiveQueryParams 只替换敏感查询参数，其余逐字保留。
func TestSanitizeURLRedactsSensitiveQueryParams(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"https://api.anthropic.com/v1/messages?key=sk-secret&foo=bar", "https://api.anthropic.com/v1/messages?key=[REDACTED]&foo=bar"},
		{"https://api.openai.com/v1/x?api_key=abc&b=1", "https://api.openai.com/v1/x?api_key=[REDACTED]&b=1"},
		// encodeURIComponent 不转义 ! ' ( ) * ~，空格编成 %20，与 Go 的 QueryEscape 不同。
		{"https://example.com/p?a=b c&d=e!f", "https://example.com/p?a=b%20c&d=e!f"},
		{"https://example.com/p", "https://example.com/p"},
		{"", "(empty url)"},
		{"   ", "(empty url)"},
	}
	for _, item := range cases {
		if got := SanitizeURL(item.raw); got != item.want {
			t.Fatalf("SanitizeURL(%q)=%q want=%q", item.raw, got, item.want)
		}
	}
}

// TestRedactJSONStringParsesOrFallsBack JSON 解不动时原样保留（Node 的 catch 分支）。
func TestRedactJSONStringParsesOrFallsBack(t *testing.T) {
	redacted, ok := redactJSONString(`{"messages":[{"content":"secret"}]}`)
	if !ok {
		t.Fatal("合法 JSON 应判为已脱敏")
	}
	if strings.Contains(redacted, "secret") {
		t.Fatalf("正文未脱敏：%s", redacted)
	}

	raw := "data: {\"delta\":\"secret\"}\n\n"
	fallback, ok := redactJSONString(raw)
	if ok {
		t.Fatal("SSE 文本不是 JSON，应走原样分支")
	}
	if fallback != raw {
		t.Fatalf("非 JSON 应原样保留：%q", fallback)
	}
}
