package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 本文件是 sensitive_words 表的管理面读写面（批次 A / lane A1-5）。
//
// 唯一真源：src/repository/sensitive-words.ts。SQL 的形状、排序与 NULL 语义逐条对齐该文件，
// 差异只在注释里写明的地方。
//
// 分道：读走 control（与 guard 的既有读面一致，不占数据面连接），写走 writer（与 adminapi 的
// 审计写入一致）。

// AdminSensitiveWord 是 sensitive_words 的一行。
//
// 字段名用 Node 的 camelCase（SensitiveWordSchema，src/lib/api/v1/schemas/sensitive-words.ts:9-17）：
// 本类型同时充当响应体的形状来源，故不能改成 Go 风格的命名再去映射。
type AdminSensitiveWord struct {
	ID          int64   `json:"id"`
	Word        string  `json:"word"`
	MatchType   string  `json:"matchType"`
	Description *string `json:"description"`
	IsEnabled   bool    `json:"isEnabled"`
	// CreatedAt / UpdatedAt 为指针：列可空（schema.ts:879-880 只有一个 defaultNow，没有 notNull），
	// Node 侧用 `?? new Date()` 兜底，兜底放在 adminapi 层做——存储层不替调用方编造时间。
	CreatedAt *string `json:"createdAt"`
	UpdatedAt *string `json:"updatedAt"`
}

