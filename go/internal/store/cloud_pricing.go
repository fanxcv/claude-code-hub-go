package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// 本文件是**后台任务**（internal/jobs 的云价格同步）需要的 model_prices / cloud_pricing_catalog 读写面。
//
// 与 admin_model_prices.go 的分工：那里是管理面（单模型 CRUD、分页），这里是整表同步需要的
// 批量读与整表清理。两边共用同一份 SQL 形状常量（modelPriceProjection），避免投影分叉。
//
// 唯一真源：
//   - src/repository/model-price.ts（findAllManualPrices / findAllLatestPrices /
//     createModelPrice / upsertModelPrice / deleteCloudPricesNotIn / countCloudModelPrices）
//   - src/repository/cloud-pricing-catalog.ts（upsertCloudPricingCatalog / getCloudPricingCatalog）
//
// 分道：读走 control，写走 writer。

// PriceSyncExistingRow 是「每模型最新一行」在同步侧需要的字段。
//
// 只取这三列（不取 id/createdAt/updatedAt）：同步只需要判断「来源是否变化」与「价格是否变化」。
// PriceData 保留**已解析**的通用结构而不是原始字节：Node 侧比对的是解析后的对象经
// JSON.stringify 的文本（src/actions/model-prices.ts:42 isPriceDataEqual），比对必须在
// 数值语义上进行，故这里也解析成 any。
type PriceSyncExistingRow struct {
	ModelName string
	Source    string
	PriceData any
}

// ListManualPriceModelNames 复刻 findAllManualPrices 的**键集合**。
//
// Node 返回 Map<modelName, ModelPrice>，调用点只做 `manualPrices.has(modelName)`（手动价保护），
// 故这里只取名字；差异（不返回整行）已记入 README 的白名单。
func (p *Pools) ListManualPriceModelNames(ctx context.Context) (map[string]struct{}, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT ON (model_name) model_name FROM model_prices
		WHERE source = 'manual'
		ORDER BY model_name, created_at DESC NULLS LAST, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询手动价格清单失败: %w", err)
	}
	defer rows.Close()

	names := make(map[string]struct{}, 64)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("store: 读动手动价格行失败: %w", err)
		}
		names[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历手动价格清单失败: %w", err)
	}
	return names, nil
}

// ListLatestPriceRowsForSync 复刻 findAllLatestPrices（每模型一行、manual 优先）。
//
// 顺序由 SQL 决定：model_name, (source='manual') DESC, created_at DESC NULLS LAST, id DESC。
// 这里与 Node 一样把整表读进内存（约 1.1 万行），不做 N+1 查询。
func (p *Pools) ListLatestPriceRowsForSync(ctx context.Context) (map[string]PriceSyncExistingRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT model_name, source, price_data::text FROM (
		SELECT DISTINCT ON (model_name) model_name, source, price_data, created_at, id
		FROM model_prices
		ORDER BY model_name, (source = 'manual') DESC, created_at DESC NULLS LAST, id DESC
	) t`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询最新价格表失败: %w", err)
	}
	defer rows.Close()

	// 容量按 Node 侧实测规模给（约 1.1 万模型），避免反复扩容。
	existing := make(map[string]PriceSyncExistingRow, 12000)
	for rows.Next() {
		var name, source, priceData string
		if err := rows.Scan(&name, &source, &priceData); err != nil {
			return nil, fmt.Errorf("store: 读取最新价格行失败: %w", err)
		}
		var decoded any
		if err := json.Unmarshal([]byte(priceData), &decoded); err != nil {
			return nil, fmt.Errorf("store: 解析价格数据失败（model=%s）: %w", name, err)
		}
		existing[name] = PriceSyncExistingRow{ModelName: name, Source: source, PriceData: decoded}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历最新价格行失败: %w", err)
	}
	return existing, nil
}

// InsertModelPrice 复刻 createModelPrice：单行插入并返回新行 id。
func (p *Pools) InsertModelPrice(
	ctx context.Context,
	modelName string,
	priceData []byte,
	source string,
) (int64, error) {
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	var id int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO model_prices (model_name, price_data, source) VALUES ($1, $2::jsonb, $3)
		 RETURNING id`, modelName, string(priceData), source).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: 写入模型价格失败: %w", err)
	}
	return id, nil
}

