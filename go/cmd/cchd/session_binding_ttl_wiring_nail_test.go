package main

import (
	"os"
	"regexp"
	"testing"
)

// 本文件是**源码结构性钉子**：断言 cmd/cchd 真的把 PREFIX_AFFINITY_TTL_SECONDS 填进了
// dataplane.StoreOptions.SessionBindingTTLSeconds。
//
// 为什么必须有它：会话绑定 TTL 这条链分三段——配置域（PREFIX_AFFINITY_TTL_SECONDS，默认 3600）、
// 装配内侧（dataplane 把 StoreOptions.SessionBindingTTLSeconds 接到 session.BinderOptions.TTL，
// 由 internal/dataplane/session_binding_ttl_wiring_nail_test.go 钉住）、以及**本文件钉的外侧**：
// cmd/cchd 的 NewStoreBacked 调用有没有填这个字段。
//
// 盲区实证（2026-09-22，v1.9.17）：外侧那一行缺失时，内侧钉子与 session 包的全部用例**全绿**，
// 而生产上绑定键 TTL 静默落回 session.DefaultBindingTTLSeconds = 300s——会话静默超过 5 分钟即
// 丢绑定，设计稿（docs/design-session-sticky.md:161）要求的一小时粘性静默失效。这正是
// 「测试全绿但功能没接」的典型盲区（同类教训见 dataplane/slow_probe_wiring_nail_test.go 与
// dataplane/session_binding_ttl_wiring_nail_test.go），故用源码发现钉死。
//
// 为何正则要连着 AffinityEnvEnabled 那一行：两者同取 options.Cfg.Env，绑在同一字面量内
// （间隙上限 400 字节，远小于该构造块）才算钉住「填在同一处」这件事；只钉赋值行会漏掉
// 「填到了别的结构上」这类走样。
var sessionBindingTTLWiringOuter = regexp.MustCompile(
	`(?s)AffinityEnvEnabled:\s+options\.Cfg\.Env\.EnablePrefixAffinity,.{0,400}?` +
		`SessionBindingTTLSeconds:\s+options\.Cfg\.Env\.PrefixAffinityTTLSeconds,`)

func TestSessionBindingTTLWiringFillsStoreOptions(t *testing.T) {
	const path = "dataplane.go"
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	if !sessionBindingTTLWiringOuter.Match(source) {
		t.Fatalf("dataplane.go 里找不到「PREFIX_AFFINITY_TTL_SECONDS → " +
			"StoreOptions.SessionBindingTTLSeconds」的接线：\n" +
			"  期望在 NewStoreBacked 的 StoreOptions 字面量里形如\n" +
			"  `SessionBindingTTLSeconds: options.Cfg.Env.PrefixAffinityTTLSeconds,`\n" +
			"  该行缺失时生产绑定键 TTL 恒为 300s（5 分钟），而设计稿要求复用 3600s。")
	}
}
