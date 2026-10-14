package dataplane

import (
	"os"
	"regexp"
	"testing"
)

// 本文件钉住门控前缀上限（STREAM_GATE_PREBUFFER_{EVENT,BYTE}_CAP）从 env 契约到
// forward.StreamOptions 的接线。
//
// 为什么必须有它：上限在 forward 侧有一位「零值即落回 500 事件 / 4MiB」的兜底
// （forward/stream.go 的 gateOptions），所以**不接线时所有测试仍全绿**——env 契约解析有自己的
// 用例、门控的溢出判定也有自己的用例，唯独「env 到底有没有接上」无人覆盖。生产上的后果是
// env 里配 64 事件 / 10MiB 被静默忽略，实际按 500 / 4MiB 门控（前缀驻留内存比配置多一个量级）。
// 同策先例见同目录 slow_probe_wiring_nail_test.go 与 session_binding_ttl_wiring_nail_test.go。

// capsEnvRead 断言装配期确实从 env 契约读了两条上限。
var capsEnvRead = regexp.MustCompile(
	`(?m)^\s*streamCapsEventCap,\s*streamCapsByteCap\s*:=\s*streamGateCapsEnv\(\)$`)

// capsEventFilled / capsByteFilled 断言两位真的被填进了 StreamOptions，且填的就是读到的那个局部变量。
var (
	capsEventFilled = regexp.MustCompile(`(?m)^\s*PrebufferEventCap:\s+streamCapsEventCap,$`)
	capsByteFilled  = regexp.MustCompile(`(?m)^\s*PrebufferByteCap:\s+streamCapsByteCap,$`)
)

func TestStreamGateCapsEnvFeedsStreamOptions(t *testing.T) {
	source := readAssembleSource(t)
	if !capsEnvRead.Match(source) {
		t.Fatalf("assemble.go 里找不到「读 STREAM_GATE_PREBUFFER_{EVENT,BYTE}_CAP」的装配期取值：\n" +
			"  期望形如 `streamCapsEventCap, streamCapsByteCap := streamGateCapsEnv()`。")
	}
	if !capsEventFilled.Match(source) {
		t.Fatalf("assemble.go 的 forward.StreamOptions 里找不到「STREAM_GATE_PREBUFFER_EVENT_CAP → " +
			"PrebufferEventCap」的接线：\n  期望形如 `PrebufferEventCap: streamCapsEventCap,`。\n" +
			"  该行缺失时该位留零，forward 落回自己的 500 事件出厂值，env 配置静默失效。")
	}
	if !capsByteFilled.Match(source) {
		t.Fatalf("assemble.go 的 forward.StreamOptions 里找不到「STREAM_GATE_PREBUFFER_BYTE_CAP → " +
			"PrebufferByteCap」的接线：\n  期望形如 `PrebufferByteCap: streamCapsByteCap,`。\n" +
			"  该行缺失时该位留零，forward 落回自己的 4MiB 出厂值，env 配置静默失效。")
	}
}

// TestStreamGateCapsEnvReadsContractVariables 断言上限确实从契约环境变量取值
// （而非被写死的常量）：设成与出厂默认不同的值，函数必须如实返回。
func TestStreamGateCapsEnvReadsContractVariables(t *testing.T) {
	t.Setenv("STREAM_GATE_PREBUFFER_EVENT_CAP", "7")
	t.Setenv("STREAM_GATE_PREBUFFER_BYTE_CAP", "8192")

	eventCap, byteCap := streamGateCapsEnv()
	if eventCap != 7 {
		t.Errorf("STREAM_GATE_PREBUFFER_EVENT_CAP=7 时应读到 7，得到 %d", eventCap)
	}
	if byteCap != 8192 {
		t.Errorf("STREAM_GATE_PREBUFFER_BYTE_CAP=8192 时应读到 8192，得到 %d", byteCap)
	}
}

// TestStreamGateCapsEnvFallsBackToContractDefaults 断言未设置时拿到的是契约默认
// （config/env.go 的 64 事件 / 10485760 字节），而不是零值——零值会让 forward 落回
// 它自己的 500 / 4MiB，即「接上去等于没接」。
func TestStreamGateCapsEnvFallsBackToContractDefaults(t *testing.T) {
	os.Unsetenv("STREAM_GATE_PREBUFFER_EVENT_CAP")
	os.Unsetenv("STREAM_GATE_PREBUFFER_BYTE_CAP")

	eventCap, byteCap := streamGateCapsEnv()
	if eventCap != 64 {
		t.Errorf("未设置时应取契约默认 64 事件，得到 %d", eventCap)
	}
	if byteCap != 10485760 {
		t.Errorf("未设置时应取契约默认 10485760 字节，得到 %d", byteCap)
	}
}
