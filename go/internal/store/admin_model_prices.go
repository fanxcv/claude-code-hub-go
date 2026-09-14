package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// 本文件是 model_prices 表的管理面读写面（批次 A / lane A1-5）。
//
// 唯一真源：src/repository/model-price.ts。分页查询的 SQL 形状（DISTINCT ON + 外层按
// updatedAt 排序 + COUNT(DISTINCT model_name)）逐字对齐该文件——不改写成「等价但不同」的
// 形式，否则两端会在并列行的取舍与每页内容上分叉。
//
// 分道：读走 control，写走 writer。

// AdminModelPrice 是 model_prices 的一行。
//
// 字段名取 Node 的 camelCase（ModelPriceSchema，src/lib/api/v1/schemas/model-prices.ts:40-47）：
// 本类型同时充当响应体的形状来源。PriceData 用 json.RawMessage 原样透传——它是 jsonb，
// 两端读的是同一份字节。
type AdminModelPrice struct {
	ID        int64           `json:"id"`
	ModelName string          `json:"modelName"`
	PriceData json.RawMessage `json:"priceData"`
	Source    string          `json:"source"`
	CreatedAt *string         `json:"createdAt"`
	UpdatedAt *string         `json:"updatedAt"`
}

// AdminModelPriceQuery 复刻 PaginationParams（src/repository/model-price.ts:13-21）。
type AdminModelPriceQuery struct {
	Page     int
	PageSize int
	// Search 为空表示不筛。
	Search string
	// Source 为空表示不筛；取值 cloud/litellm/manual。cloud 的语义是 `source <> 'manual'`
	// （把旧版 litellm 遗留行也算进来），见 model-price.ts:222-225。
	Source string
	Vendor string
	// LitellmProvider 筛 price_data->>'litellm_provider'，仅旧数据可命中。
	LitellmProvider string
}

// modelPriceProjection 是 DISTINCT ON 用的列投影，与 Node 的内联 SQL 同形。
//
// 时间列用 to_char 直接渲染成 JS Date#toISOString 的形状（毫秒、UTC、固定 24 字符），与
// Node 的 serializeDates（src/lib/api/v1/_shared/serialization.ts:24）逐字一致。
const modelPriceProjection = `id,
	model_name AS "modelName",
	price_data AS "priceData",
	source,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// AdminListModelPricesPaginated 复刻 findAllLatestPricesPaginated：返回当页去重行与去重总数。
func (p *Pools) AdminListModelPricesPaginated(
	ctx context.Context,
	query AdminModelPriceQuery,
) ([]AdminModelPrice, int64, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, 0, err
	}

	conditions := make([]string, 0, 4)
	args := make([]any, 0, 6)
	// placeholder 生成 $n 并把参数入队：条件片段里的 ? 按顺序替换，避免手写下标错位。
	placeholder := func() string { return "$" + strconv.Itoa(len(args)) }
	if trimmed := strings.TrimSpace(query.Search); trimmed != "" {
		args = append(args, "%"+trimmed+"%")
		conditions = append(conditions,
			fmt.Sprintf(`(model_name ILIKE %s OR price_data->>'display_name' ILIKE %s)`,
				placeholder(), placeholder()))
	}
	switch query.Source {
	case "":
	case "cloud":
		conditions = append(conditions, "source <> 'manual'")
	case "litellm", "manual":
		args = append(args, query.Source)
		conditions = append(conditions, "source = "+placeholder())
	}
	if trimmed := strings.TrimSpace(query.Vendor); trimmed != "" {
		args = append(args, trimmed)
		conditions = append(conditions, `price_data->>'vendor' = `+placeholder())
	}
	if trimmed := strings.TrimSpace(query.LitellmProvider); trimmed != "" {
		args = append(args, trimmed)
		conditions = append(conditions, `price_data->>'litellm_provider' = `+placeholder())
	}
	where := ""
	if len(conditions) > 0 {
		where = "WHERE " + strings.Join(conditions, " AND ")
	}

	var total int64
	if err := pool.QueryRow(ctx,
		"SELECT COUNT(DISTINCT model_name) FROM model_prices "+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("store: 统计模型价格失败: %w", err)
	}

	limitIndex := len(args) + 1
	offsetIndex := len(args) + 2
	args = append(args, query.PageSize, (query.Page-1)*query.PageSize)
	// 外层按 ISO 文本排序：本投影里的 updatedAt 是固定宽度的 UTC ISO 串，字典序与时间序一致，
	// 故与 Node 按 timestamptz 排序得到同一顺序。
	dataQuery := fmt.Sprintf(`SELECT row_to_json(t)::text FROM (
		SELECT * FROM (
			SELECT DISTINCT ON (model_name) %s
			FROM model_prices
			%s
			ORDER BY model_name, (source = 'manual') DESC, created_at DESC NULLS LAST, id DESC
		) sub
		ORDER BY sub."updatedAt" DESC NULLS LAST
		LIMIT $%d OFFSET $%d
	) t`, modelPriceProjection, where, limitIndex, offsetIndex)

	rows, err := modelPriceRows(ctx, pool, dataQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// AdminFindLatestModelPriceByName 复刻 findLatestPriceByModel 的**精确名分支**。
//
// 差异（登记进对拍白名单）：Node 在精确名未命中时还会做「候选名 + aliases」回退查询
// （model-price.ts:45-105）。本函数不回退——它的调用点只有审计的 before 快照（upsert / delete），
// 回退会把「实际改的是哪一行」写成请求里的模型名之外的名字。
func (p *Pools) AdminFindLatestModelPriceByName(ctx context.Context, modelName string) (*AdminModelPrice, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + modelPriceProjection + ` FROM model_prices
		WHERE model_name = $1
		ORDER BY (source = 'manual') DESC, created_at DESC NULLS LAST, id DESC
		LIMIT 1`
	return modelPriceRow(ctx, pool, query, modelName)
}

// AdminFindLatestModelPriceByNameAndSource 复刻 findLatestPriceByModelAndSource。
func (p *Pools) AdminFindLatestModelPriceByNameAndSource(
	ctx context.Context,
	modelName string,
	source string,
) (*AdminModelPrice, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + modelPriceProjection + ` FROM model_prices
		WHERE model_name = $1 AND source = $2
		ORDER BY created_at DESC NULLS LAST, id DESC
		LIMIT 1`
	return modelPriceRow(ctx, pool, query, modelName, source)
}

// AdminUpsertModelPrice 复刻 upsertModelPrice：同一事务里先删该模型的全部旧行，再插一行。
//
// 这不是 PG 的 ON CONFLICT：Node 侧是硬删除后插入，因而旧行（含 id 与历史）会消失。
// 改成 upsert 会让两端的行数与 id 分叉。
func (p *Pools) AdminUpsertModelPrice(
	ctx context.Context,
	modelName string,
	priceData json.RawMessage,
	source string,
) (AdminModelPrice, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminModelPrice{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return AdminModelPrice{}, fmt.Errorf("store: 开启模型价格事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "DELETE FROM model_prices WHERE model_name = $1", modelName); err != nil {
		return AdminModelPrice{}, fmt.Errorf("store: 清理旧模型价格失败: %w", err)
	}
	inserted, err := scanModelPrice(tx.QueryRow(ctx,
		`INSERT INTO model_prices (model_name, price_data, source) VALUES ($1, $2::jsonb, $3)
		 RETURNING `+modelPriceProjection, modelName, string(priceData), source))
	if err != nil {
		return AdminModelPrice{}, fmt.Errorf("store: 写入模型价格失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminModelPrice{}, fmt.Errorf("store: 提交模型价格事务失败: %w", err)
	}
	return inserted, nil
}

// AdminDeleteModelPriceByName 复刻 deleteModelPriceByName：硬删除该模型的全部行；
// 不存在也算成功（Node 侧丢弃了影响行数）。
func (p *Pools) AdminDeleteModelPriceByName(ctx context.Context, modelName string) error {
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, "DELETE FROM model_prices WHERE model_name = $1", modelName); err != nil {
		return fmt.Errorf("store: 删除模型价格失败: %w", err)
	}
	return nil
}

// AdminHasAnyModelPrice 复刻 hasAnyPriceRecords。
func (p *Pools) AdminHasAnyModelPrice(ctx context.Context) (bool, error) {
	pool, err := p.Control()
	if err != nil {
		return false, err
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM model_prices)").Scan(&exists); err != nil {
		return false, fmt.Errorf("store: 检查价格表失败: %w", err)
	}
	return exists, nil
}

// AdminListLatestModelPrices 复刻 findAllLatestPrices：每模型一行、manual 优先。
func (p *Pools) AdminListLatestModelPrices(ctx context.Context) ([]AdminModelPrice, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT DISTINCT ON (model_name) ` + modelPriceProjection + ` FROM model_prices
		ORDER BY model_name, (source = 'manual') DESC, created_at DESC NULLS LAST, id DESC
	) t`
	return modelPriceRows(ctx, pool, query)
}

