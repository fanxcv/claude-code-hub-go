import { DASHBOARD_COMPAT_HEADER } from "@/lib/api/v1/_shared/constants";
import { apiPost } from "./_compat";

/**
 * 批量累计成本读数（用户/密钥两维）。
 *
 * 后端是 Go-only 端点（Node 侧没有对应路由，Node 版靠 SSR 直接查库算）：
 *   POST /api/v1/users:costBatch  → { items: [{ id, totalCost }] }
 *   POST /api/v1/keys:costBatch   → { items: [{ id, totalCost }] }
 * 取数口径（表、计费条件、时间上限、重置语义）见 ``。
 * 两处刻意选择：
 *  1. **totalCost 在网线上是 numeric 文本**（`"1.500000000000000"`），后端不在取数层转 float64；
 *     这里在展示层入口统一 `Number(...)`，与页面其它金额字段同类型。
 *  2. **单请求上限 200**，故本模块**自己分片**（页面一次可要 2000 个用户）。
 *     分片是必须的：后端对超限是 400 `too_big` 而**不是静默截断**——静默截断会让页面
 *     把「没查到的实体」显示成 0，也就是一个确定的错数。
 */

const MAX_IDS_PER_REQUEST = 200;

const dashboardCompatOptions = {
  headers: {
    [DASHBOARD_COMPAT_HEADER]: "1",
  },
} as const;

type CostBatchResponse = {
  items?: Array<{ id?: number; totalCost?: string | number }>;
};

async function fetchCostBatch(path: string, ids: number[]): Promise<Map<number, number>> {
  const unique = [...new Set(ids)];
  const totals = new Map<number, number>();
  for (let start = 0; start < unique.length; start += MAX_IDS_PER_REQUEST) {
    const chunk = unique.slice(start, start + MAX_IDS_PER_REQUEST);
    const body = await apiPost<CostBatchResponse>(path, { ids: chunk }, dashboardCompatOptions);
    for (const item of body?.items ?? []) {
      if (typeof item?.id !== "number") continue;
      totals.set(item.id, Number(item.totalCost ?? 0));
    }
  }
  return totals;
}

/** 用户维度累计成本（键为 user id）。失败时抛错，由调用方决定降级呈现。 */
export function getUserCostBatch(userIds: number[]): Promise<Map<number, number>> {
  return fetchCostBatch("/api/v1/users:costBatch", userIds);
}

/** 密钥维度累计成本（键为 key id）。失败时抛错，由调用方决定降级呈现。 */
export function getKeyCostBatch(keyIds: number[]): Promise<Map<number, number>> {
  return fetchCostBatch("/api/v1/keys:costBatch", keyIds);
}
