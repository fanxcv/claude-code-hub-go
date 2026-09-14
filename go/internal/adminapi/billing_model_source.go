package adminapi

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// billingModelSourceDefault 是「计费模型来源」在**设置行缺失**时的缺省值。
//
// 取值域与缺省值均以 Node 为契约（四处独立证据，互相印证）：
//
//  1. 类型域：`src/types/system-config.ts:5`
//     `export type BillingModelSource = "original" | "redirected";`
//     —— **没有 "model"** 这个取值。
//  2. DB 列默认：`src/drizzle/schema.ts:898`
//     `billingModelSource: varchar('billing_model_source', { length: 20 }).notNull().default('original')`
//  3. 读侧兜底：`src/repository/_shared/transformers.ts:275`
//     `billingModelSource: dbSettings?.billingModelSource ?? "original"`
//     —— 设置行缺失（`dbSettings` 为 null）时给 `"original"`。
//  4. Node 自己的测试：`src/repository/_shared/transformers.test.ts:281`
//     `expect(result.billingModelSource).toBe("original")`。
//
// 另有 `src/repository/system-config.ts:150,566,591` 三处默认对象同样是 `"original"`。
// 故 Go 在设置行缺失时**必须**也是 `"original"`：否则排行榜与用户洞察会按「重定向后模型」
// 聚合，而 Node 按「重定向前模型」——同一页面的数字与主显示模型都会不一致。
const billingModelSourceDefault = "original"

// resolveBillingModelSource 解出本次查询该用的计费模型来源。
//
// 逐字复刻 Node 的取值链（`dbSettings?.billingModelSource ?? "original"`）：
//   - 设置行缺失（nil）→ `"original"`；
//   - 设置行在 → **原样透传其值**，含空串。
//
// 为何空串不做兜底：Node 用的是 `??`，**只覆盖 null/undefined，不覆盖空串**；空串会原样
// 传进 `billingModelSource === "original" ? originalModel : model`，即「优先 model 列」。
// 若把空串也兜成 `"original"`，就会与该分支相反，凭空造出一处新分歧。
// （生产库该列为 NOT NULL 且有默认值，空串实际不可达；此处按可达语义写，以免哪天手工改库时静默走错。）
//
// 为何不 Trim：Node 比的是原始字符串，`" original "` 在 Node 眼里**不是** original（会优先
// model 列）。这里保持同一口径，不做「善意归一」——归一化差异正是本仓反复踩过的坑。
func resolveBillingModelSource(settings *store.SystemSettings) string {
	if settings == nil {
		return billingModelSourceDefault
	}
	return settings.BillingModelSource
}
