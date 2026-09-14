package guard

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件复刻 request-filter-engine.ts 的守卫相（guard phase）。
//
// 范围：规则求值、路径读写、深合并、文本替换，以及把规则作用到 headers 与请求正文。
// 不做的：规则加载与热更新（属 cfgsync 侧）、final 相（属转发器侧，那儿才有上游改写后的
// 最终正文）。
//
// 已知差异（有意，且必须知晓）：
//   - Node 用 safe-regex 预检正则防回溯爆炸；Go 的 regexp 是 RE2，无回溯爆炸，故不预检。
//   - 因此 Node 合法而 RE2 不支持的语法（后顾断言、反向引用）在 Go 侧编译失败，按「不匹配」
//     处理并留一条 warn。这类规则在迁移期会表现为过滤器静默失效，故必须记日志而不是静默。

// unsafeKeys 是不可被路径穿越的键，防原型污染。
var unsafeKeys = map[string]bool{
	"__proto__":   true,
	"constructor": true,
	"prototype":   true,
}

// filterMatcher 是 insert 的锚点与 remove 的元素匹配条件。
type filterMatcher struct {
	Field     string
	Value     any
	MatchType string
}

// filterOperation 是 advanced 模式的一条操作（JSON 原样解释）。
type filterOperation struct {
	Op              string         `json:"op"`
	Scope           string         `json:"scope"`
	Path            string         `json:"path"`
	Value           any            `json:"value"`
	WriteMode       string         `json:"writeMode"`
	Matcher         *filterMatcher `json:"matcher"`
	Position        string         `json:"position"`
	Anchor          *filterMatcher `json:"anchor"`
	OnAnchorMissing string         `json:"onAnchorMissing"`
	Dedupe          *filterDedupe  `json:"dedupe"`
}

type filterDedupe struct {
	Enabled  *bool    `json:"enabled"`
	ByFields []string `json:"byFields"`
}

// pathSegment 是路径的一段：字符串键或数组下标。
type pathSegment struct {
	key   string
	index int
	isIdx bool
}

// parseFilterPath 复刻 parsePath：非法的路径（空、含危险键）返回 nil。
func parseFilterPath(path string) []pathSegment {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	segments := make([]pathSegment, 0, 4)
	index := 0
	for index < len(path) {
		switch {
		case path[index] == '.':
			index++
		case path[index] == '[':
			end := strings.IndexByte(path[index:], ']')
			if end < 0 {
				return segments
			}
			digits := path[index+1 : index+end]
			parsed, err := strconv.Atoi(digits)
			if err != nil {
				// 非数字下标（如 ["key"]）不是 TS 支持的语法，跳到下一个字符继续扫描。
				index += end + 1
				continue
			}
			segments = append(segments, pathSegment{index: parsed, isIdx: true})
			index += end + 1
		default:
			end := index
			for end < len(path) && path[end] != '.' && path[end] != '[' {
				end++
			}
			key := path[index:end]
			if unsafeKeys[key] {
				// 整条路径作废，与 TS 行为一致。
				return nil
			}
			segments = append(segments, pathSegment{key: key})
			index = end
		}
	}
	return segments
}

