import { readFileSync } from "node:fs";
import { describe, expect, test } from "vitest";

const docs = {
  readme: readFileSync("docs/api/v1/README.md", "utf8"),
  apiKeyAdmin: readFileSync("docs/security/api-key-admin-access.md", "utf8"),
};

describe("v1 API documentation", () => {
  test("distinguishes management and proxy API surfaces", () => {
    expect(docs.readme).toContain("/api/v1/*");
    expect(docs.readme).toContain("/v1/*");
    expect(docs.readme).toContain("/api/v1/scalar");
  });

  test("does not advertise the retired legacy actions surface", () => {
    // Node 后端退役时 `/api/actions/*` 一并删除。文档**可以**如实说明「它已删除」（那才是诚实），
    // 但**不得**把它写成可用面——所以这里逐行判定：凡命中该路径的行，必须同时说明已删除或 404。
    const mentions = docs.readme.match(/[^\n]*\/api\/actions\/\*[^\n]*/g) ?? [];
    expect(mentions.length).toBeGreaterThan(0); // 「已退役」的说明本身必须留在文档里
    for (const line of mentions) {
      expect(line).toMatch(/removed|404/);
    }
    expect(docs.readme).not.toContain("ENABLE_LEGACY_ACTIONS_API");
  });

  test("documents API key admin access as opt-in and disabled by default", () => {
    expect(docs.apiKeyAdmin).toContain("ENABLE_API_KEY_ADMIN_ACCESS=false");
    expect(docs.apiKeyAdmin).toContain("ENABLE_API_KEY_ADMIN_ACCESS=true");
    expect(docs.apiKeyAdmin).toContain("role=admin");
  });
});
