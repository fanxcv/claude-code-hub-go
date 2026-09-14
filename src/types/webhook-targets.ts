/**
 * webhook-targets 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/repository/webhook-targets.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */
export interface WebhookTarget {
  id: number;
  name: string;
  providerType: WebhookProviderType;

  webhookUrl: string | null;

  telegramBotToken: string | null;
  telegramChatId: string | null;

  dingtalkSecret: string | null;

  customTemplate: Record<string, unknown> | null;
  customHeaders: Record<string, string> | null;

  proxyUrl: string | null;
  proxyFallbackToDirect: boolean;

  isEnabled: boolean;
  lastTestAt: Date | null;
  lastTestResult: WebhookTestResult | null;

  createdAt: Date | null;
  updatedAt: Date | null;
}

export type WebhookProviderType = "wechat" | "feishu" | "dingtalk" | "telegram" | "custom";

export interface WebhookTestResult {
  success: boolean;
  error?: string;
  latencyMs?: number;
}
