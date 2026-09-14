import type { InfiniteData, QueryClient } from "@tanstack/react-query";
import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useRef, useState } from "react";
import { getUsageLogsBatch } from "@/lib/api-client/v1/actions/usage-logs";
import type { UsageLogsBatchResult } from "@/types/usage-logs";
import { type UsageLogsStreamStatus, useUsageLogsStream } from "./usage-logs-stream";

/**
 * 使用记录页的刷新机制：把「轮询拉取」与「SSE 推送信号」收敛到一处。
 *
 * 为什么自动刷新**只取新行**：
 * 列表查询服务端只要 15–21ms，但**每页载荷 284KB**、公网 p50 861ms ⇒ 体感慢在载荷与 RTT，
 * 不在查询。改前每次 tick 都会重取已加载的**所有**页（滚动越深越贵），现在改为
 * `sinceId` + `asc` 只取新行（几 KB）。
 *
 * 为什么另有「首页兜底重取」：新行按 id 增量取，但**在途行**的用量/计费在终态结算之后才写入
 * （见 `go/internal/dataplane/upstream.go` 的 `baseSettlement`），只靠增量会让这类行永远停在
 * 旧值。故按 `FIRST_PAGE_REFRESH_INTERVAL_MS` 重取首页覆盖一次。
 */

/** 自动刷新模式：`pull` 轮询拉取 / `push` SSE 推送信号（收到信号后仍走增量拉取）。 */
export type LogsRefreshMode = "pull" | "push";

/** 出厂默认刷新间隔：3 秒（用户裁决）。 */
export const DEFAULT_LOGS_REFRESH_INTERVAL_MS = 3000;

/** 出厂默认模式：轮询。推送通道异常时不影响任何人，故不做默认。 */
export const DEFAULT_LOGS_REFRESH_MODE: LogsRefreshMode = "pull";

/** 间隔取 0 表示「关闭自动刷新」（届时手动刷新仍可用）。 */
export const LOGS_REFRESH_INTERVAL_DISABLED = 0;

/** 间隔可选项（毫秒）。 */
export const LOGS_REFRESH_INTERVAL_OPTIONS = [
  1000,
  2000,
  3000,
  5000,
  10_000,
  LOGS_REFRESH_INTERVAL_DISABLED,
] as const;

/** 一页行数。与列表首屏一致，增量拉取也用同一口径。 */
export const LOGS_PAGE_SIZE = 50;

/**
 * 顶部统计的刷新间隔。统计单次服务端耗时 735–925ms（另一路在提速），故**有意**比列表稀疏：
 * 若与列表同频，每 tick 会多出近一秒的库查询，反而更慢。
 */
export const STATS_REFRESH_INTERVAL_MS = 30_000;

/** 首页兜底重取间隔（覆盖在途行的用量/计费落库）。 */
export const FIRST_PAGE_REFRESH_INTERVAL_MS = 30_000;

/** 使用记录列表的 queryKey 前缀（列表、统计各自有独立前缀）。 */
export const USAGE_LOGS_QUERY_KEY_PREFIX = "usage-logs-batch";

/**
 * 首页合并上限。增量行会 prepend 到首页，若不设上限，长时间挂着的页面会无限膨胀。
 * 200 行意味着 30 秒内超过 200 条新记录时才会丢视图（约 6.7 req/s 持续），届时下一个
 * 首页兜底重取会把最新 50 条补回来；超限只是少显示，不影响任何落库。
 */
const MAX_FIRST_PAGE_ROWS = 200;

/**
 * 推送信号里允许的 id 跨度上界（`maxId - minId`）。
 *
 * `minId` 用来把「晚结算的低 id 行」重新拉一遍（见 `usage-logs-stream.ts` 的 `minId` 说明）。
 * 跨度小 ⇒ 这一段重拉最多几十行（增量接口上限即 `LOGS_PAGE_SIZE`）；跨度大 ⇒ 说明信号窗口
 * 跨了大量新行（长时间断线重连、或异常服务端），此时重拉那一段不划算，改用首页兜底重取。
 * 与 `MAX_FIRST_PAGE_ROWS` 同量级：两者都是「一次刷新最多搬多少行」的口径。
 */
export const MAX_INCREMENTAL_SPAN_IDS = 200;

