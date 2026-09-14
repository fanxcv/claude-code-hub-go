import { describe, expect, test } from "vitest";
import {
  compareTotalUsageDesc,
  formatTotalUsage,
  toTotalUsage,
} from "@/app/[locale]/dashboard/quotas/users/_components/total-usage";

/**
 * 累计成本三态呈现的契约测试。
 *
 * 背景（线上实测缺陷）：这一列原先硬编码为 0，而它显示的是**一个确定的错数**——
 * 「累计成本 $0.00」「额度进度条恒 0%」「按成本排序等价于不排序」三处同时失真，
 * 且界面看上去一切正常。故此处钉住：**读不到就说读不到，绝不退化成 0**。
 */
describe("累计成本三态", () => {
  test("toTotalUsage：只有有限数字才算读数，其余一律「不可用」", () => {
    expect(toTotalUsage(1.5)).toBe(1.5);
    expect(toTotalUsage(0)).toBe(0);
    expect(toTotalUsage(undefined)).toBeNull();
    expect(toTotalUsage(Number.NaN)).toBeNull();
    expect(toTotalUsage(Number.POSITIVE_INFINITY)).toBeNull();
  });

  test("formatTotalUsage：null 走「读数不可用」文案，数字走金额格式", () => {
    expect(formatTotalUsage(null, "USD", "读数不可用")).toBe("读数不可用");
    expect(formatTotalUsage(12.5, "USD", "读数不可用")).toContain("12.5");
    expect(formatTotalUsage(0, "USD", "读数不可用")).not.toBe("读数不可用");
  });

  test("排序：已知读数按金额降序，未知一律排在已知之后", () => {
    expect(compareTotalUsageDesc(5, 1)).toBeLessThan(0);
    expect(compareTotalUsageDesc(1, 5)).toBeGreaterThan(0);
    expect(compareTotalUsageDesc(null, null)).toBe(0);
    // 未知不得被当成 0 参与比较：否则「读不到」会与「确实没花钱」同序。
    expect(compareTotalUsageDesc(null, 0.01)).toBeGreaterThan(0);
    expect(compareTotalUsageDesc(0.01, null)).toBeLessThan(0);

    const sorted = [{ total: null }, { total: 1 }, { total: 9 }, { total: null }].sort(
      (left, right) => compareTotalUsageDesc(left.total, right.total)
    );
    expect(sorted.map((item) => item.total)).toEqual([9, 1, null, null]);
  });
});
