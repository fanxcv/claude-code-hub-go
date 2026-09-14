import { QueryClient } from "@tanstack/react-query";
import { afterEach, describe, expect, test } from "vitest";
import type { UsageLogsBatchResult } from "@/types/usage-logs";
import {
  DEFAULT_LOGS_REFRESH_INTERVAL_MS,
  DEFAULT_LOGS_REFRESH_MODE,
  FIRST_PAGE_REFRESH_INTERVAL_MS,
  LOGS_REFRESH_INTERVAL_OPTIONS,
  MAX_INCREMENTAL_SPAN_IDS,
  mergeNewLogs,
  newestLogId,
  planNewRowsRefresh,
  readLogsRefreshPreference,
  replaceFirstPage,
  resetUsageLogsPages,
  STATS_REFRESH_INTERVAL_MS,
  type UsageLogsPages,
  writeLogsRefreshPreference,
} from "./logs-refresh";

function pageOf(ids: number[], extra?: Partial<UsageLogsBatchResult>): UsageLogsBatchResult {
  return {
    logs: ids.map((id) => ({ id }) as UsageLogsBatchResult["logs"][number]),
    nextCursor: null,
    hasMore: false,
    ...extra,
  };
}

function pagesOf(...idGroups: number[][]): UsageLogsPages {
  return {
    pages: idGroups.map((ids) => pageOf(ids)),
    pageParams: idGroups.map(() => undefined),
  } as UsageLogsPages;
}

function logIds(pages: UsageLogsPages): number[] {
  return pages.pages.flatMap((page) => page.logs.map((log) => log.id));
}

afterEach(() => {
  window.localStorage.clear();
});

describe("刷新偏好", () => {
  test("默认间隔 3 秒、默认模式轮询", () => {
    expect(DEFAULT_LOGS_REFRESH_INTERVAL_MS).toBe(3000);
    expect(DEFAULT_LOGS_REFRESH_MODE).toBe("pull");
  });

  test("统计间隔比列表间隔稀疏（否则每 tick 多打一次近一秒的库查询）", () => {
    expect(STATS_REFRESH_INTERVAL_MS).toBeGreaterThan(DEFAULT_LOGS_REFRESH_INTERVAL_MS);
    expect(FIRST_PAGE_REFRESH_INTERVAL_MS).toBeGreaterThanOrEqual(STATS_REFRESH_INTERVAL_MS);
  });

  test("写入后按用户读取", () => {
    writeLogsRefreshPreference(7, { mode: "push", intervalMs: 10_000 });
    expect(readLogsRefreshPreference(7)).toEqual({ mode: "push", intervalMs: 10_000 });
    // 按用户隔离：另一个用户读不到。
    expect(readLogsRefreshPreference(8)).toBeNull();
  });

  test("未设置时返回 null（交由默认值兜底）", () => {
    expect(readLogsRefreshPreference(7)).toBeNull();
  });

  test("非法存量值一律丢弃：未知模式、越界间隔、非对象、坏 JSON", () => {
    const key = "cch:logs-refresh:7";
    window.localStorage.setItem(key, JSON.stringify({ mode: "socket", intervalMs: 3000 }));
    expect(readLogsRefreshPreference(7)).toBeNull();

    window.localStorage.setItem(key, JSON.stringify({ mode: "pull", intervalMs: 1234 }));
    expect(readLogsRefreshPreference(7)).toBeNull();

    window.localStorage.setItem(key, JSON.stringify("pull"));
    expect(readLogsRefreshPreference(7)).toBeNull();

    window.localStorage.setItem(key, "{not json");
    expect(readLogsRefreshPreference(7)).toBeNull();
  });

  test("可选项覆盖 1/2/3/5/10 秒与关闭", () => {
    expect([...LOGS_REFRESH_INTERVAL_OPTIONS]).toEqual([1000, 2000, 3000, 5000, 10_000, 0]);
    window.localStorage.setItem(
      "cch:logs-refresh:7",
      JSON.stringify({ mode: "pull", intervalMs: 0 })
    );
    expect(readLogsRefreshPreference(7)).toEqual({ mode: "pull", intervalMs: 0 });
  });
});

/**
 * 推送信号的翻译：`minId` 专治「晚结算的低 id 行」。
 *
 * 为何要这三条分开钉：行 id 是**开行顺序**而非结算顺序。一条流式请求开行得早（id 低）、
 * 结算得晚；它结算时前端已知的高水位已经远在它之上。只用高水位作 `sinceId`，
 * 那一行的用量/计费就永远拉不回来。
 */