// setValueByPath 复刻 setValueByPath：沿途缺啥补啥，容器类型不符就整体替换。
//
// 为什么写得比 TS 长：JS 的对象与数组是引用类型，`current[key] = x` 直接生效；Go 的切片是
// 头值，越界 append 后必须写回父容器才不会丢失。writeBack 闭包就是这次写回。
func setValueByPath(root map[string]any, path string, value any) {
	segments := parseFilterPath(path)
	if len(segments) == 0 || segments[0].isIdx {
		// 根恒为对象，首段是数组下标时无处安放，与 TS 一样静默放弃。
		return
	}

	var current any = root
	var writeBack func(any)

	for index, segment := range segments {
		last := index == len(segments)-1
		var next pathSegment
		if !last {
			next = segments[index+1]
		}

		switch node := current.(type) {
		case map[string]any:
			if segment.isIdx {
				return
			}
			if last {
				node[segment.key] = value
				return
			}
			child, exists := node[segment.key]
			if !exists || !containerMatches(child, next) {
				node[segment.key] = newContainer(next)
				child = node[segment.key]
			}
			key := segment.key
			writeBack = func(updated any) { node[key] = updated }
			current = child

		case []any:
			if !segment.isIdx || segment.index < 0 || segment.index > len(node) {
				return
			}
			if last {
				if segment.index == len(node) {
					if writeBack == nil {
						return
					}
					writeBack(append(node, value))
					return
				}
				node[segment.index] = value
				return
			}
			if segment.index == len(node) {
				if writeBack == nil {
					return
				}
				node = append(node, newContainer(next))
				writeBack(node)
			}
			child := node[segment.index]
			if !containerMatches(child, next) {
				node[segment.index] = newContainer(next)
				child = node[segment.index]
			}
			position := segment.index
			writeBack = func(updated any) { node[position] = updated }
			current = child

		default:
			return
		}
	}
}

// containerMatches 报告已有值能否继续承载下一段。
func containerMatches(child any, next pathSegment) bool {
	if next.isIdx {
		_, ok := child.([]any)
		return ok
	}
	_, ok := child.(map[string]any)
	return ok
}

// newContainer 按下一段的类型建空容器。
func newContainer(next pathSegment) any {
	if next.isIdx {
		return []any{}
	}
	return map[string]any{}
}

// getValueByPath 复刻 getValueByPath：只读遍历，路径不存在时返回 (nil,false)。
func getValueByPath(root any, path string) (any, bool) {
	segments := parseFilterPath(path)
	if len(segments) == 0 {
		return nil, false
	}
	current := root
	for _, segment := range segments {
		switch container := current.(type) {
		case map[string]any:
			if segment.isIdx {
				return nil, false
			}
			value, ok := container[segment.key]
			if !ok {
				return nil, false
			}
			current = value
		case []any:
			if !segment.isIdx || segment.index < 0 || segment.index >= len(container) {
				return nil, false
			}
			current = container[segment.index]
		default:
			return nil, false
		}
	}
	return current, true
}

// deleteByPath 复刻 deleteByPath：对象删键，数组移除元素。
//
// 与 setValueByPath 同样需要 writeBack：JS 的 splice 直接改引用指向的数组，Go 的切片是头值，
// 缩短后的头必须写回父容器，否则父容器仍持有原长度的切片（这个坑由单测发现）。
func deleteByPath(root map[string]any, path string) {
	segments := parseFilterPath(path)
	if len(segments) == 0 {
		return
	}

	var current any = root
	var writeBack func(any)

	for index, segment := range segments {
		last := index == len(segments)-1
		switch node := current.(type) {
		case map[string]any:
			if segment.isIdx {
				return
			}
			if last {
				delete(node, segment.key)
				return
			}
			child, exists := node[segment.key]
			if !exists {
				return
			}
			key := segment.key
			writeBack = func(updated any) { node[key] = updated }
			current = child

		case []any:
			if !segment.isIdx || segment.index < 0 || segment.index >= len(node) {
				return
			}
			if last {
				if writeBack == nil {
					return
				}
				writeBack(append(node[:segment.index], node[segment.index+1:]...))
				return
			}
			position := segment.index
			writeBack = func(updated any) { node[position] = updated }
			current = node[segment.index]

		default:
			return
		}
	}
}

