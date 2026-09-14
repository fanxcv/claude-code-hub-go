package adminapi

import (
	"fmt"
	"regexp"
	"strings"
)

// 路径段模式。段有三种写法（相对 MountPrefix，与 Node 侧路径一一对应）：
//
//	字面量        /users/tags、/keys:batchUpdate、/request-filters/cache:refresh
//	参数          /keys/{keyId}、/usage-logs/exports/{jobId}
//	参数+正则+后缀 /keys/{keyId:[0-9]+}:enable
//
// 第三种是 Node 侧「参数后紧跟字面量后缀」的 5 条路由
// §1.2）：Hono 写作 `:keyId{[0-9]+:enable}`，语义是「整段匹配 `[0-9]+:enable`」。本包把它
// 拆成「头正则 + 字面后缀」两截，两者合起来与 Hono 同义：先剥后缀，再对头做整段锚定匹配。
//
// 字面量段可以带冒号（`users:self`、`limits:reset`），冒号不是分隔符而是段内字符——这一点
// 与 Node 一致，故不需要任何特殊处理。
type pathSegment struct {
	// literal 非空表示字面量段。
	literal string
	// name 非空表示参数段。
	name string
	// head 是参数段的头正则（已锚定）；nil 表示「任意非空段」。
	head *regexp.Regexp
	// suffix 是参数段后紧跟的字面后缀，空表示无后缀。
	suffix string
}

// compiledRoute 是一条已编译的路由。
type compiledRoute struct {
	route    Route
	segments []pathSegment
	// specificity 用于「静态优先于参数」的择路：字面量段权重 2、带后缀参数段 1、纯参数段 0。
	// 例：`/keys/{keyId}:reveal`(3) 必须压过 `/keys/{keyId}`(2)，否则 reveal 会被通用路由吞掉。
	specificity int
	// order 是注册序，用于同权择路时的稳定兜底。
	order int
}

// compilePath 把路径模式编译成段序列，并算出择路权重。
//
// 模式非法（不以 / 开头、空段、未闭合的 {}、空参数名、非法正则、非法后缀）属于**启动期编程
// 错误**，直接 panic：宁可在装配时崩掉，也不要在生产里静默漏掉一条管理路由。
func compilePath(path string) ([]pathSegment, int, error) {
	if path == "" || !strings.HasPrefix(path, "/") {
		return nil, 0, fmt.Errorf("adminapi: 路径必须以 / 开头：%q", path)
	}
	if path == "/" {
		return nil, 0, nil
	}
	parts := strings.Split(path[1:], "/")
	segments := make([]pathSegment, 0, len(parts))
	specificity := 0
	for _, part := range parts {
		if part == "" {
			return nil, 0, fmt.Errorf("adminapi: 路径含空段：%q", path)
		}
		if !strings.HasPrefix(part, "{") {
			segments = append(segments, pathSegment{literal: part})
			specificity += 2
			continue
		}
		close := strings.IndexByte(part, '}')
		if close < 0 {
			return nil, 0, fmt.Errorf("adminapi: 参数段未闭合：%q", path)
		}
		head := part[1:close]
		rest := part[close+1:]
		name, rawPattern, hasPattern := strings.Cut(head, ":")
		if name == "" {
			return nil, 0, fmt.Errorf("adminapi: 参数名为空：%q", path)
		}
		segment := pathSegment{name: name}
		if hasPattern {
			if rawPattern == "" {
				return nil, 0, fmt.Errorf("adminapi: 参数正则为空：%q", path)
			}
			compiled, err := regexp.Compile("^(?:" + rawPattern + ")$")
			if err != nil {
				return nil, 0, fmt.Errorf("adminapi: 参数正则非法：%q：%w", path, err)
			}
			segment.head = compiled
		}
		switch {
		case rest == "":
		case strings.HasPrefix(rest, ":"):
			if rest == ":" {
				return nil, 0, fmt.Errorf("adminapi: 参数后缀为空：%q", path)
			}
			segment.suffix = rest
			specificity++
		default:
			return nil, 0, fmt.Errorf("adminapi: 参数段后只能跟 :字面后缀：%q", path)
		}
		segments = append(segments, segment)
	}
	return segments, specificity, nil
}

// splitPath 把已归一化的路径切成段。归一化由调用方负责（复用 egress.NormalizePath）。
func splitPath(path string) []string {
	if path == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(path, "/"), "/")
}

// match 判断路径是否命中本路由；命中时返回 true 与路径参数（无参数时为 nil）。
//
// 段数必须先相等：Node 的路由是逐段的，`/users/tags` 与 `/users/{id}` 段数相同才需要比权重，
// 而 `/users:self` 只有一段，与本包任何 `/users/...` 两段路由都不可能混淆。
func (c compiledRoute) match(path string) (map[string]string, bool) {
	parts := splitPath(path)
	if len(parts) != len(c.segments) {
		return nil, false
	}
	var params map[string]string
	record := func(name, value string) {
		if params == nil {
			params = map[string]string{}
		}
		params[name] = value
	}
	for index, pattern := range c.segments {
		value := parts[index]
		switch {
		case pattern.name == "":
			if value != pattern.literal {
				return nil, false
			}
		case pattern.suffix != "":
			// 先剥一个字面后缀再校验头：与 Hono `{regex:suffix}` 的「整段匹配」同义。
			head, found := strings.CutSuffix(value, pattern.suffix)
			if !found || head == "" || !matchHead(pattern.head, head) {
				return nil, false
			}
			record(pattern.name, head)
		default:
			if value == "" || !matchHead(pattern.head, value) {
				return nil, false
			}
			record(pattern.name, value)
		}
	}
	return params, true
}

// matchHead 校验参数段的头部；head 为 nil 表示任意非空值。
func matchHead(head *regexp.Regexp, value string) bool {
	if head == nil {
		return true
	}
	return head.MatchString(value)
}
