/**
 * @vitest-environment happy-dom
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import type { UsageLogsBatchResult } from "@/types/usage-logs";

/**
 * 「页面不可见即暂停轮询」的钉子：后台标签页不做没人看的整页拉取，回到前台立刻补一次
 * （复用既有的「暂停→恢复补拉」路径，push 信号在暂停期间照收不丢）。
 */

const getUsageLogsBatchMock = vi.hoisted(() => vi.fn());

vi.mock("@/lib/api-client/v1/actions/usage-logs", () => ({
  getUsageLogsBatch: getUsageLogsBatchMock,
}));

import { useLogsRefreshEngine } from "./logs-refresh";

const QUERY_KEY = ["usage-logs-batch", {}];

function batchOf(ids: number[]): UsageLogsBatchResult {
  return {
    logs: ids.map((id) => ({ id }) as UsageLogsBatchResult["logs"][number]),
    nextCursor: null,
    hasMore: false,
  };
}

function Harness({ intervalMs }: { intervalMs: number }) {
  useLogsRefreshEngine({
    queryKey: QUERY_KEY,
    filters: {},
    mode: "pull",
    intervalMs,
    enabled: true,
  });
  return null;
}

/** `document.hidden` 在原型上是 getter：改实例属性来模拟切后台/回前台。 */
function setDocumentHidden(hidden: boolean) {
  Object.defineProperty(document, "hidden", { configurable: true, get: () => hidden });
  document.dispatchEvent(new Event("visibilitychange"));
}

describe("使用记录页：页面不可见时暂停轮询", () => {
  let container: HTMLElement;
  let root: Root;
  let queryClient: QueryClient;

  beforeEach(() => {
    vi.useFakeTimers();
    getUsageLogsBatchMock.mockReset();
    getUsageLogsBatchMock.mockResolvedValue({ ok: true, data: batchOf([]) });
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
    queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false, gcTime: Number.POSITIVE_INFINITY } },
    });
  });

  afterEach(async () => {
    await act(async () => {
      root.unmount();
    });
    queryClient.clear();
    vi.useRealTimers();
    document.body.innerHTML = "";
  });

  test("不可见期间 3 秒轮询停发请求，恢复可见立刻补一次", async () => {
    queryClient.setQueryData(QUERY_KEY, { pages: [batchOf([30])], pageParams: [undefined] });
    await act(async () => {
      setDocumentHidden(false);
      root.render(
        <QueryClientProvider client={queryClient}>
          <Harness intervalMs={3000} />
        </QueryClientProvider>
      );
    });
    // 挂载瞬间的补拉（暂停恢复语义）先消费掉，再计稳态。
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    getUsageLogsBatchMock.mockClear();

    // 前台：3 秒一跳照常
    await act(async () => {
      await vi.advanceTimersByTimeAsync(3000);
    });
    expect(getUsageLogsBatchMock).toHaveBeenCalledTimes(1);

    // 切后台：三路定时器（增量/首页/非终态）都不再发请求
    getUsageLogsBatchMock.mockClear();
    await act(async () => {
      setDocumentHidden(true);
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(getUsageLogsBatchMock).toHaveBeenCalledTimes(0);

    // 回前台：立即补一次，不等下一个 tick
    await act(async () => {
      setDocumentHidden(false);
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(getUsageLogsBatchMock).toHaveBeenCalledTimes(1);
  });
});
