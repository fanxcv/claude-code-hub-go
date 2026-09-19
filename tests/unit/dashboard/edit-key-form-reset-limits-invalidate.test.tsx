import fs from "node:fs";
import path from "node:path";
import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NextIntlClientProvider } from "next-intl";
import { beforeEach, describe, expect, test, vi } from "vitest";
import { Dialog } from "@/components/ui/dialog";
import { EditKeyForm } from "@/app/[locale]/dashboard/_components/user/forms/edit-key-form";

const routerMocks = vi.hoisted(() => ({ refresh: vi.fn() }));
vi.mock("next/navigation", () => ({ useRouter: () => routerMocks }));

const sonnerMocks = vi.hoisted(() => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
  },
}));
vi.mock("sonner", () => sonnerMocks);

const keysActionMocks = vi.hoisted(() => ({
  editKey: vi.fn(async () => ({ ok: true })),
  resetKeyLimitsOnly: vi.fn(async () => ({ ok: true })),
}));
vi.mock("@/lib/api-client/v1/actions/keys", () => keysActionMocks);

const providersActionMocks = vi.hoisted(() => ({
  getAvailableProviderGroups: vi.fn(async () => []),
}));
vi.mock("@/lib/api-client/v1/actions/providers", () => providersActionMocks);

const usageCacheMocks = vi.hoisted(() => ({ clearUsageCache: vi.fn() }));
vi.mock("@/lib/dashboard/user-limit-usage-cache", () => usageCacheMocks);

function loadMessages() {
  const base = path.join(process.cwd(), "messages/en");
  const read = (name: string) => JSON.parse(fs.readFileSync(path.join(base, name), "utf8"));

  return {
    common: read("common.json"),
    errors: read("errors.json"),
    quota: read("quota.json"),
    ui: read("ui.json"),
    dashboard: read("dashboard.json"),
    forms: read("forms.json"),
  };
}

function render(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);

  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const invalidateSpy = vi.spyOn(queryClient, "invalidateQueries");

  act(() => {
    root.render(<QueryClientProvider client={queryClient}>{node}</QueryClientProvider>);
  });

  return {
    invalidateSpy,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

function clickButtonByText(text: string) {
  const buttons = Array.from(document.body.querySelectorAll("button"));
  const btn = buttons.find((b) => (b.textContent || "").includes(text));
  if (!btn) {
    throw new Error(`未找到按钮: ${text}`);
  }
  btn.dispatchEvent(new MouseEvent("click", { bubbles: true }));
}

describe("EditKeyForm: 重置限额后不得做全局查询失效", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  test("仅失效 users 并清该用户的限额用量缓存，不发无参全站失效", async () => {
    const messages = loadMessages();

    const { invalidateSpy, unmount } = render(
      <NextIntlClientProvider locale="en" messages={messages} timeZone="UTC">
        <Dialog open onOpenChange={() => {}}>
          <EditKeyForm
            keyData={{ id: 1, name: "k", expiresAt: "2026-01-04T23:59:59.999Z" }}
            user={{ id: 10 }}
            isAdmin
          />
        </Dialog>
      </NextIntlClientProvider>
    );

    // 打开「Reset Quota」确认弹窗并确认
    await act(async () => {
      clickButtonByText("Reset Quota");
    });
    await act(async () => {
      clickButtonByText("Yes, reset quota");
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(keysActionMocks.resetKeyLimitsOnly).toHaveBeenCalledWith(1);

    const calls = invalidateSpy.mock.calls as unknown as Array<[unknown]>;
    // 无参调用即全站失效，必须一次都没有
    expect(calls.every(([arg]) => (arg as { queryKey?: unknown } | undefined)?.queryKey)).toBe(
      true
    );
    expect(
      calls.some(
        ([arg]) =>
          JSON.stringify((arg as { queryKey: unknown }).queryKey) === JSON.stringify(["users"])
      )
    ).toBe(true);
    expect(
      calls.filter(([arg]) => (arg as { queryKey?: unknown } | undefined)?.queryKey === undefined)
    ).toHaveLength(0);

    // 用户限额用量走模块级缓存，React Query 失效覆盖不到
    expect(usageCacheMocks.clearUsageCache).toHaveBeenCalledWith(10);

    unmount();
  });
});
