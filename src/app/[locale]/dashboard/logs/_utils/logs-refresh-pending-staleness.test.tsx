import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { UsageLogsBatchResult } from "@/types/usage-logs";

/**
 * 现场症状：「停留在页面上，新增的行全是『请求中』，且这些行**永不**变成终态；刷新页面才更新」。
 *
 * 两条独立机制（本文件既复现、也钉住修复）：
 *  A. 增量读的语义是 `id > sinceId`，而结算**不改 id** ⇒ 增量带不回终态；若合并层再用
 *     「已持有的 id」过滤，就把它整类丢掉。
 *  B. 首页兜底只重取「最新 limit 行」，在途行被新行挤出该窗口后再也没有机会被刷新；
 *     而表格自带的那条「重取所有已加载页」在使用记录页被显式关掉
 *     （`usage-logs-view-virtualized.tsx` 两处 `autoRefreshEnabled={false}`），故 B 无人兜底。
 *
 * 修复：按**非终态行所在的窗口**定向重读一段（`planNonTerminalRefresh` + `refreshRowsInPlace`），
 * 并让合并层接受同 id 的新版本。撤销任一处 ⇒ 本文件对应用例转红。
 */

const getUsageLogsBatchMock = vi.hoisted(() => vi.fn());
const getUsageLogsStatsMock = vi.hoisted(() => vi.fn());

vi.mock("@/lib/api-client/v1/actions/usage-logs", () => ({
  getUsageLogsBatch: getUsageLogsBatchMock,
  getUsageLogsStats: getUsageLogsStatsMock,
}));

import {
  mergeNewLogs,
  planNonTerminalRefresh,
  refreshRowsInPlace,
  replaceFirstPage,
  useLogsRefreshEngine,
} from "./logs-refresh";

const KEY = ["usage-logs-batch", {}];

/** 一行：`statusCode` 为 null ⇒ 非终态（界面显示「请求中」）。 */
function row(id: number, statusCode: number | null = null) {
  return {
    id,
    statusCode,
    createdAt: new Date(2026, 0, 1, 0, 0, id),
  } as unknown as UsageLogsBatchResult["logs"][number];
}

function batch(ids: number[], statusCode: number | null = null): UsageLogsBatchResult {
  return { logs: ids.map((id) => row(id, statusCode)), nextCursor: null, hasMore: false };
}

function pagesOf(first: UsageLogsBatchResult, second: UsageLogsBatchResult) {
  return { pages: [first, second], pageParams: [undefined, { createdAt: "x", id: 100 }] };
}

function statusOf(pages: { pages: UsageLogsBatchResult[] } | undefined, id: number) {
  for (const page of pages?.pages ?? []) {
    for (const log of page.logs) if (log.id === id) return log.statusCode;
  }
  return "absent";
}

function idsOfPage(pages: { pages: UsageLogsBatchResult[] } | undefined, index: number) {
  return pages?.pages[index].logs.map((log) => log.id) ?? [];
}

describe("在途行的终态回填（就地更新）", () => {
  test("合并层接受同 id 的新版本：增量带回终态时就地换值，不重复插入", () => {
    const pages = pagesOf(batch([101, 100], 200), batch([99], 200));
    pages.pages[0].logs[1] = row(100, null); // 100 在途

    const merged = mergeNewLogs(pages as never, {
      logs: [row(100, 200)],
      nextCursor: null,
      hasMore: false,
    });

    expect(statusOf(merged as never, 100)).toBe(200);
    // 仍是同一行、同一位置：不新增、不移动。
    expect(idsOfPage(merged as never, 0)).toEqual([101, 100]);
  });

  test("定向刷新就地换值，且忽略窗口外的未知行（不越界、不动游标）", () => {
    const first = {
      logs: [row(300, 200), row(299, null), row(298, 200)],
      nextCursor: { createdAt: "x", id: 298 },
      hasMore: true,
    } as unknown as UsageLogsBatchResult;
    const pages = pagesOf(first, batch([100], 200));

    const refreshed = refreshRowsInPlace(pages as never, [
      row(299, 200), // 我们持有的在途行 ⇒ 换值
      row(50, 200), // 窗口外未知行 ⇒ 忽略
    ]);

    expect(statusOf(refreshed as never, 299)).toBe(200);
    expect(statusOf(refreshed as never, 50)).toBe("absent");
    expect(idsOfPage(refreshed as never, 0)).toEqual([300, 299, 298]);
    expect((refreshed as never as { pages: UsageLogsBatchResult[] }).pages[0].nextCursor?.id).toBe(
      298
    );
  });

  test("重读窗口覆盖全部非终态行：游标取区间前一行，行数取区间跨度", () => {
    // 300..296 已终态；295、294 在途；293 已终态；292 在途。
    const first = {
      logs: [300, 299, 298, 297, 296, 295, 294, 293, 292].map((id) =>
        row(id, id === 295 || id === 294 || id === 292 ? null : 200)
      ),
      nextCursor: { createdAt: "x", id: 292 },
      hasMore: true,
    } as unknown as UsageLogsBatchResult;

    const windows = planNonTerminalRefresh(pagesOf(first, batch([100], 200)) as never);

    // 非终态区间是 295..292（索引 5..8，共 4 行）；其前一行是 296 ⇒ 游标 = 296 的位置。
    expect(windows).toEqual([
      {
        cursor: { createdAt: new Date(2026, 0, 1, 0, 0, 296).toISOString(), id: 296 },
        limit: 4,
      },
    ]);
  });

  test("带子宽于单段时分成多段（忙碌中最常见的形状：最新一行本身在途 + 深处滞后一行）", () => {
    // 索引 0（行 300）与索引 149（行 151）在途：一段覆盖不上去，必须分段才能覆盖 151。
    const first = {
      logs: Array.from({ length: 200 }, (_, index) => 300 - index).map((id) =>
        row(id, id === 300 || id === 151 ? null : 200)
      ),
      nextCursor: { createdAt: "x", id: 200 },
      hasMore: true,
    } as unknown as UsageLogsBatchResult;

    const windows = planNonTerminalRefresh(pagesOf(first, batch([100], 200)) as never);

    // 第一段从列表首行起（不给游标，覆盖最新 100 行）；第二段用索引 99 的行（201）作游标，覆盖其余 50 行。
    expect(windows).toHaveLength(2);
    expect(windows[0]).toEqual({ limit: 100 });
    expect(windows[1]).toEqual({
      cursor: { createdAt: new Date(2026, 0, 1, 0, 0, 201).toISOString(), id: 201 },
      limit: 50,
    });
  });

  test("列表首行即在途行时不给游标（服务端从最新一行起算）", () => {
    const first = {
      logs: [301, 300, 299].map((id) => row(id, id === 301 || id === 300 ? null : 200)),
      nextCursor: null,
      hasMore: false,
    } as unknown as UsageLogsBatchResult;

    expect(planNonTerminalRefresh(pagesOf(first, batch([], 200)) as never)).toEqual([{ limit: 2 }]);
  });

  test("没有非终态行时不发请求（空闲零开销）", () => {
    const first = {
      logs: [301, 300].map((id) => row(id, 200)),
      nextCursor: null,
      hasMore: false,
    } as unknown as UsageLogsBatchResult;

    expect(planNonTerminalRefresh(pagesOf(first, batch([100], 200)) as never)).toEqual([]);
    expect(planNonTerminalRefresh(undefined)).toEqual([]);
  });
});

