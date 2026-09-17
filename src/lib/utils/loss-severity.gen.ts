// AUTO-GENERATED - DO NOT EDIT
//
// 界面侧的损失档位表：真源是 Go 的 go/internal/convert/hub.go（lossSeverityByCapability /
// lossSeverityByCapabilityAction），由 go/cmd/lossseverity 渲染而成。
//
// 刷新：
//
//	cd go && go run ./cmd/lossseverity -out ../src/lib/utils/loss-severity.gen.ts
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
  cache_control: "degrade",
  prompt_cache_key: "info",
  "reasoning.replay": "degrade",
  "reasoning.summary": "info",
  store: "info",
  "thinking.derived": "degrade",
  "thinking.encrypted": "degrade",
  "thinking.signature": "degrade",
  "tool.strict": "degrade",
};

/** (能力, 动作) -> 档位；同一能力的其它动作退回上表，仍无命中则 rewrite。 */
export const LOSS_SEVERITY_BY_CAPABILITY_ACTION: Readonly<
  Record<string, Readonly<Partial<Record<string, ConversionLossSeverity>>>>
> = {
  "thinking.block": {
    downgraded: "degrade",
  },
};
