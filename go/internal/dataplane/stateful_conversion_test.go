package dataplane

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// 本文件钉住「状态型字段跨协议线 fail-closed」在**响应侧**的落法。
//
// 为什么必须钉：forward 侧拦下请求只完成了一半——若流水线把它当成上游/网关故障，客户端会收到
// 5xx（且文案说「上游不可用」），既误导客户端也污染失败归因。正确的落法是 400 + 点名冲突字段，
// 让客户端能自救（改走同协议供应商，或把上下文放进 input）。

func TestStatefulConversionStatus(t *testing.T) {
	t.Run("带字段名的拒绝错误翻成 400 且文案点名", func(t *testing.T) {
		status, message, ok := statefulConversionStatus(&forward.StatefulConversionError{Field: "previous_response_id"})
		if !ok {
			t.Fatal("应识别为状态型字段拒绝")
		}
		if status != http.StatusBadRequest {
			t.Fatalf("状态码应为 400，收到 %d", status)
		}
		if !strings.Contains(message, "previous_response_id") {
			t.Fatalf("文案必须点名冲突字段以便客户端自查，实际：%s", message)
		}
		// 文案不得泄漏上游主机名/凭据一类的内部信息。
		if strings.Contains(message, "http") {
			t.Fatalf("文案不该含上游地址：%s", message)
		}
	})

	t.Run("包装后仍可识别（errors.As 穿透）", func(t *testing.T) {
		wrapped := fmt.Errorf("forward: 计划构造失败: %w", &forward.StatefulConversionError{Field: "conversation"})
		_, message, ok := statefulConversionStatus(wrapped)
		if !ok || !strings.Contains(message, "conversation") {
			t.Fatalf("包装错误未被识别：ok=%v message=%s", ok, message)
		}
	})

	t.Run("其它错误不映射（不得把上游故障也说成客户端问题）", func(t *testing.T) {
		if _, _, ok := statefulConversionStatus(forward.ErrInvalidBody); ok {
			t.Fatal("非状态型字段错误不该走 400 映射")
		}
		if _, _, ok := statefulConversionStatus(nil); ok {
			t.Fatal("nil 错误不该映射")
		}
		if _, _, ok := statefulConversionStatus(errors.New("上游 502")); ok {
			t.Fatal("普通错误不该映射")
		}
	})
}
