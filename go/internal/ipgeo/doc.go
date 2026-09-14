// Package ipgeo 是 IP 归属地查询的移植（Node：src/lib/ip-geo/client.ts 与 src/lib/ip/private-ip.ts）。
//
// 三条铁律，逐条对应 Node：
//
//  1. **缓存与 Node 同键同形**（`ipgeo:v1:{ip}:{lang}`，值 `{"kind":"ok","data":…}` /
//     `{"kind":"error","error":…}`）：双跑期两侧读写同一份缓存，键或值形制不同就会
//     「一侧缓存永不命中」或互相读到坏值。成功 TTL = IP_GEO_CACHE_TTL_SECONDS，
//     负缓存 TTL = 60s（Node 的 NEGATIVE_TTL_SECONDS）。
//  2. **永不抛错**：查不到就返回 `status=error`，让调用方渲染优雅降级（Node 的注释原文：
//     "Never throws; returns { status: 'error' } on failure so callers can render a graceful UI"）。
//  3. **上游形状校验后才缓存**：上游漂移或返回半个 payload 时宁可判失败，也不能把坏数据缓存一小时
//     ——UI 会直接解引用 `location.country.flag.emoji`（Node 的 isValidLookupResult 注释）。
package ipgeo

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// 状态取值（Node 的 IpGeoLookupResponse 判别式联合）。
const (
	StatusOK      = "ok"
	StatusPrivate = "private"
	StatusError   = "error"
)

// Result 是一次查询的结果，形制与 Node 的 IpGeoLookupResponse 逐字段对应：
//
//	{status:"ok",      data: <上游 payload 原样>}
//	{status:"private", data: {ip, kind:"private"}}
//	{status:"error",   error: "<原因>"}
//
// Data 保持**原始 JSON**：payload 是上游的透传数据（几十个字段，且 UI 按具体子树解引用），
// 解析成固定结构再序列化会丢未知字段——这正是「上游加字段、UI 已用、网关先丢」那类缺陷的成因。
type Result struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// privateData 是私有地址的 data 子树（Node 的 IpGeoPrivateMarker）。
type privateData struct {
	IP   string `json:"ip"`
	Kind string `json:"kind"`
}

// newPrivateResult 造私有地址结果。
func newPrivateResult(ip string) Result {
	encoded, err := json.Marshal(privateData{IP: ip, Kind: "private"})
	if err != nil {
		// 定长结构体，序列化不可能失败。
		return Result{Status: StatusError, Error: "encode private marker failed"}
	}
	return Result{Status: StatusPrivate, Data: encoded}
}

// Cache 是查询缓存的最小缝隙（Redis 的 GET/SET+TTL）。
//
// 刻意只留这两个动作：ip-geo 不需要键扫描、不需要原子脚本，缝越窄越难被将来误用；
// nil 表示无缓存（Node 在 `getRedisClient()` 为 nil 时同样退化为直查）。
type Cache interface {
	Get(ctx context.Context, key string) (string, bool, error)
	Set(ctx context.Context, key, value string, ttl time.Duration) error
}

// Options 是查询器的可注入面。
type Options struct {
	// BaseURL 是上游根地址（Node 的 IP_GEO_API_URL，默认 https://ip-api.claude-code-hub.app）。
	BaseURL string
	// Token 是上游 Bearer（Node 的 IP_GEO_API_TOKEN，可空）。
	Token string
	// Timeout 是单次上游请求超时（Node 的 IP_GEO_TIMEOUT_MS，默认 1500ms）。
	Timeout time.Duration
	// CacheTTL 是成功结果的缓存时长（Node 的 IP_GEO_CACHE_TTL_SECONDS，默认 3600s）。
	CacheTTL time.Duration
	// HTTPClient 供测试注入假上游；nil 时按 Timeout 新建。
	HTTPClient HTTPDoer
}

// HTTPDoer 是发请求的最小缝隙（*http.Client 即可满足），便于单测注入假上游。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}
