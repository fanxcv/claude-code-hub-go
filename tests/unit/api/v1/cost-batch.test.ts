import { beforeEach, describe, expect, test, vi } from "vitest";

const postMock = vi.hoisted(() => vi.fn());

vi.mock("@/lib/api-client/v1/client", () => ({
  apiClient: {
    get: vi.fn(),
    patch: vi.fn(),
    post: postMock,
    delete: vi.fn(),
  },
}));

const costBatch = await vi.importActual<typeof import("@/lib/api-client/v1/actions/cost-batch")>(
  "@/lib/api-client/v1/actions/cost-batch"
);

/**
 * 批量成本客户端的契约测试。
 *
 * 重点不是「能取到数」，而是两条**会被界面误读成 0** 的边界：
 *   1. 单请求上限 200 → 必须自己分片（后端对超限是 400 `too_big`，不静默截断；
 *      若这里不分片，页面会整列失败并退化成「读数不可用」——可用但白跑）；
 *   2. `totalCost` 在网线上是 numeric 文本 → 必须转数字，否则排序/进度条会按字符串算。
 */
describe("cost-batch 客户端", () => {
  beforeEach(() => {
    postMock.mockReset();
  });

  test("单批请求：路径、正文与 numeric 文本转换", async () => {
    postMock.mockResolvedValue({
      items: [
        { id: 7, totalCost: "1.500000000000000" },
        { id: 3, totalCost: "0" },
      ],
    });

    const totals = await costBatch.getUserCostBatch([7, 3]);

    expect(postMock).toHaveBeenCalledTimes(1);
    expect(postMock.mock.calls[0][0]).toBe("/api/v1/users:costBatch");
    expect(postMock.mock.calls[0][1]).toEqual({ ids: [7, 3] });
    expect(totals.get(7)).toBe(1.5);
    expect(totals.get(3)).toBe(0);
  });

  test("超过 200 个 id 时自动分片（200 + 余数），不依赖后端截断", async () => {
    postMock.mockResolvedValue({ items: [] });
    const ids = Array.from({ length: 450 }, (_, index) => index + 1);

    await costBatch.getKeyCostBatch(ids);

    expect(postMock).toHaveBeenCalledTimes(3);
    expect(postMock.mock.calls.map((call) => call[1].ids.length)).toEqual([200, 200, 50]);
    expect(postMock.mock.calls[0][0]).toBe("/api/v1/keys:costBatch");
  });

  test("重复 id 先去重再分片（避免响应里出现重复 id）", async () => {
    postMock.mockResolvedValue({ items: [{ id: 5, totalCost: "2" }] });

    const totals = await costBatch.getUserCostBatch([5, 5, 5]);

    expect(postMock).toHaveBeenCalledTimes(1);
    expect(postMock.mock.calls[0][1]).toEqual({ ids: [5] });
    expect(totals.size).toBe(1);
  });

  test("空 id 列表不发请求", async () => {
    const totals = await costBatch.getKeyCostBatch([]);

    expect(postMock).not.toHaveBeenCalled();
    expect(totals.size).toBe(0);
  });

  test("取数失败向上抛出（由调用方决定降级为「读数不可用」，不得吞成 0）", async () => {
    postMock.mockRejectedValue(new Error("boom"));

    await expect(costBatch.getUserCostBatch([1])).rejects.toThrow("boom");
  });
});
