/**
 * @vitest-environment happy-dom
 *
 * 生产缺陷钉子：设置页的写请求此前用裸 `fetch` 直发 `/api/admin/*`，cookie 会话下被后端
 * 统一 CSRF 门拦下（403 auth.csrf_invalid；生产实证：改日志级别）。本用例逐个触发三个表单的
 * 真实提交，断言「每个 POST 都带 X-CCH-CSRF，且方法/路径/正文不变」，同时断言读路径（GET）
 * 未被改动语义——不覆盖 GET 会漏掉「顺手给读请求也加头」这类噪音改动。
 */
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { CSRF_HEADER } from "@/lib/api/v1/_shared/constants";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const CSRF_TOKEN = "csrf-token-for-tests";

const toastError = vi.fn();
const toastSuccess = vi.fn();

// 注意：必须返回**稳定引用**的 t。组件的 `useEffect(..., [t])` 依赖它；每次渲染新建函数会让
// 取日志级别的 effect 反复重跑（act 里直接表现为挂住），而真实 next-intl 的 t 是稳定的。
vi.mock("next-intl", () => {
  const translate = (key: string) => key;
  return { useTranslations: () => translate };
});

vi.mock("sonner", () => ({
  toast: { success: toastSuccess, error: toastError },
}));

vi.mock("@/components/ui/button", () => ({
  Button: ({ children, onClick, disabled, type }: any) => (
    <button type={type ?? "button"} onClick={onClick} disabled={disabled}>
      {children}
    </button>
  ),
}));

vi.mock("@/components/ui/label", () => ({
  Label: ({ children, ...rest }: any) => <label {...rest}>{children}</label>,
}));

vi.mock("@/components/ui/switch", () => ({
  Switch: ({ checked, onCheckedChange }: any) => (
    <button
      type="button"
      data-checked={checked ? "true" : "false"}
      onClick={() => onCheckedChange?.(!checked)}
    >
      switch
    </button>
  ),
}));

vi.mock("@/components/ui/select", () => ({
  Select: ({ children }: any) => <div>{children}</div>,
  SelectTrigger: ({ children }: any) => <div>{children}</div>,
  SelectValue: () => <span />,
  SelectContent: ({ children }: any) => <div>{children}</div>,
  SelectItem: ({ children }: any) => <div>{children}</div>,
}));

// 弹窗在 happy-dom 里不可交互（portal + 焦点陷阱），故换成语义等价的普通容器：内容照渲染，
// 动作按钮照调 onClick——被测的是提交路径，不是弹窗本身。
vi.mock("@/components/ui/alert-dialog", () => ({
  AlertDialog: ({ children }: any) => <div>{children}</div>,
  AlertDialogContent: ({ children }: any) => <div>{children}</div>,
  AlertDialogHeader: ({ children }: any) => <div>{children}</div>,
  AlertDialogTitle: ({ children }: any) => <div>{children}</div>,
  AlertDialogDescription: ({ children }: any) => <div>{children}</div>,
  AlertDialogFooter: ({ children }: any) => <div>{children}</div>,
  AlertDialogCancel: ({ children, disabled }: any) => (
    <button type="button" disabled={disabled}>
      {children}
    </button>
  ),
  AlertDialogAction: ({ children, onClick, disabled }: any) => (
    <button type="button" onClick={onClick} disabled={disabled}>
      {children}
    </button>
  ),
}));

type Call = { url: string; init: RequestInit };

let calls: Call[] = [];

function headersOf(call: Call): Headers {
  return new Headers(call.init.headers ?? undefined);
}

function methodOf(call: Call): string {
  return (call.init.method ?? "GET").toUpperCase();
}

function findCall(url: string, method: string): Call {
  const call = calls.find((entry) => entry.url === url && methodOf(entry) === method);
  if (!call) {
    throw new Error(
      `未发出 ${method} ${url}；实际调用：${JSON.stringify(calls.map((c) => `${methodOf(c)} ${c.url}`))}`
    );
  }
  return call;
}

function jsonBodyOf(call: Call): unknown {
  return JSON.parse(String(call.init.body));
}

function stubFetch() {
  calls = [];
  const mock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = (init?.method ?? "GET").toUpperCase();
    calls.push({ url, init: init ?? {} });

    if (url === "/api/v1/auth/csrf") {
      return Response.json({ csrfToken: CSRF_TOKEN });
    }
    if (url === "/api/admin/log-level" && method === "GET") {
      return Response.json({ level: "info" });
    }
    if (url === "/api/admin/log-cleanup/manual") {
      const body = JSON.parse(String(init?.body)) as { dryRun?: boolean };
      return Response.json({
        success: true,
        totalDeleted: body.dryRun ? 7 : 42,
        batchCount: 1,
        durationMs: 1200,
        softDeletedPurged: 0,
        vacuumPerformed: false,
      });
    }
    return Response.json({ success: true, level: "debug" });
  });
  vi.stubGlobal("fetch", mock);
  return mock;
}

async function renderComponent(ui: React.ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root: Root = createRoot(container);
  await act(async () => {
    root.render(ui);
  });
  return { container, root };
}

function buttonByText(container: HTMLElement, text: string): HTMLButtonElement {
  const button = Array.from(container.querySelectorAll("button")).find(
    (entry) => entry.textContent?.trim() === text
  );
  if (!button) {
    throw new Error(`没有文案为 ${text} 的按钮；实际：${container.innerHTML.slice(0, 400)}`);
  }
  return button as HTMLButtonElement;
}

