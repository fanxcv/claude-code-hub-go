package main

import (
	"os"
	"regexp"
	"testing"
)

// 本文件是**源码结构性钉子**：断言 cmd/cchd 真的把 config.MaxInflightBytes 填进了
// dataplane.StoreOptions.MaxInflightBytes。
//
// 为什么必须有它：这条链分两段——装配内侧（StoreOptions.MaxInflightBytes → 正文准入层，
// 由 internal/dataplane/inflight_bytes_wiring_test.go 钉住）与**本文件钉的外侧**：cmd/cchd
// 的 NewStoreBacked 调用有没有填这个字段。
//
// 盲区实证：在外侧这一行缺失时，内侧钉子与 ingress/guard 的全部用例**全绿**，而生产上
// 配置里的 CCH_GO_MAX_INFLIGHT_BYTES 是一份死的第二真源——实际预算退回 guard 包单例自己再读
// 一次同名环境变量，启动日志打印的上限因此可能与实际生效的不一致（这正是接线前的真实状态）。
// 同类教训见本目录 session_binding_ttl_wiring_nail_test.go。
var maxInflightBytesWiringOuter = regexp.MustCompile(
	`(?m)^\s*MaxInflightBytes:\s+options\.Cfg\.MaxInflightBytes,\s*$`)

func TestMaxInflightBytesWiringFillsStoreOptions(t *testing.T) {
	const path = "dataplane.go"
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败：%v", path, err)
	}
	if !maxInflightBytesWiringOuter.Match(source) {
		t.Fatalf("dataplane.go 里找不到「config.MaxInflightBytes → " +
			"StoreOptions.MaxInflightBytes」的接线：\n" +
			"  期望在 NewStoreBacked 的 StoreOptions 字面量里形如\n" +
			"  `MaxInflightBytes: options.Cfg.MaxInflightBytes,`\n" +
			"  该行缺失时配置值不生效，在途正文预算回到 guard 包单例自己读的环境变量。")
	}
}
