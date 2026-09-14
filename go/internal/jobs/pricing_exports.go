package jobs

import (
	"context"
	"errors"
	"fmt"
)

// 本文件是**给管理面 HTTP 端点**用的导出面（model-prices 三兄弟）。
//
// 为什么单独一个文件、且不改动 pricesync.go / cpt_convert.go：
//   - `PriceDataEqual` / `LoadConvertedTable` / `SyncNow` 针对既有私有实现各写一遍，
//     会让「钱路」上出现第二份实现（将来修正一处、漏一处就是静默分叉）；
//   - 同包新增文件可以在不碰既有文件的前提下复用私有实现，改动面最小。
//
// 与后台任务的差别（两者都必须存在）：
//   - 后台任务（PriceSyncer.Task/RunOnce）：有 leader 锁、有版本短路、有节流；
//   - 端点（SyncNow）：**没有**锁与短路——Node 的 `syncLiteLLMPrices`（actions/model-prices.ts:518）
//     就是直接 load + apply，用户点「立即同步」时预期的是真的重放一遍整表。

// PriceDataEqual 复刻 actions/model-prices.ts:42 的 isPriceDataEqual：
// 键序无关、丢弃原型污染键的稳定比较（云价格写入的「是否需要更新」判定用它）。
func PriceDataEqual(left, right any) bool { return priceDataEqual(left, right) }

// LoadConvertedTable 拉取并转换云端 CPT 表（syncLitellmCheck 用：只读、不落库）。
func (s *PriceSyncer) LoadConvertedTable(ctx context.Context) (*ConvertedCptTable, error) {
	rawTable, err := s.fetchTable(ctx)
	if err != nil {
		return nil, err
	}
	table, err := ParseCptTable(rawTable)
	if err != nil {
		return nil, err
	}
	return ConvertCptTable(table), nil
}

// SyncNow 复刻 syncLiteLLMPrices → applyConvertedCloudPriceTable 的即时同步：
// 无 leader 锁、无版本短路，整表写入 + 清理云端下线行 + 写目录。
//
// 与 background 任务同样的两条「只告警不失败」：清理与目录写入失败不把整轮判失败
// （价格已入库，收尾失败不该让用户看到失败）。
func (s *PriceSyncer) SyncNow(ctx context.Context) (*PriceUpdateResult, error) {
	converted, err := s.LoadConvertedTable(ctx)
	if err != nil {
		return nil, err
	}
	// 与 Node 同口径：转换结果为空会退化成「清空全部非 manual 行」，必须拒绝。
	if len(converted.Models) == 0 {
		return nil, errors.New("云端价格表转换结果为空模型集,跳过同步以避免误删现有价格")
	}

	result, err := s.apply(ctx, converted)
	if err != nil {
		return nil, fmt.Errorf("云端价格表写入失败：%w", err)
	}
	if result == nil {
		return nil, errors.New("云端价格表写入失败：返回结果为空")
	}

	if removed, cleanupErr := s.store.DeleteCloudPricesNotIn(ctx, modelNames(converted.Models)); cleanupErr != nil {
		s.logger.Warn("price_sync_stale_cleanup_failed", map[string]any{"error": cleanupErr.Error()})
	} else if removed > 0 {
		s.logger.Info("price_sync_stale_rows_removed", map[string]any{"removed": removed})
	}

	if err := s.writeCatalog(ctx, converted); err != nil {
		s.logger.Warn("price_sync_catalog_write_failed", map[string]any{"error": err.Error()})
	}

	return result, nil
}
