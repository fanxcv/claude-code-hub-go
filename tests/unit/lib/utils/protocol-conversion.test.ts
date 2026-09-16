/**
 * 专测**历史损失条目（无 `severity` 字段）的档位推导**与 Go 真源的等价性。
 *
 * 为何要逐条钉：`severity` 与三档合计都是后加的字段，库里已有 236 行历史条目没有它，
 * 推导表是这些行的唯一档位来源。判档一旦与 Go 的 `convert.LossSeverityOf`
 * （`go/internal/convert/hub.go`）不同——例如按族前缀放宽成 `thinking` / `store`——未列出的
 * 新名（`thinking.new`、`store.new`）就会被算成降级/信息档，改写档合计为 0，**徽章不再画出来**：
 * 真损失被降噪吞掉，而在没有徽章的行上用户永远不会去查。
 *
 * 两张表不再手抄：本用例的期望值来自生成物 `loss-severity.gen.ts`（由 Go 真源渲染），
 * 「生成物 vs 真源」由 `go test ./internal/convert/` 逐字节钉住，此处只钉「消费侧 vs 生成物」。
 */
import { describe, expect, test } from "vitest";
import type {
  ConversionLossAction,
  ConversionLossSeverity,
  SpecialSetting,
} from "@/types/special-settings";
import {
  LOSS_SEVERITY_BY_CAPABILITY,
  LOSS_SEVERITY_BY_CAPABILITY_ACTION,
} from "@/lib/utils/loss-severity.gen";
import { LOSS_ACTIONS, getProtocolConversionLoss } from "@/lib/utils/protocol-conversion";

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
  test("生成表里每一项都在本侧同档推导（含 thinking.block 的动作例外）", () => {
    const capabilities = Object.entries(LOSS_SEVERITY_BY_CAPABILITY);
    // 空表会让下面的循环空转通过，而「生成物坏掉」正是最该红的一刻。
    expect(
      capabilities.length,
      "生成表为空：先查 Go 的档位表与 go/cmd/lossseverity"
    ).toBeGreaterThan(0);

    for (const [capability, severity] of capabilities) {
      expect(legacySeverity(capability, "dropped"), capability).toBe(severity);
    }

    const actionDependent = Object.entries(LOSS_SEVERITY_BY_CAPABILITY_ACTION);
    expect(
      actionDependent.length,
      "动作例外表为空：thinking.block 的丢/降两态必须在此"
    ).toBeGreaterThan(0);

    for (const [capability, byAction] of actionDependent) {
      for (const [action, severity] of Object.entries(byAction)) {
        expect(
          legacySeverity(capability, action as ConversionLossAction),
          `${capability} × ${action}`
        ).toBe(severity);
      }
    }
  });

  test("动作例外表未覆盖的动作退回能力名表，仍无命中即改写档（与 Go 的回落顺序同构）", () => {
    for (const [capability, byAction] of Object.entries(LOSS_SEVERITY_BY_CAPABILITY_ACTION)) {
      const unmappedActions = LOSS_ACTIONS.filter((action) => !(action in byAction));
      expect(
        unmappedActions.length,
        `${capability} 的动作例外表覆盖了全部已知动作，本用例失去意义`
      ).toBeGreaterThan(0);

      const fallback = LOSS_SEVERITY_BY_CAPABILITY[capability] ?? "rewrite";
      for (const action of unmappedActions) {
        expect(
          legacySeverity(capability, action),
          `${capability} × ${action} 应退回 ${fallback}`
        ).toBe(fallback);
      }
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
