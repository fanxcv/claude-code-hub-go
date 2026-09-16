import { describe, expect, it } from "vitest";

import enDashboard from "../../../messages/en/dashboard.json";
import zhCNDashboard from "../../../messages/zh-CN/dashboard.json";

/**
 * 损失动作取值域镜像 Go 的 `convert.LossAction`（go/internal/convert/hub.go）。
 *
 * 钉住它的理由与 pricing-source-keys.test.ts 相同：界面用 `t(\`lossAction.${action}\`)`
 * 拼键，Go 侧新增一个动作而词表没跟上时，界面上会掉出键名而不是文案——这类缺口
 * typecheck 看不见（动态键），只能靠词表覆盖断言拦下。
 */
const LOSS_ACTIONS = ["dropped", "downgraded", "rewritten"] as const;

const dashboards = {
  en: enDashboard,
  "zh-CN": zhCNDashboard,
};

describe("dashboard protocol conversion loss translations", () => {
  for (const [locale, dashboard] of Object.entries(dashboards)) {
    const conversion = dashboard.logs.protocolConversion;

    it(`${locale} covers every loss action`, () => {
      expect(Object.keys(conversion.lossAction).sort(), `${locale} loss action keys`).toEqual(
        [...LOSS_ACTIONS].sort()
      );
      for (const action of LOSS_ACTIONS) {
        expect(conversion.lossAction[action].trim(), `${locale} ${action}`).not.toBe("");
      }
    });

    it(`${locale} keeps the loss badge count placeholder`, () => {
      // 徽章文案必须保留 {count}：丢了占位符就只剩一句「丢失 项」，看不出丢了多少。
      expect(conversion.lossBadge).toContain("{count}");
      expect(conversion.lossTooltip.trim()).not.toBe("");
      expect(conversion.lossTotalLabel.trim()).not.toBe("");
    });
  }
});
