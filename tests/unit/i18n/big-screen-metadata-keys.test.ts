import { describe, expect, test } from "vitest";

import enBigScreen from "../../../messages/en/bigScreen.json";
import zhCNBigScreen from "../../../messages/zh-CN/bigScreen.json";

describe("messages/<locale>/bigScreen metadata keys", () => {
  test("provides pageTitle/pageDescription", () => {
    const all = [enBigScreen, zhCNBigScreen];

    for (const bigScreen of all) {
      expect(bigScreen).toHaveProperty("pageTitle");
      expect(bigScreen).toHaveProperty("pageDescription");
    }
  });
});
