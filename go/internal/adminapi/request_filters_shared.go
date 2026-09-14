package adminapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 本文件是 request-filters 模块的入参业务校验（src/actions/request-filters.ts 的 validatePayload
// 与 validateOperations 的移植）与供应商分组选项的组装。
//
// 为什么放在 action 语义的位置而不是 store：这些规则产生的是 400 与中文提示文案，属于 action
// 层；store 层只管 SQL。两处调用的顺序也与 Node 一致（路由 schema 校验 → action 校验 → 落库）。
//
// 差异（登记进对拍白名单）：Node 用 safe-regex 挡 ReDoS 样式，Go 的 RE2 无回溯爆炸，故只做
// 「能否编译」。代价是 `(a+)+` 这类样式 Node 拒（400）而 Go 放行；反向地，环视/回溯引用 Node
// 放行而 Go 拒。

// rfMaxOperations 与 actions/request-filters.ts:41 的 MAX_OPERATIONS 一致。
const rfMaxOperations = 50

// rfDefaultGroup 与 PROVIDER_GROUP.DEFAULT（src/lib/constants/provider.constants.ts:37）一致。
const rfDefaultGroup = "default"

// rfUnsafePathKeys 与 actions/request-filters.ts:38 的 VALIDATION_UNSAFE_KEYS 逐字一致：
// 原型污染防护——path 里出现 __proto__ / constructor / prototype 即拒。
var rfUnsafePathKeys = regexp.MustCompile(`(?:^|[.[])(?:__proto__|constructor|prototype)(?:[.[\]]|$)`)

// rfPayload 是 validatePayload 的入参（对应 Node 的同名函数参数）。
//
// 指针字段表示「请求里是否出现」：Node 侧用 `?? 默认值`，故 nil 与空值不同。
type rfPayload struct {
	Name        string
	Scope       string
	Action      string
	Target      string
	MatchType   *string
	BindingType *string
	ProviderIDs json.RawMessage
	GroupTags   json.RawMessage
	RuleMode    *string
	Operations  json.RawMessage
}

// rfValidatePayload 复刻 validatePayload：返回 "" 表示通过，否则是 400 的 error 文案。
func rfValidatePayload(payload rfPayload) string {
	if strings.TrimSpace(payload.Name) == "" {
		return "名称不能为空"
	}

	ruleMode := "simple"
	if payload.RuleMode != nil {
		ruleMode = *payload.RuleMode
	}

	if ruleMode == "advanced" {
		// 高级模式：校验 operations，跳过 simple 的那几个字段。
		if reason := rfValidateOperations(payload.Operations); reason != "" {
			return reason
		}
	} else {
		if strings.TrimSpace(payload.Target) == "" {
			return "目标字段不能为空"
		}
		if payload.Action == "text_replace" && payload.MatchType != nil &&
			*payload.MatchType == "regex" && payload.Target != "" {
			if _, err := regexp.Compile(payload.Target); err != nil {
				return "正则表达式存在 ReDoS 风险"
			}
		}
	}

	// 绑定类型的互斥约束。
	bindingType := "global"
	if payload.BindingType != nil {
		bindingType = *payload.BindingType
	}
	providers := rfArrayLength(payload.ProviderIDs)
	groups := rfArrayLength(payload.GroupTags)
	switch bindingType {
	case "providers":
		if providers == 0 {
			return "至少选择一个 Provider"
		}
		if groups > 0 {
			return "不能同时选择 Providers 和 Groups"
		}
	case "groups":
		if groups == 0 {
			return "至少选择一个 Group Tag"
		}
		if providers > 0 {
			return "不能同时选择 Providers 和 Groups"
		}
	case "global":
		if providers > 0 || groups > 0 {
			return "Global 类型不能指定 Providers 或 Groups"
		}
	}
	return ""
}

// rfValidateOperations 复刻 validateOperations：逐条检查高级模式的 operations 数组。
func rfValidateOperations(operations json.RawMessage) string {
	items, ok := a14JSONArray(operations)
	if !ok || len(items) == 0 {
		return "Advanced mode requires at least one operation"
	}
	if len(items) > rfMaxOperations {
		return fmt.Sprintf("Operations array must not exceed %d entries", rfMaxOperations)
	}

	for index, item := range items {
		prefix := fmt.Sprintf("operations[%d]", index)
		fields, ok := a14JSONArrayElement(item)
		if !ok {
			return prefix + ": must be an object"
		}

		op, _ := a14RawString(fields, "op")
		if op != "set" && op != "remove" && op != "merge" && op != "insert" {
			return fmt.Sprintf("%s: invalid op type \"%s\"", prefix, rfRawText(fields["op"]))
		}
		scope, _ := a14RawString(fields, "scope")
		if scope != "header" && scope != "body" {
			return fmt.Sprintf("%s: invalid scope \"%s\"", prefix, rfRawText(fields["scope"]))
		}
		if (op == "merge" || op == "insert") && scope != "body" {
			return fmt.Sprintf("%s: %s operation only supports body scope", prefix, op)
		}

		pathRaw, hasPath := fields["path"]
		path, pathIsString := a14JSONString(pathRaw)
		if !hasPath || !pathIsString || strings.TrimSpace(path) == "" {
			return prefix + ": path is required"
		}
		if rfUnsafePathKeys.MatchString(path) {
			return prefix + ": path contains a forbidden property name"
		}

		switch op {
		case "set":
			if _, present := fields["value"]; !present {
				return prefix + ": value is required for set"
			}
		case "merge":
			value, present := fields["value"]
			if !present || a14IsJSONNull(value) || adminJSONTypeName(value) != "object" {
				return prefix + ": merge value must be a plain object"
			}
		case "insert":
			if _, present := fields["value"]; !present {
				return prefix + ": value is required for insert"
			}
			position, _ := a14RawString(fields, "position")
			anchorRaw, hasAnchor := fields["anchor"]
			if (position == "before" || position == "after") && (!hasAnchor || a14IsJSONNull(anchorRaw)) {
				return fmt.Sprintf("%s: anchor is required when position is \"%s\"", prefix, position)
			}
			if hasAnchor && !a14IsJSONNull(anchorRaw) {
				if reason := rfValidateMatcher(anchorRaw, prefix+".anchor"); reason != "" {
					return reason
				}
			}
			if dedupe, ok := a14JSONArrayElement(fields["dedupe"]); ok {
				if byFields, present := dedupe["byFields"]; present {
					if adminJSONTypeName(byFields) != "array" {
						return prefix + ": dedupe.byFields must be an array"
					}
				}
			}
		case "remove":
			if matcher, present := fields["matcher"]; present && !a14IsJSONNull(matcher) {
				if reason := rfValidateMatcher(matcher, prefix+".matcher"); reason != "" {
					return reason
				}
			}
		}
	}
	return ""
}

