package clientver

import (
	"strconv"
	"strings"
)

// 本文件复刻 src/lib/version.ts 的语义化比较。
//
// 返回值的基准与函数名一致（不是 Node 的 compareVersions 那套反向语义）：
// 本包只导出语义化辅助函数，Node 侧的反向比较函数不移植。

// parsedVersion 是解析后的版本号。
type parsedVersion struct {
	numbers    []int64
	prerelease []prereleaseID
	hasPre     bool
}

// prereleaseID 是预发布标识：数字段或字符串段（SemVer 规则里数字优先级更低）。
type prereleaseID struct {
	numeric bool
	number  int64
	text    string
}

// parseVersionLike 复刻 parseSemverLike：忽略 `v` 前缀与 `+build` 元数据。
//
// 解析失败返回 false——Node 侧此时视为相等（fail-open）。
func parseVersionLike(raw string) (parsedVersion, bool) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(strings.TrimPrefix(trimmed, "v"), "V")
	if trimmed == "" {
		return parsedVersion{}, false
	}
	if index := strings.Index(trimmed, "+"); index >= 0 {
		trimmed = trimmed[:index]
	}
	if trimmed == "" {
		return parsedVersion{}, false
	}
	core := trimmed
	prereleaseRaw := ""
	if index := strings.Index(trimmed, "-"); index >= 0 {
		core = trimmed[:index]
		prereleaseRaw = trimmed[index+1:]
	}
	if core == "" {
		return parsedVersion{}, false
	}
	parts := strings.Split(core, ".")
	numbers := make([]int64, 0, len(parts))
	for _, part := range parts {
		digits := leadingDigits(part)
		if digits == "" {
			return parsedVersion{}, false
		}
		value, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return parsedVersion{}, false
		}
		numbers = append(numbers, value)
	}
	result := parsedVersion{numbers: numbers}
	if prereleaseRaw != "" {
		result.hasPre = true
		for _, id := range strings.Split(prereleaseRaw, ".") {
			if isAllDigits(id) {
				value, err := strconv.ParseInt(id, 10, 64)
				if err != nil {
					result.prerelease = append(result.prerelease, prereleaseID{text: id})
					continue
				}
				result.prerelease = append(result.prerelease, prereleaseID{numeric: true, number: value})
				continue
			}
			result.prerelease = append(result.prerelease, prereleaseID{text: id})
		}
	}
	return result, true
}

func leadingDigits(raw string) string {
	end := 0
	for end < len(raw) && raw[end] >= '0' && raw[end] <= '9' {
		end++
	}
	return raw[:end]
}

func isAllDigits(raw string) bool {
	if raw == "" {
		return false
	}
	return len(leadingDigits(raw)) == len(raw)
}

// compareVersions 复刻 version.ts 的 compareVersions，但返回常规语义：
// 1 = a 更新，0 = 相等或不可解析，-1 = a 更旧。
func compareVersions(a string, b string) int {
	left, okLeft := parseVersionLike(a)
	right, okRight := parseVersionLike(b)
	if !okLeft || !okRight {
		// fail-open：不可解析即视为相等，避免误判导致拦截。
		return 0
	}
	length := len(left.numbers)
	if len(right.numbers) > length {
		length = len(right.numbers)
	}
	for index := 0; index < length; index++ {
		leftNumber := int64(0)
		if index < len(left.numbers) {
			leftNumber = left.numbers[index]
		}
		rightNumber := int64(0)
		if index < len(right.numbers) {
			rightNumber = right.numbers[index]
		}
		if leftNumber != rightNumber {
			if leftNumber > rightNumber {
				return 1
			}
			return -1
		}
	}
	// 核心段相等：稳定版 > 预发布版。
	if !left.hasPre && !right.hasPre {
		return 0
	}
	if !left.hasPre && right.hasPre {
		return 1
	}
	if left.hasPre && !right.hasPre {
		return -1
	}
	length = len(left.prerelease)
	if len(right.prerelease) > length {
		length = len(right.prerelease)
	}
	for index := 0; index < length; index++ {
		if index >= len(left.prerelease) {
			return -1
		}
		if index >= len(right.prerelease) {
			return 1
		}
		leftID := left.prerelease[index]
		rightID := right.prerelease[index]
		if leftID.numeric && rightID.numeric {
			if leftID.number != rightID.number {
				if leftID.number > rightID.number {
					return 1
				}
				return -1
			}
			continue
		}
		// 数字标识优先级低于字符串标识。
		if leftID.numeric && !rightID.numeric {
			return -1
		}
		if !leftID.numeric && rightID.numeric {
			return 1
		}
		if leftID.text != rightID.text {
			if leftID.text > rightID.text {
				return 1
			}
			return -1
		}
	}
	return 0
}

// IsVersionGreater 报告 a 是否比 b 新。
func IsVersionGreater(a string, b string) bool { return compareVersions(a, b) > 0 }

// IsVersionLess 报告 a 是否比 b 旧（不可解析时返回 false，即不提示升级）。
func IsVersionLess(a string, b string) bool { return compareVersions(a, b) < 0 }
