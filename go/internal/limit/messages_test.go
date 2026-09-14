package limit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// retiredLocaleCatalogs 已退役的 UI 语种 → 其 golden 的**仓库内冻结快照**。
//
// 为何这三国不随 UI 词表一起退役：`Message` 按 `Accept-Language` 选语言，属 **API 行为**，
// 不在「UI 语言只留简体中文与英文」的范围内——裁掉它们会让 ja/ru/zh-TW 客户端的限流文案
// 退化为默认语种，那是自己替用户做的决定。故文案保留，只是 golden 的真源由 UI 词表改为
// 冻结副本（冻结时点与语义降级见各夹具头注）。
// 现存语种（zh-CN/en）仍读 UI 词表：那里的真源还在演进，冻结它只会白白丢掉信号。
var retiredLocaleCatalogs = map[string]string{
	"zh-TW": "go/testdata/node_limit_errors_zh-TW.txt",
	"ru":    "go/testdata/node_limit_errors_ru.txt",
	"ja":    "go/testdata/node_limit_errors_ja.txt",
}

// TestMessagesMatchNodeCatalog 逐字比对九条限流文案与各语种的 golden。
//
// 文案是对客户端可见的事实：任何改写都等于改变产品行为，所以这里不做「大意相同」的断言，
// 而是直接读仓库里的 JSON 逐字比。
//
// 语义降级（2026-09-13，UI 语种裁到 zh-CN + en）：退役三语种的真源由 live UI 词表改为
// go/testdata 下的冻结快照，故对它们而言本闸门从「对 UI 词表漂移」降为「对冻结快照漂移」。
func TestMessagesMatchNodeCatalog(t *testing.T) {
	locales := []string{"zh-CN", "zh-TW", "en", "ru", "ja"}
	codes := []string{
		MessageRPMExceeded,
		Message5hExceeded,
		Message5hRollingExceeded,
		MessageDailyQuotaExceeded,
		MessageDailyRollingExceeded,
		MessageWeeklyExceeded,
		MessageMonthlyExceeded,
		MessageTotalExceeded,
		MessageConcurrentSessionsExceeded,
	}

	for _, locale := range locales {
		catalog := loadLimitErrorCatalog(t, locale)
		for _, code := range codes {
			want, ok := catalog[code]
			if !ok {
				t.Fatalf("golden 文案里缺少 %s（%s）", code, locale)
			}
			if got := Message(locale, code); got != want {
				t.Errorf("%s/%s 文案不一致:\n得到 %q\n期望 %q", locale, code, got, want)
			}
		}
	}
}

// loadLimitErrorCatalog 取某语种的 golden 词表：退役语种读冻结快照，现存语种读 UI 词表。
//
// 两条路径**读不到就硬红**：本闸门读不到真源就形同虚设，静默跳过会让它看上去比对过。
func loadLimitErrorCatalog(t *testing.T, locale string) map[string]string {
	t.Helper()

	if fixture, ok := retiredLocaleCatalogs[locale]; ok {
		path, found := findRepoFile(fixture)
		if !found {
			t.Fatalf("在仓库内找不到冻结真源 %s：本闸门读不到真源就形同虚设，不允许静默通过",
				fixture)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取冻结真源 %s 失败: %v", fixture, err)
		}
		return parseCatalog(t, locale, stripFixtureHeader(string(raw)))
	}

	raw, err := os.ReadFile(filepath.Join(uiMessagesDir(t), locale, "errors.json"))
	if err != nil {
		t.Fatalf("读取 %s 文案失败: %v", locale, err)
	}
	return parseCatalog(t, locale, string(raw))
}

// stripFixtureHeader 去掉冻结夹具头部的 `#` 注释行。
//
// JSON 写不了注释，而冻结时点与语义降级必须与数据同体才不会被忽略，故与既有夹具同法：
// 头部若干 `#` 行 + 逐字节原样的正文（用 findRepoFile 向上找，不写死相对路径）。
func stripFixtureHeader(content string) string {
	var body strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		body.WriteString(line)
		body.WriteString("\n")
	}
	return body.String()
}

func parseCatalog(t *testing.T, locale, content string) map[string]string {
	t.Helper()
	var catalog map[string]string
	if err := json.Unmarshal([]byte(content), &catalog); err != nil {
		t.Fatalf("解析 %s 文案失败: %v", locale, err)
	}
	return catalog
}

func TestMessageFallsBackToDefaultLocale(t *testing.T) {
	if got, want := Message("fr", MessageTotalExceeded), Message(DefaultLocale, MessageTotalExceeded); got != want {
		t.Errorf("未知语种应退回默认语种: 得到 %q，期望 %q", got, want)
	}
	if got := Message("zh-CN", "NOT_A_CODE"); got != "" {
		t.Errorf("未知编码应返回空串，得到 %q", got)
	}
}

func TestRenderMessageSubstitutesPlaceholders(t *testing.T) {
	// 模板里的 ${current} 是「美元符号 + 占位符」，渲染后是 $ + 数值。
	got := RenderMessage("en", Message5hExceeded, map[string]string{
		"current":   "1.5000",
		"limit":     "2.0000",
		"resetTime": "2026-03-01T10:00:00.000Z",
	})
	want := "5-hour cost limit exceeded: $1.5000 USD (limit: $2.0000 USD). Resets at 2026-03-01T10:00:00.000Z"
	if got != want {
		t.Errorf("渲染结果不符:\n得到 %q\n期望 %q", got, want)
	}

	// 并发会话文案用的是 {current}/{limit}（无美元符号）。
	got = RenderMessage("en", MessageConcurrentSessionsExceeded, map[string]string{"current": "3", "limit": "2"})
	want = "Concurrent sessions limit exceeded: 3 sessions (limit: 2). Please wait for active sessions to complete"
	if got != want {
		t.Errorf("并发文案渲染不符:\n得到 %q\n期望 %q", got, want)
	}

	// 缺参数时占位符被替换为空串，而不是留下花括号。
	if got := RenderMessage("en", MessageConcurrentSessionsExceeded, map[string]string{"current": "1"}); got == "" {
		t.Error("缺参数时不应返回空串")
	}
}

// uiMessagesDir 定位仓库根的 UI 词表目录（Go 模块在 <repo>/go 下）。
//
// 读不到即硬红（原先这里是 t.Skip）：UI 词表是本仓被跟踪的文件，缺了就是检出被破坏；
// 而静默跳过会让这条闸门在这种情况下**看起来比对过**——那比没有闸门更危险。
// 同时也是为了不让该跳过掩盖新加的冻结快照校验。
func uiMessagesDir(t *testing.T) string {
	t.Helper()
	probe, found := findRepoFile(filepath.Join("messages", "zh-CN", "errors.json"))
	if !found {
		t.Fatalf("在仓库内找不到 UI 词表 messages/zh-CN/errors.json：本闸门不允许静默通过")
	}
	return filepath.Dir(filepath.Dir(probe))
}

// findRepoFile 从当前目录向上找仓库内文件。
//
// 用向上查找而不是写死 "../../../"：写死的相对路径只在 `go test`（cwd 为包目录）下成立，
// 编译成测试二进制在别处运行时读不到文件——而一个静默跳过的漂移闸门比没有闸门更危险，
// 它会让人以为比对过。
func findRepoFile(relative string) (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for depth := 0; depth < 8; depth++ {
		candidate := filepath.Join(dir, relative)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}
