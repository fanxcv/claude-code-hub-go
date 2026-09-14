import { readFileSync } from "node:fs";
import { join } from "node:path";
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { NextIntlClientProvider } from "next-intl";
import { describe, expect, test, vi } from "vitest";
import dashboardMessages from "../../../../../../../../messages/en/dashboard.json";
import ipDetailsMessages from "../../../../../../../../messages/en/ipDetails.json";
import providerChainMessages from "../../../../../../../../messages/en/provider-chain.json";
import type { ProviderChainItem } from "@/types/message";
import {
  affinitySkippedIds,
  consideredPool,
  findAffinityProbeAttempt,
  LogicTraceTab,
} from "./LogicTraceTab";

vi.mock("@/lib/api-client/v1/actions/session-origin-chain", () => ({
  getSessionOriginChain: () => Promise.resolve({ ok: false, error: "mock" }),
}));

vi.mock("@/i18n/routing", () => ({
  Link: ({ href, children }: { href: string; children: ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

const messages = {
  dashboard: dashboardMessages,
  "provider-chain": providerChainMessages,
  ipDetails: ipDetailsMessages,
};

function renderWithIntl(node: ReactNode) {
  return renderToStaticMarkup(
    <NextIntlClientProvider locale="en" messages={messages} timeZone="UTC">
      {node}
    </NextIntlClientProvider>
  );
}

/**
 * 亲和命中条目（决策链里的「Sticky 探测」）是选路条目，按契约不写 errorMessage。
 * 真实形状（生产库取样，2026-09-13）：
 *   [0] reason='affinity_hit' provider=145 statusCode=null errorMessage='' selectionMethod='prefix_affinity'
 *   [1] reason='client_abort' provider=145 attemptNumber=1 errorMessage='Client aborted request'
 */
function affinityEntry(overrides: Partial<ProviderChainItem> = {}): ProviderChainItem {
  return {
    id: 145,
    name: "Ollama Codex",
    reason: "affinity_hit",
    selectionMethod: "prefix_affinity",
    timestamp: 1_700_000_000_000,
    affinity: { matchedDepth: 3, matchedPrefixBytes: 4096, matchedFp: "abc123" },
    ...overrides,
  } as ProviderChainItem;
}

function attemptEntry(overrides: Partial<ProviderChainItem> = {}): ProviderChainItem {
  return {
    id: 145,
    name: "Ollama Codex",
    reason: "retry_failed",
    attemptNumber: 1,
    statusCode: 400,
    errorMessage: "invalid_request_error: Invalid assistant message: content or tool_calls",
    timestamp: 1_700_000_000_500,
    ...overrides,
  } as ProviderChainItem;
}

describe("findAffinityProbeAttempt", () => {
  test("关联到亲和条目之后同一供应商的尝试条目", () => {
    const chain = [affinityEntry(), attemptEntry()];
    expect(findAffinityProbeAttempt(chain, 0)?.attemptNumber).toBe(1);
    expect(findAffinityProbeAttempt(chain, 0)?.errorMessage).toContain("Invalid assistant message");
  });

  test("不跨供应商误关联（异家尝试不算本次亲和的探测结果）", () => {
    const chain = [affinityEntry(), attemptEntry({ id: 163, errorMessage: "other provider" })];
    expect(findAffinityProbeAttempt(chain, 0)).toBeNull();
  });

  test("亲和条目之前（更早）的尝试不算", () => {
    const chain = [attemptEntry(), affinityEntry()];
    expect(findAffinityProbeAttempt(chain, 1)).toBeNull();
  });

  test("跳过同供应商的后续选路条目，取到真正的尝试条目", () => {
    const chain = [
      affinityEntry(),
      affinityEntry({
        reason: "initial_selection",
        selectionMethod: undefined,
        affinity: undefined,
      }),
      attemptEntry({ attemptNumber: 2, statusCode: 200, errorMessage: undefined }),
    ];
    const probe = findAffinityProbeAttempt(chain, 0);
    expect(probe?.attemptNumber).toBe(2);
    expect(probe?.statusCode).toBe(200);
  });

  test("无后续尝试时返回 null（越界与空链同样安全）", () => {
    expect(findAffinityProbeAttempt([affinityEntry()], 0)).toBeNull();
    expect(findAffinityProbeAttempt([], 0)).toBeNull();
    expect(findAffinityProbeAttempt([affinityEntry()], 5)).toBeNull();
  });
});

describe("consideredPool / affinitySkippedIds（统一读取口径）", () => {
  function minimalContext(patch: Record<string, unknown>) {
    return {
      totalProviders: 5,
      enabledProviders: 5,
      targetType: "codex" as const,
      groupFilterApplied: true,
      beforeHealthCheck: 5,
      afterHealthCheck: 5,
      filteredProviders: [],
      priorityLevels: [0],
      selectedPriority: 0,
      candidatesAtPriority: [],
      ...patch,
    };
  }

  const pool = [
    {
      id: 138,
      name: "OpenCode X Chat",
      priority: 2,
      effectivePriority: 2,
      weight: 1,
      costMultiplier: 1,
      selected: false,
    },
    {
      id: 163,
      name: "CommandCode Chat",
      priority: 4,
      effectivePriority: 0,
      weight: 1,
      costMultiplier: 1,
      selected: true,
    },
  ];

  test("优先读 consideredCandidates（跨路径统一的参与池）", () => {
    const context = minimalContext({
      consideredCandidates: pool,
      survivingCandidates: [
        { ...pool[0], affinitySkipped: true },
        { ...pool[1], affinitySkipped: false },
      ],
    });
    expect(consideredPool(context).map((c) => c.id)).toEqual([138, 163]);
  });

  test("兼容：只有历史键 survivingCandidates 时回退读它，旧行清单不静默消失", () => {
    // 2026-09-14 之前落库的亲和行只有这个键（生产 3 小时窗口 1549 行 affinity_hit 皆如此）。
    const context = minimalContext({
      survivingCandidates: [
        { ...pool[0], affinitySkipped: true },
        { ...pool[1], affinitySkipped: false },
      ],
    });
    expect(consideredPool(context).map((c) => c.id)).toEqual([138, 163]);
  });

  test("两个键都没有时返回空（无可报之事，不占位）", () => {
    expect(consideredPool(minimalContext({}))).toEqual([]);
  });

  test("affinitySkippedIds 只取被跳过的那批，且不与成员集混用", () => {
    const context = minimalContext({
      consideredCandidates: pool,
      survivingCandidates: [
        { ...pool[0], affinitySkipped: true },
        { ...pool[1], affinitySkipped: false },
      ],
    });
    // 被提名者（选中者）不在内：亲和短路里它是被采纳的那家，不该标成「未参与竞争」。
    expect(affinitySkippedIds(context)).toEqual([138]);
    expect(affinitySkippedIds(minimalContext({}))).toEqual([]);
  });
});

describe("LogicTraceTab 亲和短路行的参与池", () => {
  /**
   * 亲和短路行的真实形状（生产 960131/960132/960133，2026-09-14）：5 家通过全部硬校验，
   * 前缀亲和命中 163（配置档位 4、`fan` 组覆盖后分层值 0），其余四家未参与竞争。
   *
   * 两个键**同源同序**（Go 侧同一次计算两个投影）：consideredCandidates 是跨路径统一的参与池，
   * survivingCandidates 多带 affinitySkipped 一位。各渲染一遍就会把同一批供应商列两遍。
   */
  function survivor(id: number, name: string, priority: number, effectivePriority: number) {
    return {
      id,
      name,
      priority,
      effectivePriority,
      weight: 1,
      costMultiplier: 1,
      affinitySkipped: id !== 163,
      selected: id === 163,
    };
  }

  const pool = [
    { id: 138, name: "OpenCode X Chat", priority: 2, effectivePriority: 2 },
    { id: 162, name: "OpenCode Grl Chat", priority: 2, effectivePriority: 2 },
    { id: 145, name: "Ollama Codex", priority: 3, effectivePriority: 3 },
    { id: 163, name: "CommandCode Chat", priority: 4, effectivePriority: 0 },
    { id: 148, name: "Ollama2 Codex", priority: 5, effectivePriority: 5 },
  ];

  function affinityContext(withConsidered = true) {
    const survivors = pool.map((candidate) =>
      survivor(candidate.id, candidate.name, candidate.priority, candidate.effectivePriority)
    );
    return {
      totalProviders: 11,
      enabledProviders: 5,
      targetType: "codex" as const,
      requestedModel: "deepseek-v4.1-flash",
      userGroup: "codex,fan",
      afterGroupFilter: 5,
      groupFilterApplied: true,
      beforeHealthCheck: 5,
      afterHealthCheck: 5,
      filteredProviders: [],
      // 亲和行的档位仍是单档（Node 口径），要显示「有哪些档位」只能读参与池里的生效优先级。
      priorityLevels: [0],
      selectedPriority: 0,
      candidatesAtPriority: [{ id: 163, name: "CommandCode Chat", weight: 1, costMultiplier: 1 }],
      survivingCandidates: survivors,
      ...(withConsidered
        ? {
            consideredCandidates: survivors.map(
              ({ affinitySkipped: _skipped, ...candidate }) => candidate
            ),
          }
        : {}),
    };
  }

  test("亲和行也列出参与池全体，标出「未参与竞争」，且只渲染一张清单", () => {
    const html = renderWithIntl(
      <LogicTraceTab
        statusCode={200}
        errorMessage={null}
        providerChain={[affinityEntry({ decisionContext: affinityContext() })]}
        sessionId={null}
        initialExpandedChainIndex={0}
      />
    );

    expect(html).toContain('data-testid="considered-candidates"');
    // 重复渲染的防线：两个键同源同序，各渲染一遍就会出现两张一样的清单。
    expect(html.match(/data-testid="considered-candidates"/g)).toHaveLength(1);
    expect(html).not.toContain('data-testid="surviving-candidates"');

    for (const name of ["OpenCode X Chat", "OpenCode Grl Chat", "Ollama Codex", "Ollama2 Codex"]) {
      expect(html).toContain(name);
    }
    // 四家未参与竞争、一家已选中。
    expect(html.match(/data-candidate-state="affinity-skipped"/g)).toHaveLength(4);
    expect(html.match(/data-candidate-state="selected"/g)).toHaveLength(1);
    // 生效档位逐条在场：亲和行的 priorityLevels 只有单档，档位图只能从这里读。
    for (const badge of ["P0", "P2", "P3", "P5"]) {
      expect(html).toContain(badge);
    }
    // 说明文案要指名亲和命中了谁。
    expect(html).toContain("CommandCode Chat");
  });

  test("亲和行不按档位解释落选（那批的档位未必更低，按档位说会把短路说成档位不够）", () => {
    const html = renderWithIntl(
      <LogicTraceTab
        statusCode={200}
        errorMessage={null}
        providerChain={[affinityEntry({ decisionContext: affinityContext() })]}
        sessionId={null}
        initialExpandedChainIndex={0}
      />
    );

    // 145/148 的档位确实更低、138/162 也是；但落选原因是「未参与竞争」这一位，四家一律如此。
    expect(html).not.toContain('data-candidate-state="lower-tier"');
  });

  test("兼容：历史行只有 survivingCandidates 时，清单照旧在（不静默消失）", () => {
    const html = renderWithIntl(
      <LogicTraceTab
        statusCode={200}
        errorMessage={null}
        providerChain={[affinityEntry({ decisionContext: affinityContext(false) })]}
        sessionId={null}
        initialExpandedChainIndex={0}
      />
    );

    expect(html).toContain('data-testid="considered-candidates"');
    for (const name of ["OpenCode X Chat", "Ollama2 Codex"]) {
      expect(html).toContain(name);
    }
    expect(html.match(/data-candidate-state="affinity-skipped"/g)).toHaveLength(4);
  });

  test("反证：两个键都不给时，这几家又消失（只剩被选中那家）", () => {
    const {
      survivingCandidates: _droppedSurvivors,
      consideredCandidates: _droppedConsidered,
      ...bare
    } = affinityContext();
    const html = renderWithIntl(
      <LogicTraceTab
        statusCode={200}
        errorMessage={null}
        providerChain={[affinityEntry({ decisionContext: bare })]}
        sessionId={null}
        initialExpandedChainIndex={0}
      />
    );

    expect(html).not.toContain('data-testid="considered-candidates"');
    expect(html).not.toContain("OpenCode X Chat");
    expect(html).not.toContain("Ollama2 Codex");
  });
});

describe("LogicTraceTab 候选池里参与过的家（接线钉子）", () => {
  /**
   * 为什么用源码结构性断言而不是渲染断言：`Priority Selection` 那张 StepCard 与另外两张决策步骤
   * 一样**默认折叠**（StepCard 的 `defaultExpanded` 缺省 false，只有链项用 `initialExpandedChainIndex`
   * 控制），而本仓没有 @testing-library/react，静态渲染无法触发展开。渲染逻辑本身由
   * ConsideredCandidatesList.test.tsx 的用例逐条钉住，亲和行的挂载则由上面那组渲染用例覆盖；
   * 这里钉的是**初始选路路径**的接线。
   *
   * 钉它的理由（生产实证，2026-09-14）：Go 侧 consideredCandidates 先落地、界面一直没消费它，
   * 于是「Go 已交付」与「用户看得见」之间隔了一次漏接而无人发现。
   */
  const source = readFileSync(
    join(
      process.cwd(),
      "src/app/[locale]/dashboard/logs/_components/error-details-dialog/components/LogicTraceTab.tsx"
    ),
    "utf8"
  );

  test("决策链把参与池交给 ConsideredCandidatesList（含选中档位）", () => {
    expect(source).toContain("ConsideredCandidatesList");
    // 传参与字段名都要钉住：只 import 不传（或传错字段）都是漏接。
    expect(source).toMatch(/candidates=\{consideredPool\(decisionContext\)\}/);
    expect(source).toMatch(/selectedPriority=\{decisionContext\.selectedPriority\}/);
  });

  test("参与池只有一张清单组件，且已不再挂载 survivingCandidates 的旧列表", () => {
    // 两个键同源同序：若两处各挂一个组件，同一批供应商会被列两遍。
    expect(source).not.toContain("<SurvivingCandidatesList");
    // 两处挂载点：初始选路的「优先级选择」卡、亲和行的链项。
    expect(source.match(/<ConsideredCandidatesList/g)).toHaveLength(2);
  });

  test("列表挂在「优先级选择」这一步里（与 candidatesAtPriority 同一处）", () => {
    const priorityCard = source.indexOf("logicTrace.prioritySelection");
    expect(priorityCard).toBeGreaterThan(-1);
    const mount = source.indexOf("<ConsideredCandidatesList");
    expect(mount).toBeGreaterThan(priorityCard);
    const nextStepCard = source.indexOf("{/* Request Execution Steps */}");
    expect(mount).toBeLessThan(nextStepCard);
  });

  test("亲和行的挂载以「确有被跳过的候选」为条件（非亲和行不重复呈现参与池）", () => {
    // 非亲和行的参与池由「优先级选择」那一步呈现；链项若无条件挂载，同一批会被列两遍。
    expect(source).toMatch(/const skipped = affinitySkippedIds\(context\);/);
    expect(source).toMatch(/if \(skipped\.length === 0\) return null;/);
  });
});