describe("兜底窗口的边界（既有行为，非本次修复目标）", () => {
  test("兜底只重取最新 limit 行，被挤出窗口的行不在此路覆盖范围（由定向重读负责）", () => {
    const newest = Array.from({ length: 200 }, (_, index) => 300 - index);
    const first = {
      logs: newest.map((id) => row(id, 200)),
      nextCursor: { createdAt: "x", id: 200 },
      hasMore: true,
    } as unknown as UsageLogsBatchResult;
    first.logs[150] = row(150, null);

    const refreshed = replaceFirstPage(
      pagesOf(first, batch([100], 200)) as never,
      batch(newest.slice(0, 50), 200)
    );

    // 兜底窗口覆盖不到 150：这是窗口的固有限制，故 150 的终态由定向重读回填（见上一组用例）。
    expect(statusOf(refreshed as never, 150)).toBe(null);
  });
});

describe("引擎级：深于最新 limit 行的在途行也能拿到终态", () => {
  let container: HTMLElement;
  let root: Root;
  let queryClient: QueryClient;

  beforeEach(() => {
    vi.useFakeTimers();
    getUsageLogsBatchMock.mockReset();
    getUsageLogsStatsMock.mockReset();
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: Number.POSITIVE_INFINITY } },
    });
    window.localStorage.clear();
  });

  afterEach(async () => {
    await act(async () => {
      root.unmount();
    });
    queryClient.clear();
    vi.useRealTimers();
    document.body.innerHTML = "";
  });

  function Harness() {
    useLogsRefreshEngine({
      queryKey: KEY,
      filters: {},
      mode: "pull",
      intervalMs: 3000,
      enabled: true,
    });
    return null;
  }

  test("挂载后 60 秒内，深于最新 50 行的在途行得到终态", async () => {
    const ids = Array.from({ length: 200 }, (_, index) => 300 - index);
    queryClient.setQueryData(KEY, {
      pages: [
        {
          logs: ids.map((id) => row(id, id === 150 ? null : 200)),
          nextCursor: { createdAt: "x", id: 200 },
          hasMore: true,
        },
        batch([100], 200),
      ],
      pageParams: [undefined, { createdAt: "x", id: 100 }],
    });

    // 服务端行为：增量（sinceId）只回新行；兜底（无 sinceId/游标）只回最新 50 行；
    // 定向重读（带游标）回该窗口的行——150 的终态只可能从这里来。
    getUsageLogsBatchMock.mockImplementation(
      async (params: { sinceId?: number; cursor?: unknown }) => {
        if (params?.sinceId !== undefined) return { ok: true, data: batch([301, 302], null) };
        if (params?.cursor !== undefined) return { ok: true, data: batch([150], 200) };
        return { ok: true, data: batch(ids.slice(0, 50), 200) };
      }
    );

    await act(async () => {
      root.render(
        <QueryClientProvider client={queryClient}>
          <Harness />
        </QueryClientProvider>
      );
    });

    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });

    const held = queryClient.getQueryData<{ pages: UsageLogsBatchResult[] }>(KEY);
    expect(statusOf(held as never, 150)).toBe(200);

    // 定向重读确实发出过：带游标、且不带 sinceId（不与增量语义混用）。
    const targeted = getUsageLogsBatchMock.mock.calls
      .map(([params]) => params as { cursor?: unknown; sinceId?: number })
      .filter((params) => params?.cursor !== undefined);
    expect(targeted.length).toBeGreaterThan(0);
    expect(targeted.every((params) => params.sinceId === undefined)).toBe(true);
  });
});