describe("planNewRowsRefresh", () => {
  test("跨度小：从 minId-1 重拉这一段（把低于高水位的行带回来）", () => {
    // 高水位场景：最大 300，而本次被通知的最小行是 287（它等了很久才结算）。
    expect(planNewRowsRefresh({ maxId: 300, minId: 287 })).toEqual({
      kind: "incremental",
      sinceId: 286,
    });
  });

  test("跨度恰为上限 200 仍走增量；再大一点就改重取首页", () => {
    expect(planNewRowsRefresh({ maxId: 200, minId: 0 + 1 /* minId=1 */ })).toEqual({
      kind: "incremental",
      sinceId: 0,
    });
    expect(planNewRowsRefresh({ maxId: MAX_INCREMENTAL_SPAN_IDS + 1, minId: 1 })).toEqual({
      kind: "incremental",
      sinceId: 0,
    });
    // 跨度 = 201 > 200：重拉那一段不划算，改首页兜底。
    expect(planNewRowsRefresh({ maxId: MAX_INCREMENTAL_SPAN_IDS + 2, minId: 1 })).toEqual({
      kind: "first-page",
    });
  });

  test("minId 与 maxId 同值（窗口内只有一行）也走增量", () => {
    expect(planNewRowsRefresh({ maxId: 42, minId: 42 })).toEqual({
      kind: "incremental",
      sinceId: 41,
    });
  });

  test("旧服务端无 minId：不报错，退回高水位增量", () => {
    expect(planNewRowsRefresh({ maxId: 500 })).toEqual({ kind: "watermark" });
    expect(planNewRowsRefresh({ maxId: 500, minId: undefined })).toEqual({ kind: "watermark" });
  });

  test("坏载荷不退化成全量重拉：非正整数或 minId > maxId 均退回高水位", () => {
    // minId=0 会算出 sinceId=-1（相当于全表增量），故必须拦下。
    expect(planNewRowsRefresh({ maxId: 500, minId: 0 })).toEqual({ kind: "watermark" });
    expect(planNewRowsRefresh({ maxId: 500, minId: -3 })).toEqual({ kind: "watermark" });
    expect(planNewRowsRefresh({ maxId: 500, minId: 1.5 })).toEqual({ kind: "watermark" });
    // 矛盾载荷（下界大于上界）不做递增假设。
    expect(planNewRowsRefresh({ maxId: 100, minId: 900 })).toEqual({ kind: "watermark" });
  });
});

describe("newestLogId", () => {
  test("跨页取最大 id", () => {
    expect(newestLogId(pagesOf([30, 29], [28, 27]))).toBe(30);
  });

  test("无数据时返回 null", () => {
    expect(newestLogId(undefined)).toBeNull();
    expect(newestLogId(pagesOf([]))).toBeNull();
  });
});

describe("mergeNewLogs", () => {
  test("新行 prepend 到首页且保持 id 降序", () => {
    const pages = pagesOf([30, 29], [28]);
    const merged = mergeNewLogs(pages, pageOf([33, 32, 31])) as UsageLogsPages;

    expect(logIds(merged)).toEqual([33, 32, 31, 30, 29, 28]);
    // 后续页保持原样（不重取）
    expect(merged.pages[1].logs.map((log) => log.id)).toEqual([28]);
  });

  test("对端忽略 asc（返回降序）时结果仍然正确", () => {
    const merged = mergeNewLogs(pagesOf([30]), pageOf([33, 32, 31])) as UsageLogsPages;
    expect(logIds(merged)).toEqual([33, 32, 31, 30]);
  });

  test("已持有的行不会重复插入", () => {
    const merged = mergeNewLogs(pagesOf([30, 29]), pageOf([30, 29])) as UsageLogsPages;
    expect(logIds(merged)).toEqual([30, 29]);
  });

  test("没有新行时返回原引用（避免无谓重渲染）", () => {
    const pages = pagesOf([30]);
    expect(mergeNewLogs(pages, pageOf([]))).toBe(pages);
    expect(mergeNewLogs(pages, pageOf([30]))).toBe(pages);
  });

  test("合并身份映射（会话来源）", () => {
    const pages: UsageLogsPages = {
      pages: [pageOf([30], { sourceSessionIdsByIdentity: { a: ["s1"] } })],
      pageParams: [undefined],
    } as UsageLogsPages;
    const merged = mergeNewLogs(pages, {
      ...pageOf([31]),
      sourceSessionIdsByIdentity: { b: ["s2"] },
    }) as UsageLogsPages;
    expect(merged.pages[0].sourceSessionIdsByIdentity).toEqual({ a: ["s1"], b: ["s2"] });
  });

  test("首页不得超过上限（长时间挂着的页面不得无限膨胀）", () => {
    const held = Array.from({ length: 200 }, (_, index) => 1000 - index);
    const merged = mergeNewLogs(pagesOf(held), pageOf([2000, 1999])) as UsageLogsPages;
    expect(merged.pages[0].logs).toHaveLength(200);
    expect(merged.pages[0].logs[0].id).toBe(2000);
  });

  test("缓存为空时不做任何事", () => {
    expect(mergeNewLogs(undefined, pageOf([31]))).toBeUndefined();
    expect(mergeNewLogs(pagesOf(), pageOf([31]))).toEqual(pagesOf());
  });
});

