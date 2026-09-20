import fs from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";
import type { UiSessionSnapshot } from "@/components/ui-session-gate";
import { buildDashboardNavItems } from "./dashboard-nav-items";

/** 与服务端时代的 `getTranslations("dashboard.nav")` 同键名的桩翻译器。 */
const t = (key: string) =>
  ({
    availability: "Availability",
    dashboard: "Dashboard",
    documentation: "Docs",
    leaderboard: "Leaderboard",
    login: "Login",
    myQuota: "My Quota",
    providers: "Providers",
    quotasManagement: "Quotas",
    systemSettings: "Settings",
    usageLogs: "Usage Logs",
    userManagement: "Users",
  })[key] ?? key;

function buildSession(canLoginWebUi: boolean, role: "admin" | "user"): UiSessionSnapshot {
  return {
    user: { id: 1, name: "Ada Lovelace", role },
    key: { canLoginWebUi },
  };
}

const hrefs = (session: UiSessionSnapshot | null) =>
  buildDashboardNavItems(session, t).map((item) => item.href);

function readProjectFile(...segments: string[]): string {
  return fs.readFileSync(path.join(process.cwd(), ...segments), "utf8");
}

describe("buildDashboardNavItems", () => {
  it("只读会话没有任何可见项（原「只留使用文档」一项已随入口移除）", () => {
    const items = hrefs(buildSession(false, "user"));

    expect(items).toEqual([]);
  });

  it("普通用户会话保留非管理员导航，且配额入口指向 /dashboard/my-quota", () => {
    const items = hrefs(buildSession(true, "user"));

    expect(items).toContain("/dashboard/logs");
    expect(items).toContain("/dashboard/my-quota");
    // 管理员专属项不得出现
    expect(items).not.toContain("/dashboard/availability");
    expect(items).not.toContain("/dashboard/providers");
    expect(items).not.toContain("/settings");
    expect(items).not.toContain("/dashboard/quotas");
  });

  it("管理员会话额外带管理员专属项，且配额入口指向 /dashboard/quotas", () => {
    const items = hrefs(buildSession(true, "admin"));

    expect(items).toContain("/dashboard/quotas");
    expect(items).toContain("/dashboard/availability");
    expect(items).toContain("/dashboard/providers");
    expect(items).toContain("/settings");
    expect(items).not.toContain("/dashboard/my-quota");
  });

  it("未登录（壳注入 session=null）时按匿名渲染全部非管理员项，不崩", () => {
    const items = hrefs(null);

    expect(items).toContain("/dashboard");
    expect(items).toContain("/dashboard/my-quota");
    expect(items).not.toContain("/dashboard/availability");
  });
});

describe("头部入口钉子", () => {
  // 这两条曾在 1.6.0 快照里钉「不存在」，2026-09 按要求恢复该功能后改为正向钉。
  it("头部导航项不含「文档」入口（任何会话形态）", () => {
    const sessions: (UiSessionSnapshot | null)[] = [
      null,
      buildSession(true, "admin"),
      buildSession(true, "user"),
      buildSession(false, "user"),
    ];

    for (const session of sessions) {
      expect(hrefs(session)).not.toContain("/usage-doc");
    }
  });

  it("DashboardHeader 挂载新版本检查入口，且在语言切换之后", () => {
    const header = readProjectFile(
      "src",
      "app",
      "[locale]",
      "dashboard",
      "_components",
      "dashboard-header.tsx"
    );

    expect(header).toContain("VersionUpdateNotifier");
    expect(header.indexOf("<VersionUpdateNotifier />")).toBeGreaterThan(
      header.indexOf("<LanguageSwitcher")
    );
  });

  it("新版本检查组件存在（提示能力不丢）", () => {
    const component = path.join(
      process.cwd(),
      "src",
      "components",
      "customs",
      "version-update-notifier.tsx"
    );

    expect(fs.existsSync(component)).toBe(true);
  });
});
