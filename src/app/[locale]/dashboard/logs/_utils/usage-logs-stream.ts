import { useEffect, useRef } from "react";

/**
 * 使用记录页的推送通道（SSE 信号式）。
 *
 * 契约（Go 侧，已冻结）：`GET /api/v1/usage-logs/stream`，事件 `ready`（`{}`）与
 * `new-rows`（`{"maxId":int,"minId":int,"count":int}`），每 15s 一行 `: keep-alive` 注释；
 * 鉴权与既有管理面一致，**未授权返回 401**。
 *
 * 为什么用 `fetch` 读流而不是 `EventSource`：契约要求「401 不重连」，而 `EventSource` 不暴露
 * HTTP 状态码，无法区分「未授权」与「网络抖动」，只能无脑重试；`fetch` 能拿到 `response.status`，
 * 于是可以精确地「401 停、其余退避重连」。取舍是自行解析 SSE 帧（本文件已实现并有单测）。
 *
 * 推送是**信号式**：事件里不带行内容，收到信号后由刷新引擎走增量拉取（`sinceId` + `asc`）。
 */

export type UsageLogsStreamStatus =
  | "idle"
  | "connecting"
  | "connected"
  | "reconnecting"
  | "unauthorized";

export interface UsageLogsStreamEvent {
  maxId: number;
  /**
   * `minId` 是本次信号窗口内被通知行的**最小** id（加性字段，2026-09 补）。
   *
   * 为何需要它：行 id 是**开行顺序**而非结算顺序。一条流式请求开行 5 分钟后才结算，
   * 此时它的 id 低于已记的高水位——只用 `maxId` 作 `sinceId` 永远取不到它，
   * 那一行的用量/计费就一直在页面上显示为空。
   *
   * 可选：缺它时（旧服务端）行为退回「用高水位增量」，不报错。
   */
  minId?: number;
  count: number;
}

export const USAGE_LOGS_STREAM_PATH = "/api/v1/usage-logs/stream";

/** 断线退避：1s 起、每次翻倍、上限 30s。 */
const INITIAL_BACKOFF_MS = 1000;
const MAX_BACKOFF_MS = 30_000;

export interface UsageLogsStreamHandlers {
  onNewRows: (event: UsageLogsStreamEvent) => void;
  onStatusChange?: (status: UsageLogsStreamStatus) => void;
}

/**
 * 解析一帧 SSE：返回事件名与拼接后的 data；心跳注释帧与无 data 的帧返回 null。
 * 独立导出以便单测直接覆盖分帧语义（多行 data、`:` 注释、CRLF）。
 */
export function parseSseFrame(frame: string): { event: string; data: string } | null {
  let event = "message";
  const dataLines: string[] = [];

  for (const rawLine of frame.split("\n")) {
    const line = rawLine.endsWith("\r") ? rawLine.slice(0, -1) : rawLine;
    if (line === "" || line.startsWith(":")) continue;
    const colon = line.indexOf(":");
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") event = value;
    else if (field === "data") dataLines.push(value);
  }

  if (dataLines.length === 0) return null;
  return { event, data: dataLines.join("\n") };
}

/** 解析 `new-rows` 事件负载；形状不对（缺字段、非数字）时返回 null 并丢帧。 */
export function parseNewRowsPayload(data: string): UsageLogsStreamEvent | null {
  try {
    const parsed: unknown = JSON.parse(data);
    if (typeof parsed !== "object" || parsed === null) return null;
    const { maxId, minId, count } = parsed as {
      maxId?: unknown;
      minId?: unknown;
      count?: unknown;
    };
    // maxId/count 是既有键，缺失或非数即视为坏帧。
    if (typeof maxId !== "number" || typeof count !== "number") return null;
    // minId 是**加性**键：旧服务端的信号没有它，此时不带该字段而非丢帧。
    if (typeof minId === "number") return { maxId, minId, count };
    return { maxId, count };
  } catch {
    return null;
  }
}

/**
 * 建立并维持一条 SSE 连接，返回停止函数（幂等）。
 * 401/403 → 上报 `unauthorized` 并**停止重连**（避免用错误凭据打死服务端）；其余失败退避重连。
 */
export function subscribeToUsageLogsStream(
  handlers: UsageLogsStreamHandlers,
  fetchImpl: typeof fetch = fetch
): () => void {
  let stopped = false;
  let backoffMs = INITIAL_BACKOFF_MS;
  let controller: AbortController | null = null;
  let retryTimer: ReturnType<typeof setTimeout> | null = null;

  const report = (status: UsageLogsStreamStatus) => handlers.onStatusChange?.(status);

  async function readFrames(body: ReadableStream<Uint8Array>): Promise<void> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buffer = "";

    try {
      while (!stopped) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });

        let separator = buffer.indexOf("\n\n");
        while (separator !== -1) {
          const frame = buffer.slice(0, separator);
          buffer = buffer.slice(separator + 2);
          const parsed = parseSseFrame(frame);
          if (parsed?.event === "new-rows") {
            const payload = parseNewRowsPayload(parsed.data);
            if (payload) handlers.onNewRows(payload);
          }
          separator = buffer.indexOf("\n\n");
        }
      }
    } finally {
      reader.cancel().catch(() => {});
    }
  }

  async function connect(): Promise<void> {
    if (stopped) return;
    controller = new AbortController();
    report(backoffMs === INITIAL_BACKOFF_MS ? "connecting" : "reconnecting");

    try {
      const response = await fetchImpl(USAGE_LOGS_STREAM_PATH, {
        credentials: "include",
        headers: { Accept: "text/event-stream", "Cache-Control": "no-cache" },
        signal: controller.signal,
      });

      if (response.status === 401 || response.status === 403) {
        stopped = true;
        report("unauthorized");
        return;
      }
      if (!response.ok || !response.body) {
        throw new Error(`usage-logs stream failed: ${response.status}`);
      }

      backoffMs = INITIAL_BACKOFF_MS;
      report("connected");
      await readFrames(response.body);
    } catch (error) {
      if (stopped) return;
      if (error instanceof DOMException && error.name === "AbortError") return;
      // 网络层失败：落到下面的退避重连。
    }

    if (stopped) return;
    report("reconnecting");
    retryTimer = setTimeout(() => {
      retryTimer = null;
      void connect();
    }, backoffMs);
    backoffMs = Math.min(backoffMs * 2, MAX_BACKOFF_MS);
  }

  void connect();

  return () => {
    if (stopped) return;
    stopped = true;
    if (retryTimer) clearTimeout(retryTimer);
    controller?.abort();
    handlers.onStatusChange?.("idle");
  };
}

/** 订阅推送通道；`enabled` 为 false 时不建立连接（切回轮询模式即彻底关闭）。 */
export function useUsageLogsStream({
  enabled,
  onNewRows,
  onStatusChange,
}: {
  enabled: boolean;
  onNewRows: (event: UsageLogsStreamEvent) => void;
  onStatusChange?: (status: UsageLogsStreamStatus) => void;
}): void {
  // 用 ref 持有回调：回调变化（如 filters 变化重建）不应导致重连。
  const onNewRowsRef = useRef(onNewRows);
  onNewRowsRef.current = onNewRows;

  useEffect(() => {
    if (!enabled) {
      onStatusChange?.("idle");
      return;
    }
    return subscribeToUsageLogsStream({
      onNewRows: (event) => onNewRowsRef.current(event),
      onStatusChange,
    });
  }, [enabled, onStatusChange]);
}
