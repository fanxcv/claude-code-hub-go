package route

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// GroupAll 是可访问所有供应商的管理员分组标识（Node 的 PROVIDER_GROUP.ALL）。
const GroupAll = "*"

// GroupDefault 是未设置分组的 key / 供应商的默认分组标识（Node 的 PROVIDER_GROUP.DEFAULT）。
const GroupDefault = "default"

// groupSeparators 与 Node 的 /[,，\n\r]+/ 对齐：英文逗号、中文逗号、换行、回车。
func splitProviderGroups(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// resolveProviderGroupsWithDefault 与 Node 同名函数一致：空输入落回 ["default"]。
func resolveProviderGroupsWithDefault(value *string) []string {
	if value == nil {
		return []string{GroupDefault}
	}
	groups := splitProviderGroups(*value)
	if len(groups) == 0 {
		return []string{GroupDefault}
	}
	return groups
}

// checkProviderGroupMatch 复刻 Node 的 checkProviderGroupMatch：
// 用户分组含 "*" 即全通过；否则按「供应商标签（缺省 default）」与「用户分组」求交集。
func checkProviderGroupMatch(providerGroupTag *string, userGroups string) bool {
	return matchProviderGroups(providerGroupTag, splitProviderGroups(userGroups))
}

// matchProviderGroups 是分组匹配的**唯一实现**：数据面（逗号分隔的字符串）与管理面
// （`/providers/model-suggestions` 直接拿到 []string）共用它。
//
// 之所以不让管理面自己再写一遍：Node 侧同名函数在 `actions/providers.ts:5656` 与
// `provider-selector.ts:74` 本就有两份，Go 侧再抄第三份就是制造漂移源
func matchProviderGroups(providerGroupTag *string, groups []string) bool {
	for _, group := range groups {
		if group == GroupAll {
			return true
		}
	}
	providerTags := resolveProviderGroupsWithDefault(providerGroupTag)
	for _, tag := range providerTags {
		for _, group := range groups {
			if tag == group {
				return true
			}
		}
	}
	return false
}

// resolveEffectivePriority 解析某供应商在给定用户组下的**分层优先级**，并叠加低速降权。
//
// 语义：分组覆盖存在时取「匹配分组中的最小优先级」，否则回退 priority 列（缺省 0）。数值小 = 更优先。
// 然后加上 penalties 给该渠道的降权值（见 penaltyTable）。
//
// **为什么降权必须加在本函数内部**（不变量，改动前先读这段）：本函数是全仓分层值的**唯一漏斗**，
// 非测试调用点二十余处（`select.go` 的亲和短路、SelectedPriority、候选投影、selectTopPriority、
// priorityLevels，以及 `simulate.go` 的模拟预览）。排序、留痕、模拟三侧全部经它取数，
// 所以在这里加 = 一次覆盖全部读数；在外面包一层新函数则那些调用点仍走旧漏斗，
// 读数与实际选路分叉（SelectedPriority 记的档位与实际排序不一致，界面归因就错了）。
//
// **降权只作用于分组覆盖之后**：penalized 的入参是 baseEffectivePriority 的返回值，构造上不可能提前。
// 若直接在 provider.Priority 列上减，有分组覆盖时那一列根本不参与排序，降权会被覆盖吃掉、等于不生效。
//
// 与 Node 的**一处有意偏离**（2026-09-14）：覆盖键必须让**该供应商自己也在那个组里**才生效。
// Node 只校验「用户在 g 组」且「provider.group_priorities 里有 g 键」，于是把供应商从分组摘除时，
// 遗留的覆盖值会继续生效——`group_tag` 已不含 g，它却仍按 g 的覆盖值参与该组的分层。
// 生产实证：7 家存在这种越界覆盖且全是 `fan` 键（99/115 覆盖 2；154/155/156/161/163 覆盖 0），
// 其中 163 的 tag 是 `chat,CC-Paid,codex`——它根本不在 `fan` 组，却因 `{"fan":0}` 在 `fan` 组的
// 请求里拿到最高档 0，把「把供应商移出分组」这个动作直接抵消。
//
// 判定复用 matchProviderGroups（分组过滤用的同一真源），故「算不算在这个组里」与「能不能进候选」
// 不可能分叉。Node 已退役，对齐它不再是硬约束，组内语义自洽优先；本偏离已登记在
// 容错：userGroup 为空、覆盖表为空（jsonb null 解出来即 nil）、覆盖表里没有匹配键，一律回退
// priority 列，不 panic——覆盖表是历史脏数据的常见栖身处（列无写入侧校验）。

// penaltyTable 是本次请求的「渠道 -> 低速降权值」表（providerID -> 降权量）。
//
// 为何是 map 而不是每个调用点自己传一个 int：同一场选路里，排序、留痕、模拟都要求
// 「同一渠道得到同一个降权值」。若每个调用点各自取数，一处读得 10、另一处读得 0，
// 就会出现「排序按降权后的档位、留痕却记降权前的档位」这类静默分叉。
// 由调用方算一次、往下传，是让「同一事实只有一个来源」在类型层成立。
//
// nil 与空表同义：不给任何渠道降权（未开启监控时恒为此）。
type penaltyTable map[int64]int

// penalized 把降权叠加到分层值上。nil 表、无该渠道、非正值一律原样返回——
// 降权是**单向**的（只能让渠道更不优先），不允许出现负值把渠道抬上去。
func (t penaltyTable) penalized(base int, providerID int64) int {
	penalty, ok := t[providerID]
	if !ok || penalty <= 0 {
		return base
	}
	return base + penalty
}

func resolveEffectivePriority(provider Provider, userGroup string, penalties penaltyTable) int {
	return penalties.penalized(baseEffectivePriority(provider, userGroup), provider.ID)
}

// baseEffectivePriority 是**未叠加低速降权**的分层值（分组覆盖后的结果）。
//
// 拆出它不是为了多一个漏斗（那正是要避免的分叉），而是为了让「降权加在覆盖**之后**」这条规则
// 在**类型层**成立：penalized 只可能作用在覆盖完毕的返回值上，调用方无从绕过。
func baseEffectivePriority(provider Provider, userGroup string) int {
	if userGroup == "" || len(provider.GroupPriorities) == 0 {
		return provider.EffectivePriority()
	}
	lowest := 0
	found := false
	for _, group := range splitProviderGroups(userGroup) {
		override, ok := provider.GroupPriorities[group]
		if !ok {
			continue
		}
		// 覆盖只对「供应商自己也在这个组」的组生效：否则摘除分组被遗留覆盖值抵消。
		if !matchProviderGroups(provider.GroupTag, []string{group}) {
			continue
		}
		if !found || override < lowest {
			lowest = override
			found = true
		}
	}
	if found {
		return lowest
	}
	return provider.EffectivePriority()
}

// allowedModelRule 是 normalizeAllowedModelRule 的 Go 投影。
type allowedModelRule struct {
	MatchType string
	Pattern   string
}

var allowedModelMatchTypes = map[string]bool{
	"exact": true, "prefix": true, "suffix": true, "contains": true, "regex": true,
}

// normalizeAllowedModelRules 复刻 Node 的同名函数：
// 非数组或 null 视为「无规则（放行）」；数组内非法项被丢弃；全部非法同样视为放行。
func normalizeAllowedModelRules(raw json.RawMessage) ([]allowedModelRule, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	var items []any
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	out := make([]allowedModelRule, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			pattern := strings.TrimSpace(text)
			if pattern == "" {
				continue
			}
			out = append(out, allowedModelRule{MatchType: "exact", Pattern: pattern})
			continue
		}
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		matchType, _ := record["matchType"].(string)
		pattern, _ := record["pattern"].(string)
		pattern = strings.TrimSpace(pattern)
		if !allowedModelMatchTypes[matchType] || pattern == "" {
			continue
		}
		out = append(out, allowedModelRule{MatchType: matchType, Pattern: pattern})
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// providerSupportsModel 复刻 Node 的 providerSupportsModel：未设允许集即放行，否则按规则命中放行。
func providerSupportsModel(provider Provider, requestedModel string) bool {
	rules, ok := normalizeAllowedModelRules(provider.AllowedModels)
	if !ok {
		return true
	}
	for _, rule := range rules {
		if matchesPattern(requestedModel, rule.MatchType, rule.Pattern) {
			return true
		}
	}
	return false
}

// matchesPattern 复刻 src/lib/model-pattern-matcher.ts；
// regex 解析失败时回退 glob（`*` → `.*`、`?` → `.`，两端补锚点）。
func matchesPattern(model, matchType, pattern string) bool {
	switch matchType {
	case "exact":
		return model == pattern
	case "prefix":
		return strings.HasPrefix(model, pattern)
	case "suffix":
		return strings.HasSuffix(model, pattern)
	case "contains":
		return strings.Contains(model, pattern)
	case "regex":
		if re, ok := compileProviderPattern(pattern); ok {
			return re.MatchString(model)
		}
		return false
	default:
		return false
	}
}

var (
	patternCacheMu sync.Mutex
	patternCache   = map[string]*regexp.Regexp{}

	// patternCacheLimit 与 Node 侧 CACHE_LIMIT 对齐：超限则整体清空。
	patternCacheLimit = 1024

	globMeta  = regexp.MustCompile(`[*?]`)
	regexMeta = regexp.MustCompile(`[\\^$.|()[\]{}+]`)
	globStar  = regexp.MustCompile(`\*`)
	globQuest = regexp.MustCompile(`\?`)
)

// compileProviderPattern 复刻 resolveProviderPatternRegex：先按标准正则解析，
// 失败且含 glob 元字符时，把元字符转义后按 glob 语义整串匹配。
//
// 已知差异：Go 的 regexp 是 RE2，不支持回溯引用与环视；JS 能编译这两类的模式在 Go 侧
// 会走 glob 回退或返回不匹配。允许集规则里出现这类模式属罕见路径，此处不额外兼容。
func compileProviderPattern(pattern string) (*regexp.Regexp, bool) {
	patternCacheMu.Lock()
	defer patternCacheMu.Unlock()
	if cached, ok := patternCache[pattern]; ok {
		return cached, cached != nil
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil && globMeta.MatchString(pattern) {
		source := "^" + globQuest.ReplaceAllString(
			globStar.ReplaceAllString(regexMeta.ReplaceAllString(pattern, `\$0`), ".*"), ".") + "$"
		compiled, err = regexp.Compile(source)
	}
	if err != nil {
		patternCache[pattern] = nil
		return nil, false
	}
	if len(patternCache) >= patternCacheLimit {
		patternCache = make(map[string]*regexp.Regexp)
	}
	patternCache[pattern] = compiled
	return compiled, true
}
