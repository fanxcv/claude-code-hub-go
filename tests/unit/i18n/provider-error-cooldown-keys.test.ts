import { describe, expect, it } from "vitest";

import enProviderChain from "../../../messages/en/provider-chain.json";
import zhCNProviderChain from "../../../messages/zh-CN/provider-chain.json";

/**
 * 钉住 provider_error_cooldown 两条文案的**语义契约**：过滤理由（filterReasons）与过滤详情
 * （filterDetails，前端优先显示这条）。
 *
 * 为什么需要这条：这两句是人类读到的唯一解释。2026-09-23 的生产事故里详情写的是
 * 「本会话刚在该渠道上失败（上游错误），冷却期内跳过」，而那条请求落库是 200 且无错误文案、
 * 可用性投影是 green——真实成因只是上游漏发了协议终止标记。文案把「偶发收尾瑕疵」说成了
 * 「渠道故障」，又完全不提「冷却过后本会话仍粘回该渠道」（绑定保留、只是 60 秒内先绕开），
 * 于是运维看到这条跳过只能推断会话被永久搬走。
 *
 * 断言的是契约而不是句子本身：两语言都覆盖；详情必须说清冷却时长与「冷却过后回粘」；
 * 不得再归因成泛泛的「上游错误」（那正是本次修的失实之处）。emoji 由仓内全局审计
 * （scripts/audit-messages-no-emoji.js）覆盖，此处不重复。
 */
const locales = {
  en: enProviderChain,
  "zh-CN": zhCNProviderChain,
} as const;

describe("provider_error_cooldown translations", () => {
  for (const [locale, chain] of Object.entries(locales)) {
    const reason = chain.filterReasons.provider_error_cooldown;
    const detail = chain.filterDetails.provider_error_cooldown;

    it(`${locale} keeps both entries non-empty`, () => {
      expect(reason.trim(), `${locale} filterReasons`).not.toBe("");
      expect(detail.trim(), `${locale} filterDetails`).not.toBe("");
    });

    it(`${locale} states the cooldown duration`, () => {
      expect(detail).toMatch(locale === "en" ? /60s/ : /60 秒/);
    });

    it(`${locale} states that the session sticks back after the cooldown`, () => {
      expect(detail).toMatch(locale === "en" ? /sticks back/i : /粘回/);
    });

    it(`${locale} no longer blames a generic upstream error`, () => {
      expect(detail).not.toContain(locale === "en" ? "provider error" : "上游错误");
    });
  }
});
