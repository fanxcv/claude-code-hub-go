package ipgeo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 ipgeo 的移植语义（Node：src/lib/ip-geo/client.ts 与 private-ip.ts）。
// 用的是**本机假上游**（httptest），不打外网。

type fakeCache struct {
	values map[string]string
	ttls   map[string]time.Duration
	gets   int
	sets   int
}

func newFakeCache() *fakeCache {
	return &fakeCache{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (c *fakeCache) Get(_ context.Context, key string) (string, bool, error) {
	c.gets++
	value, ok := c.values[key]
	return value, ok, nil
}

func (c *fakeCache) Set(_ context.Context, key, value string, ttl time.Duration) error {
	c.sets++
	c.values[key] = value
	c.ttls[key] = ttl
	return nil
}

// validPayload 是一份**形状合法**的上游 payload，含一个 Node 未声明的额外字段
// （extra_unknown_field）——用来证明结果走原始 JSON 透传、不会丢未知字段。
const validPayload = `{
  "ip": "1.1.1.1",
  "extra_unknown_field": {"a": 1},
  "location": {
    "country": {"code": "AU", "name": "Australia", "flag": {"emoji": "🇦🇺", "svg": null, "png": null}}
  },
  "timezone": {"id": "Australia/Sydney"},
  "connection": {"asn": 13335, "route": "1.1.1.0/24"}
}`

func TestLookupInvalidIPDoesNotTouchCacheOrUpstream(t *testing.T) {
	cache := newFakeCache()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	lookup := New(cache, Options{BaseURL: server.URL, Timeout: time.Second}, nil)

	result := lookup.LookupIP(context.Background(), "  not-an-ip  ", "")
	if result.Status != StatusError || result.Error != "invalid ip" {
		t.Fatalf("非法 IP 应 error=invalid ip，实得 %+v", result)
	}
	if calls != 0 || cache.gets != 0 || cache.sets != 0 {
		t.Fatalf("非法 IP 不该碰上游或缓存：calls=%d gets=%d sets=%d", calls, cache.gets, cache.sets)
	}
}

func TestIsPrivateMatchesNodeRangeList(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
		why     string
	}{
		{"10.0.0.1", true, "RFC1918"},
		{"172.16.5.5", true, "RFC1918"},
		{"172.32.0.1", false, "刚出 172.16/12"},
		{"192.168.1.1", true, "RFC1918"},
		{"127.0.0.1", true, "回环"},
		{"169.254.10.10", true, "链路本地"},
		{"100.64.0.1", true, "CGN——Node 有、Go 的 IsPrivate() 没有"},
		{"100.127.255.255", true, "CGN 上界"},
		{"100.128.0.1", false, "刚出 100.64/10"},
		{"0.0.0.0", true, "Node 单点值"},
		{"255.255.255.255", true, "Node 单点值"},
		{"::1", true, "IPv6 回环"},
		{"::", true, "未指定地址"},
		{"fe80::1", true, "IPv6 链路本地（Node 的 /^fe[89ab]/）"},
		{"febf::1", true, "fe80::/10 上界"},
		{"fc00::1", true, "ULA"},
		{"fd12:3456::1", true, "ULA"},
		{"::ffff:10.0.0.1", true, "IPv4-mapped 解包后仍是私有"},
		{"::ffff:1.1.1.1", false, "IPv4-mapped 公网"},
		{"1.1.1.1", false, "公网"},
		{"2606:4700:4700::1111", false, "公网 v6"},
		{"garbage", false, "非法输入返回 false（Node 同判）"},
	}
	for _, testCase := range cases {
		if got := IsPrivate(testCase.ip); got != testCase.private {
			t.Errorf("IsPrivate(%q) = %v，期望 %v（%s）", testCase.ip, got, testCase.private, testCase.why)
		}
	}
}

func TestLookupPrivateIPShortCircuits(t *testing.T) {
	cache := newFakeCache()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	lookup := New(cache, Options{BaseURL: server.URL}, nil)

	result := lookup.LookupIP(context.Background(), " 10.1.2.3 ", "")
	if result.Status != StatusPrivate {
		t.Fatalf("私有地址应短路为 private，实得 %+v", result)
	}
	var data map[string]any
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatalf("private data 应是 JSON: %v", err)
	}
	if data["ip"] != "10.1.2.3" || data["kind"] != "private" {
		t.Fatalf("private data 形状应与 Node 同形（trim 后的 ip + kind），实得 %v", data)
	}
	if calls != 0 || cache.sets != 0 {
		t.Fatalf("私有地址不该碰上游或写缓存：calls=%d sets=%d", calls, cache.sets)
	}
}

func TestLookupSuccessPreservesRawPayloadAndCachesWithConfiguredTTL(t *testing.T) {
	cache := newFakeCache()
	var seenPath, seenAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path + "?" + r.URL.RawQuery
		seenAuth = r.Header.Get("authorization")
		_, _ = w.Write([]byte(validPayload))
	}))
	defer server.Close()
	lookup := New(cache, Options{
		BaseURL: server.URL + "/", Token: "tok", CacheTTL: 42 * time.Minute,
	}, nil)

	result := lookup.LookupIP(context.Background(), "1.1.1.1", "zh-CN")
	if result.Status != StatusOK {
		t.Fatalf("应成功，实得 %+v", result)
	}
	if seenPath != "/v1/ip2location/1.1.1.1?lang=zh-CN" {
		t.Fatalf("上游路径应与 Node 同形，实得 %q", seenPath)
	}
	if seenAuth != "Bearer tok" {
		t.Fatalf("配了 token 就该带 Bearer，实得 %q", seenAuth)
	}
	// 未知字段必须原样保留（不是「解析成结构体再序列化」）。
	if !strings.Contains(string(result.Data), "extra_unknown_field") {
		t.Fatalf("结果应原样透传上游 payload，实得 %s", string(result.Data))
	}
	entry, ok := cache.values[cacheKey("1.1.1.1", "zh-CN")]
	if !ok {
		t.Fatal("成功结果应写缓存")
	}
	if !strings.Contains(entry, `"kind":"ok"`) {
		t.Fatalf("缓存值形制应与 Node 同形（kind=ok），实得 %s", entry)
	}
	if cache.ttls[cacheKey("1.1.1.1", "zh-CN")] != 42*time.Minute {
		t.Fatalf("TTL 应取配置值，实得 %v", cache.ttls[cacheKey("1.1.1.1", "zh-CN")])
	}
}

