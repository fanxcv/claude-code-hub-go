// Command uipoc 是「UI 静态产物 embed 进 Go」的最小可行性验证（POC，不入生产路径）。
//
// 它只回答四个问题：
//  1. `//go:embed` 能否把静态产物（含二进制资源）打进单个二进制；
//  2. 静态文件命中时的 MIME 与缓存头是否正确（`/_next/static/**` 走 immutable，HTML 走协商缓存）；
//  3. 未命中文件的路径能否回退到 SPA 壳而不 404：带 locale 前缀的路径回退到**该 locale 的壳**，
//     无前缀的路径回退到根壳；
//  4. 路径穿越（`..`）不得逃出 embed 根。
//
// 生产路径不引用本命令；真正的接入点在 `internal/egress`（见该文档 §4）。
package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
)

// 必须用 `all:` 前缀：embed 默认**跳过**以 `_` 或 `.` 开头的文件与目录，而 Next 的静态产物正在
// `_next/static/**` 下——不带 `all:` 会静默地少打一整个目录（POC 首跑即在测试里现形）。
//
//go:embed all:assets
var assetsFS embed.FS

const (
	assetsRoot = "assets"
	// immutablePrefix 下的产物带内容哈希（Next 构建产物即如此），可长缓存。
	immutablePrefix = "_next/static/"
	rootShell       = "index.html"
)

// asset 是一份已在内存里的静态产物。
type asset struct {
	body        []byte
	contentType string
	etag        string
	immutable   bool
}

// server 是 POC 的静态服务引擎。
type server struct {
	files map[string]asset
	// locales 是「存在 <locale>/index.html 壳」的一级目录名，即 BCP-47 语言段。
	locales map[string]struct{}
	bytes   int
}

func newServer() (*server, error) {
	s := &server{files: map[string]asset{}, locales: map[string]struct{}{}}
	err := fs.WalkDir(assetsFS, assetsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, readErr := assetsFS.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		key := strings.TrimPrefix(strings.TrimPrefix(p, assetsRoot), "/")
		sum := sha256.Sum256(body)
		s.files[key] = asset{
			body:        body,
			contentType: contentTypeFor(key),
			etag:        `"` + hex.EncodeToString(sum[:16]) + `"`,
			immutable:   strings.HasPrefix(key, immutablePrefix),
		}
		s.bytes += len(body)
		if rel, ok := strings.CutSuffix(key, "/"+rootShell); ok && !strings.Contains(rel, "/") {
			s.locales[rel] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if _, ok := s.files[rootShell]; !ok {
		return nil, fmt.Errorf("uipoc: 缺少根壳 %s", rootShell)
	}
	return s, nil
}

func contentTypeFor(key string) string {
	ext := filepath.Ext(key)
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	switch ext {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".woff2":
		return "font/woff2"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

// resolve 按「静态文件 > 目录 index > locale 壳 > 根壳」的优先级定位要返回的产物。
//
// 这是 SPA 回退的核心规则：只有静态产物与壳两种结果，**不存在**「路由未命中即 404」——
// 与 Next 静态导出产物由任意静态服务器托管时的行为一致（真正的 404 由前端路由自己渲染）。
func (s *server) resolve(p string) (key string, ok bool) {
	clean := path.Clean("/" + p)
	if clean == "/" {
		return rootShell, true
	}
	rel := strings.TrimPrefix(clean, "/")
	if _, found := s.files[rel]; found {
		return rel, true
	}
	if _, found := s.files[rel+"/"+rootShell]; found {
		return rel + "/" + rootShell, true
	}
	if locale, _, _ := strings.Cut(rel, "/"); locale != "" {
		if _, isLocale := s.locales[locale]; isLocale {
			return locale + "/" + rootShell, true
		}
	}
	return rootShell, true
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 路径穿越：embed.FS 自身已拒绝 `..`，这里在入口再挡一次，避免把拒绝变成 500。
	if strings.Contains(r.URL.Path, "..") {
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	key, _ := s.resolve(r.URL.Path)
	file := s.files[key]

	h := w.Header()
	h.Set("Content-Type", file.contentType)
	h.Set("ETag", file.etag)
	if file.immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, file.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := w.Write(file.body); err != nil {
		log.Printf("uipoc: 写响应失败 path=%s err=%v", r.URL.Path, err)
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:18080", "监听地址")
	flag.Parse()

	s, err := newServer()
	if err != nil {
		log.Fatalf("uipoc: %v", err)
	}
	locales := make([]string, 0, len(s.locales))
	for l := range s.locales {
		locales = append(locales, l)
	}
	log.Printf("uipoc: 已 embed %d 个产物（%d 字节），locale 壳：%v", len(s.files), s.bytes, locales)
	httpSrv := &http.Server{Addr: *listen, Handler: s}
	log.Printf("uipoc: 监听 http://%s", *listen)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatalf("uipoc: %v", err)
	}
}
