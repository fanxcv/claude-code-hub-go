// Package appversion 是**进程版本号取值的唯一实现**。
//
// 为什么要有这个包：版本号曾在四处各取一遍（`/api/version`、管理面 `/api/health`、
// 运维面 `/api/health`、UI 壳注入），每处都自带一条「环境变量 → VERSION 文件 → 自己的兜底常量」
// 的链，于是同一个镜像里能同时出现三个版本号（2026-09 实测：镜像 tag `cchd-1.0.1`、
// `/api/health` 报 `0.9.0`、页脚写 `0.9.5`），排障时据此误判过「有两台不同的实例」。
// 现在只有一条链、一个兜底常量、一处实现。
//
// 取值链（唯一真源是**发布时注入**的 `APP_VERSION`，见 go/deploy/Dockerfile* 与 release 脚本）：
//
//	APP_VERSION → CCH_APP_VERSION → NEXT_PUBLIC_APP_VERSION → Fallback
//
// 后两个名字是历史兼容：`CCH_APP_VERSION` 是本仓 Go 侧用过的名字，`NEXT_PUBLIC_APP_VERSION`
// 是 Node 时代的名字、也是 UI 导出注入用的名字。
//
// **不再读 `VERSION` 文件**（有意）：那个文件是 Node 时代的产物，且从未进过生产镜像
// （Dockerfile 只拷二进制与 CA），故这条兜底在生产恒不命中、只在本地开发时命中——
// 正是它在开发机上把页脚写成 `0.9.5`，与运行时报告的 `0.9.0` 分叉。
package appversion

import (
	"regexp"
	"strings"
)

// Fallback 是没有任何注入时的兜底版本，与仓库 package.json 的 version 同值。
//
// 为什么要留兜底：`/api/version` 是「当前版本 vs 上游最新版」的比较端点，必须总能给出非空的
// current；而 Go 二进制里没有 package.json（生产镜像只拷二进制与 CA）。两者的相等关系由
// TestFallbackMatchesPackageJSON 钉住——一旦分叉，「页面显示的版本」与「仓库声明的版本」
// 又会各说各话，正是本次要修的病。
const Fallback = "v1.6.2"

// envNames 是注入版本用的环境变量名，**按优先级排列**。
//
// `APP_VERSION` 在前：发布时由镜像 ENV 注入（docker build 的 APP_VERSION 参数），
// 它是唯一真源；其余两个只为兼容既有部署，不改变「谁说了算」。
var envNames = []string{"APP_VERSION", "CCH_APP_VERSION", "NEXT_PUBLIC_APP_VERSION"}

// Resolve 返回**展示形态**的版本号（带小写 v 前缀）。lookup 传 os.Getenv 即可，测试注入假环境。
//
// 上限（有意）：只做「统一小写 v 前缀」这一种规整——形如 `release-1.2` 的异形值原样返回。
func Resolve(lookup func(string) string) string {
	return Display(injected(lookup))
}

// ResolveBare 返回不带 v 前缀的版本号。
//
// 用在 `/api/health` 的 version 字段：Node 的 getAppVersion 就是
// `APP_VERSION.replace(/^v/i, "")`，运维脚本按无前缀的形式比较，故这里保持去前缀。
func ResolveBare(lookup func(string) string) string {
	return Bare(Resolve(lookup))
}

// Display 把任意值规整为展示形态：统一小写 v 前缀、纯数字补 v 前缀、其余原样。
//
// 上游 release tag 的规整也走它（见 internal/adminapi 的 version 端点），故导出。
func Display(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return trimmed
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "v") {
		return "v" + trimmed[1:]
	}
	if versionReleasePattern.MatchString(trimmed) {
		return "v" + trimmed
	}
	return trimmed
}

// Bare 去掉展示形态的前导 v（大小写都认）。
func Bare(raw string) string {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "v"), "V")
	return strings.TrimSpace(trimmed)
}

// injected 按 envNames 的顺序取第一个非空值；全空时用 Fallback。
func injected(lookup func(string) string) string {
	if lookup != nil {
		for _, name := range envNames {
			if value := strings.TrimSpace(lookup(name)); value != "" {
				return value
			}
		}
	}
	return Fallback
}

// versionReleasePattern 判断「看起来像版本号」，用于补 v 前缀。
var versionReleasePattern = regexp.MustCompile(`^\d+(?:\.\d+)*(?:[-+].+)?$`)
