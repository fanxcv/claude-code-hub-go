/**
 * @vitest-environment happy-dom
 *
 * 熔断日志弹窗（provider-circuit-logs-dialog.tsx）的渲染契约。
 *
 * 为什么值得单测：这个弹窗承载着**三条容易退化的语义**，而它们在视觉上都很容易被"顺手改掉"：
 *   1. 「读不到」不得渲染成 0（后端给的是 null，界面若 `?? 0` 就会把 Redis 故障画成"闭态 0 次失败"）；
 *   2. 「没有 HTTP 状态码」不得画成 0（那是本地拒绝/客户端中断，不是上游回了 0）；
 *   3. 「已脱敏」必须显式提示，且错误列表必须写明**时间范围**（否则"24h 内无错误"会被读成"从无错误"）。
 *
 * 范式沿用仓库既有做法：`react-dom/client` 的 createRoot + act + happy-dom（本仓不用 testing-library）。
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// next-intl：把 t() 变成「键 + 参数」的可断字符串。这样断言钉的是**用没用对键**
// （含插值参数），而不是某个语种的文案——文案会变，键与参数不会。
vi.mock("next-intl", () => ({
  useLocale: () => "zh-CN",
  useTranslations: (namespace: string) => (key: string, params?: Record<string, unknown>) =>
    params ? `${namespace}.${key}(${JSON.stringify(params)})` : `${namespace}.${key}`,
}));

// react-query：由各用例替换返回值，避免真起 QueryClient。
const useQueryMock = vi.fn();
vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: unknown) => useQueryMock(options),
}));

const getProviderCircuitLogsMock = vi.fn();
vi.mock("@/lib/api-client/v1/actions/providers", () => ({
  getProviderCircuitLogs: (...args: unknown[]) => getProviderCircuitLogsMock(...args),
}));

// Radix 的 Dialog 只在 open 时渲染内容；这里换成受控的直通实现，
// 让用例可以只测"内容怎么画"（开关行为由 rich-list-item 的集成负责）。
vi.mock("@/components/ui/dialog", () => ({
  Dialog: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DialogTrigger: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  // 透传 className：本文件有一条用例要钉「弹层有视口高度上限」。
  DialogContent: ({ children, className }: { children: ReactNode; className?: string }) => (
    <div className={className}>{children}</div>
  ),
  DialogHeader: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DialogTitle: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  DialogDescription: ({ children }: { children: ReactNode }) => <div>{children}</div>,
}));

vi.mock("@/components/ui/table", () => ({
  Table: ({ children }: { children: ReactNode }) => <table>{children}</table>,
  TableHeader: ({ children }: { children: ReactNode }) => <thead>{children}</thead>,
  TableBody: ({ children }: { children: ReactNode }) => <tbody>{children}</tbody>,
  TableRow: ({ children }: { children: ReactNode }) => <tr>{children}</tr>,
  TableHead: ({ children }: { children: ReactNode }) => <th>{children}</th>,
  // 透传 className：本文件要钉「错误文案用 pre-wrap 保留换行」与「单元格宽度受限」。
  TableCell: ({ children, className }: { children: ReactNode; className?: string }) => (
    <td className={className}>{children}</td>
  ),
}));

vi.mock("@/components/ui/badge", () => ({
  Badge: ({ children }: { children: ReactNode }) => <span>{children}</span>,
}));

vi.mock("@/components/ui/button", () => ({
  // 透传 title/onClick/className：复制按钮的可用性靠真实点击事件验证，
  // 只渲染 children 会让「按钮可点且真的写入剪贴板」变成无法断言的。
  Button: ({
    children,
    onClick,
    title,
    className,
  }: {
    children: ReactNode;
    onClick?: (e: unknown) => void;
    title?: string;
    className?: string;
  }) => (
    <button type="button" onClick={onClick} title={title} className={className}>
      {children}
    </button>
  ),
}));

import { ProviderCircuitLogsDialog } from "@/app/[locale]/settings/providers/_components/provider-circuit-logs-dialog";
import type { ProviderCircuitLogs, ProviderCircuitLogsError } from "@/types/provider";

function render(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => root.render(node));
  return () => {
    act(() => root.unmount());
    container.remove();
  };
}

/** 造一份完整响应；每个用例只覆盖自己关心的字段。 */
function payload(overrides: Partial<ProviderCircuitLogs> = {}): ProviderCircuitLogs {
  return {
    providerId: 145,
    circuit: {
      available: true,
      circuitState: "open",
      failureCount: 5,
      lastFailureTime: 1789265134080,
      circuitOpenUntil: 1789266934080,
      halfOpenSuccessCount: 0,
      recoveryMinutes: 30,
      unavailableReason: null,
    },
    thresholds: { failureThreshold: 5, openDuration: 1800000, halfOpenSuccessThreshold: 2 },
    window: { limit: 20, lookbackHours: 24, since: "2026-09-13T00:00:00.000Z" },
    errors: [],
    errorsUnavailableReason: null,
    ...overrides,
  };
}

