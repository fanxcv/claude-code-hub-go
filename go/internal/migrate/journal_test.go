package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"
)

// Node 侧 golden。取值方式（2026-09，drizzle-orm 版本见 go.mod 之外的 package.json）：
//
//	node -e 'const {readMigrationFiles}=require("drizzle-orm/migrator");const m=readMigrationFiles({migrationsFolder:"drizzle"});console.log(m.length,m[0].hash,m[0].sql.length)'
//
// 这些是**跨语言**的钉子：hash 与块数由 Node 的 migrator 亲自算出，Go 侧必须逐值相同，
// 否则两侧对「已应用」的判定就会分叉（Node 按 hash 记、Go 认不出 → 重复执行 DDL）。
//
// **末条（0125 起）的取值路径不同**：drizzle-orm/drizzle-kit 已随 Node 退役从 node_modules
// 移除，上面那条命令跑不动了。末条 hash 按 `TestHashIsSHA256OfWholeFile` 已证实的等价关系
// 直接取 `sha256sum drizzle/<tag>.sql`（Node 是 `createHash("sha256").update(整份 SQL 字符串)`，
// 与文件字节等价的前提由 `TestMigrationFilesAreValidUTF8` 钉住）；块数与 when 取自本仓
// journal 条目。idx0/idx62 两条仍是 Node 亲算的原始值，不可由本仓推导。
const (
	nodeMigrationCount       = 135
	nodeStatementCount       = 460
	nodeHashIdx0             = "4928849ae51d0159c1e638acb9ee6dc4def51f1cfd27edc7d6506b2b9534e539"
	nodeHashIdx0Tag          = "0000_legal_brother_voodoo"
	nodeHashIdx0Stmts        = 26
	nodeHashIdx62            = "bdfcc41b9451c8dafb551464734692ccf42abcfd49515e0d5fd975f7d21781da"
	nodeHashIdx62Tag         = "0062_aromatic_taskmaster"
	nodeHashIdx62Stmts       = 1
	nodeHashIdxLast          = "6eecc26f2ecb4f220cb0993e17b91b13b8afe467059b869b53c690a756f1f6ee"
	nodeHashLastTag          = "0135_provider_live_stats_switch"
	nodeHashLastStmts        = 1
	nodeLastWhen       int64 = 1790016000000
)

func TestLoadMatchesNodeGolden(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if len(migrations) != nodeMigrationCount {
		t.Fatalf("迁移条数: 得到 %d，Node 侧为 %d", len(migrations), nodeMigrationCount)
	}
	total := 0
	for _, m := range migrations {
		total += len(m.Statements)
	}
	if total != nodeStatementCount {
		t.Errorf("语句块总数: 得到 %d，Node 侧为 %d（切分语义不一致）", total, nodeStatementCount)
	}

	cases := []struct {
		idx   int
		tag   string
		hash  string
		stmts int
	}{
		{0, nodeHashIdx0Tag, nodeHashIdx0, nodeHashIdx0Stmts},
		{62, nodeHashIdx62Tag, nodeHashIdx62, nodeHashIdx62Stmts},
		{len(migrations) - 1, nodeHashLastTag, nodeHashIdxLast, nodeHashLastStmts},
	}
	for _, c := range cases {
		got := migrations[c.idx]
		if got.Tag != c.tag {
			t.Errorf("第 %d 条 tag: 得到 %q，期望 %q", c.idx, got.Tag, c.tag)
		}
		if got.Hash != c.hash {
			t.Errorf("%s 的 hash: 得到 %s，Node 侧为 %s", c.tag, got.Hash, c.hash)
		}
		if len(got.Statements) != c.stmts {
			t.Errorf("%s 的语句块数: 得到 %d，Node 侧为 %d", c.tag, len(got.Statements), c.stmts)
		}
	}
	if last := migrations[len(migrations)-1]; last.When != nodeLastWhen {
		t.Errorf("末条 when: 得到 %d，期望 %d", last.When, nodeLastWhen)
	}
}

