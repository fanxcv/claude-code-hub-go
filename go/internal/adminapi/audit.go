package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件实现 AuditSink：把管理面的写操作落进 audit_log。
//
// 唯一真源：src/lib/audit/emit.ts（fire-and-forget 语义与字段映射）、src/repository/audit-log.ts
// （INSERT 的列与截断）、src/lib/audit/redact.ts（写前脱敏）、src/drizzle/schema.ts:1308-1337
// （表结构）。
//
// A0-3 扩了 AuditEvent（category / target_name / before_value / operator_key_name / user_agent /
// success / error_message），此前只能恒写 success = TRUE、before/其他四列恒 NULL。

// auditInsertTimeout 是单条审计写入的上界。
//
// 为什么要有上界：审计是 fire-and-forget，但 goroutine 不是免费的。写库卡住时（库不可达），
// 没有超时就会按请求量堆积 goroutine，把一个「审计降级」变成「进程 OOM」。
const auditInsertTimeout = 5 * time.Second

// auditRedacted 与 redact.ts:18 的 REDACTED 逐字一致。
const auditRedacted = "[REDACTED]"

// auditCircular 与 redact.ts:19 的 CIRCULAR 逐字一致。
const auditCircular = "[Circular]"

// auditMaxDepth 是脱敏遍历的深度上限。
//
// 与 Node 的差异（有意）：Node 用 WeakSet 识别循环引用。Go 的 map/slice 没有身份比较的廉价
// 写法，而审计快照来自 JSON 与字面量、本来不可能成环；深度上限用一个常量同时挡住了环与病态
// 嵌套（后者用 WeakSet 也挡不住）。超过上限的层写成 [Circular]，与环的占位符一致。
const auditMaxDepth = 32

// auditSensitiveKeys 与 redact.ts:4-16 的 DEFAULT_SENSITIVE_KEYS 逐字一致（全小写比对）。
var auditSensitiveKeys = map[string]struct{}{
	"key":            {},
	"apikey":         {},
	"api_key":        {},
	"api-key":        {},
	"password":       {},
	"secret":         {},
	"token":          {},
	"authorization":  {},
	"webhook_secret": {},
	"webhooksecret":  {},
	"webhook-secret": {},
}

// auditCategoryByPrefix 是 action 前缀到 action_category 的表。
//
// 取值域与 types/audit-log.ts:1-10 的 AuditCategory 一致。login.* 归 auth 是**唯一**前后缀不
// 相等的一族（src/app/api/auth/login/route.ts:172 等），单列一行而不是靠特例分支。
var auditCategoryByPrefix = map[string]string{
	"auth":            "auth",
	"login":           "auth",
	"user":            "user",
	"provider":        "provider",
	"provider_group":  "provider_group",
	"system_settings": "system_settings",
	"key":             "key",
	"notification":    "notification",
	"sensitive_word":  "sensitive_word",
	"model_price":     "model_price",
}

// AuditLog 实现 AuditSink。
type AuditLog struct {
	pools *store.Pools
	// Logger 为 nil 时静默。
	Logger *logx.Logger
	// Timeout 覆盖单条写入的上界；为 0 时取 auditInsertTimeout。
	Timeout time.Duration
}

// NewAuditLog 建审计写入器。
func NewAuditLog(deps Deps, options AuditLogOptions) (*AuditLog, error) {
	if options.Pools == nil {
		options.Pools = deps.Store
	}
	if options.Pools == nil {
		return nil, errors.New("adminapi: 审计写入器需要 store.Pools")
	}
	logger := options.Logger
	if logger == nil {
		logger = deps.Logger
	}
	return &AuditLog{pools: options.Pools, Logger: logger, Timeout: options.Timeout}, nil
}

// AuditLogOptions 是审计写入器的构造参数。
type AuditLogOptions struct {
	Pools   *store.Pools
	Logger  *logx.Logger
	Timeout time.Duration
}

// Emit 实现 AuditSink：立即返回，写入在后台完成，失败只告警。
//
// 语义与 emit.ts:46-51 的 `void emitAsync(args)` 一致：审计绝不拖慢、也绝不让请求失败。
func (a *AuditLog) Emit(ctx context.Context, event AuditEvent) {
	if a == nil || a.pools == nil {
		return
	}
	category, ok := resolveAuditCategory(event.Category, event.Action)
	if !ok {
		if a.Logger != nil {
			a.Logger.Warn("admin_audit_category_unknown", map[string]any{
				"action": event.Action,
				"writes": false,
				"reason": "category 为空且 action 前缀不在 AuditCategory 取值域内",
			})
		}
		return
	}

	// 断开与请求生命周期：请求返回后审计仍要写完（Node 同样是 void emitAsync）。
	base := context.Background()
	if ctx != nil {
		base = context.WithoutCancel(ctx)
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = auditInsertTimeout
	}

	go func() {
		defer func() {
			if recovered := recover(); recovered != nil && a.Logger != nil {
				a.Logger.Error("admin_audit_panic", map[string]any{"action": event.Action, "panic": fmt.Sprint(recovered)})
			}
		}()
		writeCtx, cancel := context.WithTimeout(base, timeout)
		defer cancel()
		if err := a.insert(writeCtx, category, event); err != nil {
			if a.Logger != nil {
				a.Logger.Warn("admin_audit_write_failed", map[string]any{
					"action": event.Action,
					"error":  err.Error(),
				})
			}
		}
	}()
}

