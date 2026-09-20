package uiapp

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/andybalholm/brotli"
)

// 本文件的用例钉住「压缩态存储」这条链路的四条不变量：
//
//  1. **逐字节正确性**：压缩存储的产物，无论客户端是否接受 br，解出来的字节都与原始产物相同；
//  2. **charset 不回退**：`.txt`（RSC payload）与 `.js/.mjs` 必须仍是 UTF-8——
//     这条此前修过一次（缺 charset 会让浏览器按 latin-1 解 payload），压缩链路不得把它带回；
//  3. **协商与头部**：接受 br 时带 `Content-Encoding: br` 与 `Vary: Accept-Encoding`；
//     不接受时回退明文且**不带** Content-Encoding；明文存储的资源不设 Vary；
//  4. **元数据缺失即全明文**：未压缩产物（含既有测试的 fstest 夹具）行为不变。

// brotliBytes 造一份 brotli 压缩态，供夹具使用。
func brotliBytes(t *testing.T, raw string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := brotli.NewWriterLevel(&buffer, 5)
	if _, err := writer.Write([]byte(raw)); err != nil {
		t.Fatalf("构造压缩夹具失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("构造压缩夹具失败: %v", err)
	}
	return buffer.Bytes()
}

// rscPayload 是一段形态接近真实 RSC payload 的文本（含中文，确保 UTF-8 编码差异可见）。
const rscPayload = `1:"$Sreact.fragment"` + "\n" +
	`2:{"messages":{"settings":{"title":"设置","description":"供应商与密钥配置"}}}` + "\n"

// jsChunk 是一段 JS chunk（非 ASCII 注释，用于证明 charset 未回退）。
const jsChunk = "// 供应商列表\nconsole.log(\"供应商\");\n"

// compressedAssets 造一份**压缩态**产物夹具（含 MANIFEST.json）。
func compressedAssets(t *testing.T) fstest.MapFS {
	t.Helper()
	return fstest.MapFS{
		"assets/.gitkeep": &fstest.MapFile{},
		"assets/BUILD_ID": &fstest.MapFile{Data: []byte("build-br01\n")},
		"assets/index.html": &fstest.MapFile{
			Data: []byte("<!doctype html><html><head><!--CCH_BOOTSTRAP--></head><body>root</body></html>"),
		},
		"assets/zh-CN/index.html": &fstest.MapFile{
			Data: []byte("<!doctype html><html><head><!--CCH_BOOTSTRAP--></head><body>zh</body></html>"),
		},
		"assets/zh-CN/dashboard/index.txt": &fstest.MapFile{
			Data: brotliBytes(t, rscPayload),
		},
		"assets/_next/static/chunks/app-abc123.js": &fstest.MapFile{
			Data: brotliBytes(t, jsChunk),
		},
		"assets/favicon.ico": &fstest.MapFile{Data: []byte{0x00, 0x01, 0x02, 0x03}},
		"assets/MANIFEST.json": &fstest.MapFile{Data: []byte(`{"version":1,"entries":{` +
			`"zh-CN/dashboard/index.txt":{"encoding":"br","rawSize":` + itoa(len(rscPayload)) + `},` +
			`"_next/static/chunks/app-abc123.js":{"encoding":"br","rawSize":` + itoa(len(jsChunk)) + `},` +
			`"favicon.ico":{"encoding":"identity","rawSize":4}` +
			`}}`)},
	}
}

// itoa 避免为一个测试引入 strconv 的额外 import 噪音。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func newCompressedHandler(t *testing.T, options Options) *Handler {
	t.Helper()
	if options.Locales == nil {
		options.Locales = testLocales
	}
	handler, err := newHandler(compressedAssets(t), assetsRoot, options)
	if err != nil {
		t.Fatalf("建压缩产物处理器失败: %v", err)
	}
	return handler
}