/** 收到推送信号后该做哪种刷新。 */
export type NewRowsRefreshPlan =
  /** 用指定下界做一轮增量拉取（含已看过的行，靠 id 去重，幂等）。 */
  | { kind: "incremental"; sinceId: number }
  /** 用当前高水位做增量（默认路径：只有新行，旧服务端无 `minId` 时也走这条）。 */
  | { kind: "watermark" }
  /** 跨度太大：改为重取首页，避免一次搬太多行。 */
  | { kind: "first-page" };

/**
 * 把一条推送信号翻译成刷新动作。纯函数，便于单测直接覆盖三种分支。
 *
 * 判据与理由：
 *  - 无 `minId`（旧服务端）或 `minId` 不是正整数 → 退回高水位增量，**不报错**；
 *  - `minId > maxId`（矛盾载荷）→ 同样退回高水位增量（不做递增假设）；
 *  - `maxId - minId <= 200` → 从 `minId - 1` 重拉这一段；
 *  - 否则 → 首页兜底重取。
 */
export function planNewRowsRefresh(event: { maxId: number; minId?: number }): NewRowsRefreshPlan {
  const { maxId, minId } = event;
  if (typeof minId !== "number" || !Number.isInteger(minId) || minId <= 0) {
    return { kind: "watermark" };
  }
  if (minId > maxId) return { kind: "watermark" };
  if (maxId - minId > MAX_INCREMENTAL_SPAN_IDS) return { kind: "first-page" };
  return { kind: "incremental", sinceId: minId - 1 };
}

export interface LogsRefreshPreference {
  mode: LogsRefreshMode;
  /** 0 = 关闭自动刷新。 */
  intervalMs: number;
}

const STORAGE_KEY_PREFIX = "cch:logs-refresh";

function storageKey(userId: number): string {
  return `${STORAGE_KEY_PREFIX}:${userId}`;
}

function isLogsRefreshMode(value: unknown): value is LogsRefreshMode {
  return value === "pull" || value === "push";
}

function isLogsRefreshInterval(value: unknown): value is number {
  return (
    typeof value === "number" &&
    (LOGS_REFRESH_INTERVAL_OPTIONS as readonly number[]).includes(value)
  );
}

/** 读取用户偏好；未设置或存了非法值（旧版本、手改）时返回 null 交由默认值兜底。 */
export function readLogsRefreshPreference(userId: number): LogsRefreshPreference | null {
  if (typeof window === "undefined") return null;
  try {
    const raw = window.localStorage.getItem(storageKey(userId));
    if (!raw) return null;
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return null;
    const { mode, intervalMs } = parsed as Partial<LogsRefreshPreference>;
    if (!isLogsRefreshMode(mode) || !isLogsRefreshInterval(intervalMs)) return null;
    return { mode, intervalMs };
  } catch {
    return null;
  }
}

export function writeLogsRefreshPreference(
  userId: number,
  preference: LogsRefreshPreference
): void {
  if (typeof window === "undefined") return;
  try {
    window.localStorage.setItem(storageKey(userId), JSON.stringify(preference));
  } catch {
    // 隐私模式/配额满：偏好落不下去不影响刷新本身，静默即可。
  }
}

/**
 * 刷新偏好（模式 + 间隔）。
 *
 * 来源说明：**用户偏好存 localStorage**，不做成系统设置——它是「这个人这块屏幕想多快刷新」
 * 的本地观感选项，做成系统设置要走表结构 + 管理面读写 + 五语种，收益不匹配。`defaultIntervalMs`
 * 参数保留给「默认值覆盖」来源（如 Go 壳注入），未提供时用 `DEFAULT_LOGS_REFRESH_INTERVAL_MS`。
 *
 * 首帧一律用默认值、挂载后再读 localStorage：静态导出的 HTML 在构建期没有 localStorage，
 * 若在初始化器里读会造成 hydration 不一致。
 */
export function useLogsRefreshPreference(
  userId: number,
  defaultIntervalMs: number = DEFAULT_LOGS_REFRESH_INTERVAL_MS
) {
  const [preference, setPreference] = useState<LogsRefreshPreference>({
    mode: DEFAULT_LOGS_REFRESH_MODE,
    intervalMs: defaultIntervalMs,
  });

  useEffect(() => {
    const stored = readLogsRefreshPreference(userId);
    if (stored) setPreference(stored);
  }, [userId]);

  const setMode = useCallback(
    (mode: LogsRefreshMode) => {
      setPreference((previous) => {
        const next = { ...previous, mode };
        writeLogsRefreshPreference(userId, next);
        return next;
      });
    },
    [userId]
  );

  const setIntervalMs = useCallback(
    (intervalMs: number) => {
      setPreference((previous) => {
        const next = { ...previous, intervalMs };
        writeLogsRefreshPreference(userId, next);
        return next;
      });
    },
    [userId]
  );

  return { mode: preference.mode, intervalMs: preference.intervalMs, setMode, setIntervalMs };
}