// ModelPriceWrite 是批量写入的一项（整表同步与手动导入共用）。
//
// PriceData 用 json.RawMessage 而不是 string：调用方手里本来就是编码后的 JSON，
// 这里再转一次字符串只会多一次拷贝。
type ModelPriceWrite struct {
	ModelName string
	PriceData json.RawMessage
	Source    string
}

// priceWriteColumns 把写入项摊成三条并行数组（unnest 的入参形态）。
//
// 顺序即行顺序：数组下标 i 的三个元素属于同一行。
func priceWriteColumns(writes []ModelPriceWrite) (names []string, payloads []string, sources []string) {
	names = make([]string, len(writes))
	payloads = make([]string, len(writes))
	sources = make([]string, len(writes))
	for index, write := range writes {
		names[index] = write.ModelName
		payloads[index] = string(write.PriceData)
		sources[index] = write.Source
	}
	return names, payloads, sources
}

// InsertModelPrices 复刻 N 次 createModelPrice，但只花一次往返。
//
// 语义等价性：单条 `INSERT ... SELECT unnest(...)` 本身是原子的——任一行非法则整条失败且
// 一行不落库，与「逐条插入、每条各自事务」在**全部成功**时结果相同。调用方（价格同步与手动
// 导入）在批量失败后会退化成逐条写，以保持「哪个模型失败」的精确归因，故失败路径也与改前一致。
func (p *Pools) InsertModelPrices(ctx context.Context, writes []ModelPriceWrite) error {
	if len(writes) == 0 {
		return nil
	}
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	names, payloads, sources := priceWriteColumns(writes)
	if _, err := pool.Exec(ctx,
		`INSERT INTO model_prices (model_name, price_data, source)
		 SELECT u.model_name, u.price_data::jsonb, u.source
		 FROM unnest($1::text[], $2::text[], $3::text[]) AS u(model_name, price_data, source)`,
		names, payloads, sources); err != nil {
		return fmt.Errorf("store: 批量写入模型价格失败: %w", err)
	}
	return nil
}

