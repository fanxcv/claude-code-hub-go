import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, test, vi } from "vitest";
import type { SpecialSetting } from "@/types/special-settings";
import { ProtocolConversionDisplay } from "./protocol-conversion-display";

vi.mock("next-intl", () => ({
  useTranslations: () => (key: string) => key,
}));

vi.mock("@/components/ui/tooltip", () => ({
  TooltipProvider: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  Tooltip: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));

describe("ProtocolConversionDisplay", () => {
  test("无记录时留空（原生直通请求不显示任何标记）", () => {
    const html = renderToStaticMarkup(<ProtocolConversionDisplay specialSettings={null} />);

    expect(html).toBe("");
    expect(html).not.toContain('data-slot="protocol-conversion"');
  });

  test("有其它审计但无协议转换记录时不渲染", () => {
    const html = renderToStaticMarkup(
      <ProtocolConversionDisplay
        specialSettings={[
          { type: "codex_reasoning_effort", scope: "request", hit: true, effort: "high" },
        ]}
      />
    );

    expect(html).toBe("");
  });

  test("有协议转换记录时显示徽章并给出实际协议对", () => {
    const html = renderToStaticMarkup(
      <ProtocolConversionDisplay
        specialSettings={[
          {
            type: "protocol_conversion",
            scope: "request",
            hit: true,
            clientProtocol: "openai-responses",
            targetProtocol: "openai-chat",
          },
        ]}
      />
    );

    expect(html).toContain('data-slot="protocol-conversion"');
    expect(html).toContain(">badge<");
    expect(html).toContain(">tooltip<");
    expect(html).toContain("openai-responses");
    expect(html).toContain("openai-chat");
    expect(html).toContain("lucide-arrow-right");
    expect(html).toContain("relative z-20");
  });

  test("协议名为空白的脏记录不渲染", () => {
    const html = renderToStaticMarkup(
      <ProtocolConversionDisplay
        specialSettings={[
          {
            type: "protocol_conversion",
            scope: "request",
            hit: true,
            clientProtocol: "  ",
            targetProtocol: "openai-chat",
          },
        ]}
      />
    );

    expect(html).toBe("");
  });
});

describe("ProtocolConversionDisplay 失败态", () => {
  test("转换失败必须画失败徽章而不是留空（否则与「没转换」无法区分）", () => {
    const html = renderToStaticMarkup(
      <ProtocolConversionDisplay
        specialSettings={[
          {
            type: "protocol_conversion_failed",
            scope: "request",
            hit: true,
            clientProtocol: "anthropic-messages",
            targetProtocol: "openai-chat",
            phase: "body_conversion",
            reason: "forward: 请求正文不是合法 JSON 对象: unexpected end of JSON input",
            fallback: true,
          },
        ]}
      />
    );

    expect(html).not.toBe("");
    expect(html).toContain('data-slot="protocol-conversion-failed"');
    expect(html).toContain(">failedBadge<");
    // 协议对、阶段、原因三者都要在 tooltip 里，缺一项就定位不到问题。
    expect(html).toContain("anthropic-messages");
    expect(html).toContain("openai-chat");
    expect(html).toContain(">phase.body_conversion<");
    expect(html).toContain("unexpected end of JSON input");
    expect(html).toContain(">fallbackNotice<");
    // 失败与成功是互斥状态：不得同时画成功徽章。
    expect(html).not.toContain('data-slot="protocol-conversion"');
  });

  test("未知阶段按「未记录」展示，不编造阶段名", () => {
    // 故意用库里可能出现的未知值：读取侧必须屏蔽它（转成 null），而 TS 字面量类型会拒绝，
    // 故这里显式双重断言——本用例恰恰要验证「类型系统外的脏数据」被顶住。
    const dirty = {
      type: "protocol_conversion_failed",
      scope: "request",
      hit: true,
      clientProtocol: "anthropic-messages",
      targetProtocol: null,
      phase: "something_new",
      reason: null,
      fallback: true,
    } as unknown as SpecialSetting;
    const html = renderToStaticMarkup(<ProtocolConversionDisplay specialSettings={[dirty]} />);

    expect(html).toContain('data-slot="protocol-conversion-failed"');
    expect(html).toContain(">phase.unknown<");
    expect(html).toContain(">reasonUnknown<");
    // 目标协议缺失时不画半截协议对。
    expect(html).not.toContain("lucide-arrow-right");
  });

  test("两个都缺失时按未转换处理（陈旧数据的保守取值）", () => {
    const html = renderToStaticMarkup(<ProtocolConversionDisplay specialSettings={[]} />);
    expect(html).toBe("");
  });
});
