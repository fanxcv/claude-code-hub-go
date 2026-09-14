// Package ratelimit 提供服务端 Lua 脚本的 Go 侧调用层。
//
// 脚本正文不在此处书写：真源是 src/lib/redis/lua-scripts.ts，语言中立副本在仓库根 lua/，
// 由 scripts/verify-lua-parity.ts 逐字节守护。本包持有 lua/ 的**逐字节副本**并内嵌进二进制，
// 原因是 go:embed 只能嵌入本包目录之下的文件，而模块根在 go/，够不到模块外的 lua/。
//
// 副本漂移由测试兜住：embed_test.go 会把内嵌正文与 ../../../lua/ 原文逐字节比对，
// 因此 Node 侧 Lua 变更后（verify-lua-parity 报红 -> 重新导出）本包测试也会报红，提示重新拷贝。
package ratelimit

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

//go:embed lua/*.lua lua/MANIFEST.json
var luaFS embed.FS

// manifestFile 是脚本清单的文件名（相对内嵌根）。
const manifestFile = "lua/MANIFEST.json"

// manifestEntry 对应 lua/MANIFEST.json 的单项。
type manifestEntry struct {
	ConstName     string `json:"constName"`
	File          string `json:"file"`
	SHA256        string `json:"sha256"`
	Bytes         int    `json:"bytes"`
	KeysArityNote string `json:"keysArityNote"`
}

// Script 是一段可执行 Lua 及其清单信息。
type Script struct {
	// ConstName 是 Node 侧常量名（lua-scripts.ts），用于跨语言对账。
	ConstName string
	// File 是脚本文件名（kebab-case），与 lua/<File> 同名。
	File string
	// Source 是脚本原文，逐字节等同于 lua/<File>。
	Source string
	// SHA256 是 Source 的十六进制摘要，与清单及 Node 侧一致。
	SHA256 string
	// KeysArityNote 记录 KEYS/ARGV 约定，仅供人读。
	KeysArityNote string
}

// Registry 是按常量名与文件名索引的脚本集合，构造即完成清单校验。
type Registry struct {
	byConst map[string]*Script
	byFile  map[string]*Script
	sorted  []*Script
}

// Load 解析内嵌清单并校验每段脚本的字节数与 sha256；任一项不符即返回错误。
//
// 这里刻意不在 init 阶段 panic：调用方（数据面启动流程）应把清单损坏当作可诊断的启动失败。
func Load() (*Registry, error) {
	raw, err := luaFS.ReadFile(manifestFile)
	if err != nil {
		return nil, fmt.Errorf("读取内嵌脚本清单失败: %w", err)
	}

	var entries []manifestEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("解析脚本清单失败: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("脚本清单为空: %s", manifestFile)
	}

	registry := &Registry{
		byConst: make(map[string]*Script, len(entries)),
		byFile:  make(map[string]*Script, len(entries)),
		sorted:  make([]*Script, 0, len(entries)),
	}
	for _, entry := range entries {
		script, err := loadEntry(entry)
		if err != nil {
			return nil, err
		}
		if _, dup := registry.byConst[script.ConstName]; dup {
			return nil, fmt.Errorf("脚本清单中常量名重复: %s", script.ConstName)
		}
		if _, dup := registry.byFile[script.File]; dup {
			return nil, fmt.Errorf("脚本清单中文件名重复: %s", script.File)
		}
		registry.byConst[script.ConstName] = script
		registry.byFile[script.File] = script
		registry.sorted = append(registry.sorted, script)
	}
	sort.Slice(registry.sorted, func(i, j int) bool { return registry.sorted[i].File < registry.sorted[j].File })
	return registry, nil
}

func loadEntry(entry manifestEntry) (*Script, error) {
	if entry.ConstName == "" || entry.File == "" {
		return nil, fmt.Errorf("脚本清单项缺少 constName 或 file: %+v", entry)
	}
	body, err := luaFS.ReadFile("lua/" + entry.File)
	if err != nil {
		return nil, fmt.Errorf("读取脚本 %s 失败: %w", entry.File, err)
	}
	if len(body) != entry.Bytes {
		return nil, fmt.Errorf("脚本 %s 字节数不符: 清单 %d, 实际 %d", entry.File, entry.Bytes, len(body))
	}
	sum := sha256.Sum256(body)
	actual := hex.EncodeToString(sum[:])
	if actual != entry.SHA256 {
		return nil, fmt.Errorf("脚本 %s sha256 不符: 清单 %s, 实际 %s", entry.File, entry.SHA256, actual)
	}
	return &Script{
		ConstName:     entry.ConstName,
		File:          entry.File,
		Source:        string(body),
		SHA256:        actual,
		KeysArityNote: entry.KeysArityNote,
	}, nil
}

var (
	embeddedOnce sync.Once
	embedded     *Registry
	embeddedErr  error
)

// Embedded 返回进程内共享的脚本注册表；校验只做一次。
//
// 用 sync.Once 而非包级变量初始化，是为了让「启动时校验并报错」这件事落在调用方可控的位置。
func Embedded() (*Registry, error) {
	embeddedOnce.Do(func() { embedded, embeddedErr = Load() })
	return embedded, embeddedErr
}

// LookupConst 按 Node 侧常量名取脚本。
func (r *Registry) LookupConst(constName string) (*Script, bool) {
	script, ok := r.byConst[constName]
	return script, ok
}

// LookupFile 按脚本文件名取脚本。
func (r *Registry) LookupFile(file string) (*Script, bool) {
	script, ok := r.byFile[file]
	return script, ok
}

// All 返回按文件名排序的全部脚本。
func (r *Registry) All() []*Script {
	return append([]*Script(nil), r.sorted...)
}

// Len 返回脚本数量。
func (r *Registry) Len() int {
	return len(r.sorted)
}

// Files 返回按文件名排序的全部脚本文件名。
func (r *Registry) Files() []string {
	files := make([]string, 0, len(r.sorted))
	for _, script := range r.sorted {
		files = append(files, script.File)
	}
	return files
}