describe("replaceFirstPage", () => {
  test("已持有的行就地换取值，新行并入首页，其余页与 pageParams 保持不动", () => {
    const pages = pagesOf([30, 29], [28, 27]);
    const replaced = replaceFirstPage(pages, pageOf([31, 30])) as UsageLogsPages;

    // 30 已持有 ⇒ 留在原位（不因刷新而少掉一行）；31 是新行 ⇒ 并入首页；29/28/27 不动。
    expect(logIds(replaced)).toEqual([31, 30, 29, 28, 27]);
    expect(replaced.pageParams).toEqual(pages.pageParams);
  });

  test("单页时并入新行且保留原有行", () => {
    const replaced = replaceFirstPage(pagesOf([30]), pageOf([31])) as UsageLogsPages;
    expect(replaced.pages).toHaveLength(1);
    expect(logIds(replaced)).toEqual([31, 30]);
  });

  test("没有新行时只换取值，不动页面结构", () => {
    const pages = pagesOf([30, 29]);
    // 重取到的 30 带上了结算后的取值：只有**取值有差异**才能区分「换值」与「忽略响应」
    // （fixture 取值相同时两者不可分辨，这正是「同 id 的终态被丢掉」这类缺陷能存活的原因）。
    const refetched = pageOf([30]);
    refetched.logs[0] = { id: 30, statusCode: 200 } as UsageLogsBatchResult["logs"][number];

    const replaced = replaceFirstPage(pages, refetched) as UsageLogsPages;
    expect(replaced).not.toBe(pages);
    expect(replaced.pages[0].logs[0].statusCode).toBe(200);
    expect(logIds(replaced)).toEqual([30, 29]);
    expect(replaced.pages[0].nextCursor).toEqual(pages.pages[0].nextCursor);
  });

  test("重取值与持有值完全相同时返回原引用（不做无谓重渲染）", () => {
    const pages = pagesOf([30, 29]);
    expect(replaceFirstPage(pages, pageOf([30]))).toBe(pages);
  });

  test("缓存为空时不做任何事", () => {
    expect(replaceFirstPage(undefined, pageOf([31]))).toBeUndefined();
  });
});

/**
 * 列表不变量（`mergeNewLogs` / `replaceFirstPage` 是唯二改这份缓存的地方）。
 *
 * 现场症状（用户报告）：使用记录页「数据会一直增长、刷新后又消失，且两种刷新模式一样」。
 * 下面三个场景各自钉住一条不变式；`I1`/`I2` 是渲染层的硬依赖（`virtualized-logs-table.tsx`
 * 直接 `pages.flatMap(p => p.logs)`，并以 `log.id` 作虚拟列表的 item key），`I3` 是分页链的连续性。
 */
