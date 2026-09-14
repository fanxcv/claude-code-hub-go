package egress

import "strings"

// NormalizePath 归一化请求路径：去掉尾部斜杠、折叠重复斜杠、剔除查询串。
//
// 消费方是数据面与管理面的路由键（例如 `internal/dataplane/routes.go` 把请求路径归一后
// 作为路由表的键）。规则匹配已随归属白名单删除，但归一化的必要性不变：`/v1//messages`
// 这类写法必须与 `/v1/messages` 落到同一个键，否则会绕过分层路由表。
func NormalizePath(path string) string {
	if path == "" {
		return "/"
	}
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	if path == "" {
		return "/"
	}
	return path
}
