package dataplane

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// TestLogSettleNotCommitted 钉住「终态未提交」这条 debug 的**出现条件**。
//
// 缺陷背景（2026-09-21 生产实测）：`MESSAGE_REQUEST_WRITE_MODE=async` 下入队成功即返回
// `Result{Queued:true}`，而 `Committed` 是零值 false（提交结论只有队列 flush 之后才有）。
// 旧条件只判 `!result.Committed`，于是**每个请求**都记一条「未提交」（87 行 / 84 请求、
// attempts 恒 0），把真失败淹掉。
//
// 断言的是**日志输出本身**（经 logx.New(io.Writer) 收进 buffer），不是谓词返回值：
// 「谓词判对了、日志照发」是这类缺陷的另一种形态，只有看输出才钉得住。
func TestLogSettleNotCommitted(t *testing.T) {
	previous := logx.CurrentLevel()
	if !logx.SetLevel(string(logx.LevelDebug)) {
		t.Fatalf("无法把日志级别设为 debug（当前 %s）", previous)
	}
	t.Cleanup(func() { logx.SetLevel(previous) })

	cases := []struct {
		name     string
		result   terminal.Result
		wantLine bool
	}{
		{name: "已提交", result: terminal.Result{Committed: true, Attempts: 1}, wantLine: false},
		{name: "已入队（异步写）", result: terminal.Result{Queued: true}, wantLine: false},
		{name: "既未提交也未入队", result: terminal.Result{Attempts: 3}, wantLine: true},
		{name: "已提交且已入队", result: terminal.Result{Committed: true, Queued: true, Attempts: 1}, wantLine: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var buffer bytes.Buffer
			settler := &storeSettler{logger: logx.New(&buffer)}
			settler.logSettleNotCommitted(testCase.result)

			output := buffer.String()
			gotLine := strings.Contains(output, `"event":"dataplane.settle_not_committed"`)
			if gotLine != testCase.wantLine {
				t.Fatalf("日志出现=%v，期望 %v；原始输出=%q", gotLine, testCase.wantLine, output)
			}
			// 真该报的那一条要带上尝试次数，否则与「另一处同名事件」无法区分。
			if testCase.wantLine && !strings.Contains(output, `"attempts":3`) {
				t.Errorf("未提交那一档应带 attempts 字段，实际 %q", output)
			}
		})
	}
}
