package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/appversion"
	"github.com/fanxcv/claude-code-hub-go/go/internal/clientver"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件是 **GET /api/version**（Node 侧 src/app/api/version/route.ts）。
//
// 形状要点（逐条照 Node）：
//
//   - dev 构建（版本号形如 `dev-<sha>`）走**分支头提交**对比，返回 compare URL；
//   - 正式版本对比 releases/latest，缺失时退回 raw 的 VERSION 文件；
//   - 上游不可达时**不报错**，而是退回 VERSION 文件；连它都拿不到才 500，
//     且 500 的正文仍带 current（前端据此显示「无法获取最新版本信息」）；
//   - 上游查询结果按 5 分钟缓存（Node 用 fetch 的 next.revalidate）。
//
// 与 Node 的差异（有意，登记在此）：
//
//  1. Next 的 fetch 缓存挂在请求上下文上，Go 没有那层；这里用一个进程内的「URL -> 正文」TTL 缓存
//     平替。上限：只缓存成功的 200 正文，不缓存 404/错误（Node 的 next 缓存对 404 也会留痕），
//     故 GitHub 404 的场景会每次回源。
//  2. 当前版本号由 `internal/appversion` 统一取值（Node 读 package.json）。此处**不再**自带
//     一条链：版本号曾在四个地方各取一遍，同一镜像里出现过三个不同的值（见 appversion 包注释）。
const (
	versionCacheTTLSeconds = 5 * 60
	versionUserAgent       = "claude-code-hub"
	versionRepoOwner       = "fanxcv"
	versionRepoName        = "claude-code-hub-go"
	versionGitHubAPIBase   = "https://api.github.com"
	versionRawBaseURL      = "https://raw.githubusercontent.com"
)

// VersionOptions 是 /api/version 的可注入面（测试用假上游与假时钟，不真等 5 分钟）。
type VersionOptions struct {
	// CurrentOverride 覆盖当前版本（空则由 `internal/appversion` 按 env → 兜底常量取值）。
	CurrentOverride string
	// HTTPClient 未设置时用默认客户端（带 10s 超时：上游挂住不能拖死这个端点）。
	HTTPClient *http.Client
	// Now 可注入时钟；缓存新鲜度按它判。
	Now func() time.Time
	// CacheTTL 覆盖缓存窗口（默认 5 分钟）。
	CacheTTL time.Duration
	// GitHubAPIBase / RawBaseURL 覆盖上游地址（测试指向 httptest）。
	GitHubAPIBase string
	RawBaseURL    string
}

// versionAPI 是版本端点的处理器依赖。
type versionAPI struct {
	client   *http.Client
	now      func() time.Time
	cacheTTL time.Duration
	apiBase  string
	rawBase  string
	current  string
	logger   *logx.Logger

	mu    sync.Mutex
	cache map[string]versionCacheEntry
}

// versionCacheEntry 是「URL -> 正文」的一次缓存命中。
type versionCacheEntry struct {
	body      []byte
	status    int
	fetchedAt time.Time
}

// RegisterVersionRoutes 注册根级 GET /api/version。
//
// 无认证（Node 侧该路由没有 session 判定）：AccessPublic，且不打管理面信封（见 Route.NoManagementEnvelope）。
func RegisterVersionRoutes(router *Router, deps Deps, options VersionOptions) {
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	apiBase := options.GitHubAPIBase
	if apiBase == "" {
		apiBase = versionGitHubAPIBase
	}
	rawBase := options.RawBaseURL
	if rawBase == "" {
		rawBase = versionRawBaseURL
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	ttl := options.CacheTTL
	if ttl == 0 {
		ttl = versionCacheTTLSeconds * time.Second
	}
	current := options.CurrentOverride
	if current == "" {
		current = resolveCurrentVersion(os.Getenv)
	}
	api := &versionAPI{
		client:   client,
		now:      now,
		cacheTTL: ttl,
		apiBase:  apiBase,
		rawBase:  rawBase,
		current:  current,
		logger:   logger,
		cache:    map[string]versionCacheEntry{},
	}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/version",
		Access:               AccessPublic,
		Module:               "version",
		OperationID:          "getAppVersion",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleVersion),
	})
}