export type UsageLogsPages = InfiniteData<UsageLogsBatchResult>;

/** 单页行类型（不从 `@/repository` 另引类型：迟早随 Node 资产一并移除）。 */
type UsageLogRow = UsageLogsBatchResult["logs"][number];

/** 已加载页里最大的行 id（= 最新一行）；没有数据时返回 null。 */
export function newestLogId(pages: UsageLogsPages | undefined): number | null {
  if (!pages) return null;
  let newest: number | null = null;
  for (const page of pages.pages) {
    for (const log of page.logs) {
      if (newest === null || log.id > newest) newest = log.id;
    }
  }
  return newest;
}

function sortByIdDesc<T extends { id: number }>(rows: T[]): T[] {
  return [...rows].sort((left, right) => right.id - left.id);
}

/** 一页的游标 id：该页所含的行都不得低于它（服务端按它取下一页）。 */
function pageBoundaryId(page: UsageLogsBatchResult): number | null {
  return page.nextCursor?.id ?? null;
}

/** 取两个可选下界里更小的那个（合并信号窗口时 `minId` 取 min）。 */
function minOfOptional(left: number | undefined, right: number | undefined): number | undefined {
  if (left === undefined) return right;
  if (right === undefined) return left;
  return Math.min(left, right);
}

/**
 * 手动刷新：把使用记录列表的缓存**清回首页**再重取。
 *
 * 为什么不是 `invalidateQueries`：infinite query 失效后，页 0 按 `undefined` 重取、页 1 仍按
 * **旧游标**重取，夹在「新页 0 结尾」与「旧页 1 开头」之间的那些行两边都不覆盖——用户往下滚
 * 会跳过一段（与 `replaceFirstPage` 曾整页替换是同一类缺陷）。清回首页则让分页链从**新的**
 * 首页游标重新开始，没有夹缝；代价是刷新后列表回到一页（这与「刷新」的语义相符）。
 */
export async function resetUsageLogsPages(
  queryClient: QueryClient,
  queryKeyPrefix: readonly unknown[] = [USAGE_LOGS_QUERY_KEY_PREFIX]
): Promise<void> {
  await queryClient.resetQueries({ queryKey: queryKeyPrefix });
}

function collectHeldIds(pages: UsageLogsPages): Set<number> {
  const held = new Set<number>();
  for (const page of pages.pages) {
    for (const log of page.logs) held.add(log.id);
  }
  return held;
}

function mergeSessionMaps(
  left: UsageLogsBatchResult["sourceSessionIdsByIdentity"],
  right: UsageLogsBatchResult["sourceSessionIdsByIdentity"]
): UsageLogsBatchResult["sourceSessionIdsByIdentity"] {
  if (!right) return left;
  return { ...left, ...right };
}

/**
 * 把行分成「属于首页」与「落在首页游标之下」两组。
 *
 * 首页的行必须都**不低于**首页自己的游标：一旦把更旧的行塞进首页，下一次按该游标翻页就会把
 * 同一批行再取一遍，渲染层（`pages.flatMap(p => p.logs)`）于是出现重复行——现场表现为
 * 「使用记录列表一直增长」。而这些更旧的行本来就不属于首页的窗口。
 */
function splitByFirstPageBoundary(
  firstPage: UsageLogsBatchResult,
  rows: UsageLogRow[]
): { above: UsageLogRow[]; belowOrAt: UsageLogRow[] } {
  const boundary = pageBoundaryId(firstPage);
  if (boundary === null) return { above: rows, belowOrAt: [] };
  return {
    above: rows.filter((row) => row.id > boundary),
    belowOrAt: rows.filter((row) => row.id <= boundary),
  };
}

/**
 * 定向刷新：把「按 id 重读到的行」就地换掉已持有行的取值，**不新增、不移动、不动游标**。
 *
 * 用于在途行的终态回填（结算之后才写状态/用量/计费，而结算**不改 id**）。重读窗口之外的
 * 未知行一律忽略：它们不在当前已加载窗口里，塞进来会破坏分页边界（见 `routeBelowFirstPage`）。
 */
