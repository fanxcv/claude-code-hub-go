import { NextIntlClientProvider } from "next-intl";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, test } from "vitest";
import dashboardMessages from "../../../../../../../../messages/en/dashboard.json";
import { ConsideredCandidatesList } from "./ConsideredCandidatesList";

const messages = { dashboard: dashboardMessages };

function render(node: React.ReactNode) {
  return renderToStaticMarkup(
    <NextIntlClientProvider locale="en" messages={messages} timeZone="UTC">
      {node}
    </NextIntlClientProvider>
  );
}

/**
 * 生产 session 01a09b85 的真实形状（2026-09-14，用户组 `codex,fan`，模型 deepseek-v4.1-flash）：
 * 5 家通过全部硬校验进入候选池。CommandCode 161/163 配了 `group_priorities={"fan":0}` ⇒ 分层值 0；
 * OpenCode 138/162 无覆盖 ⇒ 分层值 = 配置值 2；148 分层值 5。最终选中 163。
 *
 * 注意 161：它**与 163 同为 0 档**，但在当时落库的 `candidatesAtPriority` 里缺席（那才是用户报的
 * 「看不到 opencode 那几个渠道参与」的同类盲区）。
 */
function productionShapedCandidates() {
  return [
    { id: 138, name: "OpenCode X Chat", priority: 2, effectivePriority: 2 },
    { id: 162, name: "OpenCode Grl Chat", priority: 2, effectivePriority: 2 },
    { id: 161, name: "CommandCode Chat", priority: 4, effectivePriority: 0 },
    { id: 163, name: "CommandCode Chat", priority: 4, effectivePriority: 0 },
    { id: 148, name: "Ollama2 Codex", priority: 5, effectivePriority: 5 },
  ].map((candidate) => ({
    ...candidate,
    weight: 1,
    costMultiplier: 1,
    selected: candidate.id === 163,
  }));
}

describe("ConsideredCandidatesList", () => {
  test("候选池里每一家都在场——含与选中者同档而落选的那家", () => {
    const html = render(
      <ConsideredCandidatesList candidates={productionShapedCandidates()} selectedPriority={0} />
    );

    expect(html).toContain('data-testid="considered-candidates"');
    // 用户报的就是「看不到这几家参与」——五家全须在场。
    for (const name of ["OpenCode X Chat", "OpenCode Grl Chat", "Ollama2 Codex"]) {
      expect(html).toContain(name);
    }
    // 161 与 163 同名，用 id 断言 161 确实单独成行（它在生产里曾整条缺席）。
    const ids = [...html.matchAll(/data-candidate-id="(\d+)"/g)].map((m) => Number(m[1]));
    expect(ids).toContain(161);
    expect(ids).toHaveLength(5);
  });

  test("落选原因按档位判定：档位更低 3 家、同档未抽中 1 家、选中 1 家", () => {
    const html = render(
      <ConsideredCandidatesList candidates={productionShapedCandidates()} selectedPriority={0} />
    );

    expect(html.match(/data-candidate-state="selected"/g)).toHaveLength(1);
    // 138/162（2 > 0）、148（5 > 0）档位更低。
    expect(html.match(/data-candidate-state="lower-tier"/g)).toHaveLength(3);
    // 161 与选中者同为 0 档，落选靠加权随机而非档位——两者不可混为一谈。
    expect(html.match(/data-candidate-state="same-tier"/g)).toHaveLength(1);
  });

  test("按分层值升序排（数值小 = 优先级高 = 本该先被选中）", () => {
    const html = render(
      <ConsideredCandidatesList candidates={productionShapedCandidates()} selectedPriority={0} />
    );

    const rowOrder = [...html.matchAll(/data-candidate-id="(\d+)"/g)].map((m) => Number(m[1]));
    expect(rowOrder).toEqual([161, 163, 138, 162, 148]);
  });

  test("说明文案点出选中档位，使「2 高于 0 故落选」可读", () => {
    const html = render(
      <ConsideredCandidatesList candidates={productionShapedCandidates()} selectedPriority={0} />
    );

    // 选中档位不在文案里，「档位更低」就失去参照物。
    expect(html).toContain("P0");
    // 每家的生效档位逐条显出来。
    for (const tier of ["P2", "P5"]) {
      expect(html).toContain(tier);
    }
  });

  test("配置值被分组覆盖改写时另标一记，未被改写的不标", () => {
    const html = render(
      <ConsideredCandidatesList candidates={productionShapedCandidates()} selectedPriority={0} />
    );

    // 仅 161/163（配置 4、分层 0）需要标注；138/162/148 两个值相等，标了就是噪声。
    expect(html.match(/configured P4/g)).toHaveLength(2);
    expect(html).not.toContain("configured P2");
    expect(html).not.toContain("configured P5");
  });

  test("全候同等档位时，落选一律记为同档未抽中（不误报档位更低）", () => {
    const html = render(
      <ConsideredCandidatesList
        candidates={[
          {
            id: 1,
            name: "a",
            priority: 3,
            effectivePriority: 3,
            weight: 1,
            costMultiplier: 1,
            selected: true,
          },
          {
            id: 2,
            name: "b",
            priority: 3,
            effectivePriority: 3,
            weight: 1,
            costMultiplier: 1,
            selected: false,
          },
        ]}
        selectedPriority={3}
      />
    );

    expect(html.match(/data-candidate-state="same-tier"/g)).toHaveLength(1);
    expect(html).not.toContain('data-candidate-state="lower-tier"');
  });

  test("空数组不渲染任何东西（无可报之事时不占位）", () => {
    const html = render(<ConsideredCandidatesList candidates={[]} selectedPriority={0} />);
    expect(html).toBe("");
  });
});

