import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const queryState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const queryOptions = vi.hoisted(() => ({ current: [] as Array<Record<string, unknown>> }));
const viewProps = vi.hoisted(() => ({ current: null as Record<string, unknown> | null }));
const apiGet = vi.hoisted(() => vi.fn());
const slugState = vi.hoisted(() => ({ current: "platform" }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: Record<string, unknown>) => {
    queryOptions.current.push(options);
    return queryState.current;
  },
}));

vi.mock("next-intl", () => ({
  useTranslations: (namespace: string) => (key: string) => `${namespace}:${key}`,
  useLocale: () => "en",
}));

vi.mock("next/navigation", () => ({
  useParams: () => ({ slug: slugState.current }),
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

/**
 * 单分组状态页静态化后按 `groupSlug` 在客户端查询。本文件钉住改造后的新契约：
 * 段值来自 `useParams()`（不再有构建期枚举与服务端 `params`），分组不存在时渲染空态
 * 而不是调用 `notFound()`（客户端组件不可用 `notFound`），存在时只把目标分组交给视图。
 */
const group = (slug: string, displayName: string) => ({
  publicGroupSlug: slug,
  displayName,
  explanatoryCopy: `${displayName} copy`,
  models: [],
});

function responseBody(slug: string, groups: ReturnType<typeof group>[]) {
  return {
    generatedAt: "2026-04-22T10:00:00.000Z",
    freshUntil: null,
    status: "ready",
    rebuildState: { state: "fresh", hasSnapshot: true, reason: null },
    defaults: null,
    resolvedQuery: { intervalMinutes: 5, rangeHours: 24 },
    meta: { siteTitle: "CC Hub", siteDescription: null, timeZone: "UTC" },
    groups,
    slug,
  };
}

describe("public status slug page", () => {
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
    slugState.current = "platform";
  });

  afterEach(() => {
    container.remove();
  });

  async function renderPage() {
    const root = createRoot(container);
    await act(async () => {
      root.render(createElement(PublicStatusGroupPage));
    });
    return root;
  }

  it("queries the public status API for the route slug", async () => {
    apiGet.mockResolvedValue(responseBody("platform", [group("platform", "Platform")]));
    queryState.current = {
      data: responseBody("platform", [group("platform", "Platform")]),
      isLoading: false,
      isError: false,
    };

    const root = await renderPage();

    expect(apiGet).not.toHaveBeenCalled();
    const queryFn = queryOptions.current[0]?.queryFn as () => Promise<unknown>;
    await queryFn();
    expect(apiGet).toHaveBeenCalledWith("/api/v1/public/status?include=meta&groupSlug=platform");
    await act(async () => root.unmount());
  });

  it("scopes the payload to the target group", async () => {
    queryState.current = {
      data: responseBody("platform", [group("platform", "Platform"), group("openai", "OpenAI")]),
      isLoading: false,
      isError: false,
    };

    const root = await renderPage();

    expect(viewProps.current?.filterSlug).toBe("platform");
    expect(viewProps.current?.initialPayload).toEqual(
      expect.objectContaining({
        groups: [expect.objectContaining({ publicGroupSlug: "platform" })],
      })
    );
    await act(async () => root.unmount());
  });

  it("resolves a custom default-group slug", async () => {
    slugState.current = "custom-slug";
    queryState.current = {
      data: responseBody("custom-slug", [group("custom-slug", "Custom")]),
      isLoading: false,
      isError: false,
    };

    const root = await renderPage();

    expect(viewProps.current?.filterSlug).toBe("custom-slug");
    await act(async () => root.unmount());
  });

  it("keeps unknown slugs on an empty state instead of throwing notFound", async () => {
    slugState.current = "missing-group";
    queryState.current = {
      data: responseBody("missing-group", [group("platform", "Platform")]),
      isLoading: false,
      isError: false,
    };

    const root = await renderPage();

    expect(viewProps.current).toBeNull();
    expect(container.textContent).toContain("settings.statusPage.public:noData");
    await act(async () => root.unmount());
  });
});
