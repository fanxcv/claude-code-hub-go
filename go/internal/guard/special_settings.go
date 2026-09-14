package guard

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// buildRequestSpecialSettings 产出建行时写入的客户端侧审计（`message_request.special_settings`）。
//
// 提取规则全部在 `internal/specialsettings`（唯一真源，勿在本包复制一份）：数据面有建行与终态
// 两个写入时刻，两侧必须用同一套规则，否则同一字段会出现两种口径。
//
// 返回 nil 表示「本次没有特殊设置」——与 Node 的 `getSpecialSettings()` 返回 `null` 同义，
// 该列写 NULL（不要写空数组：两态在读取侧归一后不同）。
func buildRequestSpecialSettings(
	body map[string]any,
	format convert.ClientFormat,
	endpoint string,
) []byte {
	return specialsettings.RequestEntries(body, format, endpoint)
}