function querySuccess(data: ProviderCircuitLogs) {
  useQueryMock.mockReturnValue({
    data,
    isPending: false,
    isError: false,
    isFetching: false,
    refetch: vi.fn(),
  });
}

/** 造一条错误行；用例只覆盖自己关心的字段（其余取典形值）。 */
function errorRow(overrides: Partial<ProviderCircuitLogsError> = {}): ProviderCircuitLogsError {
  return {
    requestId: 1,
    createdAt: "2026-09-13T02:31:07.000Z",
    model: "deepseek-v4.1-flash",
    statusCode: 500,
    errorMessage: "upstream_error",
    durationMs: 202,
    endpoint: "/v1/responses",
    source: "direct",
    chainReason: null,
    redacted: false,
    ...overrides,
  };
}

/** 剪贴板：happy-dom 不自带，各用例通过它断言「真的写了什么」。 */
const writeTextMock = vi.fn(async () => undefined);

/** 渲染并取回可断言的文本。 */
function text(): string {
  return document.body.textContent ?? "";
}

describe("ProviderCircuitLogsDialog", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    // 剪贴板桩：复制全程走 navigator.clipboard.writeText（组件优先此路）。
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: { writeText: writeTextMock },
    });
  });

  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
  });

  it("熔断状态可用时显示状态与计数，并显示恢复倒计时", () => {
    querySuccess(payload());
    const unmount = render(
      <ProviderCircuitLogsDialog providerId={145} providerName="Ollama Codex" open />
    );
    const rendered = text();
    expect(rendered).toContain("settings.providers.list.circuitLogs.state.open");
    expect(rendered).toContain("settings.providers.list.circuitLogs.state.failureCount");
    // 恢复倒计时要带上分钟数（插值参数一并钉住）
    expect(rendered).toContain('"minutes":30');
    unmount();
  });

  it("熔断状态读不到时不显示 0，而是明确说不可读", () => {
    querySuccess(
      payload({
        circuit: {
          available: false,
          circuitState: null,
          failureCount: null,
          lastFailureTime: null,
          circuitOpenUntil: null,
          halfOpenSuccessCount: null,
          recoveryMinutes: null,
          unavailableReason: "redis_unavailable",
        },
      })
    );
    const unmount = render(
      <ProviderCircuitLogsDialog providerId={145} providerName="Ollama Codex" open />
    );
    const rendered = text();
    // 关键：不得把"读不到"渲染成闭态或 0 —— 那会把故障画成健康
    expect(rendered).toContain("settings.providers.list.circuitLogs.state.unavailable");
    expect(rendered).toContain("redis_unavailable");
    expect(rendered).not.toContain("settings.providers.list.circuitLogs.state.closed");
    unmount();
  });

  it("错误列表为空时给出带时间范围的空态提示", () => {
    querySuccess(payload({ errors: [] }));
    const unmount = render(
      <ProviderCircuitLogsDialog providerId={145} providerName="Ollama Codex" open />
    );
    const rendered = text();
    // 「24 小时内无错误」必须带小时数，否则会被读成「从来没有错误」
    expect(rendered).toContain('"hours":24');
    expect(rendered).toContain("settings.providers.list.circuitLogs.errors.empty");
    unmount();
  });

  it("链内失败标出来源，空状态码不画成 0，脱敏条目给出提示", () => {
    querySuccess(
      payload({
        errors: [
          {
            requestId: 1,
            createdAt: "2026-09-13T02:31:07.000Z",
            model: "deepseek-v4.1-flash",
            statusCode: 404,
            errorMessage: "resource_not_found",
            durationMs: 202,
            endpoint: "/v1/responses",
            source: "chain",
            chainReason: "hedge_launched",
            redacted: false,
          },
          {
            requestId: 2,
            createdAt: "2026-09-13T02:32:07.000Z",
            model: "deepseek-v4.1-flash",
            statusCode: null,
            errorMessage: "CLIENT_ABORTED",
            durationMs: 12,
            endpoint: "/v1/responses",
            source: "direct",
            chainReason: null,
            redacted: true,
          },
        ],
      })
    );
    const unmount = render(
      <ProviderCircuitLogsDialog providerId={145} providerName="Ollama Codex" open />
    );
    const rendered = text();
    expect(rendered).toContain("settings.providers.list.circuitLogs.errors.sourceChain");
    expect(rendered).toContain("hedge_launched");
    expect(rendered).toContain("settings.providers.list.circuitLogs.errors.sourceDirect");
    // 空状态码画成文字，不画 0
    expect(rendered).toContain("settings.providers.list.circuitLogs.errors.noStatusCode");
    // 脱敏提示只在被改写的那条上出现
    expect(rendered).toContain("settings.providers.list.circuitLogs.errors.redactedHint");
    expect(rendered).toContain("CLIENT_ABORTED");
    unmount();
  });

  it("错误记录读不到时不影响熔断状态，且给出原因", () => {
    querySuccess(payload({ errorsUnavailableReason: "store_query_failed" }));
    const unmount = render(
      <ProviderCircuitLogsDialog providerId={145} providerName="Ollama Codex" open />
    );
    const rendered = text();
    expect(rendered).toContain("settings.providers.list.circuitLogs.errors.unavailable");
    expect(rendered).toContain("store_query_failed");
    // 状态块仍然渲染（两块独立降级）
    expect(rendered).toContain("settings.providers.list.circuitLogs.state.open");
    unmount();
  });

  it("取数失败时显示失败态而不是空白", () => {
    useQueryMock.mockReturnValue({
      data: undefined,
      isPending: false,
      isError: true,
      isFetching: false,
      refetch: vi.fn(),
    });
    const unmount = render(
      <ProviderCircuitLogsDialog providerId={145} providerName="Ollama Codex" open />
    );
    expect(text()).toContain("settings.providers.list.circuitLogs.loadFailed");
    unmount();
  });

  it("未打开时不取数（enabled=false），打开后才取", () => {
    querySuccess(payload());
    const unmount = render(
      <ProviderCircuitLogsDialog providerId={145} providerName="Ollama Codex" open={false} />
    );
    const options = useQueryMock.mock.calls[0]?.[0] as { enabled?: boolean; queryKey?: unknown[] };
    expect(options?.enabled).toBe(false);
    expect(options?.queryKey?.[1]).toBe(145);
    unmount();
  });

  // ———————————————————————————————————————————————————————————————
  // 以下三例针对用户实报的界面问题：「长日志超出容器 / 展示不完整 / 无法复制」。
  // ———————————————————————————————————————————————————————————————

  it("弹层有视口高度上限，错误表不再自带 45vh 嵌套滚动（单一滚动条）", () => {
    querySuccess(
      payload({
        errors: [errorRow({ requestId: 1, errorMessage: "x".repeat(4000) })],
      })
    );
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);

    const content = document.querySelector("div[class*='max-h-']");
    expect(content?.className).toContain("max-h-[85vh]");
    // 外层给高度上限后，表容器不得再用 max-h-[45vh] 造第二个纵向滚动区（叠滚动条是"超容器"的观感来源）。
    const wrappers = [...document.querySelectorAll("div[class*='overflow-x-auto']")];
    expect(wrappers.length).toBeGreaterThan(0);
    expect(wrappers.some((w) => (w.className || "").includes("max-h-"))).toBe(false);
    unmount();
  });

  it("多行错误完整渲染：用 pre-wrap 保留换行，不用 break-all 拦腰截断", () => {
    const multi = `Provider X returned 500: new_api_error\n当前模型负载已达上限\n第三行：${"y".repeat(600)}`;
    querySuccess(payload({ errors: [errorRow({ requestId: 2, errorMessage: multi })] }));
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);

    const block = document.querySelector(".whitespace-pre-wrap");
    expect(block).not.toBeNull();
    // 全文一字不少（含换行）——这是「展示不完整」的直接反证。
    expect(block?.textContent).toBe(multi);
    expect(block?.className).toContain("break-words");
    expect(block?.className).not.toContain("break-all");
    unmount();
  });

  it("每条错误可一键复制全文，整表也可一键拷走（真的写入剪贴板而不是摆个按钮）", async () => {
    const long = `Provider Any Router_Codex returned 500: 负载已满\n${"z".repeat(800)}`;
    querySuccess(
      payload({
        errors: [
          errorRow({ requestId: 3, errorMessage: long, model: "gpt-5.6-sol", statusCode: 500 }),
        ],
      })
    );
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);

    // 注意：`errors.copy` 是 `errors.copyAll` 的子串——只按 includes("errors.copy") 找会命中「复制全部」。
    // 这里按「排除 copyAll」精确区分两个按钮（测试自己先踩过这个坑）。
    const buttons = [...document.querySelectorAll("button")];
    const rowCopy = buttons.find((b) => {
      const s = b.textContent || "";
      return s.includes("errors.copy") && !s.includes("copyAll");
    });
    const allCopy = buttons.find((b) => (b.textContent || "").includes("errors.copyAll"));
    expect(rowCopy).toBeTruthy();
    expect(allCopy).toBeTruthy();

    await act(async () => {
      rowCopy?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    // 行内复制：必须是**未截断的全文**。
    expect(writeTextMock).toHaveBeenCalledWith(long);

    writeTextMock.mockClear();
    await act(async () => {
      allCopy?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });
    const copiedAll = writeTextMock.mock.calls[0]?.[0] as string;
    expect(copiedAll).toContain("time\tmodel\tstatus\tsource\tmessage");
    expect(copiedAll).toContain(long);
    unmount();
  });
});
