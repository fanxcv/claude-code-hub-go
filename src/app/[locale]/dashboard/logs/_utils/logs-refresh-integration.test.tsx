import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { UsageLogsBatchResult } from "@/types/usage-logs";

/**
 * 刷新体验的端到端断言：刷新引擎（增量拉取 / 推送订阅） + 顶部统计面板一起挂载，
 * 于是「统计比列表稀疏」这类**跨组件**的约束可以被真正断言，而不是各测一半。
 */

const i18nMock = vi.hoisted(() => ({ t: (key: string) => key }));
const getUsageLogsBatchMock = vi.hoisted(() => vi.fn());
const getUsageLogsStatsMock = vi.hoisted(() => vi.fn());

vi.mock("next-intl", () => ({
  useTranslations: () => i18nMock.t,
}));

vi.mock("@/lib/api-client/v1/actions/usage-logs", () => ({
  getUsageLogsBatch: getUsageLogsBatchMock,
  getUsageLogsStats: getUsageLogsStatsMock,
}));

import {
  DEFAULT_LOGS_REFRESH_INTERVAL_MS,
  STATS_REFRESH_INTERVAL_MS,
  useLogsRefreshEngine,
  useLogsRefreshPreference,
} from "./logs-refresh";
import { writeLogsRefreshPreference } from "./logs-refresh";
import { UsageLogsStatsPanel } from "../_components/usage-logs-stats-panel";

function batchOf(ids: number[]): UsageLogsBatchResult {
  return {
    logs: ids.map((id) => ({ id }) as UsageLogsBatchResult["logs"][number]),
    nextCursor: null,
    hasMore: false,
  };
}

/** 已终态的行（`statusCode` 非空 ⇒ 终态写入完成；未终态时它在账本里是 NULL）。 */
function settledBatchOf(ids: number[]): UsageLogsBatchResult {
  return {
    logs: ids.map((id) => ({ id, statusCode: 200 }) as UsageLogsBatchResult["logs"][number]),
    nextCursor: null,
    hasMore: false,
  };
}

interface HarnessProps {
  mode: "pull" | "push";
  intervalMs: number;
  queryKey?: readonly unknown[];
  /** 自动刷新总开关（`false` = 正在浏览历史：暂停应用信号，但**不应**拆掉推送连接）。 */
  enabled?: boolean;
}

function Harness({
  mode,
  intervalMs,
  queryKey = ["usage-logs-batch", {}],
  enabled = intervalMs > 0,
}: HarnessProps) {
  useLogsRefreshEngine({
    queryKey,
    filters: {},
    mode,
    intervalMs,
    enabled,
  });
  return (
    <UsageLogsStatsPanel
      filters={{ userId: 1 }}
      autoRefreshIntervalMs={STATS_REFRESH_INTERVAL_MS}
    />
  );
}

/** 增量请求（带 sinceId 与 asc）的调用记录。 */
function incrementalCalls() {
  return getUsageLogsBatchMock.mock.calls.filter(([params]) => {
    const typed = params as { sinceId?: number; asc?: boolean };
    return typed?.sinceId !== undefined && typed?.asc === true;
  });
}

/** 首页重取（不带 sinceId）的调用记录。 */
function firstPageCalls() {
  return getUsageLogsBatchMock.mock.calls.filter(([params]) => {
    const typed = params as { sinceId?: number };
    return typed?.sinceId === undefined;
  });
}

