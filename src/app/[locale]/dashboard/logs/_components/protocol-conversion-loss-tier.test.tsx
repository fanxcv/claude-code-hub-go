/**
 * 专测列表徽章的**降噪口径**：只数改写档、改写档为 0 时不画徽章。
 *
 * 为何要单独钉：徽章数字决定用户会不会点开。
 *  - 全口径（旧行为）：长会话里 thinking 降级每回合一条，能把几乎每一行都顶到两位数，
 *    真损失（如采样参数被丢）反而被淹没；
 *  - 只数改写档：降级与信息档仍按三档列在 tooltip 里，信息不少，只是不进列表数字。
 *
 * 库里已有 236 行**没有 severity 字段**的历史条目（severity 与三档合计都是后加的），
 * 故每组口径都要同时验「新契约」与「历史条目」两种形状。
 */
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, test, vi } from "vitest";
import type { ConversionLossAction, SpecialSetting } from "@/types/special-settings";
import { getProtocolConversionLoss } from "@/lib/utils/protocol-conversion";
import { ProtocolConversionDisplay } from "./protocol-conversion-display";

// 把参数拼进返回值：徽章数字（{count}）必须断言到具体值，只验键名在场会被口径回归骗过。
vi.mock("next-intl", () => ({
  useTranslations:
    () =>
    (key: string, params?: Record<string, unknown>): string =>
      params
        ? `${key}(${Object.entries(params)
            .map(([name, value]) => `${name}=${String(value)}`)
            .join(",")})`
        : key,
}));

