// Package uiapp 把 Next 静态导出产物 embed 进二进制并直接作答页面请求。
//
// 它是 `CCH_EGRESS_PAGES=embed` 的终态实现：Node 下线后页面面由本包服务，不再有反代回退。
// 四条规则照 §4.3，并在 cmd/uipoc 上实测过：
//  1. 命中 embed 内的静态文件即直发（`_next/static/**` 带内容哈希 → immutable，
//     其余 → no-cache + ETag 协商缓存）；
//  2. **形如静态资源的路径未命中即 404**（末段带扩展名，或落在 `_next/` 下）——这些路径
//     永远不是页面，把 HTML 当正文发出去只会误导（浏览器拿它当 JS 解析报语法错误、
//     图标/字体解码失败），把「产物少了个文件」误诊成「代码坏了」；
//  3. 未命中且首段是已注册 locale → 返回该 locale 的壳；
//  4. 其余**无扩展名的页面路径** → 返回根壳。这才是 SPA 兜底的范围（等价于 nginx 的
//     `try_files $uri /index.html`），真正的 404 由前端路由渲染（与 Node 侧 proxy.ts 的 matcher
//     语义对齐）。API 前缀另在 resolve 里单列一条，理由见那里的注释；
//  5. 非 GET/HEAD → 405；原始路径含 `..` → 400。
//
// 订正一处旧注释：本包曾把「任何未命中都回落壳」写成有意设计（「与静态产物由任意静态
// 服务器托管时的行为一致」）。那是错的——真实静态服务器对**不存在的文件**返回 404，
// 只有路由路径才吃 SPA 兜底；旧写法会让旧产物配新 HTML 这类常见错配全部无声无息：
// 页面能打开，控制台报 JS 语法错，排查时反而去怀疑代码。
//
// 与 POC 的两处差异：
//
//   - **产物缺失即报错**（New 返回 ErrAssetsMissing），由启动方 fail fast：静默服务一个空站点
//     比起不来更难排查；
//   - 壳在返回前注入 `window.__CCH_BOOTSTRAP__`（首帧会话判定，见 bootstrap.go）。
package uiapp

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 必须用 `all:` 前缀：embed 默认**跳过**以 `_` 或 `.` 开头的文件与目录，而 Next 的静态产物
// 正在 `_next/static/**` 下——不带 `all:` 会静默地少打一整个目录（POC 首跑即在测试里现形）。
//
//go:embed all:assets
var embedded embed.FS

const (
	// assetsRoot 是 embed 内的产物根目录；内容由 scripts/build-ui-export.mjs 填充。
	assetsRoot = "assets"
	// rootShell 是根壳（未注册 locale 前缀的落点）。
	rootShell = "index.html"
	// immutablePrefix 下的产物带内容哈希，可长缓存。
	immutablePrefix = "_next/static/"
	// buildIDFile 由导出脚本写入，启动日志与 /readyz 用它核对「二进制与前端产物同版」。
	buildIDFile = "BUILD_ID"
	// placeholderFile 是未做 UI 构建时的唯一占位文件（见 .gitignore）：它的存在说明产物缺失。
	placeholderFile = ".gitkeep"
	// bootstrapMarker 是壳里可选的自定位注入点；存在时整段替换，优先于 </head>。
	bootstrapMarker = "<!--CCH_BOOTSTRAP-->"
	// headClose 是注入的兜底落点。
	headClose = "</head>"
	// defaultShellMaxBytes 是注入的体积上限：超过则不注入（避免把大媒体误当壳改坏）。
	defaultShellMaxBytes = 1 << 20
	// defaultMetaTTL 是壳元数据的缓存窗口（与公开状态快照的重发节奏同量级）。
	defaultMetaTTL = 60 * time.Second
	// DefaultLocale 是无 locale 前缀路径的落点（与 src/i18n/config.ts 的 defaultLocale 同值）。
	DefaultLocale = "zh-CN"
)

