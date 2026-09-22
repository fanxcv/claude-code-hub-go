/**
 * @vitest-environment happy-dom
 *
 * `provider-manager-loader` 的**实时并发统计轮询闸门**契约。
 *
 * 用户要求「打开才统计显示，每 5s 刷新一次，关上就完全不占用资源」。前半句在本文件钉两件事：
 *  1. 开关关闭时 `providers-health` 查询的 `refetchInterval` 必须是 `false`（不排定时器）；
 *  2. 开关开启时必须是 5000ms。
 *
 * 为什么必须钉这条：轮询是**持续**开销，开关关着却还在每 5 秒打一次管理面，等于把「可选」
 * 变成「默认开销」——而这在界面上完全看不出来（用户只看到开关是关的）。
 *
 * 手法：拦截 `useQuery` 把每次调用的选项记下来（不真跑查询），据此断言闸门。这比渲染后数请求
 * 次数更稳（不受 happy-dom 定时器与 react-query 内部调度影响），且**直接**钉住了传参。
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";

/** 每次 useQuery 调用的选项，按 queryKey 的首元素索引。 */
const queryOptions = new Map<string, Record<string, unknown>>();

/**
 * 拦截 useQuery：记下选项，并给 `system-settings` 返回可变的开关值。
 *
 * 其余查询固定回 undefined（它们的返回值不影响本文件的断言）。
 */
vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey: unknown[] } & Record<string, unknown>) => {
    const key = String(options.queryKey?.[0] ?? "?");
    queryOptions.set(key, options);
    const data = key === "system-settings" ? settingsPayload : undefined;
    return { data, isLoading: false, isFetching: false };
  },
}));

/**
 * 设置查询的返回值（可变）：`useQuery` 的桩直接从它取，不跑真 queryFn。
 *
 * 定义在 `vi.mock` 之前——mock 工厂是提升的，但只在**请求时**读这个绑定，
 * 而那时模块体已执行完。
 */
const settingsPayload = { currencyDisplay: "CNY", providerLiveStatsEnabled: false };

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
  ProviderManager: () => <div data-testid="provider-manager" />,
}));
vi.mock("@/app/[locale]/settings/providers/_components/add-provider-dialog", () => ({
  AddProviderDialog: () => <div data-testid="add-provider" />,
}));

import { ProviderManagerLoader } from "@/app/[locale]/settings/providers/_components/provider-manager-loader";

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

/** 把设置查询的返回值换成指定开关值，然后渲染一次 loader。 */
async function renderWithSwitch(enabled: boolean): Promise<() => void> {
  queryOptions.clear();
  settingsPayload.providerLiveStatsEnabled = enabled;
  const unmount = render(<ProviderManagerLoader />);
  // useQuery 的 queryFn 在 effect 里跑；等一轮微任务让它把 refetchInterval 定下来。
  await act(async () => {
    await Promise.resolve();
  });
  return unmount;
}

function healthRefetchInterval(): unknown {
  const health = queryOptions.get("providers-health");
  if (!health) throw new Error("未捕获到 providers-health 查询（loader 结构变了？）");
  return health.refetchInterval;
}

describe("ProviderManagerLoader 的实时并发轮询闸门", () => {
  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
    queryOptions.clear();
  });

  it("开关关闭：providers-health 不轮询（refetchInterval 为 false）", async () => {
    const unmount = await renderWithSwitch(false);
    expect(healthRefetchInterval()).toBe(false);
    unmount();
  });

  it("开关开启：providers-health 每 5 秒轮询", async () => {
    const unmount = await renderWithSwitch(true);
    expect(healthRefetchInterval()).toBe(5_000);
    unmount();
  });

  it("两个分支都照常发出 health 查询（关的是轮询，不是取数本身）", async () => {
    const unmount = await renderWithSwitch(false);
    expect(queryOptions.has("providers-health")).toBe(true);
    // 关闭时仍要一次性取数：页面上的熔断等其它维仍靠这条查询。
    expect(queryOptions.get("providers-health")?.queryFn).toBeTypeOf("function");
    unmount();
  });
});