describe("列表不变量：分页边界与去重", () => {
  /** 造一页：行按 id 降序，`nextCursor` 指向尾行（与服务端 `buildNextCursorOrThrow` 同口径），
   * 尾行时间戳用与 id 一一对应的串，便于断言游标没被悄悄换掉。 */
  function pageWithCursor(ids: number[]): UsageLogsBatchResult {
    const tail = ids[ids.length - 1];
    return {
      ...pageOf(ids),
      nextCursor: ids.length > 0 ? { createdAt: `t-${tail}`, id: tail } : null,
      hasMore: ids.length > 0,
    };
  }

  function pagesWithCursors(...idGroups: number[][]): UsageLogsPages {
    return {
      pages: idGroups.map((ids) => pageWithCursor(ids)),
      pageParams: idGroups.map(() => undefined),
    } as UsageLogsPages;
  }

  /** 服务端语义：返回严格旧于游标的 `limit` 行（`id < cursorId`），并给出新游标。 */
  function serverPage(allIdsDesc: number[], cursorId: number | null, limit = 50) {
    const ids = allIdsDesc.filter((id) => cursorId === null || id < cursorId).slice(0, limit);
    return pageWithCursor(ids);
  }

  /** 模拟 `useInfiniteQuery` 的 `fetchNextPage`：用**最后一页**的 `nextCursor` 再拉一页。 */
  function fetchNextPage(pages: UsageLogsPages, allIdsDesc: number[]): UsageLogsPages {
    const last = pages.pages[pages.pages.length - 1];
    const next = serverPage(allIdsDesc, last.nextCursor?.id ?? null);
    return {
      pages: [...pages.pages, next],
      pageParams: [...pages.pageParams, last.nextCursor ?? undefined],
    } as UsageLogsPages;
  }

  /** 全量 id（含重复）；重复即渲染层会出现同一行两次。 */
  function allIds(pages: UsageLogsPages): number[] {
    return logIds(pages);
  }

  function duplicates(ids: number[]): number[] {
    const seen = new Set<number>();
    const dup: number[] = [];
    for (const id of ids) {
      if (seen.has(id)) dup.push(id);
      else seen.add(id);
    }
    return dup;
  }

  /** 每页的行都必须**不低于自己那页的游标**：低于它的行属于后面那页，放进本页就会被
   * 后一次翻页重复取到（这就是「列表一直增长」的机制）。 */
  function cursorViolations(pages: UsageLogsPages): string[] {
    const bad: string[] = [];
    pages.pages.forEach((page, index) => {
      const cursorId = page.nextCursor?.id;
      if (cursorId === undefined) return;
      for (const log of page.logs) {
        if (log.id < cursorId) bad.push(`page[${index}] 含 id=${log.id}，低于本页游标 ${cursorId}`);
      }
    });
    return bad;
  }

  test("I1/I2：并入严格更新的行后，无重复且整体按 id 降序", () => {
    // 首页 100..51、第二页 50..1，两页游标均在尾行。
    const pages = pagesWithCursors(
      Array.from({ length: 50 }, (_, i) => 100 - i),
      Array.from({ length: 50 }, (_, i) => 50 - i)
    );
    const merged = mergeNewLogs(pages, pageOf([110, 109, 108])) as UsageLogsPages;
    const ids = allIds(merged);

    expect(duplicates(ids)).toEqual([]);
    expect(ids).toEqual([...ids].sort((a, b) => b - a));
    expect(cursorViolations(merged)).toEqual([]);
  });

  test("I1：晚结算的低 id 行（低于首页游标、且后续页未加载）不得被并入首页——否则翻页时重复", () => {
    // 只加载了首页：100..51，游标指向 51。
    const pages = pagesWithCursors(Array.from({ length: 50 }, (_, i) => 100 - i));
    const allIdsDesc = Array.from({ length: 300 }, (_, i) => 300 - i);

    // 推送模式的 `minId` 分支正是这种载荷：一批「晚结算」的行，id **低于**首页游标。
    const merged = mergeNewLogs(pages, pageOf([46, 45])) as UsageLogsPages;

    // 随后用户往下滚：服务端按首页游标返回 50..1，其中包含 46、45。
    const scrolled = fetchNextPage(merged, allIdsDesc);

    expect(cursorViolations(merged)).toEqual([]);
    expect(duplicates(allIds(scrolled))).toEqual([]);
  });

  test("I3：首页重取后，已加载页之间不得出现取不到的洞", () => {
    // 累积后的首页（200..151，游标 151）+ 已加载的第二页（150..101）。
    const pages = pagesWithCursors(
      Array.from({ length: 50 }, (_, i) => 200 - i),
      Array.from({ length: 50 }, (_, i) => 150 - i)
    );
    const allIdsDesc = Array.from({ length: 300 }, (_, i) => 300 - i);

    // 兜底重取拿到了「最新的 50 行」——游标因此前移到 251。
    const refreshed = replaceFirstPage(pages, serverPage(allIdsDesc, null)) as UsageLogsPages;
    const first = refreshed.pages[0];
    const second = refreshed.pages[1];

    // 链必须连续：首页尾行之下紧接着就该是第二页的首行，中间没有取不到的行。
    expect(first.nextCursor?.id ?? null).toBe((second.logs[0]?.id ?? 0) + 1);
    expect(duplicates(allIds(refreshed))).toEqual([]);
  });
});

describe("resetUsageLogsPages（手动刷新不留下取不到的夹缝）", () => {
  test("清掉旧分页链（而 invalidate 会把旧页留着 ⇒ 新旧页之间夹着取不到的行）", async () => {
    const queryClient = new QueryClient();
    const key = ["usage-logs-batch", {}];
    const twoPages = {
      pages: [pageOf([300, 299]), pageOf([250, 249])],
      pageParams: [undefined, null],
    };
    queryClient.setQueryData(key, twoPages);

    // invalidate：数据仍在（页 1 的旧游标保留）——这正是“新页 0 + 旧页 1”夹缝的来源。
    await queryClient.invalidateQueries({ queryKey: ["usage-logs-batch"] });
    expect(queryClient.getQueryData(key)).toEqual(twoPages);

    // reset：分页链整份清掉，下一次从新的首页游标重新开始。
    await resetUsageLogsPages(queryClient);
    expect(queryClient.getQueryData(key)).toBeUndefined();
  });
});