export function refreshRowsInPlace(
  pages: UsageLogsPages | undefined,
  rows: UsageLogRow[]
): UsageLogsPages | undefined {
  if (!pages || pages.pages.length === 0 || rows.length === 0) return pages;
  const { pageList, refreshed } = refreshHeldRows(pages.pages, rows);
  return refreshed === 0 ? pages : { ...pages, pages: pageList };
}

/** 定向重读的一段窗口：一次请求覆盖的行区间。`cursor` 缺省表示服务端从最新一行起算。 */
export interface NonTerminalRefreshWindow {
  cursor?: { createdAt: string; id: number };
  limit: number;
}

/** 单段窗口的行数上限：与服务端 `clampUsageLogLimit` 的上限（100）对齐。 */
const MAX_NON_TERMINAL_REFRESH_ROWS = 100;

/** 单轮最多规划的段数（超过则在最新 2000 行内分段；再宽的带子视为异常，登记为上限）。 */
const MAX_NON_TERMINAL_REFRESH_WINDOWS = 20;

/**
 * 规划覆盖全部非终态行的窗口（从新到旧、互不重叠，每段 ≤100 行）。无在途行时返回空数组。
 *
 * 为什么需要它：增量读的语义是 `id > sinceId`，而结算**不改 id** ⇒ 增量带不回终态；首页兜底
 * 又只重取最新 `limit` 行。于是一旦某行被新行挤出那个窗口，它就**永久**停在「请求中」，只有
 * 手动刷新（清回首页重取）才更新——现场症状。
 *
 * 依据两条既有语义即可精确覆盖：① `cursor` 表示「取该位置**之下**（更旧）的行」；② `limit` 上限
 * 100。故一段窗口 = 把游标设在「区间第一条的前一行」、`limit` 取区间行数；代价与**在途行数**
 * 成正比（通常个位数），不随列表总行数增长。
 *
 * **为何分段而非一段兜到底**：忙碌时**最新一行本身就是在途行**（带子顶在 index 0），而带子可能
 * 拖得很长（滞后的行 + 之后涌入的新行）。这时若只取「最新 100 行」，滞后的那条永远覆盖不到，
 * 修复在最要紧的场景下失效。分段后由调用方**逐轮轮转**，保证每一段都有机会被刷新。
 *
 * 上限（有意，登记在此）：带子宽于 20 段（2000 行）时，更旧的部分本轮不覆盖；行缺少
 * `createdAt` 时无法构造游标，该段及其更旧部分只能放弃（下一轮重试）。
 */
export function planNonTerminalRefresh(
  pages: UsageLogsPages | undefined
): NonTerminalRefreshWindow[] {
  if (!pages || pages.pages.length === 0) return [];
  const flat = pages.pages.flatMap((page) => page.logs);
  let top = -1;
  let bottom = -1;
  for (let index = 0; index < flat.length; index += 1) {
    // 只把**显式 null** 当作非终态：Go 的行渲染恒带该键（`set("statusCode", …)`，在途时为 null），
    // 而缺键意味着行形态不完整——那时频繁重读只会白耗请求，不做猜测。
    if (flat[index].statusCode !== null) continue;
    if (top === -1) top = index;
    bottom = index;
  }
  if (top === -1) return [];

  const windows: NonTerminalRefreshWindow[] = [];
  let start = top;
  while (start <= bottom && windows.length < MAX_NON_TERMINAL_REFRESH_WINDOWS) {
    const end = Math.min(bottom, start + MAX_NON_TERMINAL_REFRESH_ROWS - 1);
    if (start === 0) {
      // 区间顶就是列表首行：没有「前一行」可作游标，服务端从最新一行起算。
      windows.push({ limit: end + 1 });
    } else {
      const above = flat[start - 1];
      if (above.createdAt === null || above.createdAt === undefined) break;
      windows.push({
        cursor: { createdAt: new Date(above.createdAt).toISOString(), id: above.id },
        limit: end - start + 1,
      });
    }
    start = end + 1;
  }
  return windows;
}

/**
 * 把「首页游标之下」的行归位到它们所属的那一页。
 *
 * 归位规则：页 `i` 覆盖 `(本页游标, 上一页游标)` 区间，插入后仍按 id 降序。插入**不致页尾行**
 * 变动 ⇒ 各页游标与分页链保持有效，也不会与首页重叠。
 *
 * 没有任何已加载页覆盖它时**丢弃**：它不在当前已加载窗口里，用户滚到那个区间时服务端会
 * 把它（及最新取值）带回来。把它塞进首页恰恰就是上面那个重复的成因。
 */
