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
// **按 queryKey 分派**（而非一个全局 mockReturnValue）：本弹窗现在有两个查询
// （熔断 + 低速），全局返回会让「切 tab 才请求低速数据」变成无法断言的。
const useQueryMock = vi.fn();
vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: unknown) => useQueryMock(options),
}));

const getProviderCircuitLogsMock = vi.fn();
const getProviderSlowLogsMock = vi.fn();
vi.mock("@/lib/api-client/v1/actions/providers", () => ({
  getProviderCircuitLogs: (...args: unknown[]) => getProviderCircuitLogsMock(...args),
  getProviderSlowLogs: (...args: unknown[]) => getProviderSlowLogsMock(...args),
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
import type {
  ProviderCircuitLogs,
  ProviderCircuitLogsError,
  ProviderSlowLogEvent,
  ProviderSlowLogs,
} from "@/types/provider";

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

/** 查询结果典形：非 pending 非 error。 */
function resolved(data: unknown) {
  return { data, isPending: false, isError: false, isFetching: false, refetch: vi.fn() };
}

/** 低速日志的完整响应；用例只覆盖自己关心的字段。 */
function slowPayload(overrides: Partial<ProviderSlowLogs> = {}): ProviderSlowLogs {
  return {
    providerId: 145,
    window: { limit: 20, retentionHours: 24, since: "2026-09-22T00:00:00.000Z" },
    events: [],
    // 默认未装配：多数用例只关心事件表，汇总行的存在由专条用例钉。
    diverts: null,
    unavailableReason: null,
    ...overrides,
  };
}

/**
 * 按 queryKey 分派返回值：两个查询共用同一个 useQuery 替身，
 * 只看一个全局返回值无法区分「谁的数据」——而本文件要同时断言两个 tab。
 */
function querySuccess(data: ProviderCircuitLogs, slow: Partial<ProviderSlowLogs> = {}) {
  const slowData = slowPayload(slow);
  useQueryMock.mockImplementation((options: { queryKey?: unknown[] }) => {
    const key = String(options?.queryKey?.[0] ?? "");
    return key === "provider-slow-logs" ? resolved(slowData) : resolved(data);
  });
}

/**
 * 切到低速 tab：点 Radix 的**真实**触发器。
 *
 * 为什么按文案找而不是按 value 属性：Radix 的 TabsTrigger 只渲染 role="tab" 与
 * aria-controls/id，**不把 value 写进 DOM**——按 value 找会静默拿到 null，
 * 于是「切 tab」这一步没发生，断言跟着假绿/假红。
 */
function clickSlowTab() {
  const triggers = [...document.querySelectorAll("[role='tab']")] as HTMLElement[];
  const slow = triggers.find((node) => (node.textContent ?? "").includes("circuitLogs.tabs.slow"));
  if (!slow) {
    throw new Error(
      `未找到低速 tab 触发器，实际触发器：${triggers.map((t) => t.textContent).join(" | ")}`
    );
  }
  // Radix 在 **mousedown** 上切值（click 不触发它的 onMouseDown）；button=0 是左键。
  act(() => {
    slow.dispatchEvent(new MouseEvent("mousedown", { bubbles: true, button: 0 }));
  });
}

/**
 * 取指定 queryKey **最近一次**的 useQuery 入参（用于断言 enabled 与 queryKey）。
 *
 * 为什么取最后一次而不是第一次：useQuery 每次渲染都被调用，第一次是打开弹窗那一刻的
 * （低速侧那时 enabled=false）。取第一次会让「切 tab 后变成 true」永远断不出来。
 */
function queryOptions(key: string): { enabled?: boolean; queryKey?: unknown[] } | undefined {
  const matched = useQueryMock.mock.calls.filter(
    (args) => String((args[0] as { queryKey?: unknown[] })?.queryKey?.[0] ?? "") === key
  );
  return matched.at(-1)?.[0] as { enabled?: boolean; queryKey?: unknown[] } | undefined;
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

/** 造一条低速事件；用例只覆盖自己关心的字段（其余取典形值）。 */
function slowEvent(overrides: Partial<ProviderSlowLogEvent> = {}): ProviderSlowLogEvent {
  return {
    kind: "penalty_up",
    at: 1789993274606,
    modelKey: "deepseek-v4.1-flash",
    penaltyFrom: 10,
    penaltyTo: 30,
    median: null,
    samples: null,
    reason: null,
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
    expect(queryOptions("provider-circuit-logs")?.enabled).toBe(false);
    expect(queryOptions("provider-circuit-logs")?.queryKey?.[1]).toBe(145);
    unmount();
  });

  // ———————————————————————————————————————————————————————————————
  // 以下四例针对低速 tab（用户需求：低速日志与熔断日志合窗、tab 切换）。
  // ———————————————————————————————————————————————————————————————

  it("切到低速 tab 才请求低速数据（懒加载）", () => {
    querySuccess(payload(), { events: [slowEvent()] });
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);

    // 默认停在熔断 tab：低速查询必须处于关闭态（否则开弹窗就白付一次 Redis 读）。
    expect(queryOptions("provider-slow-logs")?.enabled).toBe(false);
    // 且低速内容不得被渲染。
    expect(text()).not.toContain("circuitLogs.slow.title");

    // 切 tab：点 Radix 的触发器（真实交互，不是直接改 state）。
    clickSlowTab();

    // 切换后启用，且低速内容出现。
    expect(queryOptions("provider-slow-logs")?.enabled).toBe(true);
    expect(text()).toContain("circuitLogs.slow.title");
    unmount();
  });

  it("低速事件按种类渲染，且「不含此维」不画成 0", () => {
    querySuccess(payload(), {
      events: [
        slowEvent(),
        slowEvent({
          kind: "baseline_published",
          penaltyFrom: null,
          penaltyTo: null,
          median: 239.68,
          samples: 7215,
          reason: "primary",
        }),
      ],
    });
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);
    clickSlowTab();

    // 事件种类走词表键（禁硬编码文案），带前后值参数。
    expect(text()).toContain("circuitLogs.slow.kinds.penalty_up");
    expect(text()).toContain("circuitLogs.slow.detail.penalty");
    expect(text()).toContain('"from":10');
    expect(text()).toContain('"to":30');
    // 基线事件走另一条详情文案（含中位数与样本数）。
    expect(text()).toContain("circuitLogs.slow.kinds.baseline_published");
    expect(text()).toContain("circuitLogs.slow.detail.baseline");
    expect(text()).toContain('"median":239.68');
    // 时间范围必须写明（否则「24h 内无降权」会被读成「从未降权」）。
    expect(text()).toContain("circuitLogs.slow.window");
    unmount();
  });

  it("低速日志为空时给出带时间范围的空态；读不到时给出原因而不是空表", () => {
    querySuccess(payload(), { events: [] });
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);
    clickSlowTab();
    expect(text()).toContain("circuitLogs.slow.empty");
    expect(text()).toContain('"hours":24');
    unmount();

    // 读不到：必须是明确原因（不是一张空表）。
    querySuccess(payload(), { events: [], unavailableReason: "redis_unavailable" });
    const unmount2 = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);
    clickSlowTab();
    expect(text()).toContain("circuitLogs.slow.unavailable");
    expect(text()).toContain("redis_unavailable");
    unmount2();
  });

  it("有改道读数时显示汇总，且两成因分列（不是只给总数）", () => {
    querySuccess(payload(), {
      events: [slowEvent()],
      diverts: { windowHours: 24, total: 7, cooldown: 2, penalty: 5 },
    });
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);
    clickSlowTab();
    const body = text();
    expect(body).toContain("circuitLogs.slow.diverts.summary");
    // 两成因都要出现：合并成一个总数会让「会话冷却」与「渠道降权」无法区分（下一步动作不同）。
    expect(body).toContain('"total":7');
    expect(body).toContain('"cooldown":2');
    expect(body).toContain('"penalty":5');
    expect(body).toContain('"hours":24');
    unmount();
  });

  it("改道读数未装配（null）时不画汇总行，且不崩", () => {
    // null 与 0 必须可区分：前者是「不知道有没有改道」，后者是「确实没改道」。
    querySuccess(payload(), { events: [slowEvent()], diverts: null });
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);
    clickSlowTab();
    expect(text()).not.toContain("circuitLogs.slow.diverts.summary");
    unmount();
  });

  it("响应缺 diverts 键（undefined）时不崩、不画汇总行", () => {
    // undefined 与 null 都不该显示这一行。`!== null` 式的判断会把 undefined 放过去、渲染时抛错。
    //
    // 为何不能用 `delete`：夹具的 slowPayload 会把默认值（diverts: null）合回来，
    // 删掉的键会当场复活，用例就失去分辨力（首版就是这么写的，变异未红才发现的）。
    querySuccess(payload(), { events: [slowEvent()], diverts: undefined });
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);
    clickSlowTab();
    expect(text()).toContain("circuitLogs.slow.title");
    expect(text()).not.toContain("circuitLogs.slow.diverts.summary");
    unmount();
  });

  it("低速 tab 取数失败只在该 tab 内显示失败态，不影响熔断 tab", () => {
    useQueryMock.mockImplementation((options: { queryKey?: unknown[] }) => {
      const key = String(options?.queryKey?.[0] ?? "");
      if (key === "provider-slow-logs") {
        return {
          data: undefined,
          isPending: false,
          isError: true,
          isFetching: false,
          refetch: vi.fn(),
        };
      }
      return resolved(payload());
    });
    const unmount = render(<ProviderCircuitLogsDialog providerId={145} providerName="P" open />);

    // 熔断 tab 正常（不受低速侧失败影响）。
    expect(text()).not.toContain("circuitLogs.loadFailed");
    expect(text()).toContain("circuitLogs.state.title");

    clickSlowTab();
    expect(text()).toContain("circuitLogs.loadFailed");
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
