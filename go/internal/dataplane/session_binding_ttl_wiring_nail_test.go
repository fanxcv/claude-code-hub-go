package dataplane

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// 本文件是**源码结构性钉子**：断言会话绑定的 TTL 真的从装配参数接到了 BinderOptions 上。
//
// 为什么必须有它：这条链的两段各自都有测试——session 包的 BinderOptions.TTL 语义
// （<=0 落回 DefaultBindingTTLSeconds = 300s）有单测，Redis 键上的 TTL 落值也有集成用例；
// 但「dataplane 到底有没有把 StoreOptions.SessionBindingTTLSeconds 填进 BinderOptions.TTL」
// 谁都盖不住：摘掉这一行时那两组用例**全绿**，而生产上绑定键的 TTL 会静默落回 300s
// ——会话静默超过 5 分钟即丢绑定，设计稿要求的一小时粘性（docs/design-session-sticky.md:161）
// 静默失效。这正是「测试全绿但功能没接」的盲区（同类教训见同目录
// slow_probe_wiring_nail_test.go 与 custom_headers_wiring_nail_test.go），故用源码发现钉死。
//
// 为何正则要连着 NewSessionBinderAdapter 那一行：只钉 TTL 那一行会漏掉「填到了别的结构上」
// 这类走样；把两者绑在同一个字面量内（间隙上限 400 字节，远小于该构造块）才算钉住了接线本身。
//
// 覆盖面边界（据实记）：本钉子只覆盖装配函数的**内侧**（StoreOptions → BinderOptions）。
// 外侧那半——cmd/cchd 的 NewStoreBacked 调用有没有填 StoreOptions.SessionBindingTTLSeconds
// ——在 cmd/ 里，本文件读不到，故不在此钉；填值点的位置见 StoreOptions.SessionBindingTTLSeconds
// 的注释。故本钉子绿**不等于**生产 TTL 已是 3600s。
var sessionBindingTTLWiring = regexp.MustCompile(
	`(?s)session\.NewSessionBinderAdapter\(session\.BinderOptions\{.{0,400}?` +
		`TTL:\s+time\.Duration\(options\.SessionBindingTTLSeconds\) \* time\.Second,`)

func TestSessionBinderWiringPropagatesConfiguredTTL(t *testing.T) {
	path := filepath.Join("assemble.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	if !sessionBindingTTLWiring.Match(source) {
		t.Fatalf("assemble.go 里找不到「StoreOptions.SessionBindingTTLSeconds → " +
			"session.BinderOptions.TTL」的接线：\n" +
			"  期望在 `session.NewSessionBinderAdapter(session.BinderOptions{...})` 里形如\n" +
			"  `TTL: time.Duration(options.SessionBindingTTLSeconds) * time.Second,`\n" +
			"  该行缺失时绑定键 TTL 恒为 session 包的默认 300s（5 分钟），" +
			"而设计稿要求复用 PREFIX_AFFINITY_TTL_SECONDS（默认 3600s）。")
	}
}