// sensitiveWordColumns 与 getAllSensitiveWords 的投影一致。
const sensitiveWordColumns = `id, word, match_type AS "matchType", description,
	is_enabled AS "isEnabled",
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// AdminListSensitiveWords 复刻 getAllSensitiveWords：按创建时间倒序，含禁用行。
//
// 排序写法与 drizzle 的 `desc(createdAt)` 一致：PG 对 DESC 的默认是 NULLS FIRST（drizzle 不发
// NULLS 子句，故 Node 侧也是 NULLS FIRST）。
func (p *Pools) AdminListSensitiveWords(ctx context.Context) ([]AdminSensitiveWord, error) {
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + sensitiveWordColumns +
		` FROM sensitive_words ORDER BY created_at DESC NULLS FIRST) t`
	return sensitiveWordRows(ctx, p, query)
}

// AdminCreateSensitiveWord 复刻 createSensitiveWord。
//
// description 为 nil 表示「没传该字段」：Node 侧 undefined 不参与 INSERT，列取默认（NULL）。
func (p *Pools) AdminCreateSensitiveWord(
	ctx context.Context,
	word string,
	matchType string,
	description *string,
) (AdminSensitiveWord, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminSensitiveWord{}, err
	}
	query := `INSERT INTO sensitive_words (word, match_type, description)
		VALUES ($1, $2, $3) RETURNING ` + sensitiveWordColumns
	return sensitiveWordRow(ctx, pool, query, word, matchType, description)
}

// AdminSensitiveWordUpdate 是一次部分更新：nil 字段不参与 SET（复刻 zod 的 .partial()）。
type AdminSensitiveWordUpdate struct {
	Word        *string
	MatchType   *string
	Description *string
	IsEnabled   *bool
}

// AdminUpdateSensitiveWord 复刻 updateSensitiveWord：部分更新 + updated_at 置当前时间。
//
// 记录不存在时返回 ErrNotFound（Node 侧 returning() 为空即 null，handler 映射成 404）。
func (p *Pools) AdminUpdateSensitiveWord(
	ctx context.Context,
	id int64,
	update AdminSensitiveWordUpdate,
) (AdminSensitiveWord, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminSensitiveWord{}, err
	}
	assignments := make([]string, 0, 5)
	args := make([]any, 0, 5)
	add := func(expression string, value any) {
		args = append(args, value)
		assignments = append(assignments, fmt.Sprintf("%s = $%d", expression, len(args)))
	}
	if update.Word != nil {
		add("word", *update.Word)
	}
	if update.MatchType != nil {
		add("match_type", *update.MatchType)
	}
	if update.Description != nil {
		add("description", *update.Description)
	}
	if update.IsEnabled != nil {
		add("is_enabled", *update.IsEnabled)
	}
	// updatedAt 恒被写入（Node 侧也如此），因此 SET 列表永不为空。
	assignments = append(assignments, "updated_at = now()")
	args = append(args, id)

	query := fmt.Sprintf("UPDATE sensitive_words SET %s WHERE id = $%d RETURNING %s",
		strings.Join(assignments, ", "), len(args), sensitiveWordColumns)
	return sensitiveWordRow(ctx, pool, query, args...)
}

// AdminDeleteSensitiveWord 复刻 deleteSensitiveWord：硬删除，返回是否删掉了行。
func (p *Pools) AdminDeleteSensitiveWord(ctx context.Context, id int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, "DELETE FROM sensitive_words WHERE id = $1", id)
	if err != nil {
		return false, fmt.Errorf("store: 删除敏感词失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminSensitiveWordCounts 是按匹配类型分组的启用词计数。
type AdminSensitiveWordCounts struct {
	Contains int64
	Exact    int64
	Regex    int64
}

// Total 复刻 detector 的 totalCount（三类之和；未知 match_type 的行不计入，与 Node 的 switch 一致）。
func (c AdminSensitiveWordCounts) Total() int64 { return c.Contains + c.Exact + c.Regex }

// AdminCountActiveSensitiveWords 复刻 getActiveSensitiveWords 的行集：只取启用行，按 match_type 计数。
//
// 未知 match_type 的行被忽略：Node 的 reload 只把 contains/exact/regex 三类放进快照
// （src/lib/sensitive-word-detector.ts:105-125），计数也随之只有三类。
func (p *Pools) AdminCountActiveSensitiveWords(ctx context.Context) (AdminSensitiveWordCounts, error) {
	pool, err := p.Control()
	if err != nil {
		return AdminSensitiveWordCounts{}, err
	}
	rows, err := pool.Query(ctx, `SELECT match_type, COUNT(*)::bigint FROM sensitive_words
		WHERE is_enabled = true GROUP BY match_type`)
	if err != nil {
		return AdminSensitiveWordCounts{}, fmt.Errorf("store: 统计敏感词失败: %w", err)
	}
	defer rows.Close()

	var counts AdminSensitiveWordCounts
	for rows.Next() {
		var matchType string
		var count int64
		if err := rows.Scan(&matchType, &count); err != nil {
			return AdminSensitiveWordCounts{}, fmt.Errorf("store: 读取敏感词计数失败: %w", err)
		}
		switch matchType {
		case "contains":
			counts.Contains = count
		case "exact":
			counts.Exact = count
		case "regex":
			counts.Regex = count
		}
	}
	if err := rows.Err(); err != nil {
		return AdminSensitiveWordCounts{}, fmt.Errorf("store: 遍历敏感词计数失败: %w", err)
	}
	return counts, nil
}

// sensitiveWordRows 用 control 分道读多行（row_to_json 文本行）。
//
// 名字带 sensitiveWord 前缀是有意的：批次 A 有五个并行 lane 各自往本包加文件，
// 通用名（rowsAs 之类）会互相撞符号。
func sensitiveWordRows(ctx context.Context, p *Pools, query string, args ...any) ([]AdminSensitiveWord, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询敏感词失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminSensitiveWord, 0, 8)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取敏感词行失败: %w", err)
		}
		word, err := decodeSensitiveWord(payload)
		if err != nil {
			return nil, err
		}
		results = append(results, word)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历敏感词行失败: %w", err)
	}
	return results, nil
}

// sensitiveWordRow 读 RETURNING 的单行并区分「不存在」。
//
// 与 sensitiveWordRows 的区别只在扫描方式：这里按 sensitiveWordColumns 的**列顺序**直接扫
// （RETURNING 给的是列，不是 row_to_json 的文本）。
func sensitiveWordRow(
	ctx context.Context,
	pool *Pool,
	query string,
	args ...any,
) (AdminSensitiveWord, error) {
	row := pool.QueryRow(ctx, query, args...)
	var word AdminSensitiveWord
	if err := row.Scan(&word.ID, &word.Word, &word.MatchType, &word.Description,
		&word.IsEnabled, &word.CreatedAt, &word.UpdatedAt); err != nil {
		if isNoRows(err) {
			return AdminSensitiveWord{}, ErrNotFound
		}
		return AdminSensitiveWord{}, fmt.Errorf("store: 敏感词写入失败: %w", err)
	}
	return word, nil
}

// decodeSensitiveWord 反序列化一行；注意 query 里用的是 row_to_json，故这里的标签与
// AdminSensitiveWord 的 json 标签一致。
func decodeSensitiveWord(payload string) (AdminSensitiveWord, error) {
	var word AdminSensitiveWord
	if err := json.Unmarshal([]byte(payload), &word); err != nil {
		return AdminSensitiveWord{}, fmt.Errorf("store: 敏感词行反序列化失败: %w", err)
	}
	return word, nil
}
