package logx

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestLogWritesSingleLineJSON(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger := New(buffer)

	logger.Info("config_loaded", map[string]any{"egressMode": "node"})

	line := strings.TrimSpace(buffer.String())
	if strings.Contains(line, "\n") {
		t.Fatalf("每条日志必须是单行，收到 %q", line)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(line), &record); err != nil {
		t.Fatalf("日志必须是 JSON: %v", err)
	}
	if record["level"] != "info" || record["event"] != "config_loaded" {
		t.Fatalf("级别与事件名不正确: %v", record)
	}
	if record["egressMode"] != "node" {
		t.Fatalf("自定义字段丢失: %v", record)
	}
	if _, ok := record["time"]; !ok {
		t.Fatalf("日志必须带时间戳: %v", record)
	}
}

func TestWithBindsFieldsWithoutMutatingParent(t *testing.T) {
	buffer := &bytes.Buffer{}
	parent := New(buffer)
	child := parent.With(map[string]any{"worker": "cchd"})

	child.Warn("shutdown_started", nil)
	parent.Error("config_invalid", map[string]any{"error": "boom"})

	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("应写入两行日志，收到 %d", len(lines))
	}
	var childRecord, parentRecord map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &childRecord); err != nil {
		t.Fatalf("子日志不是 JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &parentRecord); err != nil {
		t.Fatalf("父日志不是 JSON: %v", err)
	}
	if childRecord["worker"] != "cchd" {
		t.Fatalf("子日志缺少绑定字段: %v", childRecord)
	}
	if _, ok := parentRecord["worker"]; ok {
		t.Fatalf("父日志不得被 With 污染: %v", parentRecord)
	}
}

func TestNilWriterFallsBackToStderr(t *testing.T) {
	logger := New(nil)
	if logger.out == nil {
		t.Fatal("writer 为空时必须回退到 stderr")
	}
}

func TestAllLevelsAreWritten(t *testing.T) {
	buffer := &bytes.Buffer{}
	logger := New(buffer)

	logger.Debug("debug_event", nil)
	logger.Info("info_event", nil)
	logger.Warn("warn_event", nil)
	logger.Error("error_event", nil)

	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("四个级别应各写一行，收到 %d 行", len(lines))
	}
	wantLevels := []string{"debug", "info", "warn", "error"}
	for index, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("第 %d 行不是 JSON: %v", index, err)
		}
		if record["level"] != wantLevels[index] {
			t.Fatalf("第 %d 行级别应为 %s，收到 %v", index, wantLevels[index], record["level"])
		}
	}
}

// With 必须能在无绑定字段的日志器上使用，且不改动原日志器。
func TestWithOnUnboundLogger(t *testing.T) {
	buffer := &bytes.Buffer{}
	parent := New(buffer)
	child := parent.With(map[string]any{"stage": "M1"})
	if len(parent.bound) != 0 {
		t.Fatalf("With 不得改动父日志器的绑定字段: %v", parent.bound)
	}
	child.Info("event", nil)
	if !strings.Contains(buffer.String(), `"stage":"M1"`) {
		t.Fatalf("子日志器应携带绑定字段: %s", buffer.String())
	}
}

// TestNilLoggerIsSilent 钉住 nil 接收者不 panic（静默即「没有日志器」的语义）。
//
// 为什么必须有这条：`*Logger` 是具体类型，nil 指针一装箱进接口（adminapi 的
// `Problems.Logger`）就不再是 nil，调用方**无从判空**——4xx 日志路径第一次真正调用它时，
// 整个测试进程以 SIGSEGV 炸掉（2026-09-15 的实际表现）。
func TestNilLoggerIsSilent(t *testing.T) {
	var logger *Logger
	logger.Warn("admin_action_error", map[string]any{"resource": "provider"})
	logger.Error("admin_action_error_500", nil)
	if child := logger.With(map[string]any{"stage": "M1"}); child != nil {
		t.Fatalf("nil 日志器的 With 应返回 nil，实得 %v", child)
	}
}
