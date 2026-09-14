package route

// ProviderGroupMatches 是 checkProviderGroupMatch 的导出包装，供**数据面之外的只读视图**
// （如聚合式模型列表的供应商过滤）复用同一套分组语义。
//
// 存在的理由：分组匹配是「供应商标签与用户分组求交、用户含 `*` 即全通过、供应商标签缺省
// default」这三条规则的唯一真源。让调用方各写一份，迟早会在某次规则调整后分叉，
// 而分叉的表现是「某些供应商在模型列表里消失但请求仍能路由到它」这种静默不一致。
func ProviderGroupMatches(providerGroupTag *string, userGroups string) bool {
	return checkProviderGroupMatch(providerGroupTag, userGroups)
}