// insert 执行一条 INSERT。
//
// 列取值逐条对应 emit.ts:70-80：target_id 以字符串落库（Node 侧也是 String(...)），
// operator_* 取已认证身份，剩余列直接取自事件（before/after 两份快照都已脱敏）。
func (a *AuditLog) insert(ctx context.Context, category string, event AuditEvent) error {
	pool, err := a.pools.Writer()
	if err != nil {
		return err
	}

	details, err := marshalAuditSnapshot(event.Details)
	if err != nil {
		return err
	}
	before, err := marshalAuditSnapshot(event.Before)
	if err != nil {
		return err
	}
	// 失败审计才带错误信息：Node 侧 errorMessage 在成功路径恒 null。
	errorMessage := event.ErrorMessage
	if event.Success {
		errorMessage = ""
	}

	const query = `INSERT INTO audit_log (
		action_category, action_type, target_type, target_id, target_name,
		before_value, after_value,
		operator_user_id, operator_user_name, operator_key_id, operator_key_name, operator_ip,
		user_agent, success, error_message
	) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8, $9, $10, $11, $12, $13, $14, $15)`

	_, err = pool.Exec(ctx, query,
		category,
		event.Action,
		nullIfEmpty(event.TargetType),
		nullIfEmpty(event.TargetID),
		nullIfEmpty(event.TargetName),
		before,
		details,
		nullIfZero(event.Principal.UserID),
		nullIfEmpty(event.Principal.Username),
		nullIfZero(event.Principal.KeyID),
		nullIfEmpty(event.Principal.KeyName),
		nullIfEmpty(event.IP),
		nullIfEmpty(event.UserAgent),
		event.Success,
		nullIfEmpty(errorMessage),
	)
	return err
}

// marshalAuditSnapshot 脱敏后序列化一份快照。
//
// 脱敏在序列化之前：脱敏函数走的是 Go 值，序列化之后的字符串再脱敏就得先解析回来（还可能
// 因为非 JSON 值失败）。失败时返回错误而不是「写个空快照」——审计宁可缺行，也不要一行尸体。
func marshalAuditSnapshot(snapshot map[string]any) (any, error) {
	if snapshot == nil {
		return nil, nil
	}
	redacted, err := json.Marshal(redactSensitive(snapshot, 0))
	if err != nil {
		return nil, fmt.Errorf("adminapi: 审计快照序列化失败: %w", err)
	}
	return redacted, nil
}

// redactSensitive 复刻 redact.ts:42-68 的 walk：键名（小写后）命中即替换为 [REDACTED]。
func redactSensitive(value any, depth int) any {
	if depth > auditMaxDepth {
		return auditCircular
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if _, sensitive := auditSensitiveKeys[strings.ToLower(key)]; sensitive {
				out[key] = auditRedacted
				continue
			}
			out[key] = redactSensitive(item, depth+1)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, redactSensitive(item, depth+1))
		}
		return out
	default:
		return value
	}
}

// resolveAuditCategory 先取显式 category，再退回按 action 前缀推导。
//
// 为什么两者都保留：显式值来自调用方（与 Node 同构），前缀表是 A0-2 在没有该字段时的推导
// 路径，删掉它会让存量调用点静默停止写审计。显式值要校验：它不是类型系统能约束的字符串。
func resolveAuditCategory(category, action string) (string, bool) {
	trimmed := strings.TrimSpace(category)
	if trimmed != "" {
		if !auditCategoryValid(trimmed) {
			return "", false
		}
		return trimmed, true
	}
	return auditCategory(action)
}

// auditCategoryValid 判断显式 category 是否在 Node 的 AuditCategory 取值域内
// （src/types/audit-log.ts:1-10）。取值域复用前缀表的值集，不再抄一遍。
func auditCategoryValid(category string) bool {
	for _, known := range auditCategoryByPrefix {
		if known == category {
			return true
		}
	}
	return false
}

// auditCategory 按 action 前缀推导 action_category。
func auditCategory(action string) (string, bool) {
	trimmed := strings.TrimSpace(action)
	if trimmed == "" {
		return "", false
	}
	prefix := trimmed
	if index := strings.IndexByte(trimmed, '.'); index >= 0 {
		prefix = trimmed[:index]
	}
	category, ok := auditCategoryByPrefix[strings.ToLower(prefix)]
	return category, ok
}

// nullIfEmpty 把空串写成 NULL（Node 侧的 `??  null` 语义）。
func nullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

// nullIfZero 把零值写成 NULL。
//
// 为什么用 0 当「无操作人」：Principal 的零值就是它，而 Node 侧对应的字段为 null。
// ADMIN_TOKEN 的虚拟用户 id / key id 是 -1，**不是** 0，因此不会被误当成缺席。
func nullIfZero(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}
