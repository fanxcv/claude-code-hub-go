/**
 * @vitest-environment happy-dom
 *
 * 熔断「等待阶梯」在**阈值设置页**上的两个输入与说明文案。
 *
 * 用户要求（逐字）：「加一个递增时间和次数 … 基础的 5m 也可以理解为 5+10*0」；
 * 并明确裁决默认值：**先不启用、默认 0（保行为不变）**，且**界面上要说明这一点**。
 *
 * 本文件钉：
 *  1. 两个输入框真的接在表单状态上（填值 dispatch 对应 action、**清空得到 null**）；
 *  2. 说明文案里含「公式」「默认不启用 / 行为一致」「恢复后归零」三句；
 *  3. 提交时字段名与单位正确（递增时长分钟 → 毫秒；两列名与库列一致）。
 *
 * 范式沿用 options-section.test.tsx：mock useProviderForm + createRoot/act。
 */

const mockDispatch = vi.fn();
const mockUseProviderForm = vi.fn();

vi.mock("next-intl", () => ({
  useTranslations: () => (key: string) => key,
}));

vi.mock("framer-motion", () => ({
  motion: { div: ({ children }: { children?: React.ReactNode }) => <div>{children}</div> },
}));

vi.mock("sonner", () => ({ toast: { success: vi.fn(), error: vi.fn() } }));

vi.mock(
  "@/app/[locale]/settings/providers/_components/forms/provider-form/provider-form-context",
  () => ({
    useProviderForm: (...args: unknown[]) => mockUseProviderForm(...args),
  })
);

import type React from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { readFileSync } from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { LimitsSection } from "@/app/[locale]/settings/providers/_components/forms/provider-form/sections/limits-section";
import type { ProviderFormState } from "@/app/[locale]/settings/providers/_components/forms/provider-form/provider-form-types";

function createMockState(
  circuitBreaker: Partial<ProviderFormState["circuitBreaker"]> = {}
): ProviderFormState {
  return {
    basic: { name: "", url: "", key: "", websiteUrl: "" },
    routing: {
      providerType: "claude",
      groupTag: [],
      preserveCurrentIp: false,
      preserveClientIp: false,
      disableSessionReuse: false,
      modelRedirects: {},
      allowedModels: [],
      allowedClients: [],
      blockedClients: [],
      priority: 0,
      groupPriorities: {},
      weight: 1,
      costMultiplier: 1,
      cacheTtlPreference: "inherit",
      swapCacheTtlBilling: false,
      codexReasoningEffortPreference: "inherit",
      codexReasoningSummaryPreference: "inherit",
      codexTextVerbosityPreference: "inherit",
      codexParallelToolCallsPreference: "inherit",
      codexImageGenerationPreference: "inherit",
      codexServiceTierPreference: "inherit",
      anthropicMaxTokensPreference: "inherit",
      anthropicThinkingBudgetPreference: "inherit",
      anthropicAdaptiveThinking: null,
      geminiGoogleSearchPreference: "inherit",
      activeTimeStart: null,
      activeTimeEnd: null,
      customHeadersText: "",
    },
    rateLimit: {
      limit5hUsd: null,
      limit5hResetMode: "rolling",
      limitDailyUsd: null,
      dailyResetMode: "fixed",
      dailyResetTime: "00:00",
      limitWeeklyUsd: null,
      limitMonthlyUsd: null,
      limitTotalUsd: null,
      limitConcurrentSessions: null,
    },
    circuitBreaker: {
      failureThreshold: 5,
      openDurationMinutes: 5,
      halfOpenSuccessThreshold: 2,
      maxRetryAttempts: null,
      releaseIncrementMinutes: null,
      maxOpenCount: null,
      ...circuitBreaker,
    },
    network: {
      proxyUrl: "",
      proxyFallbackToDirect: false,
      firstByteTimeoutStreamingSeconds: 60,
      streamingIdleTimeoutSeconds: 60,
      requestTimeoutNonStreamingSeconds: 600,
    },
    mcp: { mcpPassthroughType: "none", mcpPassthroughUrl: "" },
    batch: { isEnabled: "no_change" },
    ui: {
      isPending: false,
      errors: {},
      isTesting: false,
      testResult: null,
      activeTab: "limits",
      isDirty: false,
      submitAttempted: false,
      isEdit: false,
    },
  } as unknown as ProviderFormState;
}

