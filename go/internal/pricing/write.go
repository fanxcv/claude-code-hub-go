package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// SourceCloud / SourceManual 是 model_prices.source 的两个取值，对应 Node 侧
// processPriceTableInternal 的 source 参数（云端/自动同步 = "cloud"，用户显式上传 = "manual"）。
const (
	SourceCloud  = "cloud"
	SourceManual = "manual"
)

// UpdateStore 是写入分类需要的最小 store 面。
//
// 用接口而不是 *store.Pools：分类逻辑（谁 added / 谁 updated / 谁被 manual 保护跳过）
// 是可以用无库假实现逐条验证的纯逻辑，真库只负责证明 SQL 面可用（与 internal/jobs 的
// priceStore 同一取舍）。*store.Pools 直接满足本接口。
type UpdateStore interface {
	ListManualPriceModelNames(ctx context.Context) (map[string]struct{}, error)
	ListLatestPriceRowsForSync(ctx context.Context) (map[string]store.PriceSyncExistingRow, error)
	InsertModelPrice(ctx context.Context, modelName string, priceData []byte, source string) (int64, error)
	AdminUpsertModelPrice(ctx context.Context, modelName string, priceData json.RawMessage, source string) (store.AdminModelPrice, error)
}

// WriteEntries 复刻 processPriceTableInternal（actions/model-prices.ts:112-247）。
//
// 逐条判定顺序（与 Node 完全同序，顺序本身是可观察语义——例如 `mode` 缺失要在 manual 保护
// **之前**判失败）：
//  1. 名字 trim 后为空 → 不处理（解析阶段已过滤，这里兜底）；
//  2. priceData 不是对象 → failed；
//  3. priceData 缺 `mode` → failed（Node：所有有效价格行都有这个字段）；
//  4. source 非 manual 且该模型是 manual 且未列入 overwriteManual → skippedConflicts + unchanged；
//  5. 库中无该模型 → 插入（source 用入参）→ added；
//  6. 命中但 source 不同或价格不等 → 整行替换（先删后插）→ updated；
//  7. 其余 → unchanged。
//
// 返回值与 Node 的 PriceUpdateResult 同形（skippedConflicts 恒为数组而非 null）。
func WriteEntries(
	ctx context.Context,
	st UpdateStore,
	entries []Entry,
	source string,
	overwriteManual []string,
	logger *logx.Logger,
) (*jobs.PriceUpdateResult, error) {
	manualNames, err := st.ListManualPriceModelNames(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取手动价格清单失败：%w", err)
	}
	existingRows, err := st.ListLatestPriceRowsForSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取现有价格失败：%w", err)
	}

	overwrite := make(map[string]struct{}, len(overwriteManual))
	for _, name := range overwriteManual {
		overwrite[strings.TrimSpace(name)] = struct{}{}
	}

	result := &jobs.PriceUpdateResult{
		Added:            make([]string, 0, len(entries)/2),
		Updated:          make([]string, 0, 64),
		Unchanged:        make([]string, 0, len(entries)/2),
		Failed:           make([]string, 0, 8),
		Total:            len(entries),
		SkippedConflicts: make([]string, 0, 8),
	}

	for _, entry := range entries {
		// Node 与 manual 记录入库同样用 trim 后的名字，避免云端表里带空白的同名键
		// 绕过本地手动模型的保护检查。
		modelName := strings.TrimSpace(entry.Name)
		if modelName == "" || modelName == metadataFieldSampleSpec {
			continue
		}

		priceData, failReason := decodePriceData(entry.Data)
		if failReason != "" {
			result.Failed = append(result.Failed, modelName)
			logWriteSkip(logger, modelName, failReason)
			continue
		}

		if _, isManual := manualNames[modelName]; isManual {
			// 本地优先：仅当写入来自云端/自动同步时才跳过 manual；用户显式上传（source=manual）
			// 是权威导入，不受此保护。
			if source != SourceManual {
				if _, overridden := overwrite[modelName]; !overridden {
					result.SkippedConflicts = append(result.SkippedConflicts, modelName)
					result.Unchanged = append(result.Unchanged, modelName)
					logWriteSkip(logger, modelName, "manual_protected")
					continue
				}
			}
		}

		existing, found := existingRows[modelName]
		switch {
		case !found:
			if _, err := st.InsertModelPrice(ctx, modelName, entry.Data, source); err != nil {
				result.Failed = append(result.Failed, modelName)
				logWriteError(logger, modelName, "insert_failed", err)
				continue
			}
			result.Added = append(result.Added, modelName)
		case existing.Source != source || !jobs.PriceDataEqual(existing.PriceData, priceData):
			if _, err := st.AdminUpsertModelPrice(ctx, modelName, entry.Data, source); err != nil {
				result.Failed = append(result.Failed, modelName)
				logWriteError(logger, modelName, "upsert_failed", err)
				continue
			}
			result.Updated = append(result.Updated, modelName)
		default:
			result.Unchanged = append(result.Unchanged, modelName)
		}
	}

	return result, nil
}