// resolveCurrentVersion 给出当前版本号（展示形态，带小写 v 前缀）。
//
// 取值的唯一实现在 `internal/appversion`：注入的 APP_VERSION → 兼容变量 → 兜底常量。
// 本函数只把 os.Getenv 递进去，让「这个端点报什么版本」与 /api/health、UI 壳注入完全同源。
func resolveCurrentVersion(lookup func(string) string) string {
	return appversion.Resolve(lookup)
}

// versionDevPattern / versionDevShaPattern 复刻 isDevBuild 与 parseDevBuildShortSha。
var (
	versionDevPattern    = regexp.MustCompile(`(?i)^dev(?:-|$)`)
	versionDevShaPattern = regexp.MustCompile(`(?i)^dev-([0-9a-f]{7,40})$`)
)

func isDevBuild(version string) bool {
	return versionDevPattern.MatchString(strings.TrimSpace(version))
}

func parseDevBuildShortSha(version string) string {
	match := versionDevShaPattern.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return ""
	}
	return strings.ToLower(match[1][:7])
}

// versionLatestInfo 是「最新版本」的一次查询结果。
type versionLatestInfo struct {
	latest      string
	releaseURL  string
	publishedAt string
}

// handleVersion 复刻 GET /api/version 的三条分支。
func (api *versionAPI) handleVersion(writer http.ResponseWriter, request *http.Request) {
	current := api.current
	if isDevBuild(current) {
		api.writeDevBuildVersion(writer, current)
		return
	}
	latest, err := api.latestVersionInfo(request)
	if err != nil {
		api.logger.Error("admin_version_check_failed", map[string]any{"error": err.Error()})
		writeShellJSONNoEnvelope(writer, http.StatusInternalServerError, map[string]any{
			"current":   appversion.Display(appversion.Fallback),
			"latest":    nil,
			"hasUpdate": false,
			"error":     "无法获取最新版本信息",
		})
		return
	}
	if latest == nil {
		writeShellJSONNoEnvelope(writer, http.StatusOK, map[string]any{
			"current":   current,
			"latest":    nil,
			"hasUpdate": false,
			"message":   "暂无发布版本",
		})
		return
	}
	body := map[string]any{
		"current":    current,
		"latest":     latest.latest,
		"hasUpdate":  compareVersions(current, latest.latest) == 1,
		"releaseUrl": latest.releaseURL,
	}
	if latest.publishedAt != "" {
		body["publishedAt"] = latest.publishedAt
	} else {
		// Node 的 JSON.stringify 会把 undefined 键整个丢掉，故未取到发布时间时不带该键。
		delete(body, "publishedAt")
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, body)
}

// writeDevBuildVersion 走 dev 分支：对比分支头提交。
func (api *versionAPI) writeDevBuildVersion(writer http.ResponseWriter, current string) {
	currentShortSha := parseDevBuildShortSha(current)
	head, err := api.branchHeadCommit("dev")
	if err != nil {
		api.logger.Error("admin_version_dev_branch_failed", map[string]any{"error": err.Error()})
		writeShellJSONNoEnvelope(writer, http.StatusInternalServerError, map[string]any{
			"current":   appversion.Display(appversion.Fallback),
			"latest":    nil,
			"hasUpdate": false,
			"error":     "无法获取最新版本信息",
		})
		return
	}
	latest := "dev-" + head.shortSha
	hasUpdate := strings.ToLower(strings.TrimSpace(current)) != latest
	if currentShortSha != "" {
		hasUpdate = currentShortSha != head.shortSha
	}
	compareURL := head.commitURL
	if currentShortSha != "" && hasUpdate {
		compareURL = "https://github.com/" + versionRepoOwner + "/" + versionRepoName +
			"/compare/" + currentShortSha + "..." + head.shortSha
	}
	body := map[string]any{
		"current":    current,
		"latest":     latest,
		"hasUpdate":  hasUpdate,
		"releaseUrl": compareURL,
	}
	if head.publishedAt != "" {
		body["publishedAt"] = head.publishedAt
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, body)
}