// tooltip 内容经 mock 直接进静态标记（真 Radix tooltip 只在悬停时入 portal，静态渲染取不到）。
vi.mock("@/components/ui/tooltip", () => ({
  TooltipProvider: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  Tooltip: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));

/** 成功徽章在场：列表行确实转换过协议（降噪不该把它一起藏掉）。 */
const converted: SpecialSetting = {
  type: "protocol_conversion",
  scope: "request",
  hit: true,
  clientProtocol: "anthropic-messages",
  targetProtocol: "openai-chat",
};

function render(...settings: SpecialSetting[]): string {
  return renderToStaticMarkup(<ProtocolConversionDisplay specialSettings={settings} />);
}

describe("损失徽章降噪口径", () => {
  /** 夹具一：新契约（后端已给 severity 与三档合计）。 */
  const newContract: SpecialSetting = {
    type: "protocol_conversion_loss",
    scope: "request",
    hit: true,
    clientProtocol: "openai-responses",
    targetProtocol: "openai-chat",
    total: 200,
    rewriteTotal: 35,
    degradeTotal: 164,
    infoTotal: 1,
    groups: [
      // 这一行摹写**存量行**（2026-09-22 之前落库）：image/rewritten 当时落的是 rewrite 档，
      // 而读侧优先信任落库的 severity ⇒ 存量行仍按旧档位显示（新行由后端按现行表判档）。
      { capability: "image", action: "rewritten", count: 34, severity: "rewrite" },
      {
        capability: "unknown_field.image_without_url",
        action: "dropped",
        count: 1,
        severity: "rewrite",
      },
      { capability: "thinking.block", action: "downgraded", count: 164, severity: "degrade" },
      { capability: "store", action: "dropped", count: 1, severity: "info" },
    ],
  };

  test("新契约：徽章取改写档合计，而不是全部条目数", () => {
    const html = render(converted, newContract);

    expect(html).toContain('data-slot="protocol-conversion-loss"');
    expect(html).toContain("lossBadge(count=35)");
    // 旧口径会把 200（含 164 条思考降级）画到列表上，正是本次要杜绝的噪声。
    expect(html).not.toContain("lossBadge(count=200)");
  });

  test("新契约：三档分组列示，降级与信息档的明细不因降噪而消失", () => {
    const html = render(converted, newContract);

    expect(html).toContain("lossTier.rewrite");
    expect(html).toContain("lossTier.degrade");
    expect(html).toContain("lossTier.info");
    expect(html).toContain("image");
    expect(html).toContain("thinking.block");
    expect(html).toContain("store");
    // 三档小计仍要给出（改写 35 / 降级 164 / 信息 1）。
    expect(html).toContain("lossTier.rewrite：</span>35");
    expect(html).toContain("lossTier.degrade：</span>164");
    expect(html).toContain("lossTier.info：</span>1");
  });

  test("历史条目：有明细但无三档合计时按分组现算", () => {
    // 改写档代表用 top_k（采样参数被丢，后端 LossSeverityOf 判 rewrite）：
    // 不用 image/rewritten——它自 2026-09-22 起归信息档，拿它当改写档代表会让本用例失去对象。
    const legacyMixed: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "openai-responses",
      targetProtocol: "openai-chat",
      total: 13,
      groups: [
        { capability: "top_k", action: "dropped", count: 3 },
        { capability: "thinking.block", action: "downgraded", count: 9 },
        { capability: "store", action: "dropped", count: 1 },
      ],
    };
    const html = render(converted, legacyMixed);

    expect(html).toContain("lossBadge(count=3)");
    expect(html).toContain("lossTier.degrade：</span>9");
    expect(html).toContain("lossTier.info：</span>1");
  });

  test("历史条目：image 的 rewritten 是表示归一，归信息档、不进列表徽章", () => {
    const legacyImageNormalization: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "openai-responses",
      targetProtocol: "openai-chat",
      total: 3,
      groups: [{ capability: "image", action: "rewritten", count: 3 }],
    };
    const html = render(converted, legacyImageNormalization);

    // data URL → base64 的往返不改送达内容，故不算「内容被改」：列表徽章不该计它
    // （生产实证：算进改写档会让「几乎每条带图的转换」都挂徽章，真损失反被淹没）。
    expect(html).not.toContain('data-slot="protocol-conversion-loss"');
    // 账仍在：该组落信息档，明细（含 severity）随 special settings 原样进详情面。
    const loss = getProtocolConversionLoss([legacyImageNormalization]);
    expect(loss?.rewriteTotal).toBe(0);
    expect(loss?.infoTotal).toBe(3);
    expect(loss?.groups[0]?.severity).toBe("info");
  });

  test("历史条目：只有降级与信息档时不画徽章（降噪的主场景）", () => {
    const legacyDegradeOnly: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 129,
      groups: [
        { capability: "thinking.block", action: "downgraded", count: 128 },
        { capability: "store", action: "dropped", count: 1 },
      ],
    };
    const html = render(converted, legacyDegradeOnly);

    expect(html).not.toContain('data-slot="protocol-conversion-loss"');
    // 成功徽章不受影响：该行确实转换过协议，只是没有值得占列表位置的改写。
    expect(html).toContain('data-slot="protocol-conversion"');
  });

  test("历史条目：thinking.block 被丢属改写档，与同能力的降级分开算，不被降噪吞掉", () => {
    const legacyThinkingBlockDropped: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 129,
      groups: [
        { capability: "thinking.block", action: "dropped", count: 1 },
        { capability: "thinking.block", action: "downgraded", count: 128 },
      ],
    };
    const html = render(converted, legacyThinkingBlockDropped);

    // 思考整块被删是**内容变化**（后端 LossSeverityOf 同口径）；若把 thinking 整族按前缀判成降级，
    // 这枚徽章会消失——正是「降噪吞掉真损失」的回归形态。
    expect(html).toContain('data-slot="protocol-conversion-loss"');
    expect(html).toContain("lossBadge(count=1)");
    expect(html).toContain("lossTier.rewrite：</span>1");
    expect(html).toContain("lossTier.degrade：</span>128");
  });

  test("历史条目：thinking.block 的未知动作归改写档（宁可多画，也不漏报）", () => {
    const legacyThinkingBlockUnknownAction: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 2,
      // 动作取比联合类型更新的值：读取侧**故意**保留未知动作（剔掉整组会让损失被少报），
      // 这里如实摹写「库里的行来自更新的后端」这一形状。
      groups: [
        {
          capability: "thinking.block",
          action: "brand_new_action" as ConversionLossAction,
          count: 2,
        },
      ],
    };
    const html = render(converted, legacyThinkingBlockUnknownAction);

    expect(html).toContain("lossBadge(count=2)");
  });

  test("历史条目：未列出的 thinking 续写名归改写档（前缀放宽会把徽章藏掉）", () => {
    const legacyThinkingNew: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 2,
      // Go 的 `LossSeverityOf` 只认精确常量，`thinking.new` 落到 default 分支（改写档）。
      // 此处若按 `thinking` 前缀匹配，这 2 项会被算成降级档 ⇒ 改写档为 0 ⇒ 徽章不画。
      groups: [{ capability: "thinking.new", action: "dropped", count: 2 }],
    };
    const html = render(converted, legacyThinkingNew);

    expect(html).toContain('data-slot="protocol-conversion-loss"');
    expect(html).toContain("lossBadge(count=2)");
    expect(html).toContain("lossTier.rewrite：</span>2");
    // 它不该被归进降级档：归错档正是本用例要防的分歧。
    expect(html).not.toContain("lossTier.degrade");
  });

  test("历史条目：未列出的 store 续写名归改写档（前缀放宽会归成信息档）", () => {
    const legacyStoreNew: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 1,
      groups: [{ capability: "store.new", action: "dropped", count: 1 }],
    };
    const html = render(converted, legacyStoreNew);

    expect(html).toContain('data-slot="protocol-conversion-loss"');
    expect(html).toContain("lossBadge(count=1)");
    expect(html).toContain("lossTier.rewrite：</span>1");
    expect(html).not.toContain("lossTier.info");
  });

  test("历史条目：未列出的 cache_control 续写名归改写档", () => {
    const legacyCacheControlV2: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 3,
      groups: [{ capability: "cache_control.v2", action: "dropped", count: 3 }],
    };
    const html = render(converted, legacyCacheControlV2);

    expect(html).toContain('data-slot="protocol-conversion-loss"');
    expect(html).toContain("lossBadge(count=3)");
    expect(html).toContain("lossTier.rewrite：</span>3");
    expect(html).not.toContain("lossTier.degrade");
  });

  test("历史条目：参数被丢（top_k）仍进列表数字，不被降噪吞掉", () => {
    const legacyTopK: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 1,
      groups: [{ capability: "top_k", action: "dropped", count: 1 }],
    };
    const html = render(converted, legacyTopK);

    expect(html).toContain('data-slot="protocol-conversion-loss"');
    expect(html).toContain("lossBadge(count=1)");
  });

  test("历史条目：unknown_field 的细分名仍归改写档", () => {
    const legacyUnknownField: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 2,
      groups: [{ capability: "unknown_field.role_rewrite", action: "dropped", count: 2 }],
    };
    const html = render(converted, legacyUnknownField);

    expect(html).toContain("lossBadge(count=2)");
  });

  test("未收录进推导表的新能力归改写档（宁可多画一个徽章，也不漏报真损失）", () => {
    const unknownCapability: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 1,
      groups: [{ capability: "brand.new.capability", action: "dropped", count: 1 }],
    };
    const html = render(converted, unknownCapability);

    expect(html).toContain("lossBadge(count=1)");
  });

  test("只知总数、没有明细时计入改写档（整条损失不从列表消失）", () => {
    const totalsOnly: SpecialSetting = {
      type: "protocol_conversion_loss",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: "openai-chat",
      total: 47,
      groups: [],
    };
    const html = render(converted, totalsOnly);

    expect(html).toContain("lossBadge(count=47)");
  });
});