describe("使用记录页刷新引擎 + 统计面板", () => {
  let container: HTMLElement;
  let root: Root;
  let queryClient: QueryClient;
  // 推送模式用例的推帧入口（由 stubStream 赋值）；非推送用例保持 null。
  let frameSink: ((text: string) => void) | null = null;

  beforeEach(() => {
    vi.useFakeTimers();
    getUsageLogsBatchMock.mockReset();
    getUsageLogsStatsMock.mockReset();
    getUsageLogsStatsMock.mockResolvedValue({ ok: false, error: "stub" });
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: Number.POSITIVE_INFINITY } },
    });
    window.localStorage.clear();
    frameSink = null;
  });

  afterEach(async () => {
    await act(async () => {
      root.unmount();
    });
    queryClient.clear();
    vi.useRealTimers();
    document.body.innerHTML = "";
  });

  /** 预置缓存（模拟首屏已落地），返回当前持有的行 id。 */
  function seedCache(ids: number[], queryKey: readonly unknown[] = ["usage-logs-batch", {}]) {
    queryClient.setQueryData(queryKey, {
      pages: [batchOf(ids)],
      pageParams: [undefined],
    });
  }

  async function renderHarness(props: HarnessProps) {
    await act(async () => {
      root.render(
        <QueryClientProvider client={queryClient}>
          <Harness {...props} />
        </QueryClientProvider>
      );
    });
  }

  test("默认间隔 3 秒：未到 3 秒不拉，到了才拉（间隔可配置生效）", async () => {
    seedCache([30]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([]) });

    await renderHarness({ mode: "pull", intervalMs: DEFAULT_LOGS_REFRESH_INTERVAL_MS });

    // 挂载时补一次（暂停恢复语义），先消费掉。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2999);
    });
    expect(incrementalCalls()).toHaveLength(0);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(2);
    });
    expect(incrementalCalls()).toHaveLength(1);
  });

  test("轮询只取新行（sinceId + asc），不重取已加载页", async () => {
    seedCache([30, 29]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([32, 31]) });

    await renderHarness({ mode: "pull", intervalMs: 3000 });
    // 挂载时会补拉一次（连接/恢复瞬间的空窗），先消费掉再计稳态。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    const calls = incrementalCalls();
    expect(calls).toHaveLength(1);
    const [params] = calls[0] as [{ sinceId: number; asc: boolean; limit: number }];
    // sinceId 跟的是「当前持有的最新一行」：挂载时的补拉已把 32/31 并进首页，故此处为 32。
    const cached = queryClient.getQueryData<{ pages: UsageLogsBatchResult[] }>([
      "usage-logs-batch",
      {},
    ]);
    const newestHeld = Math.max(...(cached?.pages[0].logs.map((log) => log.id) ?? [0]));
    expect(params.sinceId).toBe(newestHeld);
    expect(params.sinceId).toBe(32);
    expect(params.asc).toBe(true);
    expect(params.limit).toBe(50);

    // 整个 3 秒 tick 内没有任何一次「不带 sinceId」的全量重取。
    expect(firstPageCalls()).toHaveLength(0);
  });

  test("新行 prepend 到首页且保持去重与 id 降序", async () => {
    seedCache([30, 29]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([32, 31, 30]) });

    await renderHarness({ mode: "pull", intervalMs: 3000 });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });

    const cached = queryClient.getQueryData<{ pages: UsageLogsBatchResult[] }>([
      "usage-logs-batch",
      {},
    ]);
    expect(cached?.pages[0].logs.map((log) => log.id)).toEqual([32, 31, 30, 29]);
  });

  test("顶部统计随刷新更新，但比列表稀疏（30s vs 3s）", async () => {
    seedCache([30]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([]) });

    await renderHarness({ mode: "pull", intervalMs: 3000 });
    // 挂载期的补拉与统计首拉先消费掉，从稳态窗口开始计数。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsStatsMock.mockClear();
    getUsageLogsBatchMock.mockClear();

    // 30 秒 = 10 个列表 tick，统计只应刷 1 次。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });

    const listCalls = incrementalCalls().length;
    const statsCalls = getUsageLogsStatsMock.mock.calls.length;
    expect(listCalls).toBe(10);
    expect(statsCalls).toBe(1);
    expect(statsCalls).toBeLessThan(listCalls);
  });

  test("统计间隔为 0 时完全不自刷（关闭即关闭）", async () => {
    await act(async () => {
      root.render(
        <QueryClientProvider client={queryClient}>
          <UsageLogsStatsPanel filters={{ userId: 1 }} autoRefreshIntervalMs={0} />
        </QueryClientProvider>
      );
    });
    getUsageLogsStatsMock.mockClear();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(getUsageLogsStatsMock).not.toHaveBeenCalled();
  });

  test("首页兜底重取：间隔到期后重取第一页（覆盖在途行的用量落库）", async () => {
    seedCache([30]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([31]) });

    await renderHarness({ mode: "pull", intervalMs: 3000 });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });

    expect(firstPageCalls().length).toBeGreaterThanOrEqual(1);
  });

  test("向下翻历史时暂停自动刷新（不把用户正在看的位置顶走）", async () => {
    seedCache([30]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([]) });

    await act(async () => {
      root.render(
        <QueryClientProvider client={queryClient}>
          <HarnessDisabledWhileBrowsing />
        </QueryClientProvider>
      );
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(getUsageLogsBatchMock).not.toHaveBeenCalled();
  });

  test("推送模式建立 SSE 连接；切回轮询即彻底关闭（401 不重连）", async () => {
    seedCache([30]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([]) });

    // 永不结束的 SSE 流：连接保持打开，直到客户端主动 abort。
    const never = new ReadableStream<Uint8Array>({ start() {} });
    const signals: AbortSignal[] = [];
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input).includes("/usage-logs/stream")) {
        if (init?.signal) signals.push(init.signal as AbortSignal);
        return Promise.resolve(
          new Response(never, { status: 200, headers: { "Content-Type": "text/event-stream" } })
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    });
    vi.stubGlobal("fetch", fetchMock);

    await renderHarness({ mode: "push", intervalMs: 3000 });

    const streamCalls = fetchMock.mock.calls.filter(([input]) =>
      String(input).includes("/usage-logs/stream")
    );
    expect(streamCalls).toHaveLength(1);
    expect(signals).toHaveLength(1);
    expect(signals[0].aborted).toBe(false);

    // 挂载时的补拉（SSE 只报未来新行，连接就绪前的空窗必须补）先消费掉，再计稳态。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    // 推送模式下不再按间隔轮询列表（信号驱动）。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(9000);
    });
    expect(incrementalCalls()).toHaveLength(0);

    // 切回轮询：连接必须被彻底关闭。
    await renderHarness({ mode: "pull", intervalMs: 3000 });
    expect(signals[0].aborted).toBe(true);

    vi.unstubAllGlobals();
  });

  /**
   * 建一条可手动推字节的 SSE 流，返回推帧函数（模拟服务端发出 new-rows）。
   *
   * 用真流而非 mock 掉订阅：这条用例要证的正是「字节 → 分帧 → 解析 → 请求参数」整条链，
   * 把中间任何一环换成替身都会把要验的东西验掉。
   */
  function stubStream() {
    const encoder = new TextEncoder();
    let emit: ((text: string) => void) | null = null;
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(encoder.encode("event: ready\ndata: {}\n\n"));
        emit = (text: string) => controller.enqueue(encoder.encode(text));
      },
    });
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input).includes("/usage-logs/stream")) {
        return Promise.resolve(
          new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } })
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    });
    vi.stubGlobal("fetch", fetchMock);
    return (text: string) => emit?.(text);
  }

  /** 推开一帧并冲净流读取循环的微任务（分帧是异步的）。 */
  async function pushFrame(text: string) {
    await act(async () => {
      frameSink?.(text);
      await vi.advanceTimersByTimeAsync(0);
      await vi.advanceTimersByTimeAsync(0);
    });
  }

  test("推送信号带 minId 且跨度小：从 minId-1 重拉，把晚结算的低 id 行带回来", async () => {
    seedCache([300, 299]);
    // 晚结算那一行（id=288，低于高水位 300）会随重拉回到视图。
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([288]) });
    frameSink = stubStream();

    await renderHarness({ mode: "push", intervalMs: 3000 });
    // 连接就绪后的补拉先消费掉，再计稳态。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    await pushFrame('event: new-rows\ndata: {"maxId":300,"minId":287,"count":1}\n\n');

    const calls = incrementalCalls();
    expect(calls).toHaveLength(1);
    // 关键断言：下界是 minId-1（而非高水位 300）——否则 288 那一行永远拉不回来。
    expect((calls[0][0] as { sinceId?: number }).sinceId).toBe(286);
    expect((calls[0][0] as { asc?: boolean }).asc).toBe(true);

    const cached = queryClient.getQueryData<{ pages: { logs: { id: number }[] }[] }>([
      "usage-logs-batch",
      {},
    ]);
    expect(cached?.pages[0].logs.map((log) => log.id)).toContain(288);

    vi.unstubAllGlobals();
  });

  test("推送信号跨度超过上限：改重取首页（不带 sinceId），不拉那一段", async () => {
    seedCache([300]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([900]) });
    frameSink = stubStream();

    await renderHarness({ mode: "push", intervalMs: 3000 });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    // 跨度 800 > 200：重拉这一段不划算。
    await pushFrame('event: new-rows\ndata: {"maxId":900,"minId":100,"count":800}\n\n');

    expect(incrementalCalls()).toHaveLength(0);
    expect(firstPageCalls()).toHaveLength(1);

    vi.unstubAllGlobals();
  });

  test("旧形状信号（只有 maxId/count）：不报错，退回高水位增量", async () => {
    seedCache([300, 299]);
    // 返回空增量：让水位停在 300，单独验「无 minId 时用的是当前水位」这一件事。
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([]) });
    frameSink = stubStream();

    await renderHarness({ mode: "push", intervalMs: 3000 });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    await pushFrame('event: new-rows\ndata: {"maxId":301,"count":1}\n\n');

    const calls = incrementalCalls();
    expect(calls).toHaveLength(1);
    // 无 minId 时以**当前持有的最大 id**为水位（300），不是事件里的 maxId。
    expect((calls[0][0] as { sinceId?: number }).sinceId).toBe(300);

    vi.unstubAllGlobals();
  });

  /**
   * 永不结束的 SSE 流，并计「开了几条连接」。
   *
   * 用真流的意义：连接生命周期是本组用例要验的东西，把流换成替身就把要验的东西验掉了。
   */
  function stubCountingStream() {
    const encoder = new TextEncoder();
    let emit: ((text: string) => void) | null = null;
    const opens = { count: 0 };
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input).includes("/usage-logs/stream")) {
        opens.count += 1;
        const stream = new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(encoder.encode("event: ready\ndata: {}\n\n"));
            emit = (text: string) => controller.enqueue(encoder.encode(text));
          },
        });
        return Promise.resolve(
          new Response(stream, { status: 200, headers: { "Content-Type": "text/event-stream" } })
        );
      }
      return Promise.resolve(new Response("{}", { status: 200 }));
    });
    vi.stubGlobal("fetch", fetchMock);
    return { opens, sink: (text: string) => emit?.(text) };
  }

  test("信号节流：1 秒内 10 帧只合并成至多一次，且窗口末尾补齐（不漏数据）", async () => {
    seedCache([300, 299]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([]) });
    frameSink = stubStream();

    await renderHarness({ mode: "push", intervalMs: 3000 });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    // 高流量：同一瞬间连发 10 帧（服务端合并窗口 200ms ⇒ 上限 5 帧/秒）。
    for (let i = 1; i <= 10; i += 1) {
      await pushFrame(
        `event: new-rows\ndata: {"maxId":${300 + i},"minId":${300 + i},"count":1}\n\n`
      );
    }

    // 前沿那一次立即发送，其余九帧都被合并 ⇒ 绝不可能一帧一次。
    const burst = incrementalCalls().length;
    expect(burst).toBeLessThanOrEqual(2);

    // 窗口末尾必须补齐，并且用的是**合并后**的窗口（minId 取最小 ⇒ 302-1=301）。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    const calls = incrementalCalls();
    expect(calls.length).toBe(burst + 1);
    expect((calls[calls.length - 1][0] as { sinceId?: number }).sinceId).toBe(301);

    vi.unstubAllGlobals();
  });

  test("浏览历史（enabled=false）不拆连；暂停期间的信号在恢复时补一次，且用合并后的窗口", async () => {
    seedCache([300, 299]);
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([]) });
    const stream = stubCountingStream();
    frameSink = stream.sink;

    await renderHarness({ mode: "push", intervalMs: 3000, enabled: true });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(stream.opens.count).toBe(1);
    getUsageLogsBatchMock.mockClear();

    // 来回跨过「浏览历史」阈值：连接必须保持不变（原先每次翻转都拆连重建）。
    await renderHarness({ mode: "push", intervalMs: 3000, enabled: false });
    await renderHarness({ mode: "push", intervalMs: 3000, enabled: true });
    await renderHarness({ mode: "push", intervalMs: 3000, enabled: false });
    expect(stream.opens.count).toBe(1);

    // 隔离暂停窗口：上面的翻转会各补一次拉取（那是设计：从暂停恢复要补），先把它们清掉。
    getUsageLogsBatchMock.mockClear();

    // 暂停期间来的信号不得触发拉取（否则「暂停」名不副实）。
    await pushFrame('event: new-rows\ndata: {"maxId":301,"minId":286,"count":1}\n\n');
    expect(incrementalCalls()).toHaveLength(0);

    // 恢复：恰好补一次，且带的是暂停窗口里那个下界（286-1）——不丢一段。
    await renderHarness({ mode: "push", intervalMs: 3000, enabled: true });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    const calls = incrementalCalls();
    expect(calls).toHaveLength(1);
    expect((calls[0][0] as { sinceId?: number }).sinceId).toBe(285);
    expect(stream.opens.count).toBe(1);

    vi.unstubAllGlobals();
  });

  // 用户裁决：维持现状（保持 3 秒轮询 + 现有推送，不动刷新机制）⇒ 首页兜底**不做条件降频**。
  // 这条钉子把裁决钉住：两种模式都仍每 30 秒重取一次整页，不得悄悄改成按需/降频。
  test("首页兜底保持现状：两模式都每 30 秒重取整页（B4 未做）", async () => {
    queryClient.setQueryData(["usage-logs-batch", {}], {
      pages: [settledBatchOf([300, 299])],
      pageParams: [undefined],
    });
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: settledBatchOf([]) });

    await renderHarness({ mode: "pull", intervalMs: 3000 });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    // 一分钟 ⇒ 恰好两次（不因「取值已终态」而跳过）。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(firstPageCalls()).toHaveLength(2);

    // 推送模式同样如此：该档条件不含 mode。
    frameSink = stubStream();
    await renderHarness({ mode: "push", intervalMs: 3000 });
    getUsageLogsBatchMock.mockClear();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000);
    });
    expect(firstPageCalls()).toHaveLength(1);

    vi.unstubAllGlobals();
  });

  /**
   * 默认刷新模式的钉子（用户裁决：默认轮询）。
   *
   * 两条边界一起钉住，缺一条都会回归成「悄悄替用户改模式」：
   *  1. **无偏好** ⇒ `pull`（新用户与清过缓存的人都走轮询）；
   *  2. **有偏好** ⇒ 尊重已选项（已手动切到 push 的用户不得被默认值覆盖回去）。
   */
  function PreferenceHarness({ userId, probe }: { userId: number; probe: (mode: string) => void }) {
    const { mode } = useLogsRefreshPreference(userId);
    probe(mode);
    return null;
  }

  test("默认刷新模式为轮询：无偏好 ⇒ pull；已有偏好 ⇒ 尊重（不覆盖已选 push 的用户）", async () => {
    const seen: string[] = [];
    const probe = (mode: string) => {
      seen.push(mode);
    };
    const container = document.createElement("div");
    document.body.appendChild(container);
    const root = createRoot(container);

    // 无偏好：新用户。
    await act(async () => {
      root.render(
        <QueryClientProvider client={queryClient}>
          <PreferenceHarness userId={9001} probe={probe} />
        </QueryClientProvider>
      );
    });
    expect(seen[seen.length - 1]).toBe("pull");

    // 已有偏好 push：必须被尊重（默认值只在“读不到偏好”时生效）。
    writeLogsRefreshPreference(9002, { mode: "push", intervalMs: 5000 });
    seen.length = 0;
    await act(async () => {
      root.render(
        <QueryClientProvider client={queryClient}>
          <PreferenceHarness userId={9002} probe={probe} />
        </QueryClientProvider>
      );
    });
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(seen[seen.length - 1]).toBe("push");

    await act(async () => {
      root.unmount();
    });
    container.remove();
    window.localStorage.clear();
  });
});

/** 模拟「正在翻历史」：自动刷新总开关被上层关闭。 */
function HarnessDisabledWhileBrowsing() {
  useLogsRefreshEngine({
    queryKey: ["usage-logs-batch", {}],
    filters: {},
    mode: "pull",
    intervalMs: 3000,
    enabled: false,
  });
  return null;
}
