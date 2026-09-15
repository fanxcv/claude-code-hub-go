package responsefix

import (
	"encoding/json"
	"strings"
)

// Config 逐字对应 Node 的 `ResponseFixerConfig`（system_settings.response_fixer_config）。
type Config struct {
	// FixTruncatedJSON 开启截断 JSON 补全。
	FixTruncatedJSON bool
	// FixSSEFormat 开启 SSE 帧格式修复（仅流式路径消费）。
	FixSSEFormat bool
	// FixEncoding 开启编码修复（BOM / 空字节 / 非法 UTF-8）。
	FixEncoding bool
	// MaxJSONDepth 是 JSON 补全允许的最大嵌套深度，超过即放弃修复。
	MaxJSONDepth int
	// MaxFixSize 是单次修复的字节上限，流式路径同时是缓冲上限（超过即降级透传）。
	MaxFixSize int
}

// DefaultConfig 对齐 Node 的 `defaultResponseFixerConfig`（index.ts:30-36）。
func DefaultConfig() Config {
	return Config{
		FixTruncatedJSON: true,
		FixSSEFormat:     true,
		FixEncoding:      true,
		MaxJSONDepth:     200,
		MaxFixSize:       1024 * 1024,
	}
}

// ParseConfig 复刻 Node 的 `settings.responseFixerConfig ?? DEFAULT_CONFIG`
// 与 `{...defaultResponseFixerConfig, ...row}` 的合并语义。
//
// 为什么用指针字段：Node 的展开是「行的键存在就覆盖默认」，**显式的 false/0 也算覆盖**
// （`maxFixSize: 0` 会让每个非空正文都超限、直接不修）。用零值结构体解码会把「没写」
// 与「显式写了 0」混成一件事，故这里必须区分。
func ParseConfig(raw []byte) Config {
	config := DefaultConfig()
	if len(raw) == 0 {
		return config
	}
	var row struct {
		FixTruncatedJSON *bool `json:"fixTruncatedJson"`
		FixSSEFormat     *bool `json:"fixSseFormat"`
		FixEncoding      *bool `json:"fixEncoding"`
		MaxJSONDepth     *int  `json:"maxJsonDepth"`
		MaxFixSize       *int  `json:"maxFixSize"`
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return config
	}
	if err := json.Unmarshal(raw, &row); err != nil {
		// 解析不了就按出厂值：与 Node 的 `?? DEFAULT_CONFIG` 同向（读不到配置不阻断响应路径）。
		return config
	}
	if row.FixTruncatedJSON != nil {
		config.FixTruncatedJSON = *row.FixTruncatedJSON
	}
	if row.FixSSEFormat != nil {
		config.FixSSEFormat = *row.FixSSEFormat
	}
	if row.FixEncoding != nil {
		config.FixEncoding = *row.FixEncoding
	}
	if row.MaxJSONDepth != nil {
		config.MaxJSONDepth = *row.MaxJSONDepth
	}
	if row.MaxFixSize != nil {
		config.MaxFixSize = *row.MaxFixSize
	}
	return config
}