// TestHashIsSHA256OfWholeFile 独立复算 hash（不依赖 Load 的实现路径）。
//
// Node 是 `crypto.createHash("sha256").update(query).digest("hex")`，其中 query 是
// `readFileSync(...).toString()` —— utf8 解码后再哈希，等价于直接哈希文件字节（前提是
// 文件本身是合法 utf8，见 TestMigrationFilesAreValidUTF8）。本用例按字节复算，验证该等价性。
func TestHashIsSHA256OfWholeFile(t *testing.T) {
	raw, err := migrationsFS.ReadFile(sqlDir + nodeHashIdx0Tag + ".sql")
	if err != nil {
		t.Fatalf("读取迁移失败: %v", err)
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != nodeHashIdx0 {
		t.Fatalf("按字节复算的 sha256 与 Node 不一致：%s", hex.EncodeToString(sum[:]))
	}
	// 字符串往返（解码后再编码）也必须同值——Node 走的正是这条路。
	roundTrip := sha256.Sum256([]byte(string(raw)))
	if hex.EncodeToString(roundTrip[:]) != nodeHashIdx0 {
		t.Errorf("utf8 往返后的 sha256 与按字节不同：%s", hex.EncodeToString(roundTrip[:]))
	}
}

// TestMigrationFilesAreValidUTF8 钉住 hash 等价性的前提。
//
// 若某个迁移文件不是合法 utf8，Node 的 `toString()` 会插入 U+FFFD 替换字符，其 hash 与
// Go 直接哈希字节的结果**不同**，两侧账本随即分叉。当前 124 条全为合法 utf8。
func TestMigrationFilesAreValidUTF8(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for _, m := range migrations {
		raw, err := migrationsFS.ReadFile(sqlDir + m.Tag + ".sql")
		if err != nil {
			t.Errorf("读取 %s 失败: %v", m.Tag, err)
			continue
		}
		if !utf8.Valid(raw) {
			t.Errorf("迁移 %s 不是合法 utf8：Node 的 toString() 会改写它，hash 将与 Go 分叉", m.Tag)
		}
	}
}

// TestNoWhitespaceOnlyStatementChunks 钉住 splitStatements 那处刻意差异的前提。
//
// Node 对 `split("--> statement-breakpoint")` 的每一块**无条件执行**（空串也会发给 PG，
// 得到 EmptyQueryResponse）；本实现跳过纯空白块。两侧副作用一致的前提是：
// **不存在纯空白块**。本用例把这个前提钉死——将来某条迁移以断点结尾就会红，
// 提醒作者要么去掉尾部断点，要么复核两侧语义。
func TestNoWhitespaceOnlyStatementChunks(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	for _, m := range migrations {
		raw, err := migrationsFS.ReadFile(sqlDir + m.Tag + ".sql")
		if err != nil {
			t.Errorf("读取 %s 失败: %v", m.Tag, err)
			continue
		}
		for i, chunk := range strings.Split(string(raw), statementBreakpoint) {
			if strings.TrimSpace(chunk) == "" {
				t.Errorf("迁移 %s 的第 %d 个块是纯空白：Node 会把它当一条空语句执行，"+
					"本实现跳过它——两者语义虽等价，但前提已破，请复核", m.Tag, i)
			}
		}
	}
}

func TestSplitStatementsUsesLiteralBreakpoint(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"单条语句", "SELECT 1", 1},
		{"两条语句", "SELECT 1;--> statement-breakpointSELECT 2;", 2},
		{"断点前后带空行仍切得开", "\nSELECT 1;\n--> statement-breakpoint\nSELECT 2;\n", 2},
		{"多一个空格就不再是断点", "SELECT 1;-->  statement-breakpointSELECT 2;", 1},
		{"大小写敏感", "SELECT 1;--> Statement-BreakpointSELECT 2;", 1},
		{"行内出现同样字面量也切", "SELECT '--> statement-breakpoint'", 2},
	}
	for _, c := range cases {
		if got := len(splitStatements(c.body)); got != c.want {
			t.Errorf("%s: 得到 %d 块，期望 %d 块（Node 是裸字面量切分，不 trim、不忽略大小写）",
				c.name, got, c.want)
		}
	}
}

func TestTagsFollowJournalOrder(t *testing.T) {
	tags, err := Tags()
	if err != nil {
		t.Fatalf("Tags 失败: %v", err)
	}
	raw, err := migrationsFS.ReadFile(journalFile)
	if err != nil {
		t.Fatalf("读取 journal 失败: %v", err)
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatalf("解析 journal 失败: %v", err)
	}
	if len(tags) != len(j.Entries) {
		t.Fatalf("tag 数与 journal 条目数不符: %d vs %d", len(tags), len(j.Entries))
	}
	for i := range tags {
		if tags[i] != j.Entries[i].Tag {
			t.Errorf("第 %d 个 tag 顺序不符: 得到 %s，journal 为 %s", i, tags[i], j.Entries[i].Tag)
		}
	}
}

// TestWhenByHashIsUnambiguous 每个 hash 必须唯一：自愈与状态判定都按 hash 找水位，
// 重复 hash（例如两条内容相同的迁移）会让「已应用」判定失去意义。
func TestWhenByHashIsUnambiguous(t *testing.T) {
	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	byHash, err := WhenByHash()
	if err != nil {
		t.Fatalf("WhenByHash 失败: %v", err)
	}
	if len(byHash) != len(migrations) {
		t.Errorf("hash 去重后条数 %d 少于迁移条数 %d：存在内容重复的迁移", len(byHash), len(migrations))
	}
}

// TestLoadFailsWhenJournalEntryHasNoSQL 复刻 Node 的
// `throw new Error("No file ... found")`（migrator.js:26）：清单与正文不一致必须失败，
// 而不是悄悄少跑一条迁移。
func TestLoadFailsWhenJournalEntryHasNoSQL(t *testing.T) {
	fsys := fstest.MapFS{
		journalFile: &fstest.MapFile{Data: []byte(`{"dialect":"postgresql","entries":[{"idx":0,"when":1,"tag":"0000_missing"}]}`)},
	}
	if _, err := loadFrom(fsys); err == nil {
		t.Fatal("journal 有条目但缺 SQL 文件时应当报错")
	} else if !strings.Contains(err.Error(), "0000_missing") {
		t.Errorf("错误信息应指明缺失的 tag，实际为: %v", err)
	}
}

func TestLoadFailsOnEmptyJournal(t *testing.T) {
	fsys := fstest.MapFS{
		journalFile: &fstest.MapFile{Data: []byte(`{"dialect":"postgresql","entries":[]}`)},
	}
	if _, err := loadFrom(fsys); err == nil {
		t.Fatal("空清单应当报错（迁移链路被切断时不可静默通过）")
	}
}
