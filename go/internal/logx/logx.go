// Package logx 提供结构化 JSON 日志与凭据遮蔽。
//
// 输出目标是 stderr 的单行 JSON，字段名小驼峰，与 Node 侧结构化日志的习惯一致。
// 纪律：DSN、REDIS_URL、ADMIN_TOKEN 等凭据一律不进日志；需要表达「已配置」时只写布尔。
package logx

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Level 是日志级别。
//
// 优先级与 Node 的 levelPriority 一致（fatal > error > warn > info > debug > trace）：
// 写一条日志时，只有它的优先级 >= 当前级别才会落盘。
type Level string

const (
	LevelFatal Level = "fatal"
	LevelError Level = "error"
	LevelWarn  Level = "warn"
	LevelInfo  Level = "info"
	LevelDebug Level = "debug"
	LevelTrace Level = "trace"
)

// levelPriorities 是级别优先级表（数值越大越严重）。
var levelPriorities = map[Level]int{
	LevelFatal: 5,
	LevelError: 4,
	LevelWarn:  3,
	LevelInfo:  2,
	LevelDebug: 1,
	LevelTrace: 0,
}

// ValidLevels 是 /api/admin/log-level 接受的六个级别（顺序与 Node 的 validLevels 一致）。
var ValidLevels = []Level{LevelFatal, LevelError, LevelWarn, LevelInfo, LevelDebug, LevelTrace}

// currentLevel 是进程当前日志级别（原子存优先级，读热路径不加锁）。
//
// 默认值取自 LOG_LEVEL 环境变量；无值（或非法）时退到 debug。
// **登记进白名单的差异**：Node 的默认级别按 NODE_ENV 分叉（开发 debug / 生产 info），
// Go 侧不看 NODE_ENV，一律退到 debug——即「比 Node 生产更啰嗦，但不丢日志」，
// 这与本包改动前的行为（不过滤）一致。
var currentLevel atomic.Int32

func init() {
	level := levelFromEnv()
	currentLevel.Store(int32(levelPriorities[level]))
}

// levelFromEnv 读 LOG_LEVEL；非法值当作未配置。
func levelFromEnv() Level {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
	for _, candidate := range ValidLevels {
		if string(candidate) == raw {
			return candidate
		}
	}
	return LevelDebug
}

// SetLevel 设置进程日志级别（管理面 /api/admin/log-level 的写路径）。
//
// 返回 false 表示级别不在六个合法值里（调用方据此作答 400）。
func SetLevel(level string) bool {
	for _, candidate := range ValidLevels {
		if string(candidate) == level {
			currentLevel.Store(int32(levelPriorities[candidate]))
			return true
		}
	}
	return false
}

// CurrentLevel 返回当前日志级别的字符串形式。
//
// 名字不能叫 Level：那是本包的类型名。
func CurrentLevel() string {
	current := int(currentLevel.Load())
	for _, candidate := range ValidLevels {
		if levelPriorities[candidate] == current {
			return string(candidate)
		}
	}
	return string(LevelDebug)
}

// shouldWrite 判断该级别是否达到当前阈值。
func shouldWrite(level Level) bool {
	return levelPriorities[level] >= int(currentLevel.Load())
}

// Logger 是并发安全的结构化日志器。
type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	now   func() time.Time
	bound map[string]any
}

// New 建一个写往 out 的日志器；out 为 nil 时写 stderr。
func New(out io.Writer) *Logger {
	if out == nil {
		out = os.Stderr
	}
	return &Logger{out: out, now: time.Now, bound: map[string]any{}}
}

// With 返回一个携带固定字段的子日志器，父日志器不受影响。
func (l *Logger) With(fields map[string]any) *Logger {
	child := &Logger{out: l.out, now: l.now, bound: make(map[string]any, len(l.bound)+len(fields))}
	for key, value := range l.bound {
		child.bound[key] = value
	}
	for key, value := range fields {
		child.bound[key] = value
	}
	return child
}

// Fatal 写一条 fatal 日志（最高级别）。
func (l *Logger) Fatal(event string, fields map[string]any) { l.log(LevelFatal, event, fields) }

// Debug 写一条 debug 日志。
func (l *Logger) Debug(event string, fields map[string]any) { l.log(LevelDebug, event, fields) }

// Trace 写一条 trace 日志（最低级别）。
func (l *Logger) Trace(event string, fields map[string]any) { l.log(LevelTrace, event, fields) }

// Info 写一条 info 日志。
func (l *Logger) Info(event string, fields map[string]any) { l.log(LevelInfo, event, fields) }

// Warn 写一条 warn 日志。
func (l *Logger) Warn(event string, fields map[string]any) { l.log(LevelWarn, event, fields) }

// Error 写一条 error 日志。
func (l *Logger) Error(event string, fields map[string]any) { l.log(LevelError, event, fields) }

func (l *Logger) log(level Level, event string, fields map[string]any) {
	// 级别过滤在拼装之前：被过滤掉的日志不应该付出序列化代价。
	if !shouldWrite(level) {
		return
	}
	record := make(map[string]any, len(l.bound)+len(fields)+3)
	for key, value := range l.bound {
		record[key] = value
	}
	for key, value := range fields {
		record[key] = value
	}
	record["level"] = string(level)
	record["event"] = event
	record["time"] = l.now().UTC().Format(time.RFC3339Nano)

	line, err := json.Marshal(record)
	if err != nil {
		line = []byte(fmt.Sprintf(`{"level":"error","event":"log_marshal_failed","error":%q}`, err.Error()))
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.out.Write(append(line, '\n'))
}