// latestVersionInfo 复刻 getLatestVersionInfo：release → VERSION 文件；出错时也退回 VERSION 文件。
func (api *versionAPI) latestVersionInfo(request *http.Request) (*versionLatestInfo, error) {
	release, releaseErr := api.fetchLatestRelease(request)
	if releaseErr == nil && release != nil {
		return &versionLatestInfo{
			latest:      appversion.Display(release.TagName),
			releaseURL:  release.HTMLURL,
			publishedAt: release.PublishedAt,
		}, nil
	}
	if releaseErr == nil {
		latest, err := api.fetchLatestVersionFromVersionFile(request)
		if err != nil {
			return nil, err
		}
		if latest == "" {
			return nil, nil
		}
		return &versionLatestInfo{latest: latest, releaseURL: api.releasesPageURL()}, nil
	}
	// GitHub API 限流或被墙：退回 VERSION 文件；它也拿不到才把原错误抛出去。
	latest, err := api.fetchLatestVersionFromVersionFile(request)
	if err != nil || latest == "" {
		return nil, releaseErr
	}
	return &versionLatestInfo{latest: latest, releaseURL: api.releasesPageURL()}, nil
}

func (api *versionAPI) releasesPageURL() string {
	return "https://github.com/" + versionRepoOwner + "/" + versionRepoName + "/releases"
}

// versionRelease / versionCommit 只需 Node 用到的字段。
type versionRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	HTMLURL     string `json:"html_url"`
	PublishedAt string `json:"published_at"`
}

type versionCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Commit  struct {
		Author struct {
			Date string `json:"date"`
		} `json:"author"`
		Committer struct {
			Date string `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// fetchLatestRelease 查 releases/latest；404 表示「没有发布版本」（Node 同义），其余非 2xx 报错。
func (api *versionAPI) fetchLatestRelease(request *http.Request) (*versionRelease, error) {
	url := api.apiBase + "/repos/" + versionRepoOwner + "/" + versionRepoName + "/releases/latest"
	response, err := api.fetch(request, url, true)
	if err != nil {
		return nil, err
	}
	if response.status == http.StatusNotFound {
		return nil, nil
	}
	if response.status < 200 || response.status >= 300 {
		return nil, &versionUpstreamError{status: response.status}
	}
	var release versionRelease
	if err := json.Unmarshal(response.body, &release); err != nil {
		return nil, err
	}
	return &release, nil
}

// branchHead 是分支头提交的三要素。
type branchHead struct {
	shortSha    string
	commitURL   string
	publishedAt string
}

// branchHeadCommit 复刻 fetchBranchHeadCommit。
func (api *versionAPI) branchHeadCommit(branch string) (branchHead, error) {
	url := api.apiBase + "/repos/" + versionRepoOwner + "/" + versionRepoName +
		"/commits/" + urlQueryEscape(branch)
	response, err := api.fetch(nil, url, true)
	if err != nil {
		return branchHead{}, err
	}
	if response.status < 200 || response.status >= 300 {
		return branchHead{}, &versionUpstreamError{status: response.status}
	}
	var commit versionCommit
	if err := json.Unmarshal(response.body, &commit); err != nil {
		return branchHead{}, err
	}
	if len(commit.SHA) < 7 {
		return branchHead{}, errShortCommitSha
	}
	publishedAt := commit.Commit.Committer.Date
	if publishedAt == "" {
		publishedAt = commit.Commit.Author.Date
	}
	return branchHead{
		shortSha:    strings.ToLower(commit.SHA[:7]),
		commitURL:   commit.HTMLURL,
		publishedAt: publishedAt,
	}, nil
}

// fetchLatestVersionFromVersionFile 取**上游仓库 main 分支上发布**的 VERSION 文件（Node 的 raw.githubusercontent 路径），
// 作为「最新版本」在 releases 接口不可用时的兜底。
//
// 注意别与本进程的版本号混为一谈：本仓库工作区里那份 VERSION 文件已删、也不再参与任何判定
// （见 internal/appversion）；这里读的是**远端发布物**，与工作区无关。
func (api *versionAPI) fetchLatestVersionFromVersionFile(request *http.Request) (string, error) {
	url := api.rawBase + "/" + versionRepoOwner + "/" + versionRepoName + "/main/VERSION"
	response, err := api.fetch(request, url, false)
	if err != nil {
		return "", err
	}
	if response.status < 200 || response.status >= 300 {
		return "", nil
	}
	version := strings.TrimSpace(string(response.body))
	if version == "" {
		return "", nil
	}
	return appversion.Display(version), nil
}

// fetch 取一次上游（带 TTL 缓存）。withAuth 为真时带上可选的 GITHUB_TOKEN / GH_TOKEN。
func (api *versionAPI) fetch(request *http.Request, url string, withAuth bool) (versionCacheEntry, error) {
	if cached, ok := api.cached(url); ok {
		return cached, nil
	}
	ctx := context.Background()
	if request != nil {
		ctx = request.Context()
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return versionCacheEntry{}, err
	}
	httpRequest.Header.Set("User-Agent", versionUserAgent)
	if withAuth {
		httpRequest.Header.Set("Accept", "application/vnd.github.v3+json")
		if token := githubToken(); token != "" {
			httpRequest.Header.Set("Authorization", "Bearer "+token)
		}
	}
	response, err := api.client.Do(httpRequest)
	if err != nil {
		return versionCacheEntry{}, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return versionCacheEntry{}, err
	}
	entry := versionCacheEntry{body: body, status: response.StatusCode, fetchedAt: api.now()}
	if response.StatusCode == http.StatusOK {
		api.store(url, entry)
	}
	return entry, nil
}

// cached 读缓存；过期即失效（Node 的 revalidate 语义）。
func (api *versionAPI) cached(url string) (versionCacheEntry, bool) {
	api.mu.Lock()
	defer api.mu.Unlock()
	entry, ok := api.cache[url]
	if !ok || api.now().Sub(entry.fetchedAt) >= api.cacheTTL {
		return versionCacheEntry{}, false
	}
	return entry, true
}

func (api *versionAPI) store(url string, entry versionCacheEntry) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.cache[url] = entry
}

// versionUpstreamError 表示上游返回了非 2xx。
type versionUpstreamError struct{ status int }

func (e *versionUpstreamError) Error() string {
	return "GitHub API 错误: " + strconv.Itoa(e.status)
}

// errShortCommitSha 表示上游给的 sha 短于 7 位（取不出短 sha）。
var errShortCommitSha = &versionUpstreamError{status: 0}

// githubToken 读可选的 GitHub token（Node 的 GITHUB_TOKEN || GH_TOKEN）。
func githubToken() string {
	for _, name := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token
		}
	}
	return ""
}

// compareVersions 复刻 Node 的 compareVersions（注意返回值语义：1 表示 latest 更新）。
//
// 无法解析的版本一律视为相等（Node 的 fail-open），避免误报「有新版本」。
//
// 比较本身走 `internal/clientver`——那是本仓 SemVer 解析与比较的单一实现（版本链、
// 客户端 GA 比对同用），此处只做返回值方向的适配，不再自带第二份解析器。
func compareVersions(current, latest string) int {
	switch clientver.CompareVersions(latest, current) {
	case 1:
		return 1
	case -1:
		return -1
	default:
		return 0
	}
}

// urlQueryEscape 是 Node 的 encodeURIComponent 在路径段上的等价物（分支名可能是 feat/x）。
func urlQueryEscape(value string) string {
	return strings.ReplaceAll(url.PathEscape(value), "+", "%20")
}

// writeShellJSONNoEnvelope 写一份不带管理面信封的 JSON（根级端点的作答）。
func writeShellJSONNoEnvelope(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(body)
}