// ErrAssetsMissing 表示 embed 里没有 UI 产物（只有占位文件）。
var ErrAssetsMissing = errors.New(
	"uiapp: embed 内没有 UI 产物（只有 " + placeholderFile + "），请先跑 scripts/build-ui-export.mjs",
)

// Options 是装配参数。
type Options struct {
	// Logger 为 nil 时丢弃日志。
	Logger *logx.Logger
	// Meta 提供壳注入的站点元数据；nil 时不注入元数据字段。
	Meta MetaSource
	// Sessions 解析当前请求的会话；nil 时一律视为未登录（session: null）。
	Sessions SessionResolver
	// Now 可注入时钟（元数据缓存过期与令牌校验用）；nil 取 time.Now。
	Now func() time.Time
	// ShellMaxBytes 覆盖注入体积上限（默认 1 MiB）。
	ShellMaxBytes int
	// MetaTTL 覆盖元数据缓存窗口（默认 60s）。
	MetaTTL time.Duration
	// DefaultLocale 覆盖无前缀路径的 locale；空取 DefaultLocale。
	DefaultLocale string
	// Locales 是**已注册 locale**（与 src/i18n/config.ts 的 locales 同一份，装配方传入）。
	//
	// 它必须是传入值而不是从产物目录推断：导出产物里每一级路由目录都有自己的 index.html
	// （`dashboard/`、`usage-doc/` …），按「有壳的一级目录就是 locale」推断会把路由目录
	// 当成语言段，使 `/dashboard/<未知路径>` 落到 dashboard 壳而非根壳。空值时只认 DefaultLocale。
	Locales []string
}

// asset 是一份静态产物。
//
// body 是**存储态**字节：未压缩产物下即明文，压缩产物下是 brotli 流（见 compress.go）。
// 两者由 encoding 区分，服务时由 serveBody 还原成明文交给客户端。
type asset struct {
	body        []byte
	contentType string
	etag        string
	immutable   bool
	// shell 为 true 表示这是 HTML 壳（注入与协商缓存按「逐请求可变」处理）。
	shell bool
	// encoding 是 body 的存储编码（identity / br），取自 MANIFEST.json。
	encoding string
	// rawSize 是还原后的大小；元数据缺失（未压缩产物）时为 0。
	rawSize int
}

// Status 是对外的装配结论，供启动日志与 /readyz 如实报告。
type Status struct {
	// Enabled 表示本进程确实在服务 embed 产物（false 时其余字段为零值）。
	Enabled bool
	// Files 是产物文件数（不含占位文件与 BUILD_ID）。
	Files int
	// Bytes 是产物总字节数。
	Bytes int
	// Locales 是已注册 locale 里**产物中确实有壳**的那些（有序），供 /readyz 核对。
	Locales []string
	// BuildID 取产物里的 BUILD_ID（导出脚本写入），缺失时为空串。
	BuildID string
}

// Describe 给出一行结论（/readyz 的 notes 与启动日志共用）。
func (s Status) Describe() string {
	if !s.Enabled {
		return "未启用（页面面不归本进程）"
	}
	build := s.BuildID
	if build == "" {
		build = "未知构建号"
	}
	return fmt.Sprintf("embed 产物 %d 个 / %.1f MiB / build %s / locale %s",
		s.Files, float64(s.Bytes)/(1<<20), build, strings.Join(s.Locales, ","))
}

// Handler 是 UI 静态面处理器。
type Handler struct {
	files     map[string]asset
	localeSet map[string]struct{}
	status    Status

	logger        *logx.Logger
	meta          MetaSource
	sessions      SessionResolver
	now           func() time.Time
	shellMax      int
	metaTTL       time.Duration
	defaultLocale string

	mu         sync.Mutex
	cachedMeta Meta
	metaUntil  time.Time
}

// New 用 embed 进二进制的产物建处理器；产物缺失或缺少根壳时返回错误。
func New(options Options) (*Handler, error) {
	return newHandler(embedded, assetsRoot, options)
}

