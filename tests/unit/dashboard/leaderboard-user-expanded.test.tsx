/**
 * @vitest-environment happy-dom
 *
 * 排行榜用户视图行内展开区：默认供应商 tab、供应商行懒加载其模型、切到模型 tab 取模型聚合。
 * 展开区产出的是 <tbody> 直接子行（真 <tr>），故渲染容器包在 <table><tbody> 中。
 */
import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import commonMessages from "@messages/en/common.json";
import dashboardMessages from "@messages/en/dashboard.json";
import myUsageMessages from "@messages/en/myUsage.json";
import { LeaderboardUserExpanded } from "@/app/[locale]/dashboard/leaderboard/_components/leaderboard-user-expanded";
import { getDateRangeForPeriod } from "@/app/[locale]/dashboard/leaderboard/_components/date-range-picker";

const mockGetUserInsightsModelBreakdown = vi.hoisted(() => vi.fn());
const mockGetUserInsightsProviderBreakdown = vi.hoisted(() => vi.fn());

vi.mock("@/lib/api-client/v1/actions/admin-user-insights", () => ({
  getUserInsightsModelBreakdown: mockGetUserInsightsModelBreakdown,
  getUserInsightsProviderBreakdown: mockGetUserInsightsProviderBreakdown,
}));

const messages = {
  dashboard: dashboardMessages,
  myUsage: myUsageMessages,
  common: commonMessages,
} as const;

const providerItem = {
  providerId: 11,
  providerName: "Provider Alpha",
  requests: 3,
  cost: 1.5,
  inputTokens: 100,
  outputTokens: 50,
  cacheCreationTokens: 10,
  cacheReadTokens: 5,
};

const modelItem = (model: string) => ({
  model,
  requests: 2,
  cost: 1,
  inputTokens: 80,
  outputTokens: 40,
  cacheCreationTokens: 8,
  cacheReadTokens: 4,
});

let queryClient: QueryClient;

function renderExpanded(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);

  act(() => {
    root.render(
      <QueryClientProvider client={queryClient}>
        <NextIntlClientProvider locale="en" messages={messages} timeZone="UTC">
          <table>
            <tbody>{node}</tbody>
          </table>
        </NextIntlClientProvider>
      </QueryClientProvider>
    );
  });

  return {
    container,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

async function flushMicrotasks() {
  for (let i = 0; i < 5; i++) {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 10));
    });
  }
}

function click(node: Element) {
  act(() => {
    node.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    node.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

// 展开行须是 tbody 直接子行，且与主表列对齐（排名 + 4 列 = 5 格）。
function expectTableRow(row: Element | null, columnCount = 5) {
  expect(row).toBeTruthy();
  expect(row!.tagName).toBe("TR");
  expect(row!.parentElement?.tagName).toBe("TBODY");
  expect(row!.children.length).toBe(columnCount);
}

describe("LeaderboardUserExpanded", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
    });

    mockGetUserInsightsProviderBreakdown.mockResolvedValue({
      ok: true,
      data: { breakdown: [providerItem], currencyCode: "USD" },
    });
    mockGetUserInsightsModelBreakdown.mockImplementation(
      (_userId: number, _start?: string, _end?: string, filters?: { providerId?: number }) => {
        if (filters?.providerId === providerItem.providerId) {
          return Promise.resolve({
            ok: true,
            data: { breakdown: [modelItem("alpha-model")], currencyCode: "USD" },
          });
        }
        return Promise.resolve({
          ok: true,
          data: { breakdown: [modelItem("model-a"), modelItem("model-b")], currencyCode: "USD" },
        });
      }
    );
  });

  afterEach(() => {
    queryClient.clear();
  });

  it("默认渲染供应商 tab，且不预先请求模型聚合", async () => {
    const { container, unmount } = renderExpanded(
      <LeaderboardUserExpanded userId={7} period="daily" columnCount={5} />
    );
    await flushMicrotasks();

    const providerTab = container.querySelector(
      "[data-testid='leaderboard-user-expanded-provider-tab']"
    );
    const modelTab = container.querySelector("[data-testid='leaderboard-user-expanded-model-tab']");
    expect(providerTab?.getAttribute("data-state")).toBe("active");
    expect(modelTab?.getAttribute("data-state")).toBe("inactive");

    const providerList = container.querySelector(
      "[data-testid='leaderboard-user-expanded-provider-list']"
    );
    expectTableRow(providerList);
    expect(providerList?.textContent).toContain("Provider Alpha");
    expect(providerList?.textContent).toContain("3");
    // Token 格：总量 + 缓存命中率（5 / (100 + 10 + 5) = 4.3%）
    expect(providerList?.textContent).toContain(" · 4.3%");

    const { startDate, endDate } = getDateRangeForPeriod("daily", "UTC");
    expect(mockGetUserInsightsProviderBreakdown).toHaveBeenCalledWith(7, startDate, endDate);
    expect(mockGetUserInsightsModelBreakdown).not.toHaveBeenCalled();

    unmount();
  });

  it("展开某供应商后才懒加载并显示该供应商下的模型行", async () => {
    const { container, unmount } = renderExpanded(
      <LeaderboardUserExpanded userId={7} period="daily" columnCount={5} />
    );
    await flushMicrotasks();

    expect(mockGetUserInsightsModelBreakdown).not.toHaveBeenCalled();

    const expandButton = container.querySelector(
      "[data-testid='leaderboard-user-expanded-provider-list'] button[aria-expanded]"
    );
    expect(expandButton).toBeTruthy();
    expect(expandButton!.getAttribute("aria-expanded")).toBe("false");

    click(expandButton!);
    await flushMicrotasks();

    expect(expandButton!.getAttribute("aria-expanded")).toBe("true");
    const { startDate, endDate } = getDateRangeForPeriod("daily", "UTC");
    expect(mockGetUserInsightsModelBreakdown).toHaveBeenCalledWith(7, startDate, endDate, {
      providerId: providerItem.providerId,
    });

    const nested = container.querySelector("[data-testid='leaderboard-provider-models-11']");
    expectTableRow(nested);
    expect(nested?.textContent).toContain("alpha-model");

    unmount();
  });

  it("切到模型 tab 后请求并显示模型聚合", async () => {
    const { container, unmount } = renderExpanded(
      <LeaderboardUserExpanded userId={7} period="daily" columnCount={5} />
    );
    await flushMicrotasks();

    expect(mockGetUserInsightsModelBreakdown).not.toHaveBeenCalled();

    const modelTab = container.querySelector("[data-testid='leaderboard-user-expanded-model-tab']");
    click(modelTab!);
    await flushMicrotasks();

    expect(modelTab?.getAttribute("data-state")).toBe("active");
    const { startDate, endDate } = getDateRangeForPeriod("daily", "UTC");
    expect(mockGetUserInsightsModelBreakdown).toHaveBeenCalledWith(7, startDate, endDate);
    // 模型 tab 取的是无 providerId 过滤的全量模型聚合
    expect(mockGetUserInsightsModelBreakdown.mock.calls[0][3]).toBeUndefined();

    const modelList = container.querySelectorAll(
      "[data-testid='leaderboard-user-expanded-model-list']"
    );
    expect(modelList.length).toBe(2);
    expectTableRow(modelList[0]);
    const modelListText = Array.from(modelList)
      .map((row) => row.textContent)
      .join(" ");
    expect(modelListText).toContain("model-a");
    expect(modelListText).toContain("model-b");

    unmount();
  });
});