// 协商两端都必须解出与原始产物逐字节相同的明文。
func TestCompressedServingIsByteExact(t *testing.T) {
	handler := newCompressedHandler(t, Options{})

	cases := []struct {
		name string
		path string
		want string
	}{
		{"txt(RSC payload)", "/zh-CN/dashboard/index.txt", rscPayload},
		{"js(chunk)", "/_next/static/chunks/app-abc123.js", jsChunk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 接受 br：直出存储态（Content-Encoding: br），客户端解码后必须等于原文。
			withBR := do(handler, http.MethodGet, tc.path, map[string]string{"Accept-Encoding": "br"})
			if withBR.Code != http.StatusOK {
				t.Fatalf("br 请求应 200，实际 %d", withBR.Code)
			}
			if got := withBR.Header().Get("Content-Encoding"); got != "br" {
				t.Fatalf("br 请求应带 Content-Encoding: br，实际 %q", got)
			}
			decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(withBR.Body.Bytes())))
			if err != nil {
				t.Fatalf("解 br 响应失败: %v", err)
			}
			if string(decoded) != tc.want {
				t.Fatalf("br 响应的明文与原始产物不同：\n got=%q\nwant=%q", decoded, tc.want)
			}

			// 不接受 br：服务端即时解压，正文直接等于原文且不带 Content-Encoding。
			plain := do(handler, http.MethodGet, tc.path, nil)
			if plain.Code != http.StatusOK {
				t.Fatalf("明文请求应 200，实际 %d", plain.Code)
			}
			if got := plain.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("明文请求不应带 Content-Encoding，实际 %q", got)
			}
			if plain.Body.String() != tc.want {
				t.Fatalf("明文回退与原始产物不同：\n got=%q\nwant=%q", plain.Body.String(), tc.want)
			}
		})
	}
}

// charset 不得回退：`.txt` 与 `.js` 在两种协商下都必须声明 UTF-8。
//
// 这条此前修过一次（缺 charset 会让浏览器按 latin-1 解 RSC payload），
// 而压缩链路最容易被写错的地方就是「按存储态名字猜 MIME」——本用例把口径钉死在**逻辑名**上。
func TestCompressedServingKeepsCharset(t *testing.T) {
	handler := newCompressedHandler(t, Options{})

	cases := []struct {
		path string
		want string
	}{
		{"/zh-CN/dashboard/index.txt", "text/plain; charset=utf-8"},
		{"/_next/static/chunks/app-abc123.js", "text/javascript; charset=utf-8"},
	}
	for _, tc := range cases {
		for _, encoding := range []string{"br", "identity"} {
			headers := map[string]string{}
			if encoding == "br" {
				headers["Accept-Encoding"] = "br"
			} else {
				headers["Accept-Encoding"] = "identity"
			}
			response := do(handler, http.MethodGet, tc.path, headers)
			if got := response.Header().Get("Content-Type"); got != tc.want {
				t.Errorf("%s 在 %s 协商下 MIME 应为 %q，实际 %q", tc.path, encoding, tc.want, got)
			}
			if bytes.Contains(response.Body.Bytes(), []byte("å")) {
				t.Errorf("%s 在 %s 协商下正文出现 UTF-8 被按 latin-1 解读的痕迹", tc.path, encoding)
			}
		}
	}
}

// 协商头部与 Vary 的归属：压缩存储的产物恒带 Vary；明文存储的资源不带。
func TestCompressedNegotiationHeaders(t *testing.T) {
	handler := newCompressedHandler(t, Options{})

	text := do(handler, http.MethodGet, "/zh-CN/dashboard/index.txt", map[string]string{"Accept-Encoding": "br"})
	if got := text.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("压缩产物应带 Vary: Accept-Encoding，实际 %q", got)
	}
	shell := do(handler, http.MethodGet, "/zh-CN/dashboard", map[string]string{"Accept-Encoding": "br"})
	if got := shell.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("壳（服务时即时压缩）应带 Vary: Accept-Encoding，实际 %q", got)
	}
	// 明文存储的图片：响应不随 Accept-Encoding 变，不应带 Vary（带了只会降低缓存命中率）。
	icon := do(handler, http.MethodGet, "/favicon.ico", map[string]string{"Accept-Encoding": "br"})
	if got := icon.Header().Get("Vary"); got != "" {
		t.Errorf("明文存储的二进制不应带 Vary，实际 %q", got)
	}
	if got := icon.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("明文存储的二进制不应带 Content-Encoding，实际 %q", got)
	}
	if !bytes.Equal(icon.Body.Bytes(), []byte{0x00, 0x01, 0x02, 0x03}) {
		t.Errorf("明文存储的二进制应原样返回，实际 %v", icon.Body.Bytes())
	}
}

