/**
 * 专测**历史损失条目（无 `severity` 字段）的档位推导表**与 Go 的等价性。
 *
 * 为何要逐条钉：`severity` 与三档合计都是后加的字段，库里已有 236 行历史条目没有它，
 * 推导表是这些行的唯一档位来源。一旦此处与 Go 的 `convert.LossSeverityOf`
 * （`go/internal/convert/hub.go`）判档不同——例如按族前缀放宽成 `thinking` / `store`——
 * 未列出的新名（`thinking.new`、`store.new`）就会被算成降级/信息档，改写档合计为 0，
 * **徽章不再画出来**：真损失被降噪吞掉，而在没有徽章的行上用户永远不会去查。
 */
import { describe, expect, test } from "vitest";
import type {
  ConversionLossAction,
  ConversionLossSeverity,
  SpecialSetting,
} from "@/types/special-settings";
import { getProtocolConversionLoss } from "@/lib/utils/protocol-conversion";

/** 读一组历史损失（只给 capability + action，不给 severity）的档位。 */
function legacySeverity(capability: string, action: ConversionLossAction): ConversionLossSeverity {
  const setting: SpecialSetting = {
    type: "protocol_conversion_loss",
    scope: "request",
    hit: true,
    total: 1,
    groups: [{ capability, action, count: 1 }],
  };

  const group = getProtocolConversionLoss([setting])?.groups[0];
  if (!group) {
    throw new Error("夹具未读出分组：本用例已失效，须先查 getProtocolConversionLoss");
  }

  return group.severity;
}

describe("历史损失条目的档位推导", () => {
  test("精确名集合与 Go 的 LossSeverityOf 同口径", () => {
    const table: ReadonlyArray<readonly [string, ConversionLossAction, ConversionLossSeverity]> = [
      // Go：LossStoreFlag / LossPromptCacheKey → info
      ["store", "dropped", "info"],
      ["prompt_cache_key", "dropped", "info"],
      // Go：LossThinkingSignature / LossThinkingDerived / LossReasoningReplay / LossCacheControl → degrade
      ["thinking.signature", "dropped", "degrade"],
      ["thinking.derived", "dropped", "degrade"],
      ["reasoning.replay", "dropped", "degrade"],
      ["cache_control", "dropped", "degrade"],
      // Go：LossThinkingBlock 连 action 一起看
      ["thinking.block", "dropped", "rewrite"],
      ["thinking.block", "rewritten", "rewrite"],
      ["thinking.block", "downgraded", "degrade"],
      // Go：default → rewrite（含细分名与未收录的新能力）
      ["unknown_field", "dropped", "rewrite"],
      ["unknown_field.tool", "dropped", "rewrite"],
      ["image", "rewritten", "rewrite"],
      ["top_k", "dropped", "rewrite"],
      ["brand.new.capability", "dropped", "rewrite"],
    ];

    for (const [capability, action, expected] of table) {
      expect(legacySeverity(capability, action), `${capability} × ${action}`).toBe(expected);
    }
  });

  test("未列出的新名不按族前缀放宽，一律改写档", () => {
    // 这些名字都是「既有族的续写名」：按前缀放宽会判成降级/信息档，而 Go 只认精确常量 ⇒ rewrite。
    const familyLikeNames = [
      "thinking.new",
      "store.new",
      "cache_control.v2",
      "reasoning.replay.v2",
      "prompt_cache_key.new",
    ];

    for (const capability of familyLikeNames) {
      expect(legacySeverity(capability, "dropped"), capability).toBe("rewrite");
    }
  });

  test("未列出的新名计入改写档合计，徽章口径因此不为 0", () => {
    const setting: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      total: 3,
      groups: [
        { capability: "thinking.new", action: "dropped", count: 2 },
        { capability: "store", action: "dropped", count: 1 },
      ],
    };

    const loss = getProtocolConversionLoss([setting]);

    expect(loss?.rewriteTotal, "未列出的新名必须进改写档，否则徽章不画").toBe(2);
    expect(loss?.infoTotal).toBe(1);
  });

  test("thinking.block 的未知动作归改写档（Go 同：除 downgraded 外皆 rewrite）", () => {
    expect(legacySeverity("thinking.block", "brand_new_action" as ConversionLossAction)).toBe(
      "rewrite"
    );
  });

  test("后端给了 severity 时以它为准，推导表不覆盖", () => {
    const setting: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      total: 1,
      groups: [{ capability: "store", action: "dropped", count: 1, severity: "rewrite" }],
    };

    expect(getProtocolConversionLoss([setting])?.groups[0]?.severity).toBe("rewrite");
  });
});
