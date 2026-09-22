/**
 * @vitest-environment happy-dom
 *
 * `provider-manager-loader` 在**局部更新改造后**的轮询/指示器契约。
 *
 * 用户实报「并发数刷新时整个页面都在刷新」。根因是轮询挂在 loader 的 `providers-health` 上：
 * 同一个 queryKey 的任何观察者重取落库，都会让 loader 重渲染 ⇒ 整条链路（manager → list → 全部
 * 列表项）重渲染，且 `isHealthFetching` 让列表上方的加载条反复挂载/卸载。故轮询已下沉到叶子徽标
 * 独占的 `providers-health-live`（见 `provider-concurrency-badge.tsx` 的说明）。
 *
 * 本文件钉住 loader 这一侧的**反向**契约：
 *  1. loader 的 health 查询**不轮询**——一旦恢复，链路的每 5 秒全量重渲染就回来了；
 *  2. loader **不订阅** `providers-health-live`——轮询必须留在叶子，这是「只有徽标重渲染」的
 *     结构性保证（不是靠约定）；
 *  3. `refreshing` **不含** health 的 fetch——它正是列表每 5 秒上下跳的直接原因。
 *
 * 手法沿用原版：拦截 `useQuery` 记下每次调用的选项（不真跑查询），比数请求次数更稳；
 * 另把 `ProviderManager` 换成会记下 props 的替身，用于断言 `refreshing`。
 */

import type { ReactNode } from "react";
import { act, useState } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";

/** 每次 useQuery 调用的选项，按 queryKey 的首元素索引。 */
const queryOptions = new Map<string, Record<string, unknown>>();

/** 各查询的 `isFetching`，由用例按需拨动（默认全假）。 */
const fetchingByKey: Record<string, boolean> = {};

/** `ProviderManager` 收到的 props（用于断言整页刷新指示器）。 */
let managerProps: Record<string, unknown> | null = null;

/** 设置查询的返回值（可变）：`useQuery` 的桩直接从它取，不跑真 queryFn。 */
const settingsPayload = { currencyDisplay: "CNY", providerLiveStatsEnabled: false };

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey: unknown[] } & Record<string, unknown>) => {
    const key = String(options.queryKey?.[0] ?? "?");
    queryOptions.set(key, options);
    const data = key === "system-settings" ? settingsPayload : key === "providers" ? [] : {};
    return { data, isLoading: false, isFetching: fetchingByKey[key] ?? false };
  },
}));

vi.mock("next-intl", () => ({
  useLocale: () => "zh-CN",
  useTranslations: (namespace: string) => (key: string) => `${namespace}.${key}`,
}));

// 子系统与子组件与本文件无关，换成轻量占位——但 queryFn 必须是可识别的假实现。
vi.mock("@/lib/api-client/v1/actions/system-config", () => ({
  getSystemSettings: vi.fn(async () => settingsPayload),
}));
vi.mock("@/lib/api-client/v1/actions/providers", () => ({
  getProviders: vi.fn(async () => []),
  getProvidersHealthStatus: vi.fn(async () => ({})),
  getProviderStatisticsAsync: vi.fn(async () => ({})),
}));

vi.mock("@/app/[locale]/settings/providers/_components/provider-manager", () => ({
  ProviderManager: (props: Record<string, unknown>) => {
    managerProps = props;
    return <div data-testid="provider-manager" />;
  },
}));
vi.mock("@/app/[locale]/settings/providers/_components/add-provider-dialog", () => ({
  AddProviderDialog: () => <div data-testid="add-provider" />,
}));

import { ProviderManagerLoader } from "@/app/[locale]/settings/providers/_components/provider-manager-loader";

/**
 * 强制重渲染的壳。
 *
 * 为什么必需：被替换的 `useQuery` 是**非反应性**的 — — 它只在组件重渲染时被重新调用。
 * 若只改 `fetchingByKey` 而不触发重渲染，`refreshing` 永远不会重算，于是「health 的重取不进指示器」
 * 这条断言会**恒真**（把 `isHealthFetching` 加回去也照样绿）——那是没有分辨力的钉子。
 * 壳子把重渲染变成一个显式动作（`rerender()`），两条断言才有对照。
 */
let rerender: (() => void) | null = null;

function Harness() {
  const [, setTick] = useState(0);
  rerender = () => setTick((tick) => tick + 1);
  return <ProviderManagerLoader />;
}

function render(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => root.render(node));
  return () => {
    act(() => root.unmount());
    container.remove();
  };
}

/** 把开关换成指定值，渲染一次 loader，并等一轮微任务让 queryFn 定下选项。 */
async function renderWithSwitch(enabled: boolean): Promise<() => void> {
  queryOptions.clear();
  managerProps = null;
  settingsPayload.providerLiveStatsEnabled = enabled;
  const unmount = render(<Harness />);
  await act(async () => {
    await Promise.resolve();
  });
  return unmount;
}

/** 改扰某条查询的 fetching 状态，并**强制一次重渲染**让 `refreshing` 重算。 */
async function setFetching(key: string, value: boolean): Promise<void> {
  fetchingByKey[key] = value;
  await act(async () => {
    rerender?.();
    await Promise.resolve();
  });
}

function refetchIntervalOf(key: string): unknown {
  const options = queryOptions.get(key);
  if (!options) throw new Error(`未捕获到 ${key} 查询（loader 结构变了？）`);
  return options.refetchInterval;
}

describe("ProviderManagerLoader 的轮询与刷新指示器契约（局部更新改造后）", () => {
  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
    queryOptions.clear();
    managerProps = null;
    for (const key of Object.keys(fetchingByKey)) delete fetchingByKey[key];
  });

  it("开关开启：loader 的 providers-health 也**不**轮询（轮询已下沉到叶子徽标）", async () => {
    const unmount = await renderWithSwitch(true);
    expect(refetchIntervalOf("providers-health")).toBeFalsy();
    unmount();
  });

  it("开关关闭：loader 的 providers-health 同样不轮询", async () => {
    const unmount = await renderWithSwitch(false);
    expect(refetchIntervalOf("providers-health")).toBeFalsy();
    unmount();
  });

  it("loader 不订阅 providers-health-live（隔离的结构性保证）", async () => {
    const unmount = await renderWithSwitch(true);
    expect(queryOptions.has("providers-health-live")).toBe(false);
    unmount();
  });

  it("关的是轮询，不是取数本身：health 仍带 queryFn 发一次", async () => {
    const unmount = await renderWithSwitch(false);
    expect(queryOptions.has("providers-health")).toBe(true);
    expect(queryOptions.get("providers-health")?.queryFn).toBeTypeOf("function");
    unmount();
  });

  it("health 的重取**不**进整页刷新指示器（列表因此不再每 5 秒上下跳）", async () => {
    const unmount = await renderWithSwitch(true);
    await setFetching("providers-health", true);
    expect(managerProps?.refreshing).toBe(false);
    unmount();
  });

  it("对照：providers 的重取仍会点亮整页刷新指示器", async () => {
    const unmount = await renderWithSwitch(true);
    await setFetching("providers", true);
    expect(managerProps?.refreshing).toBe(true);
    unmount();
  });
});