func TestLookupCacheHitSkipsUpstream(t *testing.T) {
	cache := newFakeCache()
	cache.values[cacheKey("1.1.1.1", "en")] = `{"kind":"ok","data":{"ip":"1.1.1.1","cached":true}}`
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer server.Close()
	lookup := New(cache, Options{BaseURL: server.URL}, nil)

	result := lookup.LookupIP(context.Background(), "1.1.1.1", "en")
	if result.Status != StatusOK || !strings.Contains(string(result.Data), "cached") {
		t.Fatalf("应命中缓存，实得 %+v", result)
	}
	if calls != 0 {
		t.Fatalf("命中缓存不该打上游，实得 %d 次", calls)
	}

	// 负缓存同样命中（lang 由调用方给，本包不兜默认值）。
	cache.values[cacheKey("2.2.2.2", "en")] = `{"kind":"error","error":"upstream status 500"}`
	result = lookup.LookupIP(context.Background(), "2.2.2.2", "en")
	if result.Status != StatusError || result.Error != "upstream status 500" {
		t.Fatalf("负缓存应命中并回放原因，实得 %+v", result)
	}
}

func TestLookupUpstreamFailuresAreNegativeCachedFor60s(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"非 2xx", http.StatusInternalServerError, `{"oops":true}`, "upstream status 500"},
		{"形状不符", http.StatusOK, `{"ip":"1.1.1.1"}`, "upstream returned unexpected shape"},
		{"缺 flag.emoji", http.StatusOK, `{"ip":"1.1.1.1","location":{"country":{"code":"AU","name":"A","flag":{}}},"timezone":{"id":"x"},"connection":{}}`, "upstream returned unexpected shape"},
		{"坏 JSON", http.StatusOK, `{not json`, "upstream returned unexpected shape"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cache := newFakeCache()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			lookup := New(cache, Options{BaseURL: server.URL}, nil)

			result := lookup.LookupIP(context.Background(), "1.1.1.1", "en")
			if result.Status != StatusError || result.Error != testCase.wantErr {
				t.Fatalf("应报 %q，实得 %+v", testCase.wantErr, result)
			}
			entry, ok := cache.values[cacheKey("1.1.1.1", "en")]
			if !ok {
				t.Fatal("失败应写负缓存")
			}
			if !strings.Contains(entry, `"kind":"error"`) {
				t.Fatalf("负缓存值形制应与 Node 同形，实得 %s", entry)
			}
			if cache.ttls[cacheKey("1.1.1.1", "en")] != negativeTTL {
				t.Fatalf("负缓存 TTL 应为 60s（Node 的 NEGATIVE_TTL_SECONDS），实得 %v",
					cache.ttls[cacheKey("1.1.1.1", "en")])
			}
		})
	}
}

// TestLookupEmptyLangIsUsedVerbatim 钉住「空串不被本包兜成 en」——Node 的 `?? "en"` 只对
// 「未传」生效，公开面 schema 会把空串拦在 400，自服务面则放行空串。本包若自作主张兜底，
// 缓存键会与 Node 分叉（`ipgeo:v1:{ip}:` vs `ipgeo:v1:{ip}:en`），双跑期两侧互相读不到。
func TestLookupEmptyLangIsUsedVerbatim(t *testing.T) {
	cache := newFakeCache()
	var seenQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(validPayload))
	}))
	defer server.Close()
	lookup := New(cache, Options{BaseURL: server.URL}, nil)

	_ = lookup.LookupIP(context.Background(), "1.1.1.1", "")
	if seenQuery != "lang=" {
		t.Fatalf("空 lang 应原样上送（?lang=），实得 %q", seenQuery)
	}
	if _, ok := cache.values[cacheKey("1.1.1.1", "")]; !ok {
		t.Fatalf("缓存键应用空 lang（与 Node 同形），实得键集合 %v", cache.values)
	}
}

func TestLookupWithoutCacheStillWorks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validPayload))
	}))
	defer server.Close()
	lookup := New(nil, Options{BaseURL: server.URL}, nil)

	if result := lookup.LookupIP(context.Background(), "1.1.1.1", "en"); result.Status != StatusOK {
		t.Fatalf("无缓存时应直查成功，实得 %+v", result)
	}
}

func TestLookupCacheCorruptionIsTreatedAsMiss(t *testing.T) {
	cache := newFakeCache()
	cache.values[cacheKey("1.1.1.1", "en")] = `{broken`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(validPayload))
	}))
	defer server.Close()
	lookup := New(cache, Options{BaseURL: server.URL}, nil)

	if result := lookup.LookupIP(context.Background(), "1.1.1.1", "en"); result.Status != StatusOK {
		t.Fatalf("坏缓存应视作未命中并回查上游，实得 %+v", result)
	}
}
