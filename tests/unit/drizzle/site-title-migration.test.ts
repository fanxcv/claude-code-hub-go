import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";

interface JournalEntry {
  idx: number;
  tag: string;
}

interface MigrationJournal {
  entries: JournalEntry[];
}

const readMigrationFile = (path: string): string =>
  readFileSync(resolve(process.cwd(), path), "utf8");

const journal = JSON.parse(
  readMigrationFile("drizzle/meta/_journal.json")
) as MigrationJournal;

const indexOfTag = (tag: string): number =>
  journal.entries.findIndex((entry) => entry.tag === tag);

// drizzle 的 `*_snapshot.json` 是**累积** schema 快照（含此前全部迁移的叠加结果），
// 单条迁移 SQL 只含它自己那一笔。故断言「0115 时点同时存在 TTFB 与站点标题列」
// 必须分别读 0114 与 0115 两条迁移，不能只看 0115.sql。
const ttfbMigration = readMigrationFile("drizzle/0114_overconfident_ronan.sql");
const siteTitleMigration = readMigrationFile("drizzle/0115_breezy_polaris.sql");

describe("site title migration", () => {
  it("runs after the TTFB migration and preserves both schema changes", () => {
    const indexes = journal.entries.map(({ idx }) => idx);
    const tags = journal.entries.map(({ tag }) => tag);
    const ttfbMigrationIndex = indexOfTag("0114_overconfident_ronan");
    const siteTitleMigrationIndex = indexOfTag("0115_breezy_polaris");

    expect(new Set(indexes).size).toBe(indexes.length);
    expect(tags.filter((tag) => tag === "0114_overconfident_ronan")).toHaveLength(1);
    expect(tags.filter((tag) => tag === "0115_breezy_polaris")).toHaveLength(1);
    expect(ttfbMigrationIndex).toBeGreaterThanOrEqual(0);
    expect(siteTitleMigrationIndex).toBeGreaterThan(ttfbMigrationIndex);
    expect(journal.entries[ttfbMigrationIndex]).toMatchObject({
      idx: 114,
      tag: "0114_overconfident_ronan",
    });
    expect(journal.entries[siteTitleMigrationIndex]).toMatchObject({
      idx: 115,
      tag: "0115_breezy_polaris",
    });

    expect(ttfbMigration).toMatch(
      /ALTER TABLE "message_request" ADD COLUMN IF NOT EXISTS "first_byte_ms" integer/
    );
    expect(siteTitleMigration).toMatch(
      /ALTER TABLE "system_settings" ALTER COLUMN "site_title" SET DEFAULT 'CC Hub'/
    );
  });

  it("updates only titles that still use the legacy default", () => {
    expect(siteTitleMigration).toContain(
      `ALTER TABLE "system_settings" ALTER COLUMN "site_title" SET DEFAULT 'CC Hub'`
    );
    expect(siteTitleMigration).toContain(
      `UPDATE "system_settings" SET "site_title" = 'CC Hub' WHERE "site_title" = 'Claude Code Hub'`
    );
  });
});
