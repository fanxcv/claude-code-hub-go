import { describe, expect, test } from "vitest";
import { CreateProviderSchema, UpdateProviderSchema } from "@/lib/validation/schemas";

// 本文件钉住低速参数的**单位契约**（用户 2026-09-22 裁决改单位）与其 REST 侧的不变量：
//
//   1. window 存**分钟**（列是 slow_rate_window_minutes，payload 名沿旧叫 ..._seconds）
//   2. baseline 存**天**（列是 slow_rate_baseline_window_days）
//   3. ratio 存 **0-1 小数**（列是 slow_rate_ratio，payload 名沿旧叫 ..._per_mille）
//
// 为何前端也要钉：单位是「数值语义」而不是类型，类型系统看不见它。把 30 分钟送成 30 秒、
// 或把 0.3 当千分比送 300，两边都能通过类型检查，只有运行时行为错——且是**静默**的
// （判定恒真或恒假，不报错）。
//
// 为何同时钉 payload 名：用户裁决「列名换新、REST 字段名沿旧」。前端若跟着改成新名，
// Go 的白名单会 422；若只改一半，界面能填但字段没人写（无声旋钮）。

const baseProvider = { name: "wb", url: "https://example.com", key: "sk-placeholder" };

describe("创建 schema - 低速参数单位", () => {
  test("接受 0-1 小数系数（含旧千分比习惯值必须被拒）", () => {
    const accepted = CreateProviderSchema.parse({
      ...baseProvider,
      slow_rate_ratio_per_mille: 0.3,
    });
    expect(accepted.slow_rate_ratio_per_mille).toBe(0.3);

    // 上界 1 合法（区间上端是闭的）。
    expect(
      CreateProviderSchema.parse({ ...baseProvider, slow_rate_ratio_per_mille: 1 })
        .slow_rate_ratio_per_mille
    ).toBe(1);

    // 300 是旧千分比习惯值——必须被拒，否则低速线变成基线的 30000% 且静默恒假。
    for (const bad of [300, 1000, 1.5]) {
      expect(() =>
        CreateProviderSchema.parse({ ...baseProvider, slow_rate_ratio_per_mille: bad })
      ).toThrow();
    }
  });

  test("窗口与基线窗口接受分钟/天的整数，且可空", () => {
    const parsed = CreateProviderSchema.parse({
      ...baseProvider,
      slow_rate_window_seconds: 30,
      slow_rate_baseline_window_seconds: 3,
    });
    expect(parsed.slow_rate_window_seconds).toBe(30);
    expect(parsed.slow_rate_baseline_window_seconds).toBe(3);

    // 可空：null 表示取代码默认值（列可空）。
    const nullable = CreateProviderSchema.parse({
      ...baseProvider,
      slow_rate_window_seconds: null,
      slow_rate_baseline_window_seconds: null,
      slow_rate_ratio_per_mille: null,
    });
    expect(nullable.slow_rate_window_seconds).toBeNull();
    expect(nullable.slow_rate_baseline_window_seconds).toBeNull();
    expect(nullable.slow_rate_ratio_per_mille).toBeNull();
  });

  test("新参数 slow_rate_recovery_requests 可空且接受正整数", () => {
    expect(
      CreateProviderSchema.parse({ ...baseProvider, slow_rate_recovery_requests: 10 })
        .slow_rate_recovery_requests
    ).toBe(10);
    expect(
      CreateProviderSchema.parse({ ...baseProvider, slow_rate_recovery_requests: null })
        .slow_rate_recovery_requests
    ).toBeNull();
  });

  test("声明的是旧 payload 名，不声明新名（改名会让 Go 白名单 422）", () => {
    // 该 schema 不启用 .strict()（未知键被剥除而不是报错），所以拿**声明面**判：
    // 旧名在解析结果里有键、新名没有。这样即使有人改了名、测试也不会因「剥除」而假绿。
    const parsed = CreateProviderSchema.parse({
      ...baseProvider,
      slow_rate_window_seconds: 30,
      slow_rate_ratio_per_mille: 0.3,
    });
    expect(parsed).toHaveProperty("slow_rate_window_seconds");
    expect(parsed).toHaveProperty("slow_rate_ratio_per_mille");
    expect(parsed).not.toHaveProperty("slow_rate_window_minutes");
    expect(parsed).not.toHaveProperty("slow_rate_ratio");
    // 新名即使被提交也不会进入解析结果（会被剥除），故不会静默发出去。
    const withNewNames = CreateProviderSchema.parse({
      ...baseProvider,
      slow_rate_window_minutes: 30,
      slow_rate_ratio: 0.3,
    });
    expect(withNewNames).not.toHaveProperty("slow_rate_window_minutes");
    expect(withNewNames).not.toHaveProperty("slow_rate_ratio");
  });
});

describe("更新 schema - 低速参数单位", () => {
  test("与创建 schema 同口径（0-1 小数、分钟/天、可空）", () => {
    const parsed = UpdateProviderSchema.parse({
      slow_rate_window_seconds: 15,
      slow_rate_baseline_window_seconds: 7,
      slow_rate_ratio_per_mille: 0.25,
      slow_rate_recovery_requests: 10,
    });
    expect(parsed.slow_rate_window_seconds).toBe(15);
    expect(parsed.slow_rate_baseline_window_seconds).toBe(7);
    expect(parsed.slow_rate_ratio_per_mille).toBe(0.25);
    expect(parsed.slow_rate_recovery_requests).toBe(10);

    for (const bad of [2, 300, -0.1]) {
      expect(() => UpdateProviderSchema.parse({ slow_rate_ratio_per_mille: bad })).toThrow();
    }
  });
});