// 壳即使未被压缩存储，也必须能在 br 协商下压缩发出——而它的 ETag 仍按注入后的明文算。
func TestShellCompressedForClient(t *testing.T) {
	handler := newCompressedHandler(t, Options{
		Sessions: adminResolver(nil),
		Meta:     func(context.Context) (Meta, error) { return Meta{SiteTitle: "CC Hub"}, nil },
	})

	withBR := do(handler, http.MethodGet, "/zh-CN/dashboard", map[string]string{"Accept-Encoding": "br"})
	if got := withBR.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("壳在 br 协商下应压缩，实际 Content-Encoding=%q", got)
	}
	decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(withBR.Body.Bytes())))
	if err != nil {
		t.Fatalf("解壳失败: %v", err)
	}
	// 解出来必须仍是可注入过的壳：标记被替换、引导数据在内。
	if bytes.Contains(decoded, []byte(bootstrapMarker)) {
		t.Error("压缩后的壳里不应还留着注入标记")
	}
	if !bytes.Contains(decoded, []byte("__CCH_BOOTSTRAP__")) {
		t.Error("压缩后的壳里应含引导数据")
	}
	if !bytes.Contains(decoded, []byte("CC Hub")) {
		t.Error("压缩后的壳里应含站点标题")
	}

	// ETag 按注入后明文算：与不压缩那次请求的 ETag 相同（同一表示、同一验证器）。
	plain := do(handler, http.MethodGet, "/zh-CN/dashboard", map[string]string{"Accept-Encoding": "identity"})
	if withBR.Header().Get("ETag") != plain.Header().Get("ETag") {
		t.Errorf("壳的 ETag 不应随协商变化：br=%q identity=%q",
			withBR.Header().Get("ETag"), plain.Header().Get("ETag"))
	}
	// 协商缓存照常：拿 br 响应的 ETag 去问，应 304。
	notModified := do(handler, http.MethodGet, "/zh-CN/dashboard", map[string]string{
		"Accept-Encoding": "br",
		"If-None-Match":   withBR.Header().Get("ETag"),
	})
	if notModified.Code != http.StatusNotModified {
		t.Errorf("br 协商下的协商缓存应仍能 304，实际 %d", notModified.Code)
	}
}

// 协商解析要处理实际会出现的写法，且明确拒绝的（q=0）不得被当成接受。
func TestAcceptsBrotli(t *testing.T) {
	cases := []struct {
		header string
		want   bool
	}{
		{"", false},
		{"gzip, deflate", false},
		{"br", true},
		{"gzip, br", true},
		{"br;q=0.8", true},
		{"br;q=0", false},
		{"gzip;q=1, br;q=0", false},
		{"*", true},
		{"*;q=0", false},
		{"BR", true},
		{"gzip, deflate, br, zstd", true},
	}
	for _, tc := range cases {
		if got := acceptsBrotli(tc.header); got != tc.want {
			t.Errorf("acceptsBrotli(%q) 应为 %v，实际 %v", tc.header, tc.want, got)
		}
	}
}

// 元数据缺失（未压缩产物）与既有行为完全一致：无 Vary、无 Content-Encoding、正文原样。
func TestIdentityAssetsWithoutManifest(t *testing.T) {
	handler := newTestHandler(t, Options{}) // 复用既有夹具：不含 MANIFEST.json

	text := do(handler, http.MethodGet, "/_next/static/chunks/app-abc123.js", map[string]string{"Accept-Encoding": "br"})
	if got := text.Header().Get("Vary"); got != "" {
		t.Errorf("未压缩产物不应带 Vary，实际 %q", got)
	}
	if got := text.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("未压缩产物不应带 Content-Encoding，实际 %q", got)
	}
	if text.Body.String() != "console.log(1)" {
		t.Errorf("未压缩产物正文应原样，实际 %q", text.Body.String())
	}
}