function routeBelowFirstPage(
  pageList: UsageLogsBatchResult[],
  rows: UsageLogRow[]
): { pageList: UsageLogsBatchResult[]; placed: number } {
  if (pageList.length <= 1 || rows.length === 0) return { pageList, placed: 0 };
  const out = [...pageList];
  let placed = 0;
  for (const row of rows) {
    for (let index = 1; index < out.length; index += 1) {
      const upper = pageBoundaryId(out[index - 1]);
      const lower = pageBoundaryId(out[index]);
      if (upper !== null && row.id >= upper) continue;
      if (lower !== null && row.id < lower) continue;
      out[index] = { ...out[index], logs: sortByIdDesc([row, ...out[index].logs]) };
      placed += 1;
      break;
    }
  }
  return { pageList: out, placed };
}

/**
 * 用服务端重取到的行**就地刷新**已持有的行（在途行的用量/计费在结算之后才写入）。
 *
 * 就地替换（同 id、同位置）⇒ 不越界、不产生重复、不动任何页的游标。原先这里是「整页替换」，
 * 首页游标会随之前移，夹在「新首页游标」与「第二页开头」之间的行就再也取不到了（现场症状
 * 的另一半：「刷新后又消失」）。
 */
function refreshHeldRows(
  pageList: UsageLogsBatchResult[],
  rows: UsageLogRow[]
): { pageList: UsageLogsBatchResult[]; refreshed: number } {
  const byId = new Map(rows.map((row) => [row.id, row]));
  if (byId.size === 0) return { pageList, refreshed: 0 };
  let refreshed = 0;
  const out = pageList.map((page) => {
    let changed = false;
    const logs = page.logs.map((log) => {
      const replacement = byId.get(log.id);
      // 取值没变（浅比较）就不算刷新：保住「无实际变化 ⇒ 同一引用」的免重渲染不变量。
      if (replacement === undefined || isSameRowValues(log, replacement)) return log;
      changed = true;
      refreshed += 1;
      return replacement;
    });
    return changed ? { ...page, logs } : page;
  });
  return { pageList: refreshed > 0 ? out : pageList, refreshed };
}

/**
 * 行的取值是否未变（浅比较）。
 *
 * 只用于「要不要把同一 id 的行换成新对象」这个判定，故以**严格相等**为准：嵌套对象
 * （`providerChain` 等）引用不同就当作变了——向「宁可多渲染一次」的偏误靠，语义上安全。
 */
function isSameRowValues(left: UsageLogRow, right: UsageLogRow): boolean {
  const leftRecord = left as unknown as Record<string, unknown>;
  const rightRecord = right as unknown as Record<string, unknown>;
  const leftKeys = Object.keys(leftRecord);
  if (leftKeys.length !== Object.keys(rightRecord).length) return false;
  for (const key of leftKeys) {
    if (leftRecord[key] !== rightRecord[key]) return false;
  }
  return true;
}

/**
 * 把增量拉到的行并入列表。
 *
 * 按 id 去重并**重排为 id 降序**：即使对端忽略了 `asc`（并行开发期可能发生），或返回了
 * 已经持有的行，结果也仍然正确——去重后无新行时返回原引用，避免无谓重渲染。
 *
 * **只把严格新于首页游标的行并入首页**，其余归位到所属页（见 `routeBelowFirstPage`）：
 * 游标因此不会被留在「指向一个已经不在页里的行」的错位状态，翻页也就不会重复取到同一批行。
 */
export function mergeNewLogs(
  pages: UsageLogsPages | undefined,
  incoming: UsageLogsBatchResult
): UsageLogsPages | undefined {
  if (!pages || pages.pages.length === 0) return pages;

  // 已持有的行**也要**接受新版本：结算不改 id，故「同 id 的终态」必须就地换值。
  // 先前这里只用 heldIds 过滤新行，等于把「同 id 的更新」整类丢掉（现场症状的一半）。
  const refreshed = refreshHeldRows(pages.pages, incoming.logs);
  const heldIds = collectHeldIds(pages);
  const fresh = incoming.logs.filter((log) => !heldIds.has(log.id));

  const firstPage = refreshed.pageList[0];
  const { above, belowOrAt } = splitByFirstPageBoundary(firstPage, fresh);

  let pageList = refreshed.pageList;
  if (above.length > 0) {
    pageList = [
      {
        ...firstPage,
        logs: sortByIdDesc([...above, ...firstPage.logs]).slice(0, MAX_FIRST_PAGE_ROWS),
      },
      ...refreshed.pageList.slice(1),
    ];
  }
  const routed = routeBelowFirstPage(pageList, belowOrAt);
  if (refreshed.refreshed === 0 && above.length === 0 && routed.placed === 0) return pages;

  const head = routed.pageList[0];
  return {
    ...pages,
    pages: [
      {
        ...head,
        sourceSessionIdsByIdentity: mergeSessionMaps(
          head.sourceSessionIdsByIdentity,
          incoming.sourceSessionIdsByIdentity
        ),
      },
      ...routed.pageList.slice(1),
    ],
  };
}