// deepEqualValues 复刻 deepEqual。
func deepEqualValues(left, right any) bool {
	switch leftValue := left.(type) {
	case nil:
		return right == nil
	case bool:
		rightValue, ok := right.(bool)
		return ok && leftValue == rightValue
	case string:
		rightValue, ok := right.(string)
		return ok && leftValue == rightValue
	case float64:
		rightValue, ok := right.(float64)
		return ok && leftValue == rightValue
	case json.Number:
		rightValue, ok := right.(json.Number)
		return ok && leftValue.String() == rightValue.String()
	case []any:
		rightValue, ok := right.([]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false
		}
		for index := range leftValue {
			if !deepEqualValues(leftValue[index], rightValue[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		rightValue, ok := right.(map[string]any)
		if !ok || len(leftValue) != len(rightValue) {
			return false
		}
		for key, value := range leftValue {
			other, ok := rightValue[key]
			if !ok || !deepEqualValues(value, other) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// deepMergeValues 复刻 deepMerge：null 表示删除键，对象递归合并，其余覆盖。
func deepMergeValues(target map[string]any, source map[string]any) {
	for key, value := range source {
		if unsafeKeys[key] {
			continue
		}
		if value == nil {
			delete(target, key)
			continue
		}
		sourceMap, sourceIsMap := value.(map[string]any)
		targetMap, targetIsMap := target[key].(map[string]any)
		if sourceIsMap && targetIsMap {
			deepMergeValues(targetMap, sourceMap)
			continue
		}
		if sourceIsMap {
			// 目标不是对象：与 TS 一致地整体覆盖为源对象（拷贝一份，避免共享源快照）。
			copied := make(map[string]any, len(sourceMap))
			deepMergeValues(copied, sourceMap)
			target[key] = copied
			continue
		}
		target[key] = value
	}
}

// matchFilterElement 复刻 matchElement。
func matchFilterElement(element any, matcher *filterMatcher) bool {
	if matcher == nil {
		return false
	}
	fieldValue := element
	if matcher.Field != "" {
		object, ok := element.(map[string]any)
		if !ok {
			return false
		}
		current := any(object)
		for _, part := range strings.Split(matcher.Field, ".") {
			container, ok := current.(map[string]any)
			if !ok {
				return false
			}
			value, ok := container[part]
			if !ok {
				return false
			}
			current = value
		}
		fieldValue = current
	}

	switch matcher.MatchType {
	case "contains":
		return strings.Contains(toStringValue(fieldValue), toStringValue(matcher.Value))
	case "regex":
		pattern := toStringValue(matcher.Value)
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return false
		}
		return compiled.MatchString(toStringValue(fieldValue))
	case "exact", "":
		if _, ok := fieldValue.(map[string]any); ok {
			return deepEqualValues(fieldValue, matcher.Value)
		}
		if _, ok := matcher.Value.(map[string]any); ok {
			return deepEqualValues(fieldValue, matcher.Value)
		}
		return deepEqualValues(fieldValue, matcher.Value)
	default:
		return false
	}
}

// toStringValue 复刻 JS 的 String(value)：对象走 JSON 序列化。
func toStringValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "undefined"
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case json.Number:
		return typed.String()
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// replaceFilterText 复刻 replaceText。
func replaceFilterText(input, target, replacement, matchType string) string {
	switch matchType {
	case MatchRegex:
		compiled, err := regexp.Compile(target)
		if err != nil {
			return input
		}
		return compiled.ReplaceAllString(input, replacement)
	case MatchExact:
		if input == target {
			return replacement
		}
		return input
	default:
		if target == "" {
			return input
		}
		return strings.ReplaceAll(input, target, replacement)
	}
}

// deepReplaceValues 复刻 deepReplace：递归替换所有字符串值。
func deepReplaceValues(value any, target, replacement, matchType string) any {
	switch typed := value.(type) {
	case string:
		return replaceFilterText(typed, target, replacement, matchType)
	case []any:
		replaced := make([]any, len(typed))
		for index, item := range typed {
			replaced[index] = deepReplaceValues(item, target, replacement, matchType)
		}
		return replaced
	case map[string]any:
		replaced := make(map[string]any, len(typed))
		for key, item := range typed {
			replaced[key] = deepReplaceValues(item, target, replacement, matchType)
		}
		return replaced
	default:
		return value
	}
}

// applyBodyFilter 把一条 body 相规则作用到正文。
func (d Deps) applyBodyFilter(filter RequestFilter, body map[string]any) {
	if filter.RuleMode == "advanced" && len(filter.Operations) > 0 {
		var operations []filterOperation
		if err := json.Unmarshal(filter.Operations, &operations); err != nil {
			d.logger().Error("guard.filter.operations_invalid", map[string]any{
				"filterId": filter.ID,
				"error":    err.Error(),
			})
			return
		}
		for _, operation := range operations {
			applyBodyOperation(operation, body)
		}
		return
	}

	switch filter.Action {
	case "json_path":
		setValueByPath(body, filter.Target, replacementValue(filter.Replacement))
	case "text_replace":
		replaced := deepReplaceValues(body, filter.Target, toStringValue(replacementValue(filter.Replacement)), normalizeMatchType(filter.MatchType))
		replacedMap, ok := replaced.(map[string]any)
		if !ok {
			return
		}
		// TS 是「清空原对象再拷回」：保持同一棵树，避免调用方持有的引用变成死引用。
		for key := range body {
			delete(body, key)
		}
		for key, value := range replacedMap {
			body[key] = value
		}
	default:
		d.logger().Warn("guard.filter.unsupported_body_action", map[string]any{
			"filterId": filter.ID,
			"action":   filter.Action,
		})
	}
}

// applyBodyOperation 执行一条 advanced 操作。
func applyBodyOperation(operation filterOperation, body map[string]any) {
	switch operation.Op {
	case "set":
		writeMode := operation.WriteMode
		if writeMode == "if_missing" {
			if _, exists := getValueByPath(body, operation.Path); exists {
				return
			}
		}
		setValueByPath(body, operation.Path, operation.Value)
	case "remove":
		if operation.Matcher != nil {
			if existing, ok := getValueByPath(body, operation.Path); ok {
				if array, ok := existing.([]any); ok {
					filtered := make([]any, 0, len(array))
					for _, element := range array {
						if !matchFilterElement(element, operation.Matcher) {
							filtered = append(filtered, element)
						}
					}
					setValueByPath(body, operation.Path, filtered)
				}
			}
			return
		}
		deleteByPath(body, operation.Path)
	case "merge":
		valueMap, ok := operation.Value.(map[string]any)
		if !ok {
			return
		}
		existing, exists := getValueByPath(body, operation.Path)
		target, ok := existing.(map[string]any)
		if !exists || !ok {
			setValueByPath(body, operation.Path, map[string]any{})
			existing, _ = getValueByPath(body, operation.Path)
			target, ok = existing.(map[string]any)
			if !ok {
				return
			}
		}
		deepMergeValues(target, valueMap)
	case "insert":
		applyInsertOperation(operation, body)
	default:
		// 未知操作按 TS 行为记日志跳过。
	}
}

// applyInsertOperation 复刻 executeInsertOp：去重、锚点定位、越界回退。
func applyInsertOperation(operation filterOperation, body map[string]any) {
	existing, exists := getValueByPath(body, operation.Path)
	array, ok := existing.([]any)
	if !exists || !ok {
		setValueByPath(body, operation.Path, []any{})
		existing, _ = getValueByPath(body, operation.Path)
		array, ok = existing.([]any)
		if !ok {
			return
		}
	}

	dedupeEnabled := operation.Dedupe == nil || operation.Dedupe.Enabled == nil || *operation.Dedupe.Enabled
	if dedupeEnabled {
		for _, element := range array {
			if operation.Dedupe != nil && len(operation.Dedupe.ByFields) > 0 {
				elementMap, elementOK := element.(map[string]any)
				valueMap, valueOK := operation.Value.(map[string]any)
				if !elementOK || !valueOK {
					if deepEqualValues(element, operation.Value) {
						return
					}
					continue
				}
				same := true
				for _, field := range operation.Dedupe.ByFields {
					if !deepEqualValues(elementMap[field], valueMap[field]) {
						same = false
						break
					}
				}
				if same {
					return
				}
				continue
			}
			if deepEqualValues(element, operation.Value) {
				return
			}
		}
	}

	insertIndex := len(array)
	switch operation.Position {
	case "start":
		insertIndex = 0
	case "before", "after":
		if operation.Anchor == nil {
			if operation.Position == "before" {
				insertIndex = 0
			}
			break
		}
		anchorIndex := -1
		for index, element := range array {
			if matchFilterElement(element, operation.Anchor) {
				anchorIndex = index
				break
			}
		}
		if anchorIndex == -1 {
			fallback := operation.OnAnchorMissing
			if fallback == "" {
				fallback = "end"
			}
			if fallback == "skip" {
				return
			}
			if fallback == "start" {
				insertIndex = 0
			}
			break
		}
		if operation.Position == "before" {
			insertIndex = anchorIndex
		} else {
			insertIndex = anchorIndex + 1
		}
	}

	// 原地插入，保证与 getValueByPath 取到的切片是同一块底层数组的语义。
	updated := make([]any, 0, len(array)+1)
	updated = append(updated, array[:insertIndex]...)
	updated = append(updated, operation.Value)
	updated = append(updated, array[insertIndex:]...)
	setValueByPath(body, operation.Path, updated)
}

// replacementValue 把规则里的 replacement 解出为值：字符串原样，其余按 JSON 解析。
func replacementValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return string(raw)
	}
	return decoded
}

// normalizeMatchType 把空匹配类型归一到 contains（与 TS 的 default 分支一致）。
func normalizeMatchType(matchType string) string {
	switch matchType {
	case MatchRegex, MatchExact, MatchContains:
		return matchType
	default:
		return MatchContains
	}
}

// applyHeaderFilter 把一条 header 相规则作用到上下文。
func (d Deps) applyHeaderFilter(filter RequestFilter, ctx *pctx.Context) {
	if filter.RuleMode == "advanced" && len(filter.Operations) > 0 {
		var operations []filterOperation
		if err := json.Unmarshal(filter.Operations, &operations); err != nil {
			d.logger().Error("guard.filter.operations_invalid", map[string]any{
				"filterId": filter.ID,
				"error":    err.Error(),
			})
			return
		}
		for _, operation := range operations {
			applyHeaderOperation(operation, ctx)
		}
		return
	}

	switch filter.Action {
	case "remove":
		ctx.DeleteHeader(filter.Target)
	case "set":
		ctx.SetHeader(filter.Target, headerValueFrom(filter.Replacement))
	default:
		d.logger().Warn("guard.filter.unsupported_header_action", map[string]any{
			"filterId": filter.ID,
			"action":   filter.Action,
		})
	}
}

// headerValueFrom 复刻 applySimpleFilterDirect 的 header set 取值规则：
// 字符串原样，null/缺失为空串，其余走 JSON 序列化。
func headerValueFrom(raw json.RawMessage) string {
	value := replacementValue(raw)
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return toStringValue(value)
}

// applyHeaderOperation 执行一条 header 相的 advanced 操作。
func applyHeaderOperation(operation filterOperation, ctx *pctx.Context) {
	switch operation.Op {
	case "set":
		if operation.WriteMode == "if_missing" && ctx.Headers().Has(operation.Path) {
			return
		}
		ctx.SetHeader(operation.Path, toStringValue(operation.Value))
	case "remove":
		ctx.DeleteHeader(operation.Path)
	default:
		// merge / insert 只作用于正文，header 相忽略。
	}
}

// guardPhaseFilters 取守卫相、且绑定类型为 global 的规则。
//
// 顺序即执行顺序，因此不重排：规则叠加的结果依赖先后。
func guardPhaseFilters(filters []RequestFilter, binding string) []RequestFilter {
	selected := make([]RequestFilter, 0, len(filters))
	for _, filter := range filters {
		phase := filter.ExecutionPhase
		if phase == "" {
			phase = "guard"
		}
		if phase != "guard" {
			continue
		}
		bound := filter.BindingType
		if bound == "" {
			bound = "global"
		}
		if bound != binding {
			continue
		}
		selected = append(selected, filter)
	}
	return selected
}

// requestFilterStep 复刻 ProxyRequestFilter.ensure（守卫相、全局绑定）。
//
// fail-open：任何一条规则出错都只记日志，不阻塞请求。
func (d Deps) requestFilterStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		if d.bypassRequestFilters(ctx) {
			return nil, nil
		}
		d.applyFilters(ctx, "global")
		return nil, nil
	}
}

// providerRequestFilterStep 复刻 ProxyProviderRequestFilter.ensure：作用于选定供应商的规则。
func (d Deps) providerRequestFilterStep() Step {
	return func(ctx *pctx.Context) (*Response, error) {
		if d.bypassRequestFilters(ctx) {
			return nil, nil
		}
		selection, ok := ctx.Provider()
		if !ok {
			d.logger().Warn("guard.filter.no_provider", map[string]any{
				"note": "未选路，供应商相过滤器跳过",
			})
			return nil, nil
		}

		filters := d.loadFilters(ctx)
		for _, filter := range providerBoundFilters(filters, selection, d.ProviderGroupTag) {
			d.applyOneFilter(ctx, filter)
		}
		return nil, nil
	}
}

// applyFilters 应用某一绑定类型的全部守卫相规则。
func (d Deps) applyFilters(ctx *pctx.Context, binding string) {
	filters := guardPhaseFilters(d.loadFilters(ctx), binding)
	for _, filter := range filters {
		d.applyOneFilter(ctx, filter)
	}
}

// applyOneFilter 应用单条规则，失败只记日志（fail-open）。
func (d Deps) applyOneFilter(ctx *pctx.Context, filter RequestFilter) {
	if filter.Scope == "header" {
		d.applyHeaderFilter(filter, ctx)
		return
	}
	if filter.Scope != "body" {
		return
	}
	body, err := d.body(ctx)
	if err != nil {
		// 无正文可改：跳过并留痕（Node 侧同样在无 message 时什么都不做）。
		d.logger().Warn("guard.filter.body_unavailable", map[string]any{"filterId": filter.ID})
		return
	}
	d.applyBodyFilter(filter, body)
	// 改动需要写回，否则后续步骤与上游转发看不到。
	if storeErr := d.storeBody(ctx, body); storeErr != nil {
		d.logger().Error("guard.filter.body_store_failed", map[string]any{
			"filterId": filter.ID,
			"error":    storeErr.Error(),
		})
	}
}

// loadFilters 读规则快照；失败时返回空表（fail-open）。
func (d Deps) loadFilters(ctx *pctx.Context) []RequestFilter {
	if d.Filters == nil {
		return nil
	}
	filters, err := d.Filters.RequestFilters(d.runContext(ctx))
	if err != nil {
		d.logger().Error("guard.filter.source_failed", map[string]any{"error": err.Error()})
		return nil
	}
	return filters
}

// bypassRequestFilters 报告本次请求是否跳过过滤器（端点策略）。
func (d Deps) bypassRequestFilters(ctx *pctx.Context) bool {
	if d.BypassRequestFilters == nil {
		return false
	}
	return d.BypassRequestFilters(ctx)
}

// providerBoundFilters 取绑定到本次选定供应商（或所在分组）的规则。
//
// 分组标签位于 providers 行的 group_tag 列；pctx.ProviderSelection 只带路由必需字段，
// 故标签由 groupTagFor 回调补齐（返回空串时分组绑定规则不生效）。
func providerBoundFilters(filters []RequestFilter, selection pctx.ProviderSelection, groupTagFor func(pctx.ProviderSelection) string) []RequestFilter {
	selected := make([]RequestFilter, 0, len(filters))
	for _, filter := range filters {
		phase := filter.ExecutionPhase
		if phase == "" {
			phase = "guard"
		}
		if phase != "guard" {
			continue
		}
		switch filter.BindingType {
		case "providers":
			if containsInt64(filter.ProviderIDs, selection.ProviderID) {
				selected = append(selected, filter)
			}
		case "groups":
			if groupTagFor == nil {
				continue
			}
			tag := groupTagFor(selection)
			if tag != "" && containsString(filter.GroupTags, tag) {
				selected = append(selected, filter)
			}
		}
	}
	return selected
}

// containsInt64 报告切片是否含目标值。
func containsInt64(values []int64, wanted int64) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// containsString 报告切片是否含目标值。
func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
