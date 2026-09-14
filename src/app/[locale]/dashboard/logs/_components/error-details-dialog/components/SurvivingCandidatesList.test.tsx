import { NextIntlClientProvider } from "next-intl";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, test } from "vitest";
import dashboardMessages from "../../../../../../../../messages/en/dashboard.json";
import { SurvivingCandidatesList } from "./SurvivingCandidatesList";

const messages = { dashboard: dashboardMessages };

function render(node: React.ReactNode) {
  return renderToStaticMarkup(
    <NextIntlClientProvider locale="en" messages={messages} timeZone="UTC">
      {node}
    </NextIntlClientProvider>
  );
}

/**
 * 生产 session 01a09b85 的真实形状（2026-09-14）：5 家通过全部硬校验，优先级 2/2/3/4/5，
 * 前缀亲和命中优先级 4 的 CommandCode Chat，其余 4 家因此未参与竞争。
 */
function productionShapedCandidates() {
  return [
    { id: 138, name: "OpenCode X Chat", priority: 2, weight: 1, costMultiplier: 1 },
    { id: 162, name: "OpenCode Grl Chat", priority: 2, weight: 1, costMultiplier: 1 },
    { id: 145, name: "Ollama Codex", priority: 3, weight: 1, costMultiplier: 1 },
    { id: 163, name: "CommandCode Chat", priority: 4, weight: 1, costMultiplier: 1 },
    { id: 148, name: "Ollama2 Codex", priority: 5, weight: 1, costMultiplier: 1 },
  ].map((candidate) => ({
    ...candidate,
    // 本夹具没有分组覆盖，故生效档位等于配置值（Go 侧两个数组字段同形，都要给）。
    effectivePriority: candidate.priority,
    affinitySkipped: candidate.id !== 163,
    selected: candidate.id === 163,
  }));
}

describe("SurvivingCandidatesList", () => {
  test("把「通过硬校验却未参与竞争」的家逐条列出，并指名亲和命中的那一家", () => {
    const html = render(
      <SurvivingCandidatesList
        candidates={productionShapedCandidates()}
        matchedProviderName="CommandCode Chat"
      />
    );

    expect(html).toContain('data-testid="surviving-candidates"');
    // 用户报的就是「这几家看不见」——四家被跳过的必须在场。
    for (const name of ["OpenCode X Chat", "OpenCode Grl Chat", "Ollama Codex", "Ollama2 Codex"]) {
      expect(html).toContain(name);
    }
    // 说明文案要指名是**谁**的亲和把它们挤掉了，否则读的人仍然不知道原因。
    expect(html).toContain("CommandCode Chat");
  });

  test("被跳过与被选中各自成档：四家 skipped、一家 selected", () => {
    const html = render(
      <SurvivingCandidatesList
        candidates={productionShapedCandidates()}
        matchedProviderName="CommandCode Chat"
      />
    );

    expect(html.match(/data-candidate-state="skipped"/g)).toHaveLength(4);
    expect(html.match(/data-candidate-state="selected"/g)).toHaveLength(1);
    // 优先级必须逐条显出来：没有它，「本该先被选中」这件事就无从判断。
    for (const badge of ["P2", "P3", "P4", "P5"]) {
      expect(html).toContain(badge);
    }
  });

  test("按优先级升序排（数值小 = 优先级高 = 本该先被选中）", () => {
    const html = render(
      <SurvivingCandidatesList
        candidates={productionShapedCandidates()}
        matchedProviderName="CommandCode Chat"
      />
    );

    // 按行的 id 取顺序：候选名可能同时出现在说明文案里（亲和命中者），用 indexOf 会误判。
    const rowOrder = [...html.matchAll(/data-candidate-id="(\d+)"/g)].map((match) =>
      Number(match[1])
    );
    expect(rowOrder).toEqual([138, 162, 145, 163, 148]);
  });

  test("同优先级内按 id 稳定排序，避免渲染抖动", () => {
    const html = render(
      <SurvivingCandidatesList
        candidates={[
          {
            id: 900,
            name: "later",
            priority: 2,
            effectivePriority: 2,
            weight: 1,
            costMultiplier: 1,
            affinitySkipped: true,
            selected: false,
          },
          {
            id: 100,
            name: "earlier",
            priority: 2,
            effectivePriority: 2,
            weight: 1,
            costMultiplier: 1,
            affinitySkipped: true,
            selected: false,
          },
        ]}
        matchedProviderName="X"
      />
    );

    expect(html.indexOf("earlier")).toBeLessThan(html.indexOf("later"));
  });

  test("空数组不渲染任何东西（无可报之事时不占位）", () => {
    const html = render(
      <SurvivingCandidatesList candidates={[]} matchedProviderName="CommandCode Chat" />
    );
    expect(html).toBe("");
  });
});