// 元数据版本不认识时必须拒绝装配：静默按明文处理会把压缩流当明文发出去（满页乱码）。
func TestUnknownManifestVersionFailsFast(t *testing.T) {
	assets := fstest.MapFS{
		"assets/index.html": &fstest.MapFile{Data: []byte("<html><head></head></html>")},
		"assets/MANIFEST.json": &fstest.MapFile{
			Data: []byte(`{"version":99,"entries":{}}`),
		},
	}
	if _, err := newHandler(assets, assetsRoot, Options{}); err == nil {
		t.Fatal("未知元数据版本应拒绝装配")
	}
}

// 元数据声明为 br、但存储的字节不是 brotli 流（产物损坏 / 清单与实际不符）：
// 客户端不接受 br 时应 500 并留日志，绝不能把压缩流（这里是普通文本）当明文发出去。
func TestCorruptCompressedAssetFailsLoud(t *testing.T) {
	assets := fstest.MapFS{
		"assets/index.html":        &fstest.MapFile{Data: []byte("<html><head></head></html>")},
		"assets/zh-CN/payload.txt": &fstest.MapFile{Data: []byte("this is not brotli at all, just plain text")},
		"assets/MANIFEST.json":     &fstest.MapFile{Data: []byte(`{"version":1,"entries":{"zh-CN/payload.txt":{"encoding":"br","rawSize":40}}}`)},
		"assets/zh-CN/index.html":  &fstest.MapFile{Data: []byte("<html><head></head></html>")},
		"assets/_next/static/a.js": &fstest.MapFile{Data: []byte("x")},
	}
	handler, err := newHandler(assets, assetsRoot, Options{Locales: testLocales})
	if err != nil {
		t.Fatalf("建处理器失败: %v", err)
	}
	response := do(handler, http.MethodGet, "/zh-CN/payload.txt", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("声明为 br 但字节不是 brotli 时应 500，实际 %d（正文 %q）",
			response.Code, response.Body.String())
	}
}

// **壳也处于压缩存储态**时，注入仍必须生效。
//
// 这条是实测踩出来的回归：壳被 brotli 压缩存储后，早期写法拿 file.body（压缩流）去 Inject，
// 标记与 </head> 都不存在，注入**静默失败**，随后还把压缩流再压一遍（客户端解出的是一层 br，
// 不是 HTML）。表现为「页面能开但登录态与站点标题丢了」，日志里是 shell_injection_point_missing。
func TestCompressedShellStillInjects(t *testing.T) {
	assets := fstest.MapFS{
		"assets/index.html": &fstest.MapFile{Data: []byte("<html><head></head><body>root</body></html>")},
		"assets/zh-CN/index.html": &fstest.MapFile{
			Data: brotliBytes(t, "<!doctype html><html><head><!--CCH_BOOTSTRAP--></head><body>zh</body></html>"),
		},
		"assets/MANIFEST.json": &fstest.MapFile{Data: []byte(`{"version":1,"entries":{` +
			`"zh-CN/index.html":{"encoding":"br","rawSize":74}` +
			`}}`)},
	}
	handler, err := newHandler(assets, assetsRoot, Options{
		Locales:  testLocales,
		Sessions: adminResolver(nil),
		Meta:     func(context.Context) (Meta, error) { return Meta{SiteTitle: "CC Hub"}, nil },
	})
	if err != nil {
		t.Fatalf("建处理器失败: %v", err)
	}

	// 明文协商：客户端能直接读到注入了的 HTML。
	plain := do(handler, http.MethodGet, "/zh-CN/dashboard", map[string]string{"Accept-Encoding": "identity"})
	body := plain.Body.String()
	if !strings.Contains(body, "__CCH_BOOTSTRAP__") {
		t.Fatalf("压缩存储的壳也必须注入引导数据，实际正文 %q", body)
	}
	if strings.Contains(body, bootstrapMarker) {
		t.Error("注入后不应还留着注入标记")
	}
	if !strings.Contains(body, "CC Hub") || !strings.Contains(body, "admin") {
		t.Errorf("注入内容应含站点标题与角色，实际 %q", body)
	}
	if !strings.HasPrefix(body, "<!doctype html>") {
		t.Errorf("解压后应是 HTML，实际开头 %q", body[:min(len(body), 40)])
	}

	// br 协商：只压**一层**（解出来的必须已是含注入的 HTML，而不是又一层 br）。
	withBR := do(handler, http.MethodGet, "/zh-CN/dashboard", map[string]string{"Accept-Encoding": "br"})
	if got := withBR.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("br 协商应带 Content-Encoding: br，实际 %q", got)
	}
	decoded, err := io.ReadAll(brotli.NewReader(bytes.NewReader(withBR.Body.Bytes())))
	if err != nil {
		t.Fatalf("解壳失败: %v", err)
	}
	if !strings.Contains(string(decoded), "__CCH_BOOTSTRAP__") {
		t.Fatalf("br 解一层后应已是含注入的 HTML，实际 %q", string(decoded)[:min(len(decoded), 60)])
	}
	if !strings.Contains(string(decoded), "CC Hub") {
		t.Error("br 解出的壳里应含站点标题（证明只压了一层）")
	}
}

