import { afterEach, describe, expect, test, vi } from "vitest";
import {
  parseNewRowsPayload,
  parseSseFrame,
  subscribeToUsageLogsStream,
  type UsageLogsStreamStatus,
} from "./usage-logs-stream";

/** 造一个可手动推帧的 SSE 响应流。 */
function streamResponse() {
  let controllerRef: ReadableStreamDefaultController<Uint8Array> | null = null;
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      controllerRef = controller;
    },
  });
  const encoder = new TextEncoder();
  return {
    response: new Response(body, {
      status: 200,
      headers: { "Content-Type": "text/event-stream" },
    }),
    push: (chunk: string) => controllerRef?.enqueue(encoder.encode(chunk)),
    close: () => controllerRef?.close(),
  };
}

function unauthorizedResponse() {
  return new Response("unauthorized", { status: 401 });
}

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("SSE 分帧", () => {
  test("解析 event 与 data", () => {
    expect(parseSseFrame('event: new-rows\ndata: {"maxId":7,"count":1}')).toEqual({
      event: "new-rows",
      data: '{"maxId":7,"count":1}',
    });
  });

  test("多行 data 以换行拼接（SSE 规范）", () => {
    expect(parseSseFrame("event: x\ndata: a\ndata: b")).toEqual({ event: "x", data: "a\nb" });
  });

  test("心跳注释帧与无 data 帧返回 null", () => {
    expect(parseSseFrame(": keep-alive")).toBeNull();
    expect(parseSseFrame("event: ready")).toBeNull();
  });

  test("CRLF 与冒号后无空格同样解析", () => {
    expect(parseSseFrame("event:ready\r\ndata:{}\r\n")).toEqual({ event: "ready", data: "{}" });
  });

  test("缺省事件名为 message", () => {
    expect(parseSseFrame("data: 1")).toEqual({ event: "message", data: "1" });
  });
});

describe("new-rows 负载", () => {
  test("合法负载", () => {
    expect(parseNewRowsPayload('{"maxId":7,"count":1}')).toEqual({ maxId: 7, count: 1 });
  });

  test("坏 JSON、缺字段、非数字一律丢帧（返回 null）", () => {
    expect(parseNewRowsPayload("{not json")).toBeNull();
    expect(parseNewRowsPayload('{"maxId":7}')).toBeNull();
    expect(parseNewRowsPayload('{"maxId":"7","count":1}')).toBeNull();
    expect(parseNewRowsPayload("null")).toBeNull();
  });
});

describe("订阅生命周期", () => {
  test("收到 new-rows 即回调；心跳与 ready 不触发", async () => {
    const { response, push, close } = streamResponse();
    const onNewRows = vi.fn();
    const statuses: UsageLogsStreamStatus[] = [];
    const stop = subscribeToUsageLogsStream(
      { onNewRows, onStatusChange: (status) => statuses.push(status) },
      vi.fn().mockResolvedValue(response)
    );

    await vi.waitFor(() => expect(statuses).toContain("connected"));
    push(": keep-alive\n\n");
    push("event: ready\ndata: {}\n\n");
    push('event: new-rows\ndata: {"maxId":42,"count":2}\n\n');
    await vi.waitFor(() => expect(onNewRows).toHaveBeenCalledTimes(1));

    expect(onNewRows).toHaveBeenCalledWith({ maxId: 42, count: 2 });
    expect(statuses[0]).toBe("connecting");
    close();
    stop();
  });

  test("401 → 上报 unauthorized 且不再重连（避免用错误凭据打死服务端）", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn().mockResolvedValue(unauthorizedResponse());
    const statuses: UsageLogsStreamStatus[] = [];
    subscribeToUsageLogsStream(
      { onNewRows: vi.fn(), onStatusChange: (status) => statuses.push(status) },
      fetchMock
    );

    await vi.waitFor(() => expect(statuses).toContain("unauthorized"));
    // 退避窗口全部过去，仍不应有第二次请求。
    await vi.advanceTimersByTimeAsync(120_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  test("网络失败 → 指数退避重连（1s、2s、4s…上限 30s）", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn().mockRejectedValue(new Error("network down"));
    const statuses: UsageLogsStreamStatus[] = [];
    const stop = subscribeToUsageLogsStream(
      { onNewRows: vi.fn(), onStatusChange: (status) => statuses.push(status) },
      fetchMock
    );

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    // 1s 后第二次
    await vi.advanceTimersByTimeAsync(1000);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    // 2s 后第三次
    await vi.advanceTimersByTimeAsync(2000);
    expect(fetchMock).toHaveBeenCalledTimes(3);
    // 4s 后第四次
    await vi.advanceTimersByTimeAsync(4000);
    expect(fetchMock).toHaveBeenCalledTimes(4);

    expect(statuses).toContain("reconnecting");
    stop();
  });

  test("stop 之后不再请求，且中止在途连接", async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn().mockRejectedValue(new Error("network down"));
    const stop = subscribeToUsageLogsStream({ onNewRows: vi.fn() }, fetchMock);

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    stop();
    await vi.advanceTimersByTimeAsync(120_000);
    expect(fetchMock).toHaveBeenCalledTimes(1);

    // 幂等：重复 stop 不会抛错。
    expect(() => stop()).not.toThrow();
  });

  test("stop 汇报 idle（切回轮询模式即彻底关闭）", async () => {
    const { response, close } = streamResponse();
    const statuses: UsageLogsStreamStatus[] = [];
    const stop = subscribeToUsageLogsStream(
      { onNewRows: vi.fn(), onStatusChange: (status) => statuses.push(status) },
      vi.fn().mockResolvedValue(response)
    );

    await vi.waitFor(() => expect(statuses).toContain("connected"));
    stop();
    expect(statuses.at(-1)).toBe("idle");
    close();
  });

  test("请求头声明 SSE 且带上会话凭据", async () => {
    const { response, close } = streamResponse();
    const fetchMock = vi.fn().mockResolvedValue(response);
    const stop = subscribeToUsageLogsStream({ onNewRows: vi.fn() }, fetchMock);

    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/usage-logs/stream");
    expect(init.credentials).toBe("include");
    expect((init.headers as Record<string, string>).Accept).toBe("text/event-stream");

    stop();
    close();
  });

  test("跨帧到达的分片（半帧）不会被误解析", async () => {
    const { response, push, close } = streamResponse();
    const onNewRows = vi.fn();
    const stop = subscribeToUsageLogsStream({ onNewRows }, vi.fn().mockResolvedValue(response));

    push('event: new-rows\ndata: {"maxId":1,');
    await vi.waitFor(() => expect(onNewRows).not.toHaveBeenCalled());
    push('"count":1}\n\n');
    await vi.waitFor(() => expect(onNewRows).toHaveBeenCalledWith({ maxId: 1, count: 1 }));

    stop();
    close();
  });
});
