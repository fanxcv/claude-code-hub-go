import { type CurrencyCode, formatCurrency } from "@/lib/utils/currency";

/**
 * 累计成本（`totalUsage`）的三态呈现。
 *
 * 为什么不是普通数字：这一列的值来自**批量成本端点**（Go-only，
 * 见 `src/lib/api-client/v1/actions/cost-batch.ts`）。取数失败时**不能退化成 0**——
 * 0 是一个确定的错数，会让「额度进度条恒 0%」「按成本排序等价于不排序」同时失真，
 * 而且看上去一切正常。故类型是 `number | null`：`null` 表示**读数不可用**，
 * 界面必须显式说出来（见 `useTotalUsageText`）。
 */
export type TotalUsageValue = number | null;

/** 取数成功即给数字，失败即 null（调用方把异常收在这里，避免每个渲染点各判一次）。 */
export function toTotalUsage(value: number | undefined): TotalUsageValue {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

/** 数字则格式化金额，null 则给定文案（i18n 由调用方传入）。 */
export function formatTotalUsage(
  value: TotalUsageValue,
  currencyCode: CurrencyCode,
  unavailableLabel: string
): string {
  return value === null ? unavailableLabel : formatCurrency(value, currencyCode);
}

/**
 * 排序比较：已知读数按金额降序，**未知读数一律排在已知之后**。
 *
 * 不把 null 当 0 的理由同上——那会让「读不到」与「真的没花钱」在排序上等价，
 * 而这两件事在配额页上恰好是最需要区分的。
 */
export function compareTotalUsageDesc(left: TotalUsageValue, right: TotalUsageValue): number {
  if (left === null && right === null) return 0;
  if (left === null) return 1;
  if (right === null) return -1;
  return right - left;
}