// rfValidateMatcher 复刻 validateMatcher：regex 匹配器的样式必须能编译。
func rfValidateMatcher(matcher json.RawMessage, context string) string {
	fields, ok := a14JSONArrayElement(matcher)
	if !ok {
		return ""
	}
	matchType, _ := a14RawString(fields, "matchType")
	if matchType != "regex" {
		return ""
	}
	value, isString := a14JSONString(fields["value"])
	if !isString {
		return ""
	}
	if _, err := regexp.Compile(value); err != nil {
		return context + ": regex matcher has ReDoS risk"
	}
	return ""
}

// a14JSONArrayElement 把一个 JSON 值解成对象字段表；非对象返回 (nil, false)。
func a14JSONArrayElement(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	if adminJSONTypeName(raw) != "object" {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, false
	}
	return fields, true
}

// rfRawText 复刻 JS 的 `String(value)`（用于把非法 op/scope 的原文放进报错文案）。
func rfRawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "undefined"
	}
	if value, ok := a14JSONString(raw); ok {
		return value
	}
	return string(raw)
}

// rfArrayLength 返回数组字段的长度；非数组 / 未出现 / null 一律 0（Node 侧 `?.length` 的语义）。
func rfArrayLength(raw json.RawMessage) int {
	if len(raw) == 0 || a14IsJSONNull(raw) {
		return 0
	}
	items, ok := a14JSONArray(raw)
	if !ok {
		return 0
	}
	return len(items)
}

// rfValidateAdvancedPhase 复刻「高级模式只支持 final 阶段」的检查。
//
// 创建与更新两处都调用它，且都取**生效后**的 ruleMode / executionPhase。
func rfValidateAdvancedPhase(ruleMode, executionPhase string) string {
	if ruleMode == "advanced" && executionPhase == "guard" {
		return "Advanced mode filters only support final execution phase"
	}
	return ""
}

// rfGroupOptions 复刻 getDistinctProviderGroupsAction 的展开与排序。
//
// 展开语义来自 resolveProviderGroupsWithDefault：NULL / 空串落回 ["default"]。排序语义来自
// `sort((a, b) => a === DEFAULT ? -1 : b === DEFAULT ? 1 : a.localeCompare(b))`——
// default 恒在最前，其余按 JS 的 localeCompare。
//
// 差异（登记进对拍白名单）：localeCompare 是 ICU 的本地化比较，Go 侧用按码点的比较。对纯 ASCII
// 分组名两者一致；含大小写混排或非 ASCII 时顺序可能不同（分组名不参与任何判定，只影响展示顺序）。
func rfGroupOptions(tags []*string) []string {
	unique := map[string]struct{}{rfDefaultGroup: {}}
	for _, tag := range tags {
		for _, group := range rfResolveGroupsWithDefault(tag) {
			if group != "" {
				unique[group] = struct{}{}
			}
		}
	}
	options := make([]string, 0, len(unique))
	for group := range unique {
		options = append(options, group)
	}
	sort.Slice(options, func(i, j int) bool {
		if options[i] == rfDefaultGroup {
			return true
		}
		if options[j] == rfDefaultGroup {
			return false
		}
		return options[i] < options[j]
	})
	return options
}

// rfResolveGroupsWithDefault 与 Node 的同名函数一致：空输入落回 ["default"]。
func rfResolveGroupsWithDefault(value *string) []string {
	if value == nil {
		return []string{rfDefaultGroup}
	}
	groups := rfSplitGroups(*value)
	if len(groups) == 0 {
		return []string{rfDefaultGroup}
	}
	return groups
}

// rfSplitGroups 按英文逗号、中文逗号、换行、回车切分并去掉空段。
func rfSplitGroups(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// rfMatchTypes / rfScopes / rfActions / rfBindingTypes / rfRuleModes / rfExecutionPhases 逐字取自
// src/lib/api/v1/schemas/request-filters.ts 的各 enum。
var (
	rfScopes          = []string{"header", "body"}
	rfActions         = []string{"remove", "set", "json_path", "text_replace"}
	rfMatchTypes      = []string{"regex", "contains", "exact"}
	rfBindingTypes    = []string{"global", "providers", "groups"}
	rfRuleModes       = []string{"simple", "advanced"}
	rfExecutionPhases = []string{"guard", "final"}
)
