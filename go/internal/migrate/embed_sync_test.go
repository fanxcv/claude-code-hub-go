package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// repoDrizzleDir 是迁移正文的真源（仓库根 drizzle/）。本包持有它的逐字节副本，
// 因为 go:embed 够不到模块（go/）之外的目录。
const repoDrizzleDir = "../../../drizzle"

// embeddedOrphanAllowlist 是不在 journal 里的内嵌 SQL 文件（drizzle 不会执行它们）。
//
// `drizzle/0048_add_system_timezone.sql` 在真源里就是孤立的（无 journal 条目）——
// Node 的 migrator 只遍历 journal.entries，故该文件从不执行，副本保留它是为了
// 与真源逐字节一致（差异即报红）。要增删这个清单必须同时给出真源依据。
var embeddedOrphanAllowlist = map[string]bool{
	"0048_add_system_timezone.sql": true,
}

// TestEmbeddedCopyMatchesRepositoryDrizzle 是**副本漂移钉子**。
//
// 少了它会出现最阴的一类事故：有人改了 drizzle/*.sql（或加了新迁移），Go 侧仍跑旧副本，
// 于是 Go 与 Node 在同一 schema 上执行不同的 DDL，账本 hash 也对不上。
func TestEmbeddedCopyMatchesRepositoryDrizzle(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("加载内嵌迁移失败: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("内嵌迁移为空")
	}

	for _, m := range migrations {
		want, err := os.ReadFile(filepath.Join(repoDrizzleDir, m.Tag+".sql"))
		if err != nil {
			t.Errorf("真源缺少迁移 %s: %v", m.Tag, err)
			continue
		}
		got, err := migrationsFS.ReadFile(sqlDir + m.Tag + ".sql")
		if err != nil {
			t.Errorf("内嵌副本缺少迁移 %s: %v", m.Tag, err)
			continue
		}
		if !bytes.Equal(want, got) {
			t.Errorf("迁移 %s 的副本与真源不一致（真源 %d 字节 / 副本 %d 字节）——"+
				"重跑一次复制：cp %s/*.sql go/internal/migrate/sql/",
				m.Tag, len(want), len(got), repoDrizzleDir)
		}
	}

	wantJournal, err := os.ReadFile(filepath.Join(repoDrizzleDir, "meta", "_journal.json"))
	if err != nil {
		t.Fatalf("真源缺少 journal: %v", err)
	}
	gotJournal, err := migrationsFS.ReadFile(journalFile)
	if err != nil {
		t.Fatalf("内嵌 journal 读取失败: %v", err)
	}
	if !bytes.Equal(wantJournal, gotJournal) {
		t.Errorf("journal 副本与真源不一致（真源 %d 字节 / 副本 %d 字节）", len(wantJournal), len(gotJournal))
	}
}

// TestEmbeddedSQLSetMatchesJournal 钉住「内嵌 SQL 集合 == journal 条目 + 已知孤立文件」。
//
// 两个方向都要报：内嵌多一个文件（真源里那是新迁移却没进 journal？）与少一个文件
// （journal 有条目但正文缺失，Load 会报错）都会红。
func TestEmbeddedSQLSetMatchesJournal(t *testing.T) {
	tags, err := Tags()
	if err != nil {
		t.Fatalf("读取 journal 失败: %v", err)
	}
	want := make(map[string]bool, len(tags))
	for _, tag := range tags {
		want[tag+".sql"] = true
	}
	for name := range embeddedOrphanAllowlist {
		want[name] = true
	}

	entries, err := migrationsFS.ReadDir(sqlDirName)
	if err != nil {
		t.Fatalf("列举内嵌 SQL 失败: %v", err)
	}
	got := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		got[entry.Name()] = true
	}

	var unexpected, missing []string
	for name := range got {
		if !want[name] {
			unexpected = append(unexpected, name)
		}
	}
	for name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(missing)
	if len(unexpected) > 0 {
		t.Errorf("内嵌目录里有 journal 未列的文件（drizzle 不会执行它们，副本却在增大）：%v", unexpected)
	}
	if len(missing) > 0 {
		t.Errorf("journal 列了但内嵌目录缺的文件：%v", missing)
	}
}

// TestEmbeddedSQLMatchesRepositoryFileSet 反向：真源新增了 .sql 却没进副本时也要可见。
//
// 只看真源**包含 journal 条目**的集合（`meta/*_snapshot.json` 与孤立文件的差异不在此断言内，
// 前两个用例已覆盖）。
func TestEmbeddedSQLMatchesRepositoryFileSet(t *testing.T) {
	entries, err := os.ReadDir(repoDrizzleDir)
	if err != nil {
		t.Fatalf("读取真源目录失败: %v", err)
	}
	repoSQL := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		repoSQL[entry.Name()] = true
	}

	embedded, err := migrationsFS.ReadDir(sqlDirName)
	if err != nil {
		t.Fatalf("列举内嵌 SQL 失败: %v", err)
	}
	embeddedSQL := make(map[string]bool)
	for _, entry := range embedded {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		embeddedSQL[entry.Name()] = true
	}

	var onlyRepo, onlyEmbedded []string
	for name := range repoSQL {
		if !embeddedSQL[name] {
			onlyRepo = append(onlyRepo, name)
		}
	}
	for name := range embeddedSQL {
		if !repoSQL[name] {
			onlyEmbedded = append(onlyEmbedded, name)
		}
	}
	sort.Strings(onlyRepo)
	sort.Strings(onlyEmbedded)
	if len(onlyRepo) > 0 {
		t.Errorf("真源有而副本没有的 SQL：%v（新增迁移后别忘了同步副本）", onlyRepo)
	}
	if len(onlyEmbedded) > 0 {
		t.Errorf("副本有而真源没有的 SQL：%v", onlyEmbedded)
	}
}