async function submitForm(container: HTMLElement) {
  const form = container.querySelector("form");
  if (!form) throw new Error("没有 form 元素");
  await act(async () => {
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
  });
}

/** 断言「所有写请求都带 CSRF 头」——按方法筛选，新加写路径忘了带头就会红。 */
function assertEveryMutationCarriesCsrf() {
  const mutations = calls.filter((call) =>
    ["POST", "PUT", "PATCH", "DELETE"].includes(methodOf(call))
  );
  expect(mutations.length).toBeGreaterThan(0);
  for (const call of mutations) {
    expect(
      headersOf(call).get(CSRF_HEADER),
      `${methodOf(call)} ${call.url} 缺 ${CSRF_HEADER}`
    ).toBe(CSRF_TOKEN);
  }
}

describe("管理面写请求的 CSRF", () => {
  let rendered: { container: HTMLDivElement; root: Root } | null = null;

  beforeEach(() => {
    vi.clearAllMocks();
  });

  afterEach(async () => {
    if (rendered) {
      const { root, container } = rendered;
      await act(async () => {
        root.unmount();
      });
      container.remove();
      rendered = null;
    }
    const { clearCsrfTokenCache } = await import("@/lib/api-client/v1/fetcher");
    clearCsrfTokenCache();
    vi.unstubAllGlobals();
  });

  test("改日志级别（/api/admin/log-level）：POST 带 CSRF，GET 读路径不带", async () => {
    stubFetch();
    const { LogLevelForm } = await import(
      "@/app/[locale]/settings/logs/_components/log-level-form"
    );
    rendered = await renderComponent(<LogLevelForm />);

    const readCall = findCall("/api/admin/log-level", "GET");
    // 读路径保持原样：GET 不带任何 headers（后端只对写方法校验 CSRF，顺手加头属噪音变更）。
    expect(readCall.init.headers).toBeUndefined();

    const select = rendered.container.querySelector("select");
    if (!select) throw new Error("没有级别下拉框");
    await act(async () => {
      select.value = "debug";
      select.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await submitForm(rendered.container);

    const mutation = findCall("/api/admin/log-level", "POST");
    expect(mutation.init.credentials).toBe("include");
    expect(headersOf(mutation).get("Content-Type")).toBe("application/json");
    expect(jsonBodyOf(mutation)).toEqual({ level: "debug" });
    assertEveryMutationCarriesCsrf();

    // 失败路径不受影响：后端文案照旧进 toast（不是 CSRF 修好顺带改了错误提示）。
    expect(toastSuccess).toHaveBeenCalledWith("form.success");
    expect(toastError).not.toHaveBeenCalled();
  });

  test("手动清理日志（/api/admin/log-cleanup/manual）：预览与执行两发 POST 都带 CSRF", async () => {
    stubFetch();
    const { LogCleanupPanel } = await import(
      "@/app/[locale]/settings/data/_components/log-cleanup-panel"
    );
    rendered = await renderComponent(<LogCleanupPanel />);

    // 打开弹窗即触发预览（dryRun: true）
    await act(async () => {
      buttonByText(rendered.container, "button").click();
    });

    const previews = calls.filter(
      (call) => call.url === "/api/admin/log-cleanup/manual" && jsonBodyOf(call)["dryRun"] === true
    );
    expect(previews).toHaveLength(1);
    expect(jsonBodyOf(previews[0])).toMatchObject({ dryRun: true });

    // 确认执行
    await act(async () => {
      buttonByText(rendered.container, "confirm").click();
    });

    const executed = calls.filter(
      (call) =>
        call.url === "/api/admin/log-cleanup/manual" && jsonBodyOf(call)["dryRun"] === undefined
    );
    expect(executed).toHaveLength(1);
    expect(Object.keys(jsonBodyOf(executed[0]) as object)).toEqual(["beforeDate"]);
    assertEveryMutationCarriesCsrf();
    expect(toastSuccess).toHaveBeenCalled();
  });

  test("自动清理配置（/api/admin/system-config）：POST 带 CSRF，正文仍是完整设置对象", async () => {
    stubFetch();
    const { AutoCleanupForm } = await import(
      "@/app/[locale]/settings/config/_components/auto-cleanup-form"
    );
    const settings = {
      siteTitle: "demo",
      allowGlobalUsageView: true,
      enableAutoCleanup: false,
      cleanupRetentionDays: 30,
      cleanupSchedule: "0 2 * * *",
      cleanupBatchSize: 10000,
    };

    rendered = await renderComponent(
      <AutoCleanupForm settings={settings as unknown as never} onSuccess={vi.fn()} />
    );
    await submitForm(rendered.container);

    const mutation = findCall("/api/admin/system-config", "POST");
    const body = jsonBodyOf(mutation) as Record<string, unknown>;
    // 正文形状不变：站点级字段 + 表单字段原样合并后送出。
    expect(Object.keys(body).sort()).toEqual(
      [
        "allowGlobalUsageView",
        "cleanupBatchSize",
        "cleanupRetentionDays",
        "cleanupSchedule",
        "enableAutoCleanup",
        "siteTitle",
      ].sort()
    );
    expect(body).toMatchObject(settings);
    assertEveryMutationCarriesCsrf();
  });
});