// modelPriceRows 读多行（row_to_json 文本行）。
//
// 名字带 modelPrice 前缀是有意的：五个并行 lane 各有自己的 store 文件，通用名会撞符号。
func modelPriceRows(ctx context.Context, pool *Pool, query string, args ...any) ([]AdminModelPrice, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询模型价格失败: %w", err)
	}
	defer rows.Close()

	results := make([]AdminModelPrice, 0, 8)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取模型价格行失败: %w", err)
		}
		price, err := decodeModelPrice(payload)
		if err != nil {
			return nil, err
		}
		results = append(results, price)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历模型价格行失败: %w", err)
	}
	return results, nil
}

// modelPriceRow 读单行；不存在时返回 (nil, nil)。Node 侧这几个函数以 null 表示不存在，
// 与 ErrNotFound 的语义不同：调用方对「没有价格行」与「查询失败」的处理不一样。
func modelPriceRow(ctx context.Context, pool *Pool, query string, args ...any) (*AdminModelPrice, error) {
	price, err := scanModelPrice(pool.QueryRow(ctx, query, args...))
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 模型价格查询失败: %w", err)
	}
	return &price, nil
}

// scanModelPrice 按投影列顺序扫描一行（RETURNING 走这条；row_to_json 走 decodeModelPrice）。
func scanModelPrice(row pgx.Row) (AdminModelPrice, error) {
	var price AdminModelPrice
	var createdAt, updatedAt *string
	if err := row.Scan(&price.ID, &price.ModelName, &price.PriceData, &price.Source,
		&createdAt, &updatedAt); err != nil {
		return AdminModelPrice{}, err
	}
	price.CreatedAt = createdAt
	price.UpdatedAt = updatedAt
	return price, nil
}

func decodeModelPrice(payload string) (AdminModelPrice, error) {
	var price AdminModelPrice
	if err := json.Unmarshal([]byte(payload), &price); err != nil {
		return AdminModelPrice{}, fmt.Errorf("store: 模型价格行反序列化失败: %w", err)
	}
	return price, nil
}