// min 是 Go 1.21+ 内置的别名，这里显式声明以便在字符串切片上使用（可读性优于内联判断）。
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 压缩态的产物也必须计入 Status 的字节数（供 /readyz 如实报告嵌入体积）。
func TestStatusCountsStoredBytes(t *testing.T) {
	status := newCompressedHandler(t, Options{}).Status()
	if !status.Enabled {
		t.Fatal("Status.Enabled 应为 true")
	}
	if status.BuildID != "build-br01" {
		t.Errorf("应读到 BUILD_ID，实际 %q", status.BuildID)
	}
	// MANIFEST.json 是装配元数据而不是产物：不计入文件数（根壳 + zh-CN 壳 + txt + js + ico = 5，
	// 占位文件与 BUILD_ID 也不计）。
	if status.Files != 5 {
		t.Errorf("产物文件数应为 5（MANIFEST 不计），实际 %d", status.Files)
	}
}

// gzip 客户端（明确不接受 br）必须拿到明文：这是「老客户端回退」的正例。
func TestGzipOnlyClientGetsPlaintext(t *testing.T) {
	handler := newCompressedHandler(t, Options{})
	response := do(handler, http.MethodGet, "/zh-CN/dashboard/index.txt",
		map[string]string{"Accept-Encoding": "gzip, deflate"})
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("只接受 gzip 的客户端不应收到 Content-Encoding，实际 %q", got)
	}
	if response.Body.String() != rscPayload {
		t.Fatalf("只接受 gzip 的客户端应收到明文原文，实际 %q", response.Body.String())
	}
	// 顺带证明：响应体不是 gzip 流（服务端不做二次编码）。
	if _, err := gzip.NewReader(bytes.NewReader(response.Body.Bytes())); err == nil {
		t.Error("响应体不应是 gzip 流")
	}
}

// 断言 gzip 与 br 同时被接受时优先走 br（存储态直出，零解压）。
func TestBrotliPreferredWhenBothAccepted(t *testing.T) {
	handler := newCompressedHandler(t, Options{})
	response := do(handler, http.MethodGet, "/zh-CN/dashboard/index.txt",
		map[string]string{"Accept-Encoding": "gzip, deflate, br"})
	if got := response.Header().Get("Content-Encoding"); got != "br" {
		t.Fatalf("同时接受时应走 br，实际 %q", got)
	}
	if _, err := io.ReadAll(brotli.NewReader(bytes.NewReader(response.Body.Bytes()))); err != nil {
		t.Fatalf("br 响应应可解: %v", err)
	}
}
