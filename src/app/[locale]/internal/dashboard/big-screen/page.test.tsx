/**
 * @vitest-environment happy-dom
 */

import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";

/**
 * 「秒级时钟只重渲自己，不重渲整块看板」的钉子。
 *
 * 页面原先把 `setInterval(..., 1000)` 的 setState 挂在页面组件上：每秒一次状态更新会重渲整棵
 * 子树（含 4 张 recharts 图与把 `currentTime` 当 prop 的请求趋势图）。现在秒级状态只住在
 * `LiveClock` 内部、趋势图按小时派生。
 *
 * 判据：推进 3 秒后图表容器渲染次数为 0，而时钟文本仍在走（改动前同为 3 秒，图表容器渲染
 * 次数即 3 × 每次 tick 的重渲次数，见报告里的前后对照）。
 */

const chartRenderCount = vi.hoisted(() => ({ value: 0 }));
const i18nMock = vi.hoisted(() => ({ t: (key: string) => key }));

vi.mock("next-intl", () => ({
  useTranslations: () => i18nMock.t,
  useLocale: () => "zh-CN",
}));

vi.mock("@/i18n/routing", () => ({
  useRouter: () => ({ push: vi.fn() }),
  usePathname: () => "/internal/dashboard/big-screen",
}));

vi.mock("@/lib/api-client/v1/actions/dashboard-realtime", () => ({
  getDashboardRealtimeData: async () => ({ ok: true, data: {} }),
}));

vi.mock("@/lib/api-client/v1/actions/system-config", () => ({
  getSystemSettings: async () => ({ currencyDisplay: "USD" }),
}));

// 页面自带 2 秒 react-query 轮询：本用例只量「秒级时钟」这一路，数据用固定值。
vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: undefined, error: null, refetch: vi.fn() }),
}));

// framer-motion 在 happy-dom 里会产生一串被取消的动画 rejection（非本用例关注点，且会污染测试退出码）。
vi.mock("framer-motion", async () => {
  const React = await import("react");
  return {
    AnimatePresence: ({ children }: { children?: React.ReactNode }) => children,
    motion: new Proxy(
      {},
      {
        get:
          () =>
          ({ children }: { children?: React.ReactNode }) =>
            React.createElement("div", null, children),
      }
    ),
  };
});

vi.mock("recharts", async () => {
  const React = await import("react");
  const Chart = ({ children }: { children?: React.ReactNode }) => {
    chartRenderCount.value += 1;
    return React.createElement("div", null, children);
  };
  return {
    ResponsiveContainer: Chart,
    AreaChart: Chart,
    PieChart: Chart,
    Area: () => null,
    CartesianGrid: () => null,
    Cell: () => null,
    Legend: () => null,
    Pie: () => null,
    Tooltip: () => null,
    XAxis: () => null,
    YAxis: () => null,
  };
});

import BigScreenPage from "./page";

describe("大屏看板：秒级重渲染范围", () => {
  let container: HTMLElement;
  let root: Root;

  beforeEach(() => {
    vi.useFakeTimers();
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(async () => {
    await act(async () => {
      root.unmount();
    });
    vi.useRealTimers();
    document.body.innerHTML = "";
  });

  test("时钟每秒走，图表容器一次都不重渲", async () => {
    await act(async () => {
      root.render(<BigScreenPage />);
    });

    const clock = container.querySelector(".tabular-nums");
    expect(clock?.textContent).toMatch(/\d{1,2}:\d{2}:\d{2}/);
    const before = clock?.textContent;

    chartRenderCount.value = 0;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    expect(chartRenderCount.value, "每秒重渲整块看板（含图表）").toBe(0);
    expect(clock?.textContent, "时钟须仍在走").not.toBe(before);
  });
});
