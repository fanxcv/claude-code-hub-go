package guard

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件实现错误规则匹配（对应 Node 的 error-rule-detector.ts），供转发层判定
// 「上游错误是否属于客户端输入错误」——命中即不重试、不切换供应商。
//
// 语义对齐（Node 的 detect()）：
//  1. 命中顺序固定为 contains -> exact -> regex，且任一命中即返回（顺序是可见行为：
//     它决定 provider_chain 里记下的匹配类型）。
//  2. 判定一律在**小写**消息上做；contains 的样式也在装载期小写一次。
//  3. exact 用「trim 后的小写全等」，不做子串。
//  4. regex 大小写不敏感（Node 侧 `new RegExp(pattern, "i")`）。
//
// 与 Node 的差异（有意，均不改变命中结果）：
//   - Node 用 safe-regex 拒绝 ReDoS 风险的样式后再装载；Go 的 regexp 是 RE2，
//     无回溯爆炸，故只做「能否编译」的过滤。
//   - Node 在装载期校验 override_response 形状并顺带做部分丢弃；本适配器只做匹配，
//     覆写响应属于尚未落地的缝隙（见 Assembly.Missing），故不在此解析该列。
//
// 失败语义：读库失败时沿用缓存里的旧规则（ValueCache 的既有语义），没有旧值时按
// 「无规则」处理——与 Node 冷启动规则未装载时一致（此时客户端输入错误会退化为
// 「重试并切换」，不阻断请求）。

// ErrorRule 是一条启用态错误规则（只收判定需要的列）。
type ErrorRule struct {
	ID        int64  `json:"id"`
	Pattern   string `json:"pattern"`
	MatchType string `json:"match_type"`
	Category  string `json:"category"`
}

// errorRulesQuery 取启用态错误规则。
//
// 排序 (priority, id) 升序：Node 侧装载时按该序入表，命中即第一条，顺序即行为。
const errorRulesQuery = `SELECT row_to_json(t)::text FROM (
	SELECT id, pattern, match_type, category
	FROM error_rules WHERE is_enabled = true
	ORDER BY priority ASC, id ASC
) t`

// compiledErrorRule 是装载后的规则：判定只读这两张表，不再看原始行。
type compiledErrorRule struct {
	contains []string
	exact    map[string]struct{}
	regex    []*regexp.Regexp
	pattern  []string
}

// ErrorRuleCache 是错误规则快照（域规格为事件驱动，无 TTL）。
type ErrorRuleCache struct {
	pools    *store.Pools
	cache    *cfgsync.ValueCache[*compiledErrorRule]
	logger   *logx.Logger
	registry *cfgsync.Registry
	loads    counter
}

// newErrorRuleCache 构造快照缓存。
func newErrorRuleCache(pools *store.Pools, registry *cfgsync.Registry, logger *logx.Logger) *ErrorRuleCache {
	return &ErrorRuleCache{
		pools:    pools,
		cache:    cfgsync.NewValueCache[*compiledErrorRule](0, cfgsync.WithValueCacheNoExpiry[*compiledErrorRule]()),
		logger:   logger,
		registry: registry,
	}
}

// rules 返回当前快照；永不返回 nil（装载失败时给一张空表，判定为「未命中」）。
func (c *ErrorRuleCache) rules(ctx context.Context) *compiledErrorRule {
	compiled, err := c.cache.Get(ctx, c.load, func() *compiledErrorRule { return &compiledErrorRule{} })
	if err != nil {
		c.logger.Error("guard.error_rules.load_failed", map[string]any{"error": err.Error()})
		return &compiledErrorRule{}
	}
	return compiled
}

// load 读库并编译。单条规则编译失败只丢该条：一条坏样式不该让整张表失效。
func (c *ErrorRuleCache) load(ctx context.Context) (*compiledErrorRule, error) {
	rows, err := queryJSONRows(ctx, c.pools, errorRulesQuery)
	if err != nil {
		return nil, err
	}
	compiled := &compiledErrorRule{exact: map[string]struct{}{}}
	for _, raw := range rows {
		var row ErrorRule
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("guard: 错误规则行反序列化失败: %w", err)
		}
		pattern := strings.TrimSpace(row.Pattern)
		if pattern == "" {
			continue
		}
		switch row.MatchType {
		case "contains":
			compiled.contains = append(compiled.contains, strings.ToLower(pattern))
			compiled.pattern = append(compiled.pattern, row.Pattern)
		case "exact":
			compiled.exact[strings.ToLower(pattern)] = struct{}{}
			compiled.pattern = append(compiled.pattern, row.Pattern)
		case "regex":
			re, err := regexp.Compile("(?i)" + pattern)
			if err != nil {
				c.logger.Warn("guard.error_rules.invalid_pattern", map[string]any{
					"ruleId": row.ID,
					"error":  err.Error(),
				})
				continue
			}
			compiled.regex = append(compiled.regex, re)
			compiled.pattern = append(compiled.pattern, row.Pattern)
		default:
			c.logger.Warn("guard.error_rules.unknown_match_type", map[string]any{
				"ruleId":    row.ID,
				"matchType": row.MatchType,
			})
		}
	}
	c.loads.add()
	markLoaded(c.registry, cfgsync.DomainErrorRules)
	return compiled, nil
}

// Invalidate 清空快照。
func (c *ErrorRuleCache) Invalidate() { c.cache.Invalidate() }

// Loads 返回装载次数。
func (c *ErrorRuleCache) Loads() int64 { return c.loads.get() }

// Matches 实现 forward.RuleMatcher（结构匹配即可，无需 import forward）。
func (c *ErrorRuleCache) Matches(content string) bool {
	return c.MatchesContext(context.Background(), content)
}

// MatchesContext 用调用方的 context 读快照（请求路径用这条）。
func (c *ErrorRuleCache) MatchesContext(ctx context.Context, content string) bool {
	return c.rules(ctx).matches(content)
}

// matches 是判定本体，判定顺序与 Node 的 detect() 一致：contains -> exact -> regex。
func (c *compiledErrorRule) matches(content string) bool {
	if strings.TrimSpace(content) == "" {
		return false
	}
	lower := strings.ToLower(content)
	for _, pattern := range c.contains {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	if len(c.exact) > 0 {
		if _, ok := c.exact[strings.TrimSpace(lower)]; ok {
			return true
		}
	}
	for _, re := range c.regex {
		if re.MatchString(content) {
			return true
		}
	}
	return false
}