// newHandler 是 New 的可注入版本（assets 换成任意 fs.FS，测试用）。
func newHandler(assets fs.FS, root string, options Options) (*Handler, error) {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	shellMax := options.ShellMaxBytes
	if shellMax <= 0 {
		shellMax = defaultShellMaxBytes
	}
	metaTTL := options.MetaTTL
	if metaTTL <= 0 {
		metaTTL = defaultMetaTTL
	}
	defaultLocale := options.DefaultLocale
	if defaultLocale == "" {
		defaultLocale = DefaultLocale
	}
	registered := options.Locales
	if len(registered) == 0 {
		// 未声明注册集时只认默认 locale：路由语义降级成「无语言段」而不是猜。
		registered = []string{defaultLocale}
	}
	localeSet := make(map[string]struct{}, len(registered))
	for _, name := range registered {
		localeSet[name] = struct{}{}
	}

	h := &Handler{
		files:         map[string]asset{},
		localeSet:     localeSet,
		logger:        logger,
		meta:          options.Meta,
		sessions:      options.Sessions,
		now:           now,
		shellMax:      shellMax,
		metaTTL:       metaTTL,
		defaultLocale: defaultLocale,
	}

	payloadFiles := 0

	// 压缩元数据（scripts/build-ui-embed.mjs 写入）。缺失即「未压缩产物」：全部按明文处理，
	// 行为与本改动前一字不差（回退只需一个构建参数，不必回滚代码）。
	stored, hasManifest, manifestErr := loadManifest(assets, root)
	if manifestErr != nil {
		return nil, manifestErr
	}

	err := fs.WalkDir(assets, root, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		body, readErr := fs.ReadFile(assets, p)
		if readErr != nil {
			return readErr
		}
		key := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		// MANIFEST.json 是装配元数据而不是产物：不参与择路、不计入文件数与字节数。
		if key == manifestFile {
			return nil
		}
		encoding, rawSize := encodingIdentity, 0
		if hasManifest {
			if meta, ok := stored.Entries[key]; ok {
				encoding, rawSize = meta.Encoding, meta.RawSize
			}
		}
		sum := sha256.Sum256(body)
		h.files[key] = asset{
			body:        body,
			contentType: contentTypeFor(key),
			etag:        formatETag(sum[:16]),
			immutable:   strings.HasPrefix(key, immutablePrefix),
			shell:       isShellKey(key),
			encoding:    encoding,
			rawSize:     rawSize,
		}
		h.status.Bytes += len(body)
		if key != placeholderFile && key != buildIDFile {
			payloadFiles++
		}
		if key == buildIDFile {
			h.status.BuildID = strings.TrimSpace(string(body))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("uiapp: 读取 embed 产物失败: %w", err)
	}
	if payloadFiles == 0 {
		return nil, ErrAssetsMissing
	}
	if _, ok := h.files[rootShell]; !ok {
		return nil, fmt.Errorf("uiapp: 产物缺少根壳 %s", rootShell)
	}

	// 只报「已注册且产物里真有壳」的 locale：这是运维核对「前端产物是否漏了某个语言」的依据。
	for name := range localeSet {
		if _, ok := h.files[name+"/"+rootShell]; ok {
			h.status.Locales = append(h.status.Locales, name)
		}
	}
	sort.Strings(h.status.Locales)
	h.status.Enabled = true
	h.status.Files = payloadFiles
	return h, nil
}

// Status 返回装配结论。
func (h *Handler) Status() Status {
	return h.status
}

// isShellKey 判断 key 是否是 HTML 壳（含各路由的 index.html）。
func isShellKey(key string) bool {
	return key == rootShell || strings.HasSuffix(key, "/"+rootShell)
}

// contentTypeFor 取 MIME。mime.TypeByExtension 覆盖绝大多数；余下几个由内建表兜底——
// 它们的缺失会直接表现成「浏览器把 JS 当纯文本」，属功能性故障，故不依赖系统 mime 表。
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
	case ".html":
		return "text/html; charset=utf-8"
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

// formatETag 与 POC 同格式（取摘要前 16 字节的十六进制并加引号），便于两端对拍。
func formatETag(sum []byte) string {
	return `"` + hex.EncodeToString(sum) + `"`
}

// apiPrefixes 是归 API 面的路径前缀（`httpapi` 的 mux 已在更外层认领它们）。
//
// 为什么终端还要再挡一次：本处理器挂在 `/` 上，是**所有没被 mux 认领的路径的落点**。
// 管理面的挂载是条件式的（`cmd/cchd/boot.go` 里 `storePools != nil` 才装配，没配 DSN 时
// `/api/` 无人认领），那种进程里 `/api/v1/...` 会落到这里——回一个 HTML 壳就是把
// 「接口不存在」伪装成 200 页面，与下面「资源缺失不得回落 HTML」是同一类误导。
// 正常配置下这些路径根本到不了这里，故本判断在热路径上不做事。
var apiPrefixes = []string{"api", "v1", "v1beta"}

// isAPIPath 判断路径首段是否归 API 面。
func isAPIPath(rel string) bool {
	first, _, _ := strings.Cut(rel, "/")
	for _, prefix := range apiPrefixes {
		if first == prefix {
			return true
		}
	}
	return false
}

// isAssetPath 判断路径是否形如静态资源：末段带扩展名（`app-abc123.js`、`favicon.ico`），
// 或落在 `_next/` 下。
//
// 只认**末段**的扩展名：「点在目录里」的页面路径（`/a.b/route`）仍按页面处理。
func isAssetPath(rel string) bool {
	return rel == "_next" || strings.HasPrefix(rel, "_next/") || path.Ext(rel) != ""
}

// resolve 按「静态文件 > 目录 index > locale 壳 > 根壳」定位要返回的产物。
//
// notFound 为真表示：产物里没有这个路径，而它**又不该拿壳兜底**（见下方注释），调用方据此回 404。
// 只有「无扩展名的页面路径」才按 SPA 语义回落壳——那正是前端路由自己渲染 404 的路径。
func (h *Handler) resolve(p string) (key string, locale string, notFound bool) {
	clean := path.Clean("/" + p)
	if clean == "/" {
		return rootShell, h.defaultLocale, false
	}
	rel := strings.TrimPrefix(clean, "/")
	if _, found := h.files[rel]; found {
		first, _, _ := strings.Cut(rel, "/")
		return rel, h.localeFor(first), false
	}
	if _, found := h.files[rel+"/"+rootShell]; found {
		return rel + "/" + rootShell, h.localeFor(rel), false
	}
	// 走到这里：产物里没有这个路径。下列两类**不得**回落壳，否则缺失被伪装成 200：
	//
	//  1. API 前缀：mux 正常已认领，终端再挡一次（见 apiPrefixes）；
	//  2. 形如静态资源的路径：浏览器按 `Content-Type` 解析正文，把 HTML 当 JS 会报语法错误，
	//     于是「产物里少了个 chunk」被误诊成「代码坏了」；图标/字体请求也会拿 HTML 去解码。
	//
	// 其余路径都是应用路由（`/dashboard`、`/zh-CN/dashboard/logs`），浏览器直接访问或前端跳转
	// 都会进壳，由前端的 404 页接管（保留 SPA 语义）。
	if isAPIPath(rel) || isAssetPath(rel) {
		return "", h.defaultLocale, true
	}
	first, _, _ := strings.Cut(rel, "/")
	if _, isLocale := h.localeSet[first]; isLocale {
		return first + "/" + rootShell, first, false
	}
	return rootShell, h.defaultLocale, false
}

// localeFor 把候选段归成已注册 locale；未注册时回落默认 locale。
func (h *Handler) localeFor(candidate string) string {
	if _, ok := h.localeSet[candidate]; ok {
		return candidate
	}
	return h.defaultLocale
}

// ServeHTTP 实现 http.Handler。
func (h *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// 路径穿越：embed.FS 自身已拒绝 `..`，这里在入口再挡一次，避免把拒绝变成 500。
	// 注意 fetch/curl 会在客户端就归一化 `..`，故这条只在原始 socket 送达时才会触发（POC 已实测）。
	if strings.Contains(request.URL.Path, "..") {
		http.Error(writer, "bad path", http.StatusBadRequest)
		return
	}

	key, locale, notFound := h.resolve(request.URL.Path)
	if notFound {
		// 缺失的静态资源/API 路径**不得**回落 HTML 壳：回 HTML 会把「没有这个文件」误导成
		// 「页面正常但内容不对」（JS 报语法错误、图标解码失败）。状态码 404 与正文用纯文本，
		// 而不是产物里的 404.html——一个 `.js` 请求收到 HTML 正文本身就是这次要修掉的误导。
		//
		// 日志用 debug：产物一旦真的缺文件，同一次部署里会收到成百条（每个页面都引同一批 chunk），
		// 而它是可预期的（旧产物 + 新 HTML），不值得在 error 级别刷屏。
		h.logger.Debug("uiapp_asset_missing", map[string]any{
			"path": request.URL.Path,
		})
		http.Error(writer, "not found", http.StatusNotFound)
		return
	}
	file := h.files[key]

	// 壳逐请求可变：会话与站点元数据都在正文里，故必须**先还原明文、再注入、再按注入后正文算 ETag**，
	// 否则 304 会把上一名用户的引导数据当成新鲜回复发出去。
	//
	// 注意：压缩存储下 file.body 是 brotli 流，注入必须在明文上做——若拿压缩流去 Inject，
	// 标记与 </head> 都不存在，注入会静默失败（前端退到回退探针），且随后还会把压缩流再压一遍。
	plainBody, err := h.plainBody(file)
	if err != nil {
		// 产物损坏：绝不把压缩流当明文发出去（浏览器里就是满页乱码），宁可 500 并留日志。
		h.logger.Error("uiapp_decode_failed", map[string]any{
			"path":  request.URL.Path,
			"key":   key,
			"error": err.Error(),
		})
		http.Error(writer, "asset decode failed", http.StatusInternalServerError)
		return
	}
	body, etag := plainBody, file.etag
	if file.shell {
		if injected, ok := h.inject(request.Context(), request, plainBody, locale); ok {
			body = injected
			sum := sha256.Sum256(injected)
			etag = formatETag(sum[:16])
		}
	}

	handler := writer.Header()
	handler.Set("Content-Type", file.contentType)
	handler.Set("ETag", etag)
	if needsVary(file) {
		// 压缩存储的产物与服务时即时压缩的壳：同一 URL 的正文随 Accept-Encoding 变。
		handler.Set("Vary", "Accept-Encoding")
	}
	if file.immutable {
		// 内容哈希已在路径里，一年长缓存是安全的；这是首屏性能的主要来源。
		handler.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// 壳与不带哈希的产物（favicon、svg）一律协商缓存。
		handler.Set("Cache-Control", "no-cache")
	}

	if etagMatches(request.Header.Get("If-None-Match"), etag) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}

	// 正文与编码：壳在注入后即时压缩（已在上方还原明文），其余按协商直出存储态或解压。
	contentEncoding := ""
	if file.shell {
		body, contentEncoding = compressForClient(request, body)
	} else if file.encoding == encodingBrotli && acceptsBrotli(request.Header.Get("Accept-Encoding")) {
		// 压缩存储 + 客户端接受 br：直出存储态（零解压）。
		body, contentEncoding = file.body, "br"
	}
	if contentEncoding != "" {
		handler.Set("Content-Encoding", contentEncoding)
	}
	// Content-Length 由 net/http 根据写入字节自动定（这里不预置，否则会与压缩后的实际长度不符）。
	writer.WriteHeader(http.StatusOK)
	if request.Method == http.MethodHead {
		return
	}
	if _, err := writer.Write(body); err != nil {
		h.logger.Warn("uiapp_write_failed", map[string]any{
			"path":  request.URL.Path,
			"error": err.Error(),
		})
	}
}

// etagMatches 判 If-None-Match 是否命中本响应的 ETag。
//
// 要处理三种写法，否则协商缓存会静默失效（表现为每次都重传）：`*`、
// 逗号分隔的多个 ETag、以及 `W/` 弱校验前缀。
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == etag {
			return true
		}
	}
	return false
}
