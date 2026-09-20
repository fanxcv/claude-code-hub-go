/**
 * @vitest-environment happy-dom
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { VersionUpdateNotifier } from "./version-update-notifier";

vi.mock("next-intl", () => ({
  useTranslations: () => (key: string) => key,
}));

vi.mock("@/components/ui/tooltip", () => ({
  Tooltip: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));

/** 组件在 useEffect 里 fetch，须等微任务落定后再断言。 */
async function renderNotifier() {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);

  await act(async () => {
    root.render(<VersionUpdateNotifier />);
  });

  return container;
}

function stubVersionResponse(payload: unknown) {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(JSON.stringify(payload), { status: 200 }))
  );
}

beforeEach(() => {
  document.body.innerHTML = "";
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("VersionUpdateNotifier", () => {
  it("无新版本时不渲染任何节点", async () => {
    stubVersionResponse({ current: "v1.0.0", latest: "v1.0.0", hasUpdate: false });

    const container = await renderNotifier();

    expect(container.querySelector("a")).toBeNull();
    expect(container.textContent).toBe("");
  });

  it("有新版本时渲染链接，href 指向 releaseUrl 且带无障碍标签", async () => {
    stubVersionResponse({
      current: "v1.0.0",
      latest: "v2.0.0",
      hasUpdate: true,
      releaseUrl: "https://example.com/releases",
    });

    const container = await renderNotifier();
    const link = container.querySelector("a");

    expect(link).not.toBeNull();
    expect(link?.getAttribute("href")).toBe("https://example.com/releases");
    expect(link?.getAttribute("aria-label")).toBe("ariaUpdateAvailable");
    expect(link?.getAttribute("target")).toBe("_blank");
    expect(link?.getAttribute("rel")).toBe("noopener noreferrer");
  });

  it("hasUpdate 为真但缺 releaseUrl 时不渲染（避免死链）", async () => {
    stubVersionResponse({ current: "v1.0.0", latest: "v2.0.0", hasUpdate: true });

    const container = await renderNotifier();

    expect(container.querySelector("a")).toBeNull();
  });

  it("请求失败时静默，不渲染任何节点", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new Error("network down");
      })
    );

    const container = await renderNotifier();

    expect(container.querySelector("a")).toBeNull();
  });
});
