/**
 * 本文件专测「转换损失说明」的**文案语义**。
 *
 * 既有的 protocol-conversion-display.test.tsx 把 useTranslations mock 成键名（断言的是 `lossTooltip`
 * 这个键本身），因此验不到真实文案——文案里写「上游收到的东西变少了」也能长期全绿，而 `rewritten`
 * 覆盖 ID 规范化、data URL 重编码等**非删减**转换，运维照这行字会把纯改写误判成内容丢失。
 *
 * 故这里引真词表渲染，把「该说明不得声称内容变少、必须表述为与客户端请求存在差异」钉成断言，
 * 中英各一条。动作语义（丢弃/降级/改写）由逐组明细承担，故一并断言明细仍在场——否则把 tooltip
 * 改中性就等于把信息改没了。
 *
 * 能力取改写档（image）：降级/信息档的条目在列表上不画徽章，tooltip 根本不渲染，也就无从断言其措辞。
 */
import type { ReactNode } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, test, vi } from "vitest";
import enDashboard from "@messages/en/dashboard.json";
import zhCNDashboard from "@messages/zh-CN/dashboard.json";
import type { SpecialSetting } from "@/types/special-settings";
import { ProtocolConversionDisplay } from "./protocol-conversion-display";

// vi.mock 的工厂会被提升到文件顶部，故真词表放在 vi.hoisted 的状态里，由每个用例切换。
const i18nState = vi.hoisted(() => ({ table: null as unknown }));

function lookup(root: unknown, path: string): unknown {
  return path.split(".").reduce<unknown>((node, part) => {
    if (typeof node !== "object" || node === null) {
      return undefined;
    }
    return (node as Record<string, unknown>)[part];
  }, root);
}

vi.mock("next-intl", () => ({
  useTranslations:
    (namespace: string) =>
    (key: string, params?: Record<string, unknown>): string => {
      const value = lookup(lookup(i18nState.table, namespace), key);
      if (typeof value !== "string") {
        return key;
      }
      return params
        ? value.replace(/\{(\w+)\}/g, (_match, name: string) => String(params[name]))
        : value;
    },
}));

// tooltip 内容经 mock 直接进静态标记（真 Radix tooltip 只在悬停时入 portal，静态渲染取不到）。
vi.mock("@/components/ui/tooltip", () => ({
  TooltipProvider: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  Tooltip: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));

/** 只声明本用例真正读取的路径：两语词表的键集并不完全对齐（zh-CN 少若干键），用整体形状会误报。 */
interface LossWordingTable {
  logs: {
    protocolConversion: {
      lossTooltip: string;
      lossAction: { rewritten: string };
    };
  };
}

/** 每条：[locale, 真词表, 不得出现的「内容变少」类表述, 必须出现的「与客户端请求不一致」类表述]。 */
const locales: Array<[string, LossWordingTable, RegExp, RegExp]> = [
  ["zh-CN", zhCNDashboard, /变少|减少|更少|丢失/, /差异|不一致/],
  ["en", enDashboard, /\bless\b|\blost\b|\bloss\b|\breduc/i, /differ|mismatch/i],
];

/** 只含 `rewritten` 的损失条目：rewritten 是**非删减**转换，说明里不得暗示内容变少。 */
const rewrittenOnly: SpecialSetting = {
  type: "protocol_conversion_loss",
  scope: "request",
  hit: true,
  clientProtocol: "openai-responses",
  targetProtocol: "openai-chat",
  total: 1,
  groups: [{ capability: "image", action: "rewritten", count: 1 }],
};

describe("转换损失说明对 rewritten 的概括", () => {
  for (const [locale, table, forbidden, neutral] of locales) {
    test(`${locale}：仅 rewritten 时说明不得声称上游收到的内容变少`, () => {
      // 词表文件本身即 dashboard 命名空间，而组件取的是 "dashboard.logs.protocolConversion"，
      // 故这里按 next-intl 提供方那样再包一层。
      i18nState.table = { dashboard: table };

      const html = renderToStaticMarkup(
        <ProtocolConversionDisplay specialSettings={[rewrittenOnly]} />
      );
      const tooltip = table.logs.protocolConversion.lossTooltip;

      // 先把真文案钉进渲染结果：接线或本文件的 mock 断了，下面的语义断言就没有意义。
      expect(html).toContain(tooltip);
      // 禁止性判据在前：它是本用例的锚（改回「内容变少」的措辞必须先红在这里），
      // 正向判据只防止把 tooltip 改成不含差异语义的空话。
      expect(tooltip).not.toMatch(forbidden);
      expect(tooltip).toMatch(neutral);
      // 中性化 tooltip 不等于抹掉信息：动作语义仍须在逐组明细里出现。
      expect(html).toContain(table.logs.protocolConversion.lossAction.rewritten);
      expect(html).toContain("image");
    });
  }
});
