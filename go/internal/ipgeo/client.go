package ipgeo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件是 Node `src/lib/ip-geo/client.ts` 的移植：上游请求 + 形状校验 + 两级缓存 + 永不抛错。

// cachePrefix 是 Node 的 `CACHE_PREFIX`，逐字节相同（双跑期两侧共用同一份缓存）。
const cachePrefix = "ipgeo:v1:"

// negativeTTL 是 Node 的 `NEGATIVE_TTL_SECONDS`。
const negativeTTL = 60 * time.Second

// 上游响应体读取上限：payload 是几十个字段的 JSON，正常体积个位数 KB；
// 给一个宽松上界（1 MiB）是为了防上游异常返回巨型正文把内存打满——Node 用 `response.text()`
// 没有这个上限，属**有意的加固**（登记在报告里：行为差异只在异常路径上可见）。
const maxUpstreamBodyBytes = 1 << 20

// cachedEntry 是缓存里的值形制（Node 的 CachedEntry 联合）。
type cachedEntry struct {
	Kind  string          `json:"kind"` // "ok" | "error"
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Lookup 是查询器。
type Lookup struct {
	cache  Cache
	opts   Options
	client HTTPDoer
	logger *logx.Logger
}

// New 建查询器；cache 为 nil 表示不缓存（Node 在无 Redis 时同样直查）。
func New(cache Cache, opts Options, logger *logx.Logger) *Lookup {
	if logger == nil {
		logger = logx.New(nil)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 1500 * time.Millisecond
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	if opts.CacheTTL <= 0 {
		opts.CacheTTL = time.Hour
	}
	return &Lookup{cache: cache, opts: opts, client: client, logger: logger}
}

// LookupIP 解析一个 IP 的归属地与网络信息。**永不抛错**（Node 的同名契约）。
//
// 顺序逐条对齐 Node 的 lookupIp：
//
//  1. `ip.trim()`；
//  2. 非法 IP → `{status:"error", error:"invalid ip"}`（**不**查缓存、**不**写缓存）；
//  3. 私有/回环/链路本地/CGN → `{status:"private", data:{ip, kind:"private"}}`（同样不碰缓存）；
//  4. 查缓存：命中即返回（成功与负缓存都命中）；
//  5. 未命中 → 查上游；失败**写负缓存 60s** 并返回 error；成功**按配置 TTL 写缓存**并返回 ok。
func (l *Lookup) LookupIP(ctx context.Context, rawIP, lang string) Result {
	ip := strings.TrimSpace(rawIP)
	if !IsValidIP(ip) {
		return Result{Status: StatusError, Error: "invalid ip"}
	}
	if IsPrivate(ip) {
		return newPrivateResult(ip)
	}

	// lang **不由本包兜默认值**：Node 的 `options.lang ?? "en"` 只在「未传」时兜底，
	// 「传了空串」会一路用空串（缓存键变成 `ipgeo:v1:{ip}:`、上游 `?lang=`）。两条 API 的
	// schema 也不一致（公开面 `lang` 是 min(1)，自服务面是裸 string），故由调用方决定缺省，
	// 这里只按收到的值行事——本包兜底会把「传了空串」与「没传」合成同一条，静默改掉缓存键。
	key := cacheKey(ip, lang)

	if entry, ok := l.readCache(ctx, key); ok {
		if entry.Kind == "ok" {
			return Result{Status: StatusOK, Data: entry.Data}
		}
		return Result{Status: StatusError, Error: entry.Error}
	}

	data, errText := l.fetch(ctx, ip, lang)
	if errText != "" {
		l.logger.Warn("ip_geo_lookup_failed", map[string]any{"ip": ip, "lang": lang, "error": errText})
		l.writeCache(ctx, key, cachedEntry{Kind: "error", Error: errText}, negativeTTL)
		return Result{Status: StatusError, Error: errText}
	}
	l.writeCache(ctx, key, cachedEntry{Kind: "ok", Data: data}, l.opts.CacheTTL)
	return Result{Status: StatusOK, Data: data}
}

// cacheKey 复刻 Node 的 cacheKey（`${prefix}${ip}:${lang}`）。
func cacheKey(ip, lang string) string {
	return cachePrefix + ip + ":" + lang
}

func (l *Lookup) readCache(ctx context.Context, key string) (cachedEntry, bool) {
	if l.cache == nil {
		return cachedEntry{}, false
	}
	raw, found, err := l.cache.Get(ctx, key)
	if err != nil || !found {
		// 缓存故障与未命中同判（Node 的 catch 直接 return null）。
		return cachedEntry{}, false
	}
	var entry cachedEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		// 坏值当作未命中：宁可多查一次上游，也不能把坏 JSON 当结果返回给 UI。
		l.logger.Debug("ip_geo_cache_decode_failed", map[string]any{"key": key})
		return cachedEntry{}, false
	}
	return entry, true
}

func (l *Lookup) writeCache(ctx context.Context, key string, entry cachedEntry, ttl time.Duration) {
	if l.cache == nil {
		return
	}
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := l.cache.Set(ctx, key, string(encoded), ttl); err != nil {
		l.logger.Debug("ip_geo_cache_write_failed", map[string]any{"key": key})
	}
}

// fetch 查上游；第二个返回值非空表示失败原因（Node 的 `{error}` 分支）。
func (l *Lookup) fetch(ctx context.Context, ip, lang string) (json.RawMessage, string) {
	base := strings.TrimRight(l.opts.BaseURL, "/")
	if base == "" {
		return nil, "upstream url not configured"
	}
	target := fmt.Sprintf("%s/v1/ip2location/%s?lang=%s", base, url.PathEscape(ip), url.QueryEscape(lang))

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err.Error()
	}
	request.Header.Set("accept", "application/json")
	if l.opts.Token != "" {
		request.Header.Set("authorization", "Bearer "+l.opts.Token)
	}

	response, err := l.client.Do(request)
	if err != nil {
		return nil, err.Error()
	}
	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamBodyBytes))
	if err != nil {
		return nil, err.Error()
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		l.logger.Error("ip_geo_upstream_failed", map[string]any{
			"ip": ip, "lang": lang, "status": response.StatusCode,
			"bodySnippet": snippet(body),
		})
		return nil, fmt.Sprintf("upstream status %d", response.StatusCode)
	}
	if !isValidLookupResult(body) {
		l.logger.Error("ip_geo_upstream_unexpected_shape", map[string]any{
			"ip": ip, "lang": lang, "bodySnippet": snippet(body),
		})
		return nil, "upstream returned unexpected shape"
	}
	return json.RawMessage(body), ""
}

