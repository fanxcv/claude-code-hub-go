import { describe, expect, it } from "vitest";

import enDashboard from "../../../messages/en/dashboard.json";
import zhCNDashboard from "../../../messages/zh-CN/dashboard.json";

const PRICING_SOURCES = [
  "local_manual",
  "cloud_exact",
  "cloud_model_fallback",
  "cloud_official",
  "priority_fallback",
  "single_provider_top_level",
  "official_fallback",
] as const;

const dashboards = {
  en: enDashboard,
  "zh-CN": zhCNDashboard,
};

describe("dashboard pricing source translations", () => {
  for (const [locale, dashboard] of Object.entries(dashboards)) {
    it(`${locale} covers every pricing source in list and detail views`, () => {
      const maps = [
        dashboard.logs.billingDetails.pricingSource,
        dashboard.logs.details.billingDetails.pricingSource,
      ];

      for (const pricingSource of maps) {
        expect(Object.keys(pricingSource).sort(), `${locale} pricing source keys`).toEqual(
          [...PRICING_SOURCES].sort()
        );
        for (const source of PRICING_SOURCES) {
          expect(pricingSource[source].trim(), `${locale} ${source}`).not.toBe("");
        }
      }
    });
  }
});
