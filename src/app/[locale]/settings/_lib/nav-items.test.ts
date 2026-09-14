import fs from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import { SETTINGS_NAV_ITEMS } from "./nav-items";

function readProjectFile(...segments: string[]): string {
  return fs.readFileSync(path.join(process.cwd(), ...segments), "utf8");
}

describe("SETTINGS_NAV_ITEMS", () => {
  it("不含任何外链项（API 文档 / 使用文档 / 反馈问题三处外链已按要求移除）", () => {
    const externalItems = SETTINGS_NAV_ITEMS.filter((item) => /^https?:\/\//.test(item.href));

    expect(externalItems).toEqual([]);
  });

  it("三个外链菜单的目标地址与词条键都不再出现", () => {
    const hrefs = SETTINGS_NAV_ITEMS.map((item) => item.href);
    const labelKeys = SETTINGS_NAV_ITEMS.map((item) => item.labelKey);

    // 目标地址：外部站点与已下线的文档站
    expect(hrefs).not.toContain("/api/v1/scalar");
    expect(hrefs).not.toContain("https://claude-code-hub.app/");
    expect(hrefs).not.toContain("https://github.com/fanxcv/claude-code-hub-go/issues");
    // 词条键：留着会诱导下一个人再接回来
    expect(labelKeys).not.toContain("nav.apiDocs");
    expect(labelKeys).not.toContain("nav.docs");
    expect(labelKeys).not.toContain("nav.feedback");
  });

  it("首项仍是 /settings/config（settings 首页重定向依赖它）", () => {
    expect(SETTINGS_NAV_ITEMS[0]?.href).toBe("/settings/config");
  });
});

describe("设置侧栏与外链渲染（回归钉子）", () => {
  it("SettingsNav 不再有外链分区与 external 分支", () => {
    const source = readProjectFile(
      "src",
      "app",
      "[locale]",
      "settings",
      "_components",
      "settings-nav.tsx"
    );

    expect(source).not.toContain("ExternalLink");
    expect(source).not.toContain("externalItems");
    expect(source).not.toContain('target="_blank"');
  });

  it("settings 下拉子菜单不再有外链分支", () => {
    const source = readProjectFile(
      "src",
      "app",
      "[locale]",
      "dashboard",
      "_components",
      "dashboard-nav.tsx"
    );

    // 该文件仍保留 DashboardNavItem.external 这一既有（本批之前就无使用者的）通用分支，
    // 故只钉「设置子菜单不再按 SettingsNavItem.external 分流」这一点。
    expect(source).not.toContain("subItem.external");
  });
});
