/**
 * @vitest-environment happy-dom
 */

import fs from "node:fs";
import path from "node:path";
import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { NextIntlClientProvider } from "next-intl";
import { beforeEach, describe, expect, test, vi } from "vitest";
import { Dialog } from "@/components/ui/dialog";
import { DeleteUserConfirm } from "@/app/[locale]/dashboard/_components/user/forms/delete-user-confirm";

const routerMocks = vi.hoisted(() => ({ refresh: vi.fn() }));
vi.mock("next/navigation", () => ({ useRouter: () => routerMocks }));

const sonnerMocks = vi.hoisted(() => ({
  toast: {
    error: vi.fn(),
    success: vi.fn(),
  },
}));
vi.mock("sonner", () => sonnerMocks);

const usersActionMocks = vi.hoisted(() => ({
  removeUser: vi.fn(async () => ({ ok: true })),
}));
vi.mock("@/lib/api-client/v1/actions/users", () => usersActionMocks);

function loadMessages(locale: "en" | "zh-CN") {
  const read = (name: string) =>
    JSON.parse(fs.readFileSync(path.join(process.cwd(), "messages", locale, name), "utf8"));

  return {
    common: read("common.json"),
    errors: read("errors.json"),
    dashboard: read("dashboard.json"),
  };
}

function render(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);

  act(() => {
    root.render(node);
  });

  return {
    container,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

async function flushTicks(times = 2) {
  for (let i = 0; i < times; i++) {
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }
}

function confirmButton(container: HTMLElement, label: string) {
  const button = Array.from(container.querySelectorAll("button")).find(
    (candidate) => candidate.textContent === label
  );
  if (!button) {
    throw new Error(`未找到确认按钮: ${label}`);
  }
  return button;
}

describe("DeleteUserConfirm 文案全部走词表", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    usersActionMocks.removeUser.mockResolvedValue({ ok: true });
  });

  test("按当前语言渲染标题、提示与按钮，缺词表即失败", async () => {
    const en = loadMessages("en").dashboard.deleteUserConfirm;
    const { container, unmount } = render(
      <NextIntlClientProvider locale="en" timeZone="UTC" messages={loadMessages("en")}>
        <Dialog open onOpenChange={() => {}}>
          <DeleteUserConfirm
            user={{
              id: 1,
              name: "alice",
              keys: [
                { id: 1, name: "k1" },
                { id: 2, name: "k2" },
              ],
            }}
          />
        </Dialog>
      </NextIntlClientProvider>
    );

    await flushTicks();

    const text = container.textContent ?? "";
    expect(text).toContain(en.title);
    expect(text).toContain("alice");
    expect(text).toContain("2");
    expect(text).toContain(en.cancel);
    expect(text).toContain(en.confirm);
    expect(text).not.toContain("确认删除用户");

    unmount();
  });

  test("失败提示优先用错误码，无错误码时回落到本命名空间文案", async () => {
    const zh = loadMessages("zh-CN");

    usersActionMocks.removeUser.mockResolvedValueOnce({ ok: false, errorCode: "INTERNAL_ERROR" });
    const first = render(
      <NextIntlClientProvider locale="zh-CN" timeZone="UTC" messages={zh}>
        <Dialog open onOpenChange={() => {}}>
          <DeleteUserConfirm user={{ id: 1, name: "alice", keys: [] }} />
        </Dialog>
      </NextIntlClientProvider>
    );
    await flushTicks();

    await act(async () => {
      confirmButton(first.container, zh.dashboard.deleteUserConfirm.confirm).dispatchEvent(
        new MouseEvent("click", { bubbles: true })
      );
    });
    await flushTicks(3);

    expect(sonnerMocks.toast.error).toHaveBeenCalledWith(zh.errors.INTERNAL_ERROR);
    first.unmount();

    sonnerMocks.toast.error.mockClear();
    usersActionMocks.removeUser.mockResolvedValueOnce({ ok: false, error: "" });
    const second = render(
      <NextIntlClientProvider locale="zh-CN" timeZone="UTC" messages={zh}>
        <Dialog open onOpenChange={() => {}}>
          <DeleteUserConfirm user={{ id: 1, name: "alice", keys: [] }} />
        </Dialog>
      </NextIntlClientProvider>
    );
    await flushTicks();

    await act(async () => {
      confirmButton(second.container, zh.dashboard.deleteUserConfirm.confirm).dispatchEvent(
        new MouseEvent("click", { bubbles: true })
      );
    });
    await flushTicks(3);

    expect(sonnerMocks.toast.error).toHaveBeenCalledWith(
      zh.dashboard.deleteUserConfirm.errors.deleteFailed
    );
    second.unmount();
  });
});
