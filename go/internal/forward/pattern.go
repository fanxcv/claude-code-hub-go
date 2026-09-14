package forward

import (
	"regexp"
	"strings"
	"sync"
)

// compileModelPattern 复刻 provider-pattern-regex.ts 的 resolveProviderPatternRegex：
// 先按标准正则解析；失败且含 glob 元字符时，把其余元字符转义后按 glob 语义整串匹配。
//
// 缓存与 Node 侧一致：容量上限 1024，超限整体清空。
var (
	modelPatternCacheMu  sync.Mutex
	modelPatternCache    = map[string]*regexp.Regexp{}
	modelPatternMaxItems = 1024

	globMetaPattern  = regexp.MustCompile(`[*?]`)
	regexMetaPattern = regexp.MustCompile(`[\\^$.|()[\]{}+]`)
)

func compileModelPattern(pattern string) (*regexp.Regexp, bool) {
	modelPatternCacheMu.Lock()
	cached, ok := modelPatternCache[pattern]
	modelPatternCacheMu.Unlock()
	if ok {
		return cached, cached != nil
	}

	compiled, ok := compileModelPatternUncached(pattern)

	modelPatternCacheMu.Lock()
	if len(modelPatternCache) >= modelPatternMaxItems {
		modelPatternCache = map[string]*regexp.Regexp{}
	}
	if ok {
		modelPatternCache[pattern] = compiled
	} else {
		// 负缓存：解析失败的形态不重复尝试。
		modelPatternCache[pattern] = nil
	}
	modelPatternCacheMu.Unlock()
	return compiled, ok
}

func compileModelPatternUncached(pattern string) (*regexp.Regexp, bool) {
	if re, err := regexp.Compile(pattern); err == nil {
		return re, true
	}
	if !globMetaPattern.MatchString(pattern) {
		return nil, false
	}
	quoted := regexMetaPattern.ReplaceAllStringFunc(pattern, func(meta string) string {
		return "\\" + meta
	})
	quoted = strings.ReplaceAll(quoted, `\*`, `[\s\S]*`)
	quoted = strings.ReplaceAll(quoted, `\?`, `[\s\S]`)
	re, err := regexp.Compile("^(?:" + quoted + ")$")
	if err != nil {
		return nil, false
	}
	return re, true
}
