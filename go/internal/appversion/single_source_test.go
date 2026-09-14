package appversion

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// injectionNames 是允许出现在本包之外**代码**里的注入变量名——即一个也不许。
var injectionNames = []string{"APP_VERSION", "CCH_APP_VERSION", "NEXT_PUBLIC_APP_VERSION"}

// fallbackVersionDecl 匹配「兜底版本常量」的声明式写法（`const xxxVersion = "..."`）。
var fallbackVersionDecl = regexp.MustCompile(`(?i)\b(?:default|fallback)[A-Za-z_]*version[A-Za-z_]*\s*=\s*"`)

// TestVersionSourcesOnlyLiveHere 是**结构性钉子**：版本号只许在本包产生。
//
// 为什么扫源码而不只测行为：四处消费点各自那条旧链，在只设 `APP_VERSION` 时也可能碰巧报对
// （旧 httpapi 链的最后一级就是它），行为断言抓不住「又长出一条链」。而这类分叉的成因
// 永远是同一件事——**别处又自带了一份环境变量名字面量或兜底常量**，那就直接扫它。
//
// 三条判据（只扫**非测试**的 .go）：
//
//  1. 三个注入变量名的**字符串字面量**不得在别处出现（新链必然要读其中之一）；
//  2. 不得出现「读名为 `VERSION` 的文件」（Node 时代那条文件兜底，已废弃）；
//  3. 不得声明形如 `defaultVersion` / `fallbackAppVersion` 的兜底常量。
//
// 反证已逐条验过（改回旧写法即转红）：见报告
// 的「反证」一节。
// 上限（有意）：按行扫描，多行拼出来的 ReadFile 参数会漏判；只认字符串字面量，
// 故注释里的名字不算数。它是第二道网——第一道是行为断言（设 APP_VERSION 后各处报同一个值）。
func TestVersionSourcesOnlyLiveHere(t *testing.T) {
	self := filepath.Join("..", "..", "internal", "appversion")
	var problems []string

	walk := filepath.WalkDir(filepath.Join("..", ".."), func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			switch {
			case current == self:
				return fs.SkipDir
			case entry.Name() == "vendor" || entry.Name() == "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(current, ".go") || strings.HasSuffix(current, "_test.go") {
			return nil
		}
		raw, readErr := os.ReadFile(current)
		if readErr != nil {
			return readErr
		}
		for index, line := range strings.Split(string(raw), "\n") {
			where := fmt.Sprintf("%s:%d", current, index+1)
			for _, name := range injectionNames {
				if strings.Contains(line, `"`+name+`"`) {
					problems = append(problems, fmt.Sprintf(
						"%s 出现注入变量名字面量 %s：版本取值只许经 internal/appversion", where, name))
				}
			}
			if strings.Contains(line, "ReadFile") && strings.Contains(line, `"VERSION"`) {
				problems = append(problems, fmt.Sprintf(
					"%s 读了名为 VERSION 的文件：文件兜底已废弃（那份文件在生产镜像里从不存在）", where))
			}
			if fallbackVersionDecl.MatchString(line) {
				problems = append(problems, fmt.Sprintf(
					"%s 声明了兜底版本常量：兜底只许有 appversion.Fallback 这一份", where))
			}
		}
		return nil
	})
	if walk != nil {
		t.Fatalf("扫描 go/ 失败: %v", walk)
	}
	for _, problem := range problems {
		t.Errorf("版本号来源分叉：%s", problem)
	}
}