// decodePriceData 复刻 Node 对单条 priceData 的两项校验（对象 + 必须含 mode）。
//
// 返回规范化后的值与失败原因（空串表示通过）。规范化只做 JSON 往返，使后续与库中行
// （已解成 any）能走同一个等值比较入口。
func decodePriceData(raw json.RawMessage) (any, string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, "price_data_missing"
	}
	switch trimmed[0] {
	case '{':
	case 'n':
		// null：Node 的 `typeof priceData !== "object" || priceData === null` 判失败。
		return nil, "price_data_not_object"
	default:
		return nil, "price_data_not_object"
	}

	var decoded any
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, "price_data_unparsable"
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, "price_data_not_object"
	}
	if _, hasMode := object["mode"]; !hasMode {
		return nil, "price_data_missing_mode"
	}
	return decoded, ""
}

func logWriteSkip(logger *logx.Logger, modelName, reason string) {
	if logger == nil {
		return
	}
	logger.Debug("price_table_entry_skipped", map[string]any{"model": modelName, "reason": reason})
}

func logWriteError(logger *logx.Logger, modelName, reason string, err error) {
	if logger == nil {
		return
	}
	logger.Warn("price_table_entry_failed", map[string]any{
		"model": modelName, "reason": reason, "error": err.Error(),
	})
}

// ConflictItem 对应 checkLiteLLMSyncConflicts 的 conflicts[] 元素。
type ConflictItem struct {
	ModelName   string          `json:"modelName"`
	ManualPrice json.RawMessage `json:"manualPrice"`
	CloudPrice  json.RawMessage `json:"cloudPrice"`
}

// ConflictResult 对应 SyncConflictCheckResult（lib/api/v1/schemas/model-prices.ts）。
type ConflictResult struct {
	HasConflicts bool           `json:"hasConflicts"`
	Conflicts    []ConflictItem `json:"conflicts"`
}

// FindConflicts 复刻 checkLiteLLMSyncConflicts（actions/model-prices.ts:464-512）：
// 以**手动价**为主表，逐个看云端表里是否有同名且带 `mode` 的行；有则算一条冲突。
//
// 两处必须照抄的细节：
//   - 只按 canonical 模型名精确匹配（不做别名回退）；
//   - 云端行必须含 `mode`（Node：`"mode" in cloudPrice`）——纯粹的形状过滤，
//     没有 mode 的云端行不算冲突。
func FindConflicts(
	ctx context.Context,
	st UpdateStore,
	cloudModels map[string]map[string]any,
) (*ConflictResult, error) {
	manualRows, err := st.ListLatestPriceRowsForSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取现有价格失败：%w", err)
	}

	result := &ConflictResult{Conflicts: make([]ConflictItem, 0, 8)}
	for modelName, row := range manualRows {
		if row.Source != SourceManual {
			continue
		}
		cloudPrice, ok := cloudModels[modelName]
		if !ok {
			continue
		}
		if _, hasMode := cloudPrice["mode"]; !hasMode {
			continue
		}
		manualEncoded, err := json.Marshal(row.PriceData)
		if err != nil {
			continue
		}
		cloudEncoded, err := json.Marshal(cloudPrice)
		if err != nil {
			continue
		}
		result.Conflicts = append(result.Conflicts, ConflictItem{
			ModelName:   modelName,
			ManualPrice: manualEncoded,
			CloudPrice:  cloudEncoded,
		})
	}

	// Node 的顺序 = 数据库返回的手动价顺序（findAllManualPrices 的 Map 插入序）。
	// Go 的 map 无序，按模型名排序以保证响应稳定、可比对（顺序差异登记为白名单项）。
	sortConflicts(result.Conflicts)
	result.HasConflicts = len(result.Conflicts) > 0
	return result, nil
}

func sortConflicts(items []ConflictItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && items[j].ModelName < items[j-1].ModelName; j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}
