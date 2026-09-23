/**
 * @vitest-environment happy-dom
 */
import { act } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LeaderboardView } from "@/app/[locale]/dashboard/leaderboard/_components/leaderboard-view";

const fetchMock = vi.fn<typeof fetch>();
const { getAllUserTagsMock, getAllUserKeyGroupsMock } = vi.hoisted(() => ({
  getAllUserTagsMock: vi.fn(),
  getAllUserKeyGroupsMock: vi.fn(),
}));
// 用户视图行内展开区（LeaderboardUserExpanded）走 insights breakdown；setup.ts 的全局
// doMock 只给空列表形状，这里覆盖成 { ok, data } 信封。
const { providerBreakdownMock, modelBreakdownMock } = vi.hoisted(() => ({
  providerBreakdownMock: vi.fn(),
  modelBreakdownMock: vi.fn(),
}));
const searchParamsState = vi.hoisted(() => ({
  value: new URLSearchParams(),
}));
const tMock = vi.hoisted(() => vi.fn((key: string) => key));

vi.mock("@/lib/api-client/v1/actions/admin-user-insights", () => ({
  getUserInsightsProviderBreakdown: providerBreakdownMock,
  getUserInsightsModelBreakdown: modelBreakdownMock,
}));

vi.mock("next/navigation", () => ({
  useSearchParams: () => searchParamsState.value,
}));

vi.mock("next-intl", () => ({
  useTranslations: () => tMock,
  useTimeZone: () => "Asia/Shanghai",
}));

vi.mock("@/lib/api-client/v1/actions/users", () => ({
  getAllUserTags: getAllUserTagsMock,
  getAllUserKeyGroups: getAllUserKeyGroupsMock,
}));

vi.mock("@/app/[locale]/settings/providers/_components/provider-type-filter", () => ({
  ProviderTypeFilter: ({ value }: { value: string }) => (
    <div data-testid="provider-filter">{value}</div>
  ),
}));

vi.mock("@/i18n/routing", () => ({
  Link: ({ children, href, ...props }: any) => (
    <a href={href} {...props}>
      {children}
    </a>
  ),
}));

const globalFetch = global.fetch;

describe("LeaderboardView user cache hit rate scope", () => {
  let container: HTMLDivElement | null = null;
  let root: ReturnType<typeof createRoot> | null = null;
  let queryClient: QueryClient;

  // 用户视图的行内展开区现在渲染 LeaderUserExpanded（供应商 tab 默认 + 模型 tab），
  // 它用 react-query 取 insights breakdown，故必须给 Provider。
  function renderView() {
    root!.render(
      <QueryClientProvider client={queryClient}>
        <LeaderboardView isAdmin />
      </QueryClientProvider>
    );
  }

  beforeEach(() => {
    vi.clearAllMocks();
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, refetchOnWindowFocus: false } },
    });
    searchParamsState.value = new URLSearchParams("scope=userCacheHitRate");
    getAllUserTagsMock.mockResolvedValue({ ok: true, data: ["vip"] });
    getAllUserKeyGroupsMock.mockResolvedValue({ ok: true, data: ["default"] });
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);

    fetchMock.mockImplementation(async (input) => {
      const url = String(input);
      if (url.includes("scope=userCacheHitRate")) {
        return {
          ok: true,
          json: async () => [
            {
              userId: 9,
              userName: "cache-user",
              totalRequests: 15,
              totalCost: 1.5,
              totalCostFormatted: "$1.50",
              cacheReadTokens: 500,
              cacheCreationCost: 0.2,
              totalInputTokens: 1000,
              totalTokens: 1000,
              cacheHitRate: 0.5,
              modelStats: [
                {
                  model: "claude-sonnet-4",
                  totalRequests: 15,
                  cacheReadTokens: 500,
                  totalInputTokens: 1000,
                  cacheHitRate: 0.5,
                },
              ],
            },
          ],
        } as Response;
      }

      return {
        ok: true,
        json: async () => [],
      } as Response;
    });

    providerBreakdownMock.mockResolvedValue({
      ok: true,
      data: {
        breakdown: [
          {
            providerId: 5,
            providerName: "provider-one",
            requests: 7,
            cost: 2.1,
            inputTokens: 1400,
            outputTokens: 0,
            cacheCreationTokens: 0,
            cacheReadTokens: 0,
          },
        ],
        currencyCode: "USD",
      },
    });
    modelBreakdownMock.mockResolvedValue({
      ok: true,
      data: {
        breakdown: [
          {
            model: "claude-sonnet-4",
            requests: 7,
            cost: 2.1,
            inputTokens: 1400,
            outputTokens: 0,
            cacheCreationTokens: 0,
            cacheReadTokens: 0,
          },
        ],
        currencyCode: "USD",
      },
    });

    global.fetch = fetchMock as typeof fetch;
  });

  afterEach(() => {
    queryClient.clear();
    if (root) {
      act(() => root!.unmount());
      root = null;
    }
    if (container) {
      container.remove();
      container = null;
    }
    global.fetch = globalFetch;
  });

  it("fetches and renders user cache hit rate leaderboard for admin", async () => {
    await act(async () => {
      renderView();
    });

    expect(fetchMock).toHaveBeenCalled();
    const requestedUrls = fetchMock.mock.calls.map((call) => String(call[0]));
    expect(requestedUrls.some((url) => url.includes("scope=userCacheHitRate"))).toBe(true);
    expect(
      requestedUrls.some(
        (url) => url.includes("scope=userCacheHitRate") && url.includes("includeUserModelStats=1")
      )
    ).toBe(true);
    expect(
      container!.querySelector("[data-testid='leaderboard-primary-tab-user'][data-state='active']")
    ).not.toBeNull();
    expect(
      container!.querySelector(
        "[data-testid='leaderboard-secondary-tab-cache-hit'][data-state='active']"
      )
    ).not.toBeNull();
    expect(container!.textContent).toContain("cache-user");
    expect(container!.textContent).toContain("50.0%");
  });

  it("keeps model cost visible in the existing user leaderboard drilldown", async () => {
    searchParamsState.value = new URLSearchParams("scope=user");
    fetchMock.mockImplementationOnce(async (input) => {
      const url = String(input);
      if (url.includes("scope=user")) {
        return {
          ok: true,
          json: async () => [
            {
              userId: 3,
              userName: "cost-user",
              totalRequests: 12,
              totalCost: 4.2,
              totalCostFormatted: "$4.20",
              totalTokens: 2400,
            },
          ],
        } as Response;
      }

      return {
        ok: true,
        json: async () => [],
      } as Response;
    });

    await act(async () => {
      renderView();
    });

    const expandButton = container!.querySelector(
      'button[aria-label="expandModelStats"]'
    ) as HTMLButtonElement | null;
    expect(expandButton).toBeTruthy();

    await act(async () => {
      expandButton!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });

    expect(container!.textContent).toContain("cost-user");
    // 展开区默认是供应商 tab：模型成本改由模型 tab 提供（模型聚合走 insights model-breakdown）
    expect(container!.textContent).toContain("provider-one");

    const modelTab = container!.querySelector(
      '[data-testid="leaderboard-user-expanded-model-tab"]'
    ) as HTMLElement | null;
    expect(modelTab).toBeTruthy();

    await act(async () => {
      modelTab!.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
      modelTab!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });

    expect(container!.textContent).toContain("claude-sonnet-4");
    expect(container!.textContent).toContain("$2.10");
  });
});
