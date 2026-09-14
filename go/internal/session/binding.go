package session

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// 绑定状态机：复刻 src/lib/redis/session-binding.ts 的 Lua 驱动部分。
//
// 语义要点（与 Node 一致）：
//   - 规范键（canonical）只由 Lua 脚本维护；脚本缺失等「能力不可用」情形按 Node 的
//     unavailable 处理，调用方自行决定 legacy 回退（本包只提供版本化路径）。
//   - 每条命令都要 owner/gen 校验：generation 不匹配或 owner 不是当前 key 即 conflict，
//     这正是「generation fence 拒绝迟到写入」的落点。
//   - 冲突原因取值与 Node 的 conflict reason 逐字一致。

// BindingSource 是绑定结果的来源（Node 侧 SessionBindingOkResult.source）。
type BindingSource string

const (
	SourceCreated        BindingSource = "created"
	SourceExisting       BindingSource = "existing"
	SourceLegacyUpgraded BindingSource = "legacy_upgraded"
	SourceUpdated        BindingSource = "updated"
	SourceCleared        BindingSource = "cleared"
	SourceTouched        BindingSource = "touched"
	SourceTerminated     BindingSource = "terminated"
)

// BindingSnapshot 是绑定成功后的快照。
type BindingSnapshot struct {
	SessionID  string
	KeyID      int64
	Generation string
	// ProviderID 为 0 表示空绑定（脚本返回空字符串）。Lua 里空绑定 = 无 provider。
	ProviderID int64
}

// BindingResult 是绑定命令的结果。
type BindingResult struct {
	// OK 为 true 表示脚本返回 ok；否则 ConflictReason 有值。
	OK             bool
	Source         BindingSource
	Snapshot       BindingSnapshot
	ConflictReason string
}

// bindingAllowedSources 是每条命令允许的 ok source 集合（Node 侧 READ_SOURCES 等）。
func bindingAllowedSources(srcs ...BindingSource) map[BindingSource]bool {
	set := make(map[BindingSource]bool, len(srcs))
	for _, s := range srcs {
		set[s] = true
	}
	return set
}

// knownConflictReasons 与 Node 的 CONFLICT_REASONS 一致；未收录的一律记 unknown_conflict。
var knownConflictReasons = map[string]bool{
	"canonical_corrupt":       true,
	"canonical_exists":        true,
	"canonical_key_mismatch":  true,
	"canonical_missing":       true,
	"foreign_legacy_owner":    true,
	"generation_mismatch":     true,
	"invalid_input":           true,
	"invalid_legacy_provider": true,
	"lease_held":              true,
	"mirror_conflict":         true,
	"mirror_missing":          true,
	"not_owner_or_missing":    true,
	"orphan_legacy_provider":  true,
	"provider_mismatch":       true,
	"unknown_conflict":        true,
}

// Client 是绑定状态机的 Redis 访问层：EvalConst 由 ratelimit.Client 提供。
//
// 每个方法一次脚本调用，不额外往返。
type Client struct {
	rc *ratelimit.Client
}

// NewClient 组装绑定访问层。rc 不能为 nil。
func NewClient(rc *ratelimit.Client) *Client {
	return &Client{rc: rc}
}

// ErrNilClient 表示脚本调用层未装配。
var ErrNilClient = errors.New("session.binding: 缺少 ratelimit.Client")

// eval 执行脚本并把回复归一为字符串数组（Node 的 normalizeEvalResult）。
func (c *Client) eval(ctx context.Context, constName string, keys []string, argv []any) ([]string, error) {
	if c == nil || c.rc == nil {
		return nil, ErrNilClient
	}
	raw, err := c.rc.EvalConst(ctx, constName, keys, argv)
	if err != nil {
		return nil, err
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("session.binding: %s 结果不是数组", constName)
	}
	values := make([]string, len(arr))
	for i, v := range arr {
		values[i] = evalString(v)
	}
	return values, nil
}

