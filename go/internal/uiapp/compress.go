package uiapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
)

// 本文件是「产物压缩态存储」的读取面：元数据格式、明文还原、以及 Accept-Encoding 协商。
//
// 为什么要有这一层：原始产物 142.3 MiB / 736 文件几乎全是文本（HTML 壳、RSC payload、
// JS/CSS），直接 embed 会把这 142 MiB 打进二进制。scripts/build-ui-embed.mjs 把文本类
// 逐文件 brotli 压缩（实测 4.33x，142.3 -> 32.9 MiB），本包按元数据**按需还原**。
//
// 三条不变量：
//
//  1. **元数据缺失即按全明文处理**。没有 MANIFEST.json 时行为与本改动前一字不差——
//     这既让既有测试（fstest.MapFS）无需改动，也让回退只需一个构建参数。
//  2. **每个文件独立压缩存储，不打包成归档**：一次请求只解压它要的那一份，
//     不必为取 `index.html` 解开整棵树。
//  3. **按需解压，不缓存明文**。解压后即用即弃（见 serve_compressed.go 的说明与上限）。

const (
	// manifestFile 是压缩元数据文件名，由 scripts/build-ui-embed.mjs 写入。
	//
	// 它**不进** files 表：它是装配元数据而不是产物，故既不参与择路（请求 `/MANIFEST.json`
	// 落回壳，与「未知路径不 404」的既定语义一致），也不计入 Status 的文件数与字节数。
	manifestFile = "MANIFEST.json"
	// manifestVersion 是本文能识别的元数据版本；不认识的版本必须拒绝启动。
	//
	// 拒绝而不是尽力解析：版本漂移意味着「编码字段的语义变了」，误读会把压缩态当明文直发，
	// 表现为「页面全是乱码」，比起不来更难排查。
	manifestVersion = 1
	// encodingIdentity 是明文存储；encodingBrotli 是 brotli 压缩态。
	encodingIdentity = "identity"
	encodingBrotli   = "br"
)

// manifestEntry 是一个产物的存储元数据。
type manifestEntry struct {
	Encoding string `json:"encoding"`
	RawSize  int    `json:"rawSize"`
}

// manifest 是压缩嵌入的元数据（结构由 scripts/build-ui-embed.mjs 写入）。
type manifest struct {
	Version int                      `json:"version"`
	Entries map[string]manifestEntry `json:"entries"`
}

// loadManifest 读嵌入产物里的 MANIFEST.json。
//
// 返回 found=false 表示「这是未压缩产物」（元数据不存在），调用方按全明文处理。
func loadManifest(assets fs.FS, root string) (manifest, bool, error) {
	name := root + "/" + manifestFile
	body, err := fs.ReadFile(assets, name)
	if err != nil {
		if errors_IsNotExist(err) {
			return manifest{}, false, nil
		}
		return manifest{}, false, fmt.Errorf("uiapp: 读取 %s 失败: %w", manifestFile, err)
	}
	var parsed manifest
	if err := json.Unmarshal(body, &parsed); err != nil {
		return manifest{}, false, fmt.Errorf("uiapp: 解析 %s 失败: %w", manifestFile, err)
	}
	if parsed.Version != manifestVersion {
		return manifest{}, false, fmt.Errorf(
			"uiapp: %s 版本为 %d，本二进制只认 %d（请重跑 scripts/build-ui-embed.mjs 并重建）",
			manifestFile, parsed.Version, manifestVersion,
		)
	}
	return parsed, true, nil
}

// errors_IsNotExist 是 fs.ErrNotExist 的别名化判断（避免在调用处多引一个 errors 包）。
func errors_IsNotExist(err error) bool {
	return err != nil && (err == fs.ErrNotExist || strings.Contains(err.Error(), "file does not exist"))
}

// decodeBrotli 把压缩态还原成明文；rawSize 用于预分配（元数据缺失时为 0，退化成增长式读取）。
func decodeBrotli(stored []byte, rawSize int) ([]byte, error) {
	reader := brotli.NewReader(bytes.NewReader(stored))
	var out bytes.Buffer
	if rawSize > 0 {
		out.Grow(rawSize)
	}
	if _, err := io.Copy(&out, reader); err != nil {
		return nil, fmt.Errorf("uiapp: brotli 解压失败: %w", err)
	}
	return out.Bytes(), nil
}

// acceptsBrotli 判断请求头是否接受 brotli。
//
// 只处理实际会出现的三种写法（浏览器与 curl 都不会发出更复杂的组合）：
// 显式 `br`（带或不带 q）、通配 `*`、以及 `br;q=0` 这种明确拒绝。
// ponytail: 不做完整的 RFC 7231 优先级比较（不必选出「最优」编码——只有 br 与 identity 两档，
// 只要判断 br 可用即可）；若将来加入 gzip/deflate 需要真正排序，再引入通用协商器。
func acceptsBrotli(header string) bool {
	header = strings.TrimSpace(header)
	if header == "" {
		return false
	}
	wildcard := false
	for _, part := range strings.Split(header, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		name, params, _ := strings.Cut(token, ";")
		name = strings.ToLower(strings.TrimSpace(name))
		quality := 1.0
		for _, param := range strings.Split(params, ";") {
			key, value, found := strings.Cut(param, "=")
			if found && strings.EqualFold(strings.TrimSpace(key), "q") {
				if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
					quality = parsed
				}
			}
		}
		switch name {
		case "br":
			return quality > 0
		case "*":
			wildcard = quality > 0
		}
	}
	return wildcard
}
