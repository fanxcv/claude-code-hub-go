package ratelimit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// repoLuaDir 是仓库根的权威副本目录；与 internal/convert 的语料读取保持同一相对路径约定。
const repoLuaDir = "../../../lua"

func TestLoadVerifiesManifestAgainstEmbeddedCopies(t *testing.T) {
	registry, err := Load()
	if err != nil {
		t.Fatalf("加载内嵌脚本失败: %v", err)
	}
	if registry.Len() != 18 {
		t.Fatalf("脚本数量不符: 期望 18, 实际 %d", registry.Len())
	}
	for _, script := range registry.All() {
		if script.Source == "" {
			t.Fatalf("脚本 %s 正文为空", script.File)
		}
		sum := sha256.Sum256([]byte(script.Source))
		if hex.EncodeToString(sum[:]) != script.SHA256 {
			t.Fatalf("脚本 %s 摘要与正文不符", script.File)
		}
		byFile, ok := registry.LookupFile(script.File)
		if !ok || byFile != script {
			t.Fatalf("脚本 %s 按文件名索引不一致", script.File)
		}
		byConst, ok := registry.LookupConst(script.ConstName)
		if !ok || byConst != script {
			t.Fatalf("脚本 %s 按常量名索引不一致", script.ConstName)
		}
	}
}

func TestEmbeddedIsMemoized(t *testing.T) {
	first, err := Embedded()
	if err != nil {
		t.Fatalf("首次加载失败: %v", err)
	}
	second, err := Embedded()
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if first != second {
		t.Fatal("Embedded 未复用同一注册表")
	}
}

// TestEmbeddedCopiesMatchRepoLua 是本包的安全网：内嵌副本必须与仓库根 lua/ 原文逐字节相同，
// 且文件名集合双向一致（既不能少拷贝，也不能留下已删除的脚本）。
func TestEmbeddedCopiesMatchRepoLua(t *testing.T) {
	repoFiles, err := os.ReadDir(repoLuaDir)
	if err != nil {
		t.Fatalf("读取权威副本目录 %s 失败: %v", repoLuaDir, err)
	}

	registry, err := Load()
	if err != nil {
		t.Fatalf("加载内嵌脚本失败: %v", err)
	}

	repoLua := map[string]bool{}
	for _, entry := range repoFiles {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lua" {
			continue
		}
		repoLua[entry.Name()] = true
		want, err := os.ReadFile(filepath.Join(repoLuaDir, entry.Name()))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", entry.Name(), err)
		}
		script, ok := registry.LookupFile(entry.Name())
		if !ok {
			t.Fatalf("内嵌副本缺少脚本 %s", entry.Name())
		}
		if script.Source != string(want) {
			t.Fatalf("脚本 %s 与权威副本不一致（须重新拷贝 lua/ 到 internal/ratelimit/lua/）", entry.Name())
		}
	}
	for _, script := range registry.All() {
		if !repoLua[script.File] {
			t.Fatalf("内嵌副本多出脚本 %s（权威副本中已不存在）", script.File)
		}
	}

	repoManifest, err := os.ReadFile(filepath.Join(repoLuaDir, "MANIFEST.json"))
	if err != nil {
		t.Fatalf("读取权威清单失败: %v", err)
	}
	embeddedManifest, err := luaFS.ReadFile(manifestFile)
	if err != nil {
		t.Fatalf("读取内嵌清单失败: %v", err)
	}
	var repoEntries, embeddedEntries []manifestEntry
	if err := json.Unmarshal(repoManifest, &repoEntries); err != nil {
		t.Fatalf("解析权威清单失败: %v", err)
	}
	if err := json.Unmarshal(embeddedManifest, &embeddedEntries); err != nil {
		t.Fatalf("解析内嵌清单失败: %v", err)
	}
	if len(repoEntries) != len(embeddedEntries) {
		t.Fatalf("清单条数不一致: 权威 %d, 内嵌 %d", len(repoEntries), len(embeddedEntries))
	}
	for i := range repoEntries {
		if repoEntries[i] != embeddedEntries[i] {
			t.Fatalf("清单第 %d 项不一致: 权威 %+v, 内嵌 %+v", i, repoEntries[i], embeddedEntries[i])
		}
	}
}

func TestLoadRejectsCorruptedManifest(t *testing.T) {
	cases := []struct {
		name  string
		entry manifestEntry
	}{
		{name: "缺少常量名", entry: manifestEntry{File: "cas-session-binding.lua", Bytes: 2285}},
		{name: "字节数不符", entry: manifestEntry{ConstName: "CAS_SESSION_BINDING", File: "cas-session-binding.lua", SHA256: "c1e9eaf7353d3733ac2551b232629dc936510f48c184ca5896c96e470f34950a", Bytes: 1}},
		{name: "摘要不符", entry: manifestEntry{ConstName: "CAS_SESSION_BINDING", File: "cas-session-binding.lua", SHA256: "00", Bytes: 2285}},
		{name: "文件不存在", entry: manifestEntry{ConstName: "NOPE", File: "nope.lua", SHA256: "00", Bytes: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadEntry(tc.entry); err == nil {
				t.Fatal("损坏的清单项必须被拒绝")
			}
		})
	}
}