describe("ConsideredCandidatesList 亲和短路行", () => {
  /**
   * 亲和短路行的形状（生产 960131/960132/960133，2026-09-14）：5 家通过全部硬校验、前缀亲和
   * 命中 163（配置档位 4、`fan` 组覆盖后分层值 0），其余四家**未参与竞争**。
   *
   * 与初始选路的关键差别：落选原因是「没参与竞争」，不是「档位更低」——拿档位去解释会把一次
   * 亲和短路误说成档位不够。故这类行一律标 affinity-skipped，且不再出现 lower-tier。
   */
  function affinityCandidates() {
    return [
      { id: 138, name: "OpenCode X Chat", priority: 2, effectivePriority: 2 },
      { id: 162, name: "OpenCode Grl Chat", priority: 2, effectivePriority: 2 },
      { id: 145, name: "Ollama Codex", priority: 3, effectivePriority: 3 },
      { id: 163, name: "CommandCode Chat", priority: 4, effectivePriority: 0 },
      { id: 148, name: "Ollama2 Codex", priority: 5, effectivePriority: 5 },
    ].map((candidate) => ({
      ...candidate,
      weight: 1,
      costMultiplier: 1,
      selected: candidate.id === 163,
    }));
  }

  const skipped = [138, 162, 145, 148];

  test("四家未参与竞争、一家已选中；档位照旧逐条在场", () => {
    const html = render(
      <ConsideredCandidatesList
        candidates={affinityCandidates()}
        selectedPriority={0}
        affinitySkippedIds={skipped}
      />
    );

    expect(html.match(/data-candidate-state="affinity-skipped"/g)).toHaveLength(4);
    expect(html.match(/data-candidate-state="selected"/g)).toHaveLength(1);
    // 亲和行的 priorityLevels 是单档，档位图只能从各家的生效优先级读出来。
    for (const badge of ["P0", "P2", "P3", "P5"]) {
      expect(html).toContain(badge);
    }
  });

  test("不回落到「档位更低」——那四家里有三家档位明明更高", () => {
    const html = render(
      <ConsideredCandidatesList
        candidates={affinityCandidates()}
        selectedPriority={0}
        affinitySkippedIds={skipped}
      />
    );

    expect(html).not.toContain('data-candidate-state="lower-tier"');
  });

  test("说明文案指名亲和命中的那一家与被跳过的家数（取自选中者，不靠外部传名）", () => {
    const html = render(
      <ConsideredCandidatesList
        candidates={affinityCandidates()}
        selectedPriority={0}
        affinitySkippedIds={skipped}
      />
    );

    expect(html).toContain("CommandCode Chat");
    expect(html).toContain("4");
    expect(html).toContain("did not compete");
  });

  test("提名者不在通过集内时没有选中项，退用链项名说清命中者（Go 侧登记的边界）", () => {
    // Go 侧的边界：选择器级闸门放行、请求级闸门把提名者滤掉时，整表都记 affinitySkipped、
    // 不会有 Selected 项。此时文案不能出现空白的「命中 」。
    const html = render(
      <ConsideredCandidatesList
        candidates={affinityCandidates().map((candidate) => ({ ...candidate, selected: false }))}
        selectedPriority={0}
        affinitySkippedIds={affinityCandidates().map((candidate) => candidate.id)}
        matchedProviderName="CommandCode Chat"
      />
    );

    expect(html.match(/data-candidate-state="affinity-skipped"/g)).toHaveLength(5);
    expect(html).not.toContain('data-candidate-state="selected"');
    expect(html).toContain("CommandCode Chat");
  });

  test("只有被跳过的家进 affinity 档：未标记的落选者仍按档位解释", () => {
    // 两套判定的优先级：选中 > 未参与竞争 > 档位。少一侧都会把原因说错。
    const html = render(
      <ConsideredCandidatesList
        candidates={affinityCandidates()}
        selectedPriority={0}
        affinitySkippedIds={[138]}
      />
    );

    expect(html.match(/data-candidate-state="affinity-skipped"/g)).toHaveLength(1);
    expect(html.match(/data-candidate-state="lower-tier"/g)).toHaveLength(3);
  });
});
