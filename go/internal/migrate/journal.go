package migrate

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed sql/*.sql sql/meta/_journal.json
var migrationsFS embed.FS

const (
	// journalFile 是迁移清单的相对内嵌路径（对应 Node 的 `${folder}/meta/_journal.json`）。
	journalFile = "sql/meta/_journal.json"
	// sqlDirName 是迁移正文的内嵌目录名（fs.ReadDir 不接受尾部斜杠）。
	sqlDirName = "sql"
	// sqlDir 是迁移正文的相对内嵌路径前缀（供 ReadFile 拼接文件名）。
	sqlDir = sqlDirName + "/"
	// statementBreakpoint 是 Node 的语句分隔字面量（migrator.js:16）。
	//
	// 刻意用**字面量**常量而不是复用某个拼接结果：Node 侧是一次裸 `split("--> statement-breakpoint")`，
	// 前后空白不参与匹配也不做 trim，多一个空格就切不开。
	statementBreakpoint = "--> statement-breakpoint"
)

// journalEntry 对应 drizzle/meta/_journal.json 的单项。
//
// 只取迁移语义用得上的字段：tag（文件名）、when（folderMillis，账本 created_at 的取值）、
// breakpoints（sqlite 系方言用；pg 侧读入即弃，见 migrator.js:21）。
type journalEntry struct {
	Idx         int    `json:"idx"`
	Version     string `json:"version"`
	When        int64  `json:"when"`
	Tag         string `json:"tag"`
	Breakpoints bool   `json:"breakpoints"`
}

type journal struct {
	Dialect string         `json:"dialect"`
	Version string         `json:"version"`
	Entries []journalEntry `json:"entries"`
}

// Migration 是一条已加载的迁移。
type Migration struct {
	// Tag 是迁移标识（journal 的 tag，也是 sql/<Tag>.sql 的文件名）。
	Tag string
	// When 是 journal 的 when，即账本 created_at 的取值与排序依据。
	When int64
	// Hash 是整份 SQL 文件的 sha256 十六进制摘要（对 utf8 内容，不切分、不 trim）。
	Hash string
	// Statements 是按 statementBreakpoint 切分后的语句块，顺序即执行顺序。
	Statements []string
	// Breakpoints 是 journal 的 breakpoints 标记，仅为忠实记录（pg 侧不使用）。
	Breakpoints bool
}

var (
	loadOnce sync.Once
	loaded   []Migration
	loadErr  error
)

// Load 解析内嵌清单与迁移正文；结果在进程内缓存。
//
// 任一 journal 条目缺对应 SQL 文件即报错——与 Node 的
// `throw new Error("No file ... found")`（migrator.js:26）同义：
// 清单与正文不一致时必须失败，而不是悄悄少跑一条迁移。
func Load() ([]Migration, error) {
	loadOnce.Do(func() { loaded, loadErr = loadFrom(migrationsFS) })
	return loaded, loadErr
}

// loadFrom 从给定文件系统加载迁移，便于测试注入。
func loadFrom(fsys interface {
	ReadFile(name string) ([]byte, error)
}) ([]Migration, error) {
	raw, err := fsys.ReadFile(journalFile)
	if err != nil {
		return nil, fmt.Errorf("migrate: 读取迁移清单失败: %w", err)
	}
	var j journal
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, fmt.Errorf("migrate: 解析迁移清单失败: %w", err)
	}
	if len(j.Entries) == 0 {
		return nil, fmt.Errorf("migrate: 迁移清单为空: %s", journalFile)
	}

	out := make([]Migration, 0, len(j.Entries))
	for _, entry := range j.Entries {
		if entry.Tag == "" {
			return nil, fmt.Errorf("migrate: 清单第 %d 项缺少 tag", entry.Idx)
		}
		body, err := fsys.ReadFile(sqlDir + entry.Tag + ".sql")
		if err != nil {
			return nil, fmt.Errorf("migrate: 清单条目 %s 找不到对应 SQL 文件: %w", entry.Tag, err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{
			Tag:         entry.Tag,
			When:        entry.When,
			Hash:        hex.EncodeToString(sum[:]),
			Statements:  splitStatements(string(body)),
			Breakpoints: entry.Breakpoints,
		})
	}
	return out, nil
}

// splitStatements 复刻 Node 的 `query.split("--> statement-breakpoint")`。
//
// 一处**刻意的不等同**：Node 对切分出的每一块都无条件执行，本实现跳过纯空白块。
// 依据：PostgreSQL 对空白查询返回 EmptyQueryResponse（无语句可执行），
// 而当前 124 条迁移的 440 个块**不含任何空白块**（由 journal_test.go 钉住），
// 故两条路径的副作用与账本完全一致；跳过只是避免将来某条迁移以断点结尾时
// 多一次空往返。
func splitStatements(body string) []string {
	chunks := strings.Split(body, statementBreakpoint)
	out := make([]string, 0, len(chunks))
	for _, chunk := range chunks {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		out = append(out, chunk)
	}
	return out
}

// Tags 返回清单中的全部 tag（按 journal 顺序）。
func Tags() ([]string, error) {
	migrations, err := Load()
	if err != nil {
		return nil, err
	}
	tags := make([]string, 0, len(migrations))
	for _, m := range migrations {
		tags = append(tags, m.Tag)
	}
	return tags, nil
}

// WhenByHash 返回 hash → journal when 的映射，供账本自愈与状态判定使用。
func WhenByHash() (map[string]int64, error) {
	migrations, err := Load()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(migrations))
	for _, m := range migrations {
		out[m.Hash] = m.When
	}
	return out, nil
}
