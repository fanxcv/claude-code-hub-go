import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, test, vi } from "vitest";
import { ThinkingEffortDisplay } from "./thinking-effort-display";

vi.mock("next-intl", () => ({
  useTranslations: () => (key: string) => key,
}));

vi.mock("@/components/ui/tooltip", () => ({
  TooltipProvider: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  Tooltip: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));

describe("ThinkingEffortDisplay", () => {
  test("未记录思考强度时显示占位符", () => {
    const html = renderToStaticMarkup(<ThinkingEffortDisplay specialSettings={null} />);

    expect(html).toContain(">-</span>");
    expect(html).not.toContain('data-slot="thinking-effort"');
  });

  test("显示 Codex 请求中的思考强度", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "codex_reasoning_effort",
            scope: "request",
            hit: true,
            effort: "high",
          },
        ]}
      />
    );

    expect(html).toContain('data-slot="thinking-effort"');
    expect(html).toContain("high");
    expect(html).toContain("reasoningEffort.tooltip");
    expect(html).not.toContain("overridden");
  });

  test("协议转换改写了强度时展示「请求值 → 转发值」与协议对", () => {
    // 用户口径（2026-09-13）：这一列要能回答「转换之后、即将发给供应商的是哪个值」，
    // 并据此判断转换是否正确处理了该字段——故 ① 两侧都要显示；② 转发值来自探针（转换器产物）。
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "anthropic_effort",
            scope: "request",
            hit: true,
            effort: "high",
          },
          {
            type: "thinking_effort_forwarded",
            scope: "request",
            hit: true,
            requestedEffort: "high",
            forwardedEffort: "medium",
            dropped: false,
            converted: true,
            clientProtocol: "anthropic-messages",
            targetProtocol: "openai-chat",
            requestedField: "output_config.effort",
            forwardedField: "reasoning_effort",
          },
        ]}
      />
    );

    expect(html).toContain('data-slot="thinking-effort"');
    expect(html).toContain("high");
    expect(html).toContain("medium");
    expect(html).toContain("lucide-arrow-right");
    expect(html).toContain("anthropic-messages");
    expect(html).toContain("openai-chat");
  });

  test("协议转换保留了原值时只显示一个值（不画多余箭头）", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "anthropic_effort",
            scope: "request",
            hit: true,
            effort: "high",
          },
          {
            type: "thinking_effort_forwarded",
            scope: "request",
            hit: true,
            requestedEffort: "high",
            forwardedEffort: "high",
            dropped: false,
            converted: true,
            clientProtocol: "anthropic-messages",
            targetProtocol: "openai-chat",
          },
        ]}
      />
    );

    expect(html).toContain("high");
    // 只断言**行内**没有箭头：工具提示里的协议对（client → target）本来就有一个同图标箭头，
    // 两者靠类名区分（行内那个带 `text-muted-foreground`）。
    expect(html).not.toContain("shrink-0 text-muted-foreground");
    expect(html).not.toContain('data-slot="thinking-effort-dropped"');
  });

  test("转换把强度丢了时显式标记丢弃，不得静默成占位符", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "codex_reasoning_effort",
            scope: "request",
            hit: true,
            effort: "high",
          },
          {
            type: "thinking_effort_forwarded",
            scope: "request",
            hit: true,
            requestedEffort: "high",
            forwardedEffort: null,
            dropped: true,
            converted: true,
            clientProtocol: "openai-responses",
            targetProtocol: "openai-chat",
          },
        ]}
      />
    );

    expect(html).toContain("high");
    expect(html).toContain('data-slot="thinking-effort-dropped"');
    expect(html).toContain("effortConversion.dropped");
    expect(html).toContain("effortConversion.droppedTooltip");
    expect(html).not.toContain(">-</span>");
  });

  test("只有探针（客户端审计缺失）时仍显示转发值", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "thinking_effort_forwarded",
            scope: "request",
            hit: true,
            requestedEffort: null,
            forwardedEffort: "low",
            dropped: false,
            converted: true,
            clientProtocol: "anthropic-messages",
            targetProtocol: "openai-chat",
          },
        ]}
      />
    );

    expect(html).toContain("low");
    expect(html).not.toContain(">-</span>");
  });

  test("原生直通（探针 converted=false）不标丢弃，也不多画箭头", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "anthropic_effort",
            scope: "request",
            hit: true,
            effort: "medium",
          },
          {
            type: "thinking_effort_forwarded",
            scope: "request",
            hit: true,
            requestedEffort: "medium",
            forwardedEffort: "medium",
            dropped: false,
            converted: false,
            clientProtocol: "anthropic-messages",
            targetProtocol: "anthropic-messages",
          },
        ]}
      />
    );

    expect(html).toContain("medium");
    expect(html).not.toContain('data-slot="thinking-effort-dropped"');
    expect(html).not.toContain("shrink-0 text-muted-foreground");
  });

  test("供应商覆写 Codex 强度时显示请求值和实际值", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "codex_reasoning_effort",
            scope: "request",
            hit: true,
            effort: "low",
          },
          {
            type: "provider_parameter_override",
            scope: "provider",
            providerId: 1,
            providerName: "Codex",
            providerType: "codex",
            hit: true,
            changed: true,
            changes: [{ path: "reasoning.effort", before: "low", after: "max", changed: true }],
          },
        ]}
      />
    );

    expect(html).toContain("low");
    expect(html).toContain("max");
    expect(html).toContain("reasoningEffort.overridden");
    expect(html).toContain("lucide-arrow-right");
    expect(html).toContain("relative z-20");
  });

  test("显示 Anthropic 请求中的思考强度", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "anthropic_effort",
            scope: "request",
            hit: true,
            effort: "medium",
          },
        ]}
      />
    );

    expect(html).toContain('data-slot="thinking-effort"');
    expect(html).toContain("medium");
    expect(html).toContain("effort.tooltip");
    expect(html).not.toContain("overridden");
  });

  test("供应商覆写 Anthropic 强度时显示请求值和实际值", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "anthropic_effort",
            scope: "request",
            hit: true,
            effort: "medium",
          },
          {
            type: "provider_parameter_override",
            scope: "provider",
            providerId: 2,
            providerName: "Anthropic",
            providerType: "claude",
            hit: true,
            changed: true,
            changes: [
              { path: "output_config.effort", before: "medium", after: "high", changed: true },
            ],
          },
        ]}
      />
    );

    expect(html).toContain("medium");
    expect(html).toContain("high");
    expect(html).toContain("effort.overridden");
    expect(html).toContain("lucide-arrow-right");
  });

  test("供应商剥离 Anthropic 强度时仅显示请求值与覆写说明", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "anthropic_effort",
            scope: "request",
            hit: true,
            effort: "medium",
          },
          {
            type: "provider_parameter_override",
            scope: "provider",
            providerId: 2,
            providerName: "Anthropic",
            providerType: "claude",
            hit: true,
            changed: true,
            changes: [
              { path: "output_config.effort", before: "medium", after: null, changed: true },
            ],
          },
        ]}
      />
    );

    expect(html).toContain("medium");
    expect(html).toContain("effort.overridden");
    expect(html).not.toContain("lucide-arrow-right");
  });

  test("同时存在两种审计时优先展示 Codex 强度", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "anthropic_effort",
            scope: "request",
            hit: true,
            effort: "medium",
          },
          {
            type: "codex_reasoning_effort",
            scope: "request",
            hit: true,
            effort: "xhigh",
          },
        ]}
      />
    );

    expect(html).toContain("xhigh");
    expect(html).toContain("reasoningEffort.tooltip");
    expect(html).not.toContain(">medium<");
  });

  test("显示 OpenAI chat/completions 请求中的思考强度", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "openai_reasoning_effort",
            scope: "request",
            hit: true,
            effort: "max",
            source: "reasoning_effort",
          },
        ]}
      />
    );

    expect(html).toContain('data-slot="thinking-effort"');
    expect(html).toContain("max");
    expect(html).toContain("reasoningEffortOpenai.tooltip");
    expect(html).not.toContain("overridden");
  });

  test("OpenAI 与 Codex 审计并存时优先展示 Codex 强度", () => {
    const html = renderToStaticMarkup(
      <ThinkingEffortDisplay
        specialSettings={[
          {
            type: "openai_reasoning_effort",
            scope: "request",
            hit: true,
            effort: "max",
            source: "reasoning_effort",
          },
          {
            type: "codex_reasoning_effort",
            scope: "request",
            hit: true,
            effort: "high",
          },
        ]}
      />
    );

    expect(html).toContain("high");
    expect(html).toContain("reasoningEffort.tooltip");
    expect(html).not.toContain(">max<");
  });
});