/**
 * 用重取到的「最新一页」刷新列表：已持有的行**就地换取值**（用于刷新在途行的用量/计费），
 * 尚未持有的行按首页边界归位。
 *
 * 有意**不动首页游标**：整页替换会让游标前移，把「刚被挤出首页、又还没进第二页」的那些行
 * 变成取不到的洞（用户看到的是「列表一直增长、一刷新就少一批」）。就地刷新既保住了刷新取值
 * 这个初衷，也不会挪动分页链。
 */
export function replaceFirstPage(
  pages: UsageLogsPages | undefined,
  first: UsageLogsBatchResult
): UsageLogsPages | undefined {
  if (!pages || pages.pages.length === 0) return pages;

  const refreshed = refreshHeldRows(pages.pages, first.logs);
  const heldIds = collectHeldIds(pages);
  const fresh = first.logs.filter((log) => !heldIds.has(log.id));
  const { above, belowOrAt } = splitByFirstPageBoundary(pages.pages[0], fresh);

  let pageList = refreshed.pageList;
  if (above.length > 0) {
    pageList = [
      {
        ...pageList[0],
        logs: sortByIdDesc([...above, ...pageList[0].logs]).slice(0, MAX_FIRST_PAGE_ROWS),
      },
      ...pageList.slice(1),
    ];
  }
  const routed = routeBelowFirstPage(pageList, belowOrAt);
  if (refreshed.refreshed === 0 && above.length === 0 && routed.placed === 0) return pages;

  const head = routed.pageList[0];
  return {
    ...pages,
    pages: [
      {
        ...head,
        sourceSessionIdsByIdentity: mergeSessionMaps(
          head.sourceSessionIdsByIdentity,
          first.sourceSessionIdsByIdentity
        ),
      },
      ...routed.pageList.slice(1),
    ],
  };
}

interface UseLogsRefreshEngineOptions {
  /** 与 `useInfiniteQuery` 完全一致的 queryKey（增量结果要写回同一份缓存）。 */
  queryKey: readonly unknown[];
  /** 列表筛选条件（增量与首页重取都带上，否则会混入别的筛选结果）。 */
  filters: object;
  mode: LogsRefreshMode;
  /** 0 = 关闭自动刷新。 */
  intervalMs: number;
  /** 自动刷新总开关（用户关掉开关、或正在浏览历史时为 false）。 */
  enabled: boolean;
  onStreamStatusChange?: (status: UsageLogsStreamStatus) => void;
}

/**
 * 自动刷新引擎：`pull` 模式按间隔取新行，`push` 模式订阅 SSE 信号后取新行；
 * 两者都另有首页兜底重取。返回推送连接状态供界面显示。
 */