// ReplaceModelPrices 复刻 N 次 upsertModelPrice（同一事务里先删该模型全部旧行、再插一行），
// 但把 N 个模型合到**一个事务**里：两条语句、四次往返，而不是每个模型四条。
//
// 为什么仍是「先删后插」而不是 ON CONFLICT：与 AdminUpsertModelPrice 同一口径——Node 侧就是硬删除，
// 改成真 upsert 会让旧行（含 id 与历史）留存，两端行数与 id 分叉。
//
// 原子范围的变化：改前每个模型一个事务，改后整批一个事务。成功路径结果相同（每个模型都是
// 「删净再插一行」）；失败时整批回滚，由调用方退化为逐条写来恢复「部分成功 + 精确归因」。
// 同一批内出现同名模型时，删除只做一次、插入按出现次数落行，与改前「每个模型各删一次再插」
// 的终态一致（都是每个出现一次落一行）。
func (p *Pools) ReplaceModelPrices(ctx context.Context, writes []ModelPriceWrite) error {
	if len(writes) == 0 {
		return nil
	}
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启模型价格批量事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	names, payloads, sources := priceWriteColumns(writes)
	if _, err := tx.Exec(ctx, "DELETE FROM model_prices WHERE model_name = ANY($1::text[])", names); err != nil {
		return fmt.Errorf("store: 清理旧模型价格失败: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO model_prices (model_name, price_data, source)
		 SELECT u.model_name, u.price_data::jsonb, u.source
		 FROM unnest($1::text[], $2::text[], $3::text[]) AS u(model_name, price_data, source)`,
		names, payloads, sources); err != nil {
		return fmt.Errorf("store: 批量替换模型价格失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交模型价格批量事务失败: %w", err)
	}
	return nil
}

// DeleteCloudPricesNotIn 复刻 deleteCloudPricesNotIn：删除不在保留列表中的非 manual 行。
//
// 空保留列表直接返回 0（Node 同口径）：否则等同于清空全部非 manual 行。
// 数组参数用 pgx 的原生 []string → text[]，对应 Node 的 sql.param 单数组绑定。
func (p *Pools) DeleteCloudPricesNotIn(ctx context.Context, keepModelNames []string) (int64, error) {
	if len(keepModelNames) == 0 {
		return 0, nil
	}
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, `DELETE FROM model_prices
		WHERE source <> 'manual' AND NOT (model_name = ANY($1))`, keepModelNames)
	if err != nil {
		return 0, fmt.Errorf("store: 清理过期云端价格失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// CountCloudModelPrices 复刻 countCloudModelPrices：非 manual 来源的去重模型数。
func (p *Pools) CountCloudModelPrices(ctx context.Context) (int, error) {
	pool, err := p.Control()
	if err != nil {
		return 0, err
	}
	var total int
	if err := pool.QueryRow(ctx, `SELECT COUNT(DISTINCT model_name) FROM model_prices
		WHERE source <> 'manual'`).Scan(&total); err != nil {
		return 0, fmt.Errorf("store: 统计云端价格行数失败: %w", err)
	}
	return total, nil
}

// CloudPricingCatalogInput 复刻 upsertCloudPricingCatalog 的入参。
//
// Providers 与 Vendors 用 json.RawMessage 原样透传：它们的形状由 internal/jobs 的转换器
// 决定，store 不做结构与语义加工（与 Node 侧把对象直接交给 drizzle 的 jsonb 列一致）。
type CloudPricingCatalogInput struct {
	Version     string
	Currency    string
	RefreshedAt *string
	Providers   json.RawMessage
	Vendors     json.RawMessage
	ModelCount  int
}

// UpsertCloudPricingCatalog 复刻 upsertCloudPricingCatalog：同一事务里清空再插入单行。
//
// 不是 PG 的 ON CONFLICT：Node 侧就是 DELETE + INSERT（表只保留最新一份），
// 改成 upsert 会让并发同步下的行数与 id 分叉。
func (p *Pools) UpsertCloudPricingCatalog(ctx context.Context, input CloudPricingCatalogInput) error {
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: 开启目录事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "DELETE FROM cloud_pricing_catalog"); err != nil {
		return fmt.Errorf("store: 清空价格目录失败: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO cloud_pricing_catalog (version, currency, refreshed_at, providers, vendors, model_count)
		 VALUES ($1, $2, $3::timestamptz, $4::jsonb, $5::jsonb, $6)`,
		input.Version, input.Currency, input.RefreshedAt, string(input.Providers), string(input.Vendors),
		input.ModelCount); err != nil {
		return fmt.Errorf("store: 写入价格目录失败: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: 提交价格目录事务失败: %w", err)
	}
	return nil
}

// CloudPricingCatalogRow 是 cloud_pricing_catalog 的最新一行。
//
// Vendors 是原样透传的 jsonb（Node 的 getCloudPricingCatalog 也是把该列直接当 JS 值给出），
// 管理面 /api/prices/vendors 的优先分支按它是否非空数组决定是否短路降级查询。
type CloudPricingCatalogRow struct {
	Version    string
	Currency   string
	Vendors    json.RawMessage
	ModelCount int
}

// GetCloudPricingCatalog 复刻 getCloudPricingCatalog：并发同步可能残留多行，固定取最新一条。
//
// 表未迁移等场景 Node 返回 null 且不阻断调用方；本实现把「无行」与「查询失败」分开返回，
// 由调用方决定是否降级（jobs 侧按 Node 语义降级）。
func (p *Pools) GetCloudPricingCatalog(ctx context.Context) (*CloudPricingCatalogRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	var row CloudPricingCatalogRow
	err = pool.QueryRow(ctx,
		`SELECT version, currency, vendors, model_count FROM cloud_pricing_catalog ORDER BY id DESC LIMIT 1`).
		Scan(&row.Version, &row.Currency, &row.Vendors, &row.ModelCount)
	if err != nil {
		if isNoRows(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: 读取价格目录失败: %w", err)
	}
	return &row, nil
}