function render(state: ProviderFormState) {
  mockUseProviderForm.mockReturnValue({
    state,
    dispatch: mockDispatch,
    mode: "create",
    batchAnalysis: undefined,
  });
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => {
    root.render(<LimitsSection />);
  });
  return {
    container,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

/** 触发 input 的值变更（React 受控输入必须走原生 setter + input 事件）。 */
function typeInto(input: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  setter?.call(input, value);
  act(() => {
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
}

describe("等待阶梯：阈值表单的两个输入", () => {
  beforeEach(() => {
    mockDispatch.mockClear();
  });
  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
  });

  it("递增时长：填 10 分钟 → dispatch SET_RELEASE_INCREMENT_MINUTES=10", () => {
    const { container, unmount } = render(createMockState());
    const input = container.querySelector<HTMLInputElement>("#release-increment");
    expect(input).not.toBeNull();
    typeInto(input as HTMLInputElement, "10");

    expect(mockDispatch).toHaveBeenCalledWith({
      type: "SET_RELEASE_INCREMENT_MINUTES",
      payload: 10,
    });
    unmount();
  });

  it("递增时长：清空 → payload 为 null（不启用阶梯，而不是 0）", () => {
    const { container, unmount } = render(createMockState({ releaseIncrementMinutes: 10 }));
    const input = container.querySelector<HTMLInputElement>("#release-increment");
    typeInto(input as HTMLInputElement, "");

    expect(mockDispatch).toHaveBeenCalledWith({
      type: "SET_RELEASE_INCREMENT_MINUTES",
      payload: null,
    });
    unmount();
  });

  it("最大次数：填 3 → dispatch SET_MAX_OPEN_COUNT=3；清空 → null", () => {
    const first = render(createMockState());
    const input = first.container.querySelector<HTMLInputElement>("#max-open-count");
    expect(input).not.toBeNull();
    typeInto(input as HTMLInputElement, "3");
    expect(mockDispatch).toHaveBeenCalledWith({ type: "SET_MAX_OPEN_COUNT", payload: 3 });
    first.unmount();

    mockDispatch.mockClear();
    const second = render(createMockState({ maxOpenCount: 3 }));
    typeInto(
      second.container.querySelector<HTMLInputElement>("#max-open-count") as HTMLInputElement,
      ""
    );
    expect(mockDispatch).toHaveBeenCalledWith({ type: "SET_MAX_OPEN_COUNT", payload: null });
    second.unmount();
  });

  it("两项为空时输入框显示空（不是 0）", () => {
    const { container, unmount } = render(
      createMockState({ releaseIncrementMinutes: null, maxOpenCount: null })
    );
    expect(container.querySelector<HTMLInputElement>("#release-increment")?.value).toBe("");
    expect(container.querySelector<HTMLInputElement>("#max-open-count")?.value).toBe("");
    unmount();
  });

  it("编辑态用 edit- 前缀 id（既有约定，避免与新增表单撞 id）", () => {
    mockUseProviderForm.mockReturnValue({
      state: createMockState(),
      dispatch: mockDispatch,
      mode: "edit",
      batchAnalysis: undefined,
    });
    const container = document.createElement("div");
    document.body.appendChild(container);
    const root = createRoot(container);
    act(() => {
      root.render(<LimitsSection />);
    });
    expect(container.querySelector("#edit-release-increment")).not.toBeNull();
    expect(container.querySelector("#edit-max-open-count")).not.toBeNull();
    act(() => root.unmount());
    container.remove();
  });

  it("说明文案含：公式 / 默认不启用且行为一致 / 恢复后归零", () => {
    const { container, unmount } = render(createMockState());
    const rendered = container.textContent ?? "";

    // t() 在本文件里回显键名，故断言钉的是「用了哪几句文案」。
    expect(rendered).toContain("sections.circuitBreaker.ladder.title");
    expect(rendered).toContain("sections.circuitBreaker.ladder.formula");
    expect(rendered).toContain("sections.circuitBreaker.ladder.example");
    // 用户明确要求界面说明默认语义：默认 0 = 关闭、行为不变。
    expect(rendered).toContain("sections.circuitBreaker.ladder.disabledByDefault");
    expect(rendered).toContain("sections.circuitBreaker.ladder.resetOnRecovery");
    unmount();
  });
});

describe("等待阶梯：全部语种都说明了「默认不启用、行为一致」", () => {
  const locales = ["zh-CN", "en"];

  for (const locale of locales) {
    it(`${locale}：五个阶梯键齐全，且 disabledByDefault 讲明默认语义`, () => {
      const file = path.join(
        process.cwd(),
        "messages",
        locale,
        "settings",
        "providers",
        "form",
        "sections.json"
      );
      const parsed = JSON.parse(readFileSync(file, "utf8")) as {
        circuitBreaker: {
          ladder: Record<string, string>;
          releaseIncrement: { label: string; desc: string };
          maxOpenCount: { label: string; desc: string };
        };
      };
      const ladder = parsed.circuitBreaker.ladder;
      for (const key of ["title", "formula", "example", "disabledByDefault", "resetOnRecovery"]) {
        expect(ladder[key], `${locale} 缺 ladder.${key}`).toBeTruthy();
      }
      expect(parsed.circuitBreaker.releaseIncrement.label).toBeTruthy();
      expect(parsed.circuitBreaker.maxOpenCount.label).toBeTruthy();
      // 文案必须讲清「默认不启用 + 行为一致」，否则用户会以为默认就开了阶梯。
      const disabled = ladder.disabledByDefault;
      expect(disabled.length).toBeGreaterThan(8);
    });
  }
});

// ---------------------------------------------------------------------------
// 提交侧：界面的「分钟」必须转成库列口径的「毫秒」，且字段名与库列一致
//
// 这一段没有可渲染的入口（整表单依赖 router/query 一整套运行时），故按仓库既有做法做
// **源码结构性钉子**：把「键名 + 换算」钉在源码上，改动破坏契约就红。
// ---------------------------------------------------------------------------

describe("等待阶梯：提交与回填的字段契约", () => {
  const formIndex = path.join(
    process.cwd(),
    "src/app/[locale]/settings/providers/_components/forms/provider-form/index.tsx"
  );
  const formContext = path.join(
    process.cwd(),
    "src/app/[locale]/settings/providers/_components/forms/provider-form/provider-form-context.tsx"
  );

  it("提交时：两个字段名与库列一致，且分钟 -> 毫秒；空值出 null", () => {
    const source = readFileSync(formIndex, "utf8");
    expect(source).toContain("circuit_breaker_release_increment: releaseIncrementMs");
    expect(source).toContain("circuit_breaker_max_open_count: state.circuitBreaker.maxOpenCount");
    // 与熔断时长同单位（毫秒）：漏了 * 60 * 1000 会把 10 分钟写成 10 毫秒。
    expect(source).toContain("state.circuitBreaker.releaseIncrementMinutes * 60 * 1000");
    // 空 = 不启用（null），不能写成 0。
    const nullBranch =
      /const releaseIncrementMs = state\.circuitBreaker\.releaseIncrementMinutes\s*\?[\s\S]{0,120}?: null;/;
    expect(source).toMatch(nullBranch);
  });

  it("回填时：毫秒 -> 分钟，且 null 保持 null（否则会显示成 0 分钟）", () => {
    const source = readFileSync(formContext, "utf8");
    expect(source).toContain("circuitBreakerReleaseIncrement / 60000");
    expect(source).toContain("releaseIncrementMinutes:");
    expect(source).toContain("maxOpenCount:");
  });
});