export function useLogsRefreshEngine({
  queryKey,
  filters,
  mode,
  intervalMs,
  enabled,
  onStreamStatusChange,
}: UseLogsRefreshEngineOptions): { streamStatus: UsageLogsStreamStatus } {
  const queryClient = useQueryClient();
  const [streamStatus, setStreamStatus] = useState<UsageLogsStreamStatus>("idle");
  const inFlightRef = useRef(false);
  const filtersRef = useRef(filters);
  filtersRef.current = filters;
  const queryKeyRef = useRef(queryKey);
  queryKeyRef.current = queryKey;
  // 「暂停」与间隔都放 ref：它们要参与信号处理却又不应重建回调（重建会被 useUsageLogsStream
  // 当作新回调，而该 hook 内部已用 ref 挡住，不必要地重建徒增可读性成本）。
  const enabledRef = useRef(enabled);
  enabledRef.current = enabled;
  const intervalMsRef = useRef(intervalMs);
  intervalMsRef.current = intervalMs;
  // 回调放 ref：调用方若传内联箭头函数，其身份每次渲染都变；若把它挂在订阅的 effect 依赖上
  // 就会「每次渲染重连」，所以这里不参与依赖计算。
  const onStreamStatusChangeRef = useRef(onStreamStatusChange);
  onStreamStatusChangeRef.current = onStreamStatusChange;

  const pullNewRows = useCallback(
    async (sinceIdOverride?: number) => {
      if (inFlightRef.current) return;
      let sinceId = sinceIdOverride;
      if (sinceId === undefined) {
        const cached = queryClient.getQueryData<UsageLogsPages>(queryKeyRef.current);
        const watermark = newestLogId(cached);
        // 首屏还没落地：让首屏自己负责，这里不发请求。
        if (watermark === null) return;
        sinceId = watermark;
      }

      inFlightRef.current = true;
      try {
        const result = await getUsageLogsBatch({
          ...filtersRef.current,
          sinceId,
          asc: true,
          limit: LOGS_PAGE_SIZE,
        });
        // 一次增量失败不弹错：下一个 tick 会重试，弹错只会在弱网下刷屏。
        if (!result.ok) return;
        queryClient.setQueryData<UsageLogsPages>(queryKeyRef.current, (previous) =>
          mergeNewLogs(previous, result.data)
        );
      } finally {
        inFlightRef.current = false;
      }
    },
    [queryClient]
  );

  const refreshFirstPage = useCallback(async () => {
    const result = await getUsageLogsBatch({ ...filtersRef.current, limit: LOGS_PAGE_SIZE });
    if (!result.ok) return;
    queryClient.setQueryData<UsageLogsPages>(queryKeyRef.current, (previous) =>
      replaceFirstPage(previous, result.data)
    );
  }, [queryClient]);

  // 非终态行的定向重读：增量读带不回终态（结算不改 id），首页兜底又只覆盖最新 limit 行 ⇒
  // 被新行挤出该窗口的在途行会永久停在「请求中」。这里按「非终态行所在的窗口」精确重读一段
  // （段与**在途行数**成正比，见 planNonTerminalRefresh），命中后由 refreshRowsInPlace 就地换值。
  const nonTerminalRefreshingRef = useRef(false);
  const nonTerminalWindowRef = useRef(0);
  const refreshNonTerminalRows = useCallback(async () => {
    // 重入保护：间隔短于上一次请求的往返时，不叠加第二个同义请求。
    if (nonTerminalRefreshingRef.current) return;
    const cached = queryClient.getQueryData<UsageLogsPages>(queryKeyRef.current);
    const windows = planNonTerminalRefresh(cached);
    // 没有非终态行就不发请求：空闲时这一路零开销。
    if (windows.length === 0) return;

    // 逐轮轮转：带子宽于一段时（忙碌中常见——最新一行本身就在途），每轮推进一步，
    // 保证每一段都有机会被刷新，不会让滞后的那几行饿死。单段时恒为 0。
    const index = nonTerminalWindowRef.current % windows.length;
    nonTerminalWindowRef.current = (index + 1) % windows.length;
    const window = windows[index];

    nonTerminalRefreshingRef.current = true;
    try {
      const result = await getUsageLogsBatch({
        ...filtersRef.current,
        cursor: window.cursor,
        limit: window.limit,
      });
      if (!result.ok) return;
      queryClient.setQueryData<UsageLogsPages>(queryKeyRef.current, (previous) =>
        refreshRowsInPlace(previous, result.data.logs)
      );
    } finally {
      nonTerminalRefreshingRef.current = false;
    }
  }, [queryClient]);

  const handleStreamStatusChange = useCallback((status: UsageLogsStreamStatus) => {
    setStreamStatus(status);
    onStreamStatusChangeRef.current?.(status);
  }, []);

  const streaming = mode === "push" && intervalMs > LOGS_REFRESH_INTERVAL_DISABLED;

  // 信号节流：把信号当**脏标记**，每 intervalMs 至多拉一次（前沿立即、窗口内合并、窗口末尾补齐）。
  // 服务端的合并窗口是 200ms（⇒ 信号率上限 5 次/秒），没有这层节流时，高流量下推送会比轮询更贵
  const pendingSignalRef = useRef<{ maxId: number; minId?: number } | null>(null);
  const flushTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const lastFlushAtRef = useRef(0);

  const flushPendingSignal = useCallback(() => {
    const pending = pendingSignalRef.current;
    pendingSignalRef.current = null;
    lastFlushAtRef.current = Date.now();
    if (pending === null) return;
    // 动作仍由 planNewRowsRefresh 决定（纯函数，三条分支各有单测）：
    // 跨度小时从 minId-1 重拉一段（把晚结算的低 id 行带回来），跨度大时改重取首页。
    const plan = planNewRowsRefresh(pending);
    if (plan.kind === "first-page") void refreshFirstPage();
    else if (plan.kind === "incremental") void pullNewRows(plan.sinceId);
    else void pullNewRows();
  }, [pullNewRows, refreshFirstPage]);

  // 窗口内信号合并成一份摘要：maxId 取大、minId 取小 ⇒ 语义等价于「一次收到这批行」。
  // 合并（而非丢弃）保证不丢数据：窗口末尾的那一拍会用合并后的窗口补上。
  const applyNewRowsSignal = useCallback(
    (event: { maxId: number; minId?: number }) => {
      const pending = pendingSignalRef.current;
      pendingSignalRef.current =
        pending === null
          ? { maxId: event.maxId, minId: event.minId }
          : {
              maxId: Math.max(pending.maxId, event.maxId),
              minId: minOfOptional(pending.minId, event.minId),
            };
      // 暂停期间（浏览历史/关掉开关）不拉取，但把窗口攒下来——恢复时补一次就够，不会缺一段。
      if (!enabledRef.current) return;
      if (flushTimerRef.current !== null) return;
      const wait = Math.max(0, intervalMsRef.current - (Date.now() - lastFlushAtRef.current));
      if (wait === 0) {
        flushPendingSignal();
        return;
      }
      flushTimerRef.current = setTimeout(() => {
        flushTimerRef.current = null;
        flushPendingSignal();
      }, wait);
    },
    [flushPendingSignal]
  );

  useUsageLogsStream({
    enabled: streaming,
    onNewRows: applyNewRowsSignal,
    onStatusChange: handleStreamStatusChange,
  });

  // 卸载时清掉待执行的那一拍（否则组件消失后仍会拉一次）。
  useEffect(
    () => () => {
      if (flushTimerRef.current !== null) {
        clearTimeout(flushTimerRef.current);
        flushTimerRef.current = null;
      }
    },
    []
  );

  // 轮询模式：按间隔取新行。
  useEffect(() => {
    if (!enabled || mode !== "pull" || intervalMs <= 0) return;
    const timer = setInterval(() => void pullNewRows(), intervalMs);
    return () => clearInterval(timer);
  }, [enabled, mode, intervalMs, pullNewRows]);

  // 首页兜底重取：两种模式都需要（在途行的用量/计费在结算后才写）。
  //
  // 用户裁决：**维持现状**（保持 3 秒轮询 + 现有推送，不动刷新机制）⇒ 本档不做条件降频。
  // 已知代价：两种模式都在付这笔整页载荷（约 284KB/30s），审计见
  // `` §0-⑦；若将来要降，判据与实测见
  // `` 的「用户裁决」一节（方案已备、未启用）。
  useEffect(() => {
    if (!enabled || intervalMs <= 0) return;
    const timer = setInterval(() => void refreshFirstPage(), FIRST_PAGE_REFRESH_INTERVAL_MS);
    return () => clearInterval(timer);
  }, [enabled, intervalMs, refreshFirstPage]);

  // 非终态行的定向重读：按同一间隔（两种模式都跑）——推送模式下「某行结算」不产生信号
  // （id 不变、无新行），故这一路只能靠定时器补。仅在**存在非终态行**时发请求。
  // 代价（有意，登记在此）：请求量与列表刷新同阶（+1/interval），上限 = 在途行数决定的窗口，
  // 空闲时为零；实测对照与判据见。
  useEffect(() => {
    if (!enabled || intervalMs <= 0) return;
    const timer = setInterval(() => void refreshNonTerminalRows(), intervalMs);
    return () => clearInterval(timer);
  }, [enabled, intervalMs, refreshNonTerminalRows]);

  // 从「暂停」恢复（如从历史浏览回到顶部、重新打开自动刷新开关）时补一次：
  // 推送模式的信号在暂停期间已经丢了，必须主动补，否则会一直缺一段。
  const wasEnabledRef = useRef(false);
  useEffect(() => {
    const wasEnabled = wasEnabledRef.current;
    wasEnabledRef.current = enabled;
    if (!enabled || wasEnabled) return;
    // 暂停期间攒下的信号按**合并后的窗口**补（连 minId-1 那段一起带回来）；没攒下就按高水位补。
    if (pendingSignalRef.current !== null) flushPendingSignal();
    else void pullNewRows();
  }, [enabled, flushPendingSignal, pullNewRows]);

  return { streamStatus };
}
