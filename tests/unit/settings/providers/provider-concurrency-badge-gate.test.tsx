/**
 * @vitest-environment happy-dom
 *
 * `ProviderConcurrencyBadge` 的**轮询闸门**契约。
 *
 * 用户要求「打开才统计显示，每 5s 刷新一次，关上就完全不占用资源」。轮询原先挂在
 * `provider-manager-loader` 上，导致每 5 秒整页链路重渲染（用户报的「整个页面刷新」），
 * 现已下沉到本叶子组件独占的 `providers-health-live`。故这条契约的**唯一**钉子放在这里：
 *  1. 开关开：`enabled` 为真、`refetchInterval` 为 5000；
 *  2. 开关关：`enabled` 为假**且** `refetchInterval` 为假——两道闸门都给，且请求根本不发；
 *  3. 本组件**不**自己取设置（开关只有一个真源，由页面级查询透传），否则每行都会多一条设置订阅。
 *
 * 手法：整体替换 `useQuery` 把每次调用的选项记下来（不真跑查询），据此断言闸门——比渲染后数请求
 * 次数更稳，且**直接**钉住传参。
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";

const queryOptions = new Map<string, Record<string, unknown>>();

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey: unknown[] } & Record<string, unknown>) => {
    queryOptions.set(String(options.queryKey?.[0] ?? "?"), options);
    return { data: undefined, isLoading: false, isFetching: false };
  },
}));

vi.mock("next-intl", () => ({
  useTranslations: (namespace: string) => (key: string) => `${namespace}.${key}`,
}));

vi.mock("@/lib/api-client/v1/actions/providers", () => ({
  getProvidersHealthStatus: vi.fn(async () => ({})),
}));

import { ProviderConcurrencyBadge } from "@/app/[locale]/settings/providers/_components/provider-concurrency-badge";

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

function liveOptions(): Record<string, unknown> {
  const options = queryOptions.get("providers-health-live");
  if (!options) throw new Error("未捕获到 providers-health-live 查询（徽标结构变了？）");
  return options;
}

const BASE_PROPS = { providerId: 156, limit: 20, isEnabled: true };

describe("ProviderConcurrencyBadge 的轮询闸门", () => {
  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
    queryOptions.clear();
  });

  it("开关开：enabled 为真、每 5 秒轮询，且带 queryFn", () => {
    const unmount = render(<ProviderConcurrencyBadge {...BASE_PROPS} liveStatsEnabled={true} />);
    expect(liveOptions().enabled).toBe(true);
    expect(liveOptions().refetchInterval).toBe(5_000);
    expect(liveOptions().queryFn).toBeTypeOf("function");
    unmount();
  });

  it("开关关：enabled 与 refetchInterval 双双为假（连请求都不发）", () => {
    const unmount = render(<ProviderConcurrencyBadge {...BASE_PROPS} liveStatsEnabled={false} />);
    expect(liveOptions().enabled).toBe(false);
    expect(liveOptions().refetchInterval).toBe(false);
    unmount();
  });

  it("开关只有一个真源：本组件不订阅 system-settings", () => {
    const unmount = render(<ProviderConcurrencyBadge {...BASE_PROPS} liveStatsEnabled={true} />);
    expect(queryOptions.has("system-settings")).toBe(false);
    unmount();
  });
});
