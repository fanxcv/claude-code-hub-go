package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// FindGroupMultipliers 按名取供应商分组的成本倍率（provider_groups.cost_multiplier）。
//
// 只做取数，不做判定：命中顺序、多分组拼接与「全不命中按 1」的口径在调用方（复刻
// src/repository/provider-groups.ts 的 getGroupCostMultiplier）。命名带 FindGroup 前缀
// 是有意的：五个并行 lane 各有自己的 store 文件，通用名会撞符号。
//
// 返回的 map 只含**确实存在**的分组：缺失的名字不在结果里，调用方据此区分「没这个分组」
// 与「这个分组的倍率是 0」（0 是合法的免费倍率）。
func (p *Pools) FindGroupMultipliers(ctx context.Context, names []string) (map[string]float64, error) {
	if len(names) == 0 {
		return map[string]float64{}, nil
	}
	pool, err := p.Data()
	if err != nil {
		return nil, err
	}
	// cost_multiplier 是 numeric：row_to_json 可能给数字也可能给字符串，故取文本形态
	// 再按 float 解析（与 read.go 里 numeric 列的处理口径一致）。
	rows, err := pool.Query(ctx,
		`SELECT name, cost_multiplier::text FROM provider_groups WHERE name = ANY($1)`,
		names,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商分组倍率失败: %w", err)
	}
	defer rows.Close()

	result := make(map[string]float64, len(names))
	for rows.Next() {
		var name string
		var raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("store: 读取供应商分组倍率失败: %w", err)
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			// 单行坏了不拖垮整次查询：倍率是账务的软输入，缺失会按 1 计（宁可少收不炸请求）。
			continue
		}
		result[name] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商分组倍率失败: %w", err)
	}
	return result, nil
}