// evalInt 执行脚本并把整数回复归一为 int64。
//
// 租约的续期/释放脚本用 EXPIRE / DEL 的返回值作答，是整数而非数组，因此不能走 eval 的
// 数组解析：那会把「续期成功」误报成调用层错误。
func (c *Client) evalInt(ctx context.Context, constName string, keys []string, argv []any) (int64, error) {
	if c == nil || c.rc == nil {
		return 0, ErrNilClient
	}
	raw, err := c.rc.EvalConst(ctx, constName, keys, argv)
	if err != nil {
		return 0, err
	}
	switch value := raw.(type) {
	case int64:
		return value, nil
	case string:
		parsed, parseErr := strconv.ParseInt(value, 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("session.binding: %s 结果不是整数", constName)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("session.binding: %s 结果不是整数", constName)
	}
}

// Binder 是把绑定/租约脚本语义合并成一个命令集的门面：守卫链适配与测试共用。
type Binder struct {
	client *Client
}

// NewBinder 组装绑定门面。
func NewBinder(rc *ratelimit.Client) *Binder {
	return &Binder{client: NewClient(rc)}
}

// Ready 报告脚本调用层是否可用（nil 视为未准备好）。
func (b *Binder) Ready() bool {
	return b != nil && b.client != nil && b.client.rc != nil
}

// validInput 是绑定输入预检：keyId 与 TTL 必须为正整数。
func validInput(keyID int64, ttlSeconds int) bool {
	return keyID > 0 && ttlSeconds > 0
}

// ReadOrReconcile 复刻 readOrReconcileSessionBinding。
//
// 幂等读取：规范键存在则续期并返回 existing；不存在且 legacy 镜像干净则创建
// （source=created）或升级（source=legacy_upgraded）。
func (b *Binder) ReadOrReconcile(ctx context.Context, sessionID string, keyID int64, ttlSeconds int) (BindingResult, error) {
	if !b.Ready() {
		return BindingResult{}, ErrNilClient
	}
	if !validInput(keyID, ttlSeconds) {
		return BindingResult{ConflictReason: "invalid_input"}, nil
	}
	keys := BuildBindingKeys(sessionID, keyID)
	values, err := b.client.eval(ctx, "READ_OR_RECONCILE_SESSION_BINDING", []string{
		keys.Canonical, keys.LegacyProvider, keys.LegacyOwner,
	}, []any{
		strconv.FormatInt(keyID, 10),
		GenerateSessionID(), // Node 用 randomUUID() 做新 generation
		strconv.Itoa(ttlSeconds),
	})
	if err != nil {
		return BindingResult{}, err
	}
	return parseStringsResult(sessionID, keyID, values,
		bindingAllowedSources(SourceCreated, SourceExisting, SourceLegacyUpgraded))
}

// CompareAndSet 复刻 compareAndSetSessionBinding。
//
// 只有 expectedGeneration 与规范键一致才更新 provider（generation fence）。
func (b *Binder) CompareAndSet(
	ctx context.Context, sessionID string, keyID int64,
	expectedGeneration string, providerID int64, ttlSeconds int,
) (BindingResult, error) {
	if !b.Ready() {
		return BindingResult{}, ErrNilClient
	}
	if !validInput(keyID, ttlSeconds) || expectedGeneration == "" || providerID <= 0 {
		return BindingResult{ConflictReason: "invalid_input"}, nil
	}
	keys := BuildBindingKeys(sessionID, keyID)
	values, err := b.client.eval(ctx, "CAS_SESSION_BINDING", []string{
		keys.Canonical, keys.LegacyProvider, keys.LegacyOwner,
	}, []any{
		strconv.FormatInt(keyID, 10),
		expectedGeneration,
		GenerateSessionID(),
		strconv.FormatInt(providerID, 10),
		strconv.Itoa(ttlSeconds),
	})
	if err != nil {
		return BindingResult{}, err
	}
	return parseStringsResult(sessionID, keyID, values, bindingAllowedSources(SourceUpdated))
}

// Touch 复刻 touchSessionBinding：只续 TTL，不旋转 generation。
//
// expectedProviderID 为 0 表示期望空绑定。
func (b *Binder) Touch(
	ctx context.Context, sessionID string, keyID int64,
	expectedGeneration string, expectedProviderID int64, ttlSeconds int,
) (BindingResult, error) {
	if !b.Ready() {
		return BindingResult{}, ErrNilClient
	}
	if !validInput(keyID, ttlSeconds) || expectedGeneration == "" {
		return BindingResult{ConflictReason: "invalid_input"}, nil
	}
	keys := BuildBindingKeys(sessionID, keyID)
	providerArg := ""
	if expectedProviderID > 0 {
		providerArg = strconv.FormatInt(expectedProviderID, 10)
	}
	values, err := b.client.eval(ctx, "TOUCH_SESSION_BINDING", []string{
		keys.Canonical, keys.LegacyProvider, keys.LegacyOwner,
	}, []any{
		strconv.FormatInt(keyID, 10),
		expectedGeneration,
		providerArg,
		strconv.Itoa(ttlSeconds),
	})
	if err != nil {
		return BindingResult{}, err
	}
	return parseStringsResult(sessionID, keyID, values, bindingAllowedSources(SourceTouched))
}

// Clear 复刻 clearSessionBinding：清掉 provider 绑定；可选写供应商冷却。
//
// expectedProviderID 为 0 表示期望空绑定；cooldownProviderID 为 0 表示不写冷却。
func (b *Binder) Clear(
	ctx context.Context, sessionID string, keyID int64,
	expectedGeneration string, expectedProviderID int64,
	cooldownProviderID int64, cooldownTTLSeconds int, ttlSeconds int,
) (BindingResult, error) {
	if !b.Ready() {
		return BindingResult{}, ErrNilClient
	}
	if !validInput(keyID, ttlSeconds) || expectedGeneration == "" ||
		cooldownTTLSeconds < 0 || (cooldownTTLSeconds > 0 && expectedProviderID <= 0) {
		return BindingResult{ConflictReason: "invalid_input"}, nil
	}
	keys := BuildBindingKeys(sessionID, keyID)
	expectedProviderArg := ""
	if expectedProviderID > 0 {
		expectedProviderArg = strconv.FormatInt(expectedProviderID, 10)
	}
	cooldownKey := keys.Canonical
	if expectedProviderID > 0 {
		cooldownKey = ProviderCooldownKey(sessionID, keyID, expectedProviderID)
	}
	cooldownProviderArg := ""
	if cooldownTTLSeconds > 0 {
		cooldownProviderArg = expectedProviderArg
	}
	values, err := b.client.eval(ctx, "CLEAR_SESSION_BINDING", []string{
		keys.Canonical, keys.LegacyProvider, keys.LegacyOwner, cooldownKey,
	}, []any{
		strconv.FormatInt(keyID, 10),
		expectedGeneration,
		GenerateSessionID(),
		expectedProviderArg,
		strconv.Itoa(ttlSeconds),
		cooldownProviderArg,
		strconv.Itoa(cooldownTTLSeconds),
	})
	if err != nil {
		return BindingResult{}, err
	}
	return parseStringsResult(sessionID, keyID, values, bindingAllowedSources(SourceCleared))
}

// Terminate 复刻 terminateSessionBinding：终结合话绑定（不写冷却）。
//
// expectedProviderID 为 0 表示不校验期望供应商。
func (b *Binder) Terminate(
	ctx context.Context, sessionID string, keyID int64,
	expectedProviderID int64, ttlSeconds int,
) (BindingResult, error) {
	if !b.Ready() {
		return BindingResult{}, ErrNilClient
	}
	if !validInput(keyID, ttlSeconds) {
		return BindingResult{ConflictReason: "invalid_input"}, nil
	}
	keys := BuildBindingKeys(sessionID, keyID)
	providerArg := ""
	if expectedProviderID > 0 {
		providerArg = strconv.FormatInt(expectedProviderID, 10)
	}
	values, err := b.client.eval(ctx, "TERMINATE_SESSION_BINDING", []string{
		keys.Canonical, keys.LegacyProvider, keys.LegacyOwner,
	}, []any{
		strconv.FormatInt(keyID, 10),
		GenerateSessionID(),
		strconv.Itoa(ttlSeconds),
		providerArg,
	})
	if err != nil {
		return BindingResult{}, err
	}
	return parseStringsResult(sessionID, keyID, values, bindingAllowedSources(SourceTerminated))
}

// parseStringsResult 复刻 Node 的 parseBindingResult（输入已归一为字符串数组）。
//
// Lua 回复形态：[ok|conflict, source|reason, generation, providerIdOrEmpty]。
func parseStringsResult(
	sessionID string, keyID int64, values []string, allowed map[BindingSource]bool,
) (BindingResult, error) {
	if len(values) < 2 {
		return BindingResult{}, fmt.Errorf("session.binding: Lua 结果不足: %v", values)
	}
	if values[0] == "conflict" {
		reason := values[1]
		if !knownConflictReasons[reason] {
			reason = "unknown_conflict"
		}
		return BindingResult{ConflictReason: reason}, nil
	}
	if values[0] != "ok" || len(values) < 4 {
		return BindingResult{}, fmt.Errorf("session.binding: 意外的 Lua 结果: %v", values)
	}
	source := BindingSource(values[1])
	if !allowed[source] {
		return BindingResult{}, fmt.Errorf("session.binding: 来源不在允许集合: %s", source)
	}
	generation := values[2]
	if generation == "" {
		return BindingResult{}, fmt.Errorf("session.binding: 结果缺少 generation")
	}
	providerID := int64(0)
	if isPositiveIntegerString(values[3]) {
		providerID, _ = strconv.ParseInt(values[3], 10, 64)
	}
	return BindingResult{
		OK:     true,
		Source: source,
		Snapshot: BindingSnapshot{
			SessionID:  sessionID,
			KeyID:      keyID,
			Generation: generation,
			ProviderID: providerID,
		},
	}, nil
}

// evalString 把 Lua 返回的字符串/数字/Buffer 归一到 string。
func evalString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int64:
		return strconv.FormatInt(t, 10)
	case []byte:
		return string(t)
	default:
		return ""
	}
}

// isPositiveIntegerString 判断十进制正整数串。
func isPositiveIntegerString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
