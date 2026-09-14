import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const queryState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const queryOptions = vi.hoisted(() => ({ current: [] as Array<Record<string, unknown>> }));
const viewProps = vi.hoisted(() => ({ current: null as Record<string, unknown> | null }));
const apiGet = vi.hoisted(() => vi.fn());

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: Record<string, unknown>) => {
    queryOptions.current.push(options);
    return queryState.current;
  },
}));

// 文案按命名空间加前缀返回：既能断言「用了哪个命名空间」，也避免在测试里维护一份文案表。
vi.mock("next-intl", () => ({
  useTranslations: (namespace: string) => (key: string) => `${namespace}:${key}`,
  useLocale: () => "en",
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ slug: "anthropic" }),
}));

vi.mock("@/lib/api-client/v1/client", () => ({
  apiClient: { get: apiGet },
}));

vi.mock("@/app/[locale]/status/_components/public-status-view", () => ({
  PublicStatusView: (props: Record<string, unknown>) => {
    viewProps.current = props;
    return null;
  },
}));

import PublicStatusGroupPage from "@/app/[locale]/status/[slug]/page";
import PublicStatusPage from "@/app/[locale]/status/page";

/**
 * 两个公开状态页静态化后都在客户端取数。本文件钉住两件事：
 * ① 文案来自 `settings.statusPage.public` 命名空间且 locale 取路由（原来由服务端
 *    `getTranslations({ locale })` 完成）；
 * ② 单分组页按 `groupSlug` 查询，并把该分组收窄后交给视图。
 */
function responseBody(overrides: Record<string, unknown> = {}) {
  return {
    generatedAt: "2026-04-22T10:00:00.000Z",
    freshUntil: "2026-04-22T10:05:00.000Z",
    status: "ready",
    rebuildState: { state: "fresh", hasSnapshot: true, reason: null },
    defaults: null,
    resolvedQuery: { intervalMinutes: 5, rangeHours: 24 },
    meta: { siteTitle: "CC Hub", siteDescription: "CC Hub public status", timeZone: "UTC" },
    groups: [],
    ...overrides,
  };
}

describe("PublicStatusPage locale handling", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    container = document.createElement("div");
    document.body.appendChild(container);
    viewProps.current = null;
    queryOptions.current = [];
    apiGet.mockReset();
  });

  afterEach(() => {
    container.remove();
  });

  async function renderPage(element: React.ReactElement) {
    const root = createRoot(container);
    await act(async () => {
      root.render(element);
    });
    return root;
  }

  it("passes the route locale and the public status namespace into the view", async () => {
    queryState.current = { data: responseBody(), isLoading: false, isError: false };
    const root = await renderPage(createElement(PublicStatusPage));

    expect(viewProps.current?.locale).toBe("en");
    expect(viewProps.current?.labels).toMatchObject({
      heroPrimary: "settings.statusPage.public:heroPrimary",
      heroSecondary: "settings.statusPage.public:heroSecondary",
      fresh: "settings.statusPage.public:fresh",
    });
    await act(async () => root.unmount());
  });

  it("loads the slug page through the public status API and scopes it to that group", async () => {
    apiGet.mockResolvedValue(
      responseBody({
        groups: [
          {
            publicGroupSlug: "anthropic",
            displayName: "Anthropic",
            explanatoryCopy: "Anthropic public models",
            models: [],
          },
        ],
      })
    );

    queryState.current = { data: responseBody(), isLoading: true, isError: false };
    const root = await renderPage(createElement(PublicStatusGroupPage));

    // 查询 URL 由被捕获的 queryFn 决定：断言它打的是 groupSlug 收窄路径。
    const queryFn = queryOptions.current[0]?.queryFn as () => Promise<unknown>;
    const fetched = await queryFn();
    expect(apiGet).toHaveBeenCalledWith("/api/v1/public/status?include=meta&groupSlug=anthropic");

    queryState.current = { data: fetched, isLoading: false, isError: false };
    await act(async () => {
      root.render(createElement(PublicStatusGroupPage));
    });

    expect(viewProps.current?.filterSlug).toBe("anthropic");
    expect(viewProps.current?.initialPayload).toEqual(
      expect.objectContaining({
        groups: [expect.objectContaining({ publicGroupSlug: "anthropic" })],
      })
    );
    await act(async () => root.unmount());
  });
});