// snippet 取正文前 300 字节用于日志（Node 的 bodySnippet 同长），避免把整份 payload 写进日志。
func snippet(body []byte) string {
	const limit = 300
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit])
}

// isValidLookupResult 复刻 Node 的 isValidLookupResult：只校验 UI **会直接解引用**的子树。
//
// 逐条对应（注释保留 Node 的原意，因为每条都是踩过的坑）：
//
//	ip                                  非空字符串
//	location.country.{code,name}        字符串
//	location.country.flag.emoji         字符串——UI 直接解引用它，缺了会在渲染期崩
//	timezone.id                         字符串
//	connection                          对象（其内 asn 允许 null：CGN/Tailscale/bogon 没有 ASN）
//
// `flag.svg`/`flag.png` 允许 null（Node 注释：未知国家的 "ZZ" 兜底 payload 就是这样）。
func isValidLookupResult(body []byte) bool {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	if !isNonEmptyString(payload["ip"]) {
		return false
	}

	var location map[string]json.RawMessage
	if !unmarshalObject(payload["location"], &location) {
		return false
	}
	var country map[string]json.RawMessage
	if !unmarshalObject(location["country"], &country) {
		return false
	}
	if !isString(country["code"]) || !isString(country["name"]) {
		return false
	}
	var flag map[string]json.RawMessage
	if !unmarshalObject(country["flag"], &flag) {
		return false
	}
	if !isString(flag["emoji"]) {
		return false
	}

	var timezone map[string]json.RawMessage
	if !unmarshalObject(payload["timezone"], &timezone) {
		return false
	}
	if !isString(timezone["id"]) {
		return false
	}

	var connection map[string]json.RawMessage
	if !unmarshalObject(payload["connection"], &connection) {
		return false
	}
	// asn: number | null。
	if raw, present := connection["asn"]; present && !isNull(raw) {
		var number json.Number
		if err := json.Unmarshal(raw, &number); err != nil {
			return false
		}
		if _, err := number.Int64(); err != nil {
			// Node 用 typeof === "number"，浮点也算合法；这里放行可解析为数字的值。
			if _, floatErr := number.Float64(); floatErr != nil {
				return false
			}
		}
	}
	return true
}

func unmarshalObject(raw json.RawMessage, target *map[string]json.RawMessage) bool {
	if len(raw) == 0 || isNull(raw) {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

func isNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

func isString(raw json.RawMessage) bool {
	if len(raw) == 0 || isNull(raw) {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func isNonEmptyString(raw json.RawMessage) bool {
	if !isString(raw) {
		return false
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	return value != ""
}
