import { describe, expect, test } from "vitest";

import enSettings from "../../../messages/en/settings";
import zhCNSettings from "../../../messages/zh-CN/settings";

describe("messages/<locale>/settings module", () => {
  test("loads and keeps expected top-level keys", () => {
    const all = [enSettings, zhCNSettings];

    for (const settings of all) {
      expect(settings).toHaveProperty("providers");
      expect(settings).toHaveProperty("providers.form");
      expect(settings).toHaveProperty("mcpPassthroughConfig");
      expect(settings).toHaveProperty("mcpPassthroughUrlPlaceholder");
    }
  });
});
