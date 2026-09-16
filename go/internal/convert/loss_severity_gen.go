package convert

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// LossSeverityGeneratedPath 是界面侧档位表的仓库相对路径。
//
// 由 go/cmd/lossseverity 写入，由 loss_severity_gen_test.go 逐字节比对；两处都不许手改生成物。
const LossSeverityGeneratedPath = "src/lib/utils/loss-severity.gen.ts"

// RenderLossSeverityTS 渲染界面侧消费的档位表（生成物的完整内容）。
//
// 为何把渲染器放在判档同一个包里：生成物必须与 LossSeverityOf 同源，否则「代码生成」只是把
// 手工分叉换成生成器分叉。故本函数只读 lossSeverityByCapability / lossSeverityByCapabilityAction，
// 两张表之外的任何东西都不进生成物；新增或调整档位只改那两张表。
//
// 输出形态与 biome 的格式化结果一致（双引号、两空格缩进、末尾逗号）：生成物在 src/ 下，会走
// `bun run lint`（biome check 含格式检查），故渲染结果必须直接是仓库的规范形态，不能靠事后手改。
func RenderLossSeverityTS() string {
	var builder strings.Builder
	builder.WriteString(`// AUTO-GENERATED - DO NOT EDIT
//
// 界面侧的损失档位表：真源是 Go 的 go/internal/convert/hub.go（lossSeverityByCapability /
// lossSeverityByCapabilityAction），由 go/cmd/lossseverity 渲染而成。
//
// 刷新：
//
//	cd go && go run ./cmd/lossseverity -out ../` + LossSeverityGeneratedPath + `
//
// 校验：go test ./internal/convert/ 的 TestLossSeverityGeneratedTableIsUpToDate 会逐字节比对；
// 改了 Go 的档位表却忘了重新生成，该用例即转红。
//
// 为何必须生成：库里已有不带 severity 字段的历史损失条目，它们的档位只能按 (能力, 动作) 推导，
// 而判档真源在 Go。手抄一份表曾分叉（一侧按族前缀、一侧按精确名），后果是该显示的徽章整枚不画，
// 真损失被降噪吞掉。
import type { ConversionLossSeverity } from "@/types/special-settings";

/** 能力 -> 档位；表中未列出的能力一律 rewrite（与 Go 的 LossSeverityOf 同口径）。 */
export const LOSS_SEVERITY_BY_CAPABILITY: Readonly<
  Partial<Record<string, ConversionLossSeverity>>
> = {
`)

	capabilities := make([]string, 0, len(lossSeverityByCapability))
	for capability := range lossSeverityByCapability {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	for _, capability := range capabilities {
		fmt.Fprintf(&builder, "  %s: %q,\n", tsPropertyKey(capability), lossSeverityByCapability[capability])
	}

	builder.WriteString(`};

/** (能力, 动作) -> 档位；同一能力的其它动作退回上表，仍无命中则 rewrite。 */
export const LOSS_SEVERITY_BY_CAPABILITY_ACTION: Readonly<
  Record<string, Readonly<Partial<Record<string, ConversionLossSeverity>>>>
> = {
`)

	for _, capability := range sortedActionCapabilities() {
		fmt.Fprintf(&builder, "  %s: {\n", tsPropertyKey(capability))
		actions := lossSeverityByCapabilityAction[capability]
		names := make([]string, 0, len(actions))
		for action := range actions {
			names = append(names, string(action))
		}
		sort.Strings(names)
		for _, action := range names {
			fmt.Fprintf(&builder, "    %s: %q,\n", tsPropertyKey(action), actions[LossAction(action)])
		}
		builder.WriteString("  },\n")
	}

	builder.WriteString("};\n")
	return builder.String()
}

// tsPropertyKey 按 TS 属性名的写法渲染键：合法标识符不加引号，其余加引号。
//
// 为何要费这一步：生成物在 src/ 下，会走 `bun run lint`（biome check 含格式检查），而 biome
// 会去掉标识符式键的引号（`store: "info"`）、保留点号键的引号（`"thinking.block": {`）。生成物
// 要么直接是格式化后的形态，要么就得把它排除在 lint 之外——后者会给格式门禁留个洞，故取前者。
func tsPropertyKey(name string) string {
	if isTSIdentifier(name) {
		return name
	}
	return strconv.Quote(name)
}

// isTSIdentifier 判定 name 能否不加引号地作为 TS 属性名。
func isTSIdentifier(name string) bool {
	for index, char := range name {
		isLetter := char == '_' || char == '$' || unicode.IsLetter(char)
		if index == 0 {
			if !isLetter {
				return false
			}
			continue
		}
		if !isLetter && !unicode.IsDigit(char) {
			return false
		}
	}
	return name != ""
}

// sortedActionCapabilities 返回按名字排序的「按动作分档」能力名，保证生成物逐字节可复现。
func sortedActionCapabilities() []string {
	capabilities := make([]string, 0, len(lossSeverityByCapabilityAction))
	for capability := range lossSeverityByCapabilityAction {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	return capabilities
}
