import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const queryState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const viewProps = vi.hoisted(() => ({ current: null as Record<string, unknown> | null }));
const localeState = vi.hoisted(() => ({ current: "en" }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => queryState.current,
}));

vi.mock("next-intl", () => ({
  useTranslations: () => (key: string) => key,
  useLocale: () => localeState.current,
}));

vi.mock("@/app/[locale]/status/_components/public-status-view", () => ({
  PublicStatusView: (props: Record<string, unknown>) => {
    viewProps.current = props;
    return null;
  },
}));

import PublicStatusPage from "@/app/[locale]/status/page";

/**
 * 根状态页静态化后由客户端取数。本文件钉住「响应体 meta → 视图 props」的转发契约：
 * 站点标题与时区不再来自服务端 loader，而来自 `/api/v1/public/status` 的 `meta`
 * （缺失时由 `resolveSiteTitle` 兜底），locale 取自路由。
 */
function responseBody(overrides: Record<string, unknown> = {}) {
  return {
    generatedAt: "2026-04-22T00:00:00.000Z",
    freshUntil: null,
    status: "ready",
    rebuildState: { state: "fresh", hasSnapshot: true, reason: null },
    defaults: null,
    resolvedQuery: { intervalMinutes: 5, rangeHours: 24 },
    meta: {
      siteTitle: "CC Hub",
      siteDescription: "CC Hub public status",
      timeZone: "UTC",
    },
    groups: [],
    ...overrides,
  };
}

describe("public status page title", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    container = document.createElement("div");
    document.body.appendChild(container);
    viewProps.current = null;
    localeState.current = "en";
  });

  afterEach(() => {
    container.remove();
  });

  async function renderPage() {
    const root = createRoot(container);
    await act(async () => {
      root.render(<PublicStatusPage />);
    });
    return root;
  }

  it("forwards the meta-provided site title", async () => {
    queryState.current = { data: responseBody(), isLoading: false, isError: false };
    const root = await renderPage();

    expect(viewProps.current?.siteTitle).toBe("CC Hub");
    await act(async () => root.unmount());
  });

  it("forwards the meta-provided timezone", async () => {
    queryState.current = {
      data: responseBody({
        meta: {
          siteTitle: "Snapshot Title",
          siteDescription: "Snapshot public status",
          timeZone: "Asia/Shanghai",
        },
      }),
      isLoading: false,
      isError: false,
    };
    const root = await renderPage();

    expect(viewProps.current?.siteTitle).toBe("Snapshot Title");
    expect(viewProps.current?.timeZone).toBe("Asia/Shanghai");
    await act(async () => root.unmount());
  });

  it("accepts a default-group payload on the root status page", async () => {
    queryState.current = {
      data: responseBody({
        groups: [
          {
            publicGroupSlug: "platform",
            displayName: "Platform",
            explanatoryCopy: "Default group",
            models: [],
          },
        ],
      }),
      isLoading: false,
      isError: false,
    };
    const root = await renderPage();

    expect(viewProps.current?.initialPayload).toEqual(
      expect.objectContaining({
        groups: [
          expect.objectContaining({
            publicGroupSlug: "platform",
            displayName: "Platform",
          }),
        ],
      })
    );
    await act(async () => root.unmount());
  });
});
