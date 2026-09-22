/**
 * @vitest-environment happy-dom
 *
 * 单元测试：用户管理 Dialog 组件
 *
 * 测试对象：
 * - EditUserDialog
 * - EditKeyDialog
 * - AddKeyDialog
 * - CreateUserDialog
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { NextIntlClientProvider } from "next-intl";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, test, vi, beforeEach, afterEach } from "vitest";
import { toast } from "sonner";

// ==================== Mocks ====================

// Mock next/navigation
vi.mock("next/navigation", () => ({
  useRouter: () => ({
    push: vi.fn(),
    refresh: vi.fn(),
    replace: vi.fn(),
  }),
}));

// Mock @/i18n/routing
vi.mock("@/i18n/routing", () => ({
  Link: ({ children }: { children: ReactNode }) => children,
  useRouter: () => ({
    push: vi.fn(),
    refresh: vi.fn(),
    replace: vi.fn(),
  }),
}));

// Mock Server Actions
const mockEditUser = vi.fn().mockResolvedValue({ ok: true });
const mockRemoveUser = vi.fn().mockResolvedValue({ ok: true });
const mockToggleUserEnabled = vi.fn().mockResolvedValue({ ok: true });
const mockResetUser5hLimitOnly = vi.fn().mockResolvedValue({ ok: true });
const mockResetUserLimitsOnly = vi.fn().mockResolvedValue({ ok: true });
const mockResetUserAllStatistics = vi.fn().mockResolvedValue({ ok: true });
const mockGetUserStatisticsReset = vi.fn();
const mockAddKey = vi.fn().mockResolvedValue({ ok: true, data: { key: "sk-test-key" } });
const mockEditKey = vi.fn().mockResolvedValue({ ok: true });
const mockCreateUserOnly = vi.fn().mockResolvedValue({ ok: true, data: { user: { id: 1 } } });

// api-client 的 users 动作：单一 mock（取原本两个 mock 的并集）。
//
// 2026-09 node 退役：这里原有两个 mock（`@/actions/users` 与 `@/lib/api-client/v1/actions/users`）
// 加一个指向 `.../user/actions/reset-user-5h-limit` 的 mock。旧动作与那个 server action 已随
// `src/actions/**` 删除，三者实际只剩 api-client 这一层，故合并为一个：否则同名 mock 会互相覆盖，
// 先声明的导出（如 `createUserOnly`）会被静默丢掉。
vi.mock("@/lib/api-client/v1/actions/users", () => ({
  createUserOnly: (...args: unknown[]) => mockCreateUserOnly(...args),
  editUser: (...args: unknown[]) => mockEditUser(...args),
  getUserStatisticsReset: (...args: unknown[]) => mockGetUserStatisticsReset(...args),
  removeUser: (...args: unknown[]) => mockRemoveUser(...args),
  resetUser5hLimitOnly: (...args: unknown[]) => mockResetUser5hLimitOnly(...args),
  resetUserLimitsOnly: (...args: unknown[]) => mockResetUserLimitsOnly(...args),
  resetUserAllStatistics: (...args: unknown[]) => mockResetUserAllStatistics(...args),
  toggleUserEnabled: (...args: unknown[]) => mockToggleUserEnabled(...args),
}));

vi.mock("@/components/ui/alert-dialog", () => {
  type PropsWithChildren = { children?: ReactNode };
  const Wrap = ({ children }: PropsWithChildren) => <div>{children}</div>;
  const Button = ({ children, ...props }: PropsWithChildren & Record<string, unknown>) => (
    <button {...props}>{children}</button>
  );
  return {
    AlertDialog: Wrap,
    AlertDialogAction: Button,
    AlertDialogCancel: Button,
    AlertDialogContent: Wrap,
    AlertDialogDescription: Wrap,
    AlertDialogFooter: Wrap,
    AlertDialogHeader: Wrap,
    AlertDialogTitle: Wrap,
    AlertDialogTrigger: Wrap,
  };
});

vi.mock("@/lib/api-client/v1/actions/keys", () => ({
  addKey: (...args: unknown[]) => mockAddKey(...args),
  editKey: (...args: unknown[]) => mockEditKey(...args),
  removeKey: vi.fn().mockResolvedValue({ ok: true }),
}));

vi.mock("@/lib/api-client/v1/actions/usage-logs", () => {
  return {
    getFilterOptions: () => Promise.resolve({ ok: true, data: { models: [] } }),
  };
});

// Mock sonner toast
vi.mock("sonner", () => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
  },
}));

// Mock Dialog components to simplify rendering
vi.mock("@/components/ui/dialog", () => {
  type PropsWithChildren = { children?: ReactNode };
  type DialogContentProps = PropsWithChildren & { className?: string };

  function Dialog({ children }: PropsWithChildren) {
    return <div data-testid="dialog-root">{children}</div>;
  }

  function DialogContent({ children, className }: DialogContentProps) {
    return (
      <div data-testid="dialog-content" className={className}>
        {children}
      </div>
    );
  }

  function DialogHeader({ children }: PropsWithChildren) {
    return <div data-testid="dialog-header">{children}</div>;
  }

  function DialogTitle({ children }: PropsWithChildren) {
    return <h2 data-testid="dialog-title">{children}</h2>;
  }

  function DialogDescription({ children, className }: PropsWithChildren & { className?: string }) {
    return (
      <p data-testid="dialog-description" className={className}>
        {children}
      </p>
    );
  }

  function DialogFooter({ children, className }: PropsWithChildren & { className?: string }) {
    return (
      <div data-testid="dialog-footer" className={className}>
        {children}
      </div>
    );
  }

  return { Dialog, DialogContent, DialogHeader, DialogTitle, DialogDescription, DialogFooter };
});

// Mock form components
vi.mock("@/app/[locale]/dashboard/_components/user/forms/user-edit-section", () => ({
  UserEditSection: ({ user, onChange, translations: _translations }: any) => (
    <div data-testid="user-edit-section" data-user-id={user?.id}>
      <input
        data-testid="user-name-input"
        value={user?.name || ""}
        onChange={(e) => onChange("name", e.target.value)}
      />
    </div>
  ),
}));

vi.mock("@/app/[locale]/dashboard/_components/user/forms/key-edit-section", () => ({
  KeyEditSection: ({ keyData, onChange, translations: _translations }: any) => (
    <div data-testid="key-edit-section" data-key-id={keyData?.id}>
      <input
        data-testid="key-name-input"
        value={keyData?.name || ""}
        onChange={(e) => onChange("name", e.target.value)}
      />
    </div>
  ),
}));

vi.mock("@/app/[locale]/dashboard/_components/user/forms/danger-zone", () => ({
  DangerZone: ({ userId, userName, onDelete }: any) => (
    <div data-testid="danger-zone" data-user-id={userId}>
      <button data-testid="delete-button" onClick={onDelete}>
        Delete {userName}
      </button>
    </div>
  ),
}));

vi.mock("@/app/[locale]/dashboard/_components/user/forms/add-key-form", () => ({
  AddKeyForm: ({ userId, onSuccess }: any) => (
    <div data-testid="add-key-form" data-user-id={userId}>
      <button
        data-testid="add-key-submit"
        onClick={() => onSuccess({ generatedKey: "sk-test", name: "test" })}
      >
        Add Key
      </button>
    </div>
  ),
}));

vi.mock("@/app/[locale]/dashboard/_components/user/forms/edit-key-form", () => ({
  EditKeyForm: ({ keyData, onSuccess }: any) => (
    <div data-testid="edit-key-form" data-key-id={keyData?.id}>
      <button data-testid="edit-key-submit" onClick={() => onSuccess()}>
        Save Key
      </button>
    </div>
  ),
}));

vi.mock("@/app/[locale]/dashboard/_components/user/hooks/use-model-suggestions", () => ({
  useModelSuggestions: () => [],
}));

// Import components after mocks
import { EditUserDialog } from "@/app/[locale]/dashboard/_components/user/edit-user-dialog";
import { EditKeyDialog } from "@/app/[locale]/dashboard/_components/user/edit-key-dialog";
import { AddKeyDialog } from "@/app/[locale]/dashboard/_components/user/add-key-dialog";
import { CreateUserDialog } from "@/app/[locale]/dashboard/_components/user/create-user-dialog";
import type { UserDisplay } from "@/types/user";
import enMessages from "../../messages/en";

// ==================== Test Utilities ====================

const messages = {
  common: enMessages.common,
  dashboard: enMessages.dashboard,
  errors: enMessages.errors,
  forms: enMessages.forms,
  quota: enMessages.quota,
  ui: enMessages.ui,
};

let queryClient: QueryClient;

function renderWithProviders(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);

  act(() => {
    root.render(
      <QueryClientProvider client={queryClient}>
        <NextIntlClientProvider locale="en" messages={messages} timeZone="UTC">
          {node}
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

// Mock user data
const mockUser: UserDisplay = {
  id: 1,
  name: "Test User",
  note: "Test note",
  role: "user",
  rpm: 10,
  dailyQuota: 100,
  providerGroup: "default",
  tags: ["test"],
  keys: [],
  limit5hUsd: 5,
  limit5hResetMode: "rolling",
  isEnabled: true,
  expiresAt: null,
};

// ==================== Tests ====================

describe("EditUserDialog", () => {
  beforeEach(() => {
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    vi.clearAllMocks();
    mockResetUserAllStatistics.mockResolvedValue({
      ok: true,
      data: {
        resetId: "00000000-0000-4000-8000-000000000001",
        userId: 1,
        status: "queued",
        requestedAt: "2026-08-02T12:00:00.000Z",
        startedAt: null,
        completedAt: null,
        deletedMessageRequests: 0,
        deletedUsageLedger: 0,
        errorCode: null,
      },
    });
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  test("renders dialog with user data when open", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={mockUser} />
    );

    expect(container.querySelector('[data-testid="dialog-root"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="dialog-title"]')?.textContent).toContain(
      messages.dashboard.userManagement.editDialog.title
    );
    expect(container.querySelector('[data-testid="user-edit-section"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="danger-zone"]')).not.toBeNull();

    unmount();
  });

  test("does not render content when closed", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={false} onOpenChange={onOpenChange} user={mockUser} />
    );

    // Dialog root exists but content should be minimal
    expect(container.querySelector('[data-testid="user-edit-section"]')).toBeNull();

    unmount();
  });

  test("passes correct user id to UserEditSection", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={mockUser} />
    );

    const userEditSection = container.querySelector('[data-testid="user-edit-section"]');
    expect(userEditSection?.getAttribute("data-user-id")).toBe("1");

    unmount();
  });

  test("passes correct user id to DangerZone", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={mockUser} />
    );

    const dangerZone = container.querySelector('[data-testid="danger-zone"]');
    expect(dangerZone?.getAttribute("data-user-id")).toBe("1");

    unmount();
  });

  test("has save and cancel buttons", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={mockUser} />
    );

    const buttons = container.querySelectorAll("button");
    const buttonTexts = Array.from(buttons).map((b) => b.textContent);

    expect(buttonTexts).toContain("Save");
    expect(buttonTexts).toContain("Cancel");

    unmount();
  });

  test("EditUserDialog renders reset 5h button beside reset all limits button", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={mockUser} />
    );

    const buttonTexts = Array.from(container.querySelectorAll("button")).map((button) =>
      button.textContent?.trim()
    );

    expect(buttonTexts).toContain(messages.dashboard.userManagement.editDialog.reset5h.button);
    expect(buttonTexts).toContain(messages.dashboard.userManagement.editDialog.resetLimits.button);

    unmount();
  });

  test("EditUserDialog disables reset 5h button when no 5h limit is configured", () => {
    const onOpenChange = vi.fn();
    const userWithout5hLimit: UserDisplay = {
      ...mockUser,
      limit5hUsd: null,
    };

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={userWithout5hLimit} />
    );

    const reset5hButton = Array.from(container.querySelectorAll("button")).find(
      (button) =>
        button.textContent?.trim() === messages.dashboard.userManagement.editDialog.reset5h.button
    );

    expect(reset5hButton).toBeDefined();
    expect(reset5hButton?.hasAttribute("disabled")).toBe(true);
    expect(
      Array.from(container.querySelectorAll("button")).some(
        (button) =>
          button.textContent?.trim() ===
          messages.dashboard.userManagement.editDialog.resetLimits.button
      )
    ).toBe(true);

    unmount();
  });

  // 「仅重置 5H 限额」在静态导出改造后从 server action 改为 REST
  // （`@/lib/api-client/v1/actions/users`）。这两条钉住「交互不退化」：
  // 调用走新入口、成功文案按 resetMode 选、失败文案取 `errors` 命名空间的本地化串
  // （而不是服务端 Problem 的英文 title）。
  test("『仅重置 5H 限额』走 REST，成功文案按 resetMode 选（fixed）", async () => {
    mockResetUser5hLimitOnly.mockResolvedValue({ ok: true, data: { resetMode: "fixed" } });
    // 成功后处理器应**只失效用户列表那条 query**，不得整页重载：重载会丢掉搜索词、筛选
    // 与展开态，用户得重新逐个点开。故这里不但要断言失效被触发，还要断言 reload 不再被调。
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    const reload = vi.fn();
    const originalLocation = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: { ...originalLocation, reload },
    });

    try {
      const { container, unmount } = renderWithProviders(
        <EditUserDialog open={true} onOpenChange={vi.fn()} user={mockUser} />
      );

      const confirmButton = Array.from(container.querySelectorAll("button")).find(
        (button) =>
          button.textContent?.trim() ===
          messages.dashboard.userManagement.editDialog.reset5h.confirm
      );
      expect(confirmButton).toBeDefined();

      await act(async () => {
        confirmButton?.click();
      });

      expect(mockResetUser5hLimitOnly).toHaveBeenCalledWith(mockUser.id);
      expect(toast.success).toHaveBeenCalledWith(
        messages.dashboard.userManagement.editDialog.reset5h.successFixed
      );
      expect(invalidate).toHaveBeenCalledWith({ queryKey: ["users"] });
      expect(reload).not.toHaveBeenCalled();

      unmount();
    } finally {
      Object.defineProperty(window, "location", {
        configurable: true,
        writable: true,
        value: originalLocation,
      });
    }
  });

  test("『仅重置限额』成功后同相处理：失效用户列表，不整页重载", async () => {
    mockResetUserLimitsOnly.mockResolvedValue({ ok: true, data: {} });
    const invalidate = vi.spyOn(queryClient, "invalidateQueries");
    const reload = vi.fn();
    const originalLocation = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: { ...originalLocation, reload },
    });

    try {
      const { container, unmount } = renderWithProviders(
        <EditUserDialog open={true} onOpenChange={vi.fn()} user={mockUser} />
      );

      const confirmButton = Array.from(container.querySelectorAll("button")).find(
        (button) =>
          button.textContent?.trim() ===
          messages.dashboard.userManagement.editDialog.resetLimits.confirm
      );
      expect(confirmButton).toBeDefined();

      await act(async () => {
        confirmButton?.click();
      });

      expect(mockResetUserLimitsOnly).toHaveBeenCalledWith(mockUser.id);
      expect(invalidate).toHaveBeenCalledWith({ queryKey: ["users"] });
      expect(reload).not.toHaveBeenCalled();

      unmount();
    } finally {
      Object.defineProperty(window, "location", {
        configurable: true,
        writable: true,
        value: originalLocation,
      });
    }
  });

  test("『仅重置 5H 限额』失败时用 errors 命名空间的本地化文案", async () => {
    mockResetUser5hLimitOnly.mockResolvedValue({
      ok: false,
      // 服务端 Problem 的 detail 是英文 title；应改用 errorCode 查本地化串。
      error: "Bad request",
      errorCode: "USER_5H_LIMIT_NOT_CONFIGURED",
    });

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={vi.fn()} user={mockUser} />
    );

    const confirmButton = Array.from(container.querySelectorAll("button")).find(
      (button) =>
        button.textContent?.trim() === messages.dashboard.userManagement.editDialog.reset5h.confirm
    );
    expect(confirmButton).toBeDefined();

    await act(async () => {
      confirmButton?.click();
    });

    expect(toast.error).toHaveBeenCalledWith(messages.errors.USER_5H_LIMIT_NOT_CONFIGURED);
    expect(toast.error).not.toHaveBeenCalledWith("Bad request");

    unmount();
  });

  test("keeps the authoritative reset state after polling retries are exhausted", async () => {
    vi.useFakeTimers();
    mockGetUserStatisticsReset.mockResolvedValue({
      ok: false,
      error: "Service unavailable",
      errorCode: "CONNECTION_FAILED",
    });
    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={vi.fn()} user={mockUser} />
    );
    const buttons = Array.from(container.querySelectorAll("button"));
    const resetButton = buttons.find(
      (button) =>
        button.textContent?.trim() === messages.dashboard.userManagement.editDialog.resetData.button
    );
    const confirmButton = buttons.find(
      (button) =>
        button.textContent?.trim() ===
        messages.dashboard.userManagement.editDialog.resetData.confirm
    );

    act(() => resetButton?.click());
    await act(async () => confirmButton?.click());
    await act(async () => vi.advanceTimersByTimeAsync(32_000));

    expect(mockGetUserStatisticsReset).toHaveBeenCalledTimes(6);
    expect(container.querySelector('[data-testid="statistics-reset-status"]')?.textContent).toBe(
      messages.dashboard.userManagement.editDialog.resetData.queued
    );
    expect(
      container.querySelector('[data-testid="statistics-reset-poll-error"]')?.textContent
    ).toBe(messages.dashboard.userManagement.editDialog.resetData.statusUnavailable);
    expect(mockResetUserAllStatistics).toHaveBeenCalledTimes(1);

    const retryStatusButton = Array.from(container.querySelectorAll("button")).find(
      (button) =>
        button.textContent?.trim() ===
        messages.dashboard.userManagement.editDialog.resetData.retryStatus
    );
    act(() => retryStatusButton?.click());
    await act(async () => vi.advanceTimersByTimeAsync(1_000));

    expect(mockGetUserStatisticsReset).toHaveBeenCalledTimes(7);
    expect(mockResetUserAllStatistics).toHaveBeenCalledTimes(1);

    unmount();
    vi.useRealTimers();
  });

  test("aborts a hung statistics reset status request and continues polling", async () => {
    vi.useFakeTimers();
    const requestSignals: AbortSignal[] = [];
    mockGetUserStatisticsReset.mockImplementation(
      (_userId: number, _resetId: string, options: { signal: AbortSignal }) => {
        requestSignals.push(options.signal);
        return new Promise((resolve) => {
          options.signal.addEventListener("abort", () => {
            resolve({ ok: false, error: "aborted", errorCode: "NETWORK_ERROR" });
          });
        });
      }
    );
    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={vi.fn()} user={mockUser} />
    );
    const buttons = Array.from(container.querySelectorAll("button"));
    const resetButton = buttons.find(
      (button) =>
        button.textContent?.trim() === messages.dashboard.userManagement.editDialog.resetData.button
    );
    const confirmButton = buttons.find(
      (button) =>
        button.textContent?.trim() ===
        messages.dashboard.userManagement.editDialog.resetData.confirm
    );

    act(() => resetButton?.click());
    await act(async () => confirmButton?.click());
    await act(async () => vi.advanceTimersByTimeAsync(17_000));

    expect(requestSignals[0]?.aborted).toBe(true);
    expect(mockGetUserStatisticsReset).toHaveBeenCalledTimes(2);

    unmount();
    expect(requestSignals[1]?.aborted).toBe(true);
    vi.useRealTimers();
  });
});

describe("EditKeyDialog", () => {
  beforeEach(() => {
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    vi.clearAllMocks();
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  const mockKeyData = {
    id: 1,
    name: "Test Key",
    expiresAt: "2025-12-31",
    canLoginWebUi: false,
    providerGroup: null,
  };

  test("renders dialog with key data when open", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditKeyDialog open={true} onOpenChange={onOpenChange} keyData={mockKeyData} />
    );

    expect(container.querySelector('[data-testid="dialog-root"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="dialog-title"]')?.textContent).toContain(
      "Edit Key"
    );
    expect(container.querySelector('[data-testid="edit-key-form"]')).not.toBeNull();

    unmount();
  });

  test("passes keyData to EditKeyForm", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditKeyDialog open={true} onOpenChange={onOpenChange} keyData={mockKeyData} />
    );

    const editKeyForm = container.querySelector('[data-testid="edit-key-form"]');
    expect(editKeyForm?.getAttribute("data-key-id")).toBe("1");

    unmount();
  });

  test("calls onOpenChange when dialog is closed", () => {
    const onOpenChange = vi.fn();
    const onSuccess = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditKeyDialog
        open={true}
        onOpenChange={onOpenChange}
        keyData={mockKeyData}
        onSuccess={onSuccess}
      />
    );

    // Simulate clicking save in the mocked form
    const submitButton = container.querySelector('[data-testid="edit-key-submit"]') as HTMLElement;
    act(() => {
      submitButton?.click();
    });

    expect(onSuccess).toHaveBeenCalled();
    expect(onOpenChange).toHaveBeenCalledWith(false);

    unmount();
  });
});

describe("AddKeyDialog", () => {
  beforeEach(() => {
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    vi.clearAllMocks();
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  test("renders dialog with add key form when open", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <AddKeyDialog open={true} onOpenChange={onOpenChange} userId={1} />
    );

    expect(container.querySelector('[data-testid="dialog-root"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="dialog-title"]')?.textContent).toContain(
      "Add Key"
    );
    expect(container.querySelector('[data-testid="add-key-form"]')).not.toBeNull();

    unmount();
  });

  test("passes userId to AddKeyForm", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <AddKeyDialog open={true} onOpenChange={onOpenChange} userId={42} />
    );

    const addKeyForm = container.querySelector('[data-testid="add-key-form"]');
    expect(addKeyForm?.getAttribute("data-user-id")).toBe("42");

    unmount();
  });

  test("calls onSuccess after successful key creation", () => {
    const onOpenChange = vi.fn();
    const onSuccess = vi.fn();

    const { container, unmount } = renderWithProviders(
      <AddKeyDialog open={true} onOpenChange={onOpenChange} userId={1} onSuccess={onSuccess} />
    );

    // Initially shows form
    expect(container.querySelector('[data-testid="add-key-form"]')).not.toBeNull();

    // Simulate successful key creation
    const submitButton = container.querySelector('[data-testid="add-key-submit"]') as HTMLElement;
    act(() => {
      submitButton?.click();
    });

    // onSuccess should be called
    expect(onSuccess).toHaveBeenCalled();

    // The component should now show the success view with generated key info
    // (key name "test" from mock result)
    expect(container.textContent).toContain("Key Created");

    unmount();
  });
});

describe("CreateUserDialog", () => {
  beforeEach(() => {
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    vi.clearAllMocks();
    mockCreateUserOnly.mockResolvedValue({ ok: true, data: { user: { id: 1 } } });
    mockAddKey.mockResolvedValue({ ok: true, data: { key: "sk-new-user-key" } });
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  test("renders dialog with user and key sections when open", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <CreateUserDialog open={true} onOpenChange={onOpenChange} />
    );

    expect(container.querySelector('[data-testid="dialog-root"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="dialog-title"]')?.textContent).toContain(
      messages.dashboard.userManagement.createDialog.title
    );
    expect(container.querySelector('[data-testid="user-edit-section"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="key-edit-section"]')).not.toBeNull();

    unmount();
  });

  test("does not render content when closed", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <CreateUserDialog open={false} onOpenChange={onOpenChange} />
    );

    expect(container.querySelector('[data-testid="user-edit-section"]')).toBeNull();
    expect(container.querySelector('[data-testid="key-edit-section"]')).toBeNull();

    unmount();
  });

  test("has create and cancel buttons", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <CreateUserDialog open={true} onOpenChange={onOpenChange} />
    );

    const buttons = container.querySelectorAll("button");
    const buttonTexts = Array.from(buttons).map((b) => b.textContent);

    expect(buttonTexts).toContain("Create");
    expect(buttonTexts).toContain("Cancel");

    unmount();
  });
});

describe("Dialog Component Integration", () => {
  beforeEach(() => {
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    vi.clearAllMocks();
  });

  afterEach(() => {
    document.body.innerHTML = "";
  });

  test("EditUserDialog re-renders with new user when user prop changes", () => {
    const onOpenChange = vi.fn();

    const { container, unmount } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={mockUser} />
    );

    // Check initial user
    let userEditSection = container.querySelector('[data-testid="user-edit-section"]');
    expect(userEditSection?.getAttribute("data-user-id")).toBe("1");

    unmount();

    // Render with different user
    const newUser = { ...mockUser, id: 2, name: "New User" };
    const { container: container2, unmount: unmount2 } = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={newUser} />
    );

    userEditSection = container2.querySelector('[data-testid="user-edit-section"]');
    expect(userEditSection?.getAttribute("data-user-id")).toBe("2");

    unmount2();
  });

  test("all dialogs have accessible title", () => {
    const onOpenChange = vi.fn();

    // EditUserDialog
    const edit = renderWithProviders(
      <EditUserDialog open={true} onOpenChange={onOpenChange} user={mockUser} />
    );
    expect(edit.container.querySelector('[data-testid="dialog-title"]')).not.toBeNull();
    edit.unmount();

    // EditKeyDialog
    const editKey = renderWithProviders(
      <EditKeyDialog
        open={true}
        onOpenChange={onOpenChange}
        keyData={{ id: 1, name: "Key", expiresAt: "" }}
      />
    );
    expect(editKey.container.querySelector('[data-testid="dialog-title"]')).not.toBeNull();
    editKey.unmount();

    // AddKeyDialog
    const addKey = renderWithProviders(
      <AddKeyDialog open={true} onOpenChange={onOpenChange} userId={1} />
    );
    expect(addKey.container.querySelector('[data-testid="dialog-title"]')).not.toBeNull();
    addKey.unmount();

    // CreateUserDialog
    const create = renderWithProviders(
      <CreateUserDialog open={true} onOpenChange={onOpenChange} />
    );
    expect(create.container.querySelector('[data-testid="dialog-title"]')).not.toBeNull();
    create.unmount();
  });
});
