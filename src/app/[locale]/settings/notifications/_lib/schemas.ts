"use client";

import { z } from "zod";
import { WebhookProviderTypeSchema } from "@/lib/api/v1/schemas/webhook-targets";

type TranslateFn = (key: string) => string;

export const createWebhookTargetFormSchema = (t: TranslateFn) =>
  z
    .object({
      name: z.string().trim().min(1).max(100),
      providerType: WebhookProviderTypeSchema,

      webhookUrl: z.string().trim().optional().nullable(),

      telegramBotToken: z.string().trim().optional().nullable(),
      telegramChatId: z.string().trim().optional().nullable(),

      dingtalkSecret: z.string().trim().optional().nullable(),

      customTemplate: z.string().trim().optional().nullable(),
      customHeaders: z.string().trim().optional().nullable(),

      proxyUrl: z.string().trim().optional().nullable(),
      proxyFallbackToDirect: z.boolean().default(false),

      isEnabled: z.boolean().default(true),
    })
    .superRefine((value, ctx) => {
      const webhookUrl = value.webhookUrl?.trim();
      const telegramBotToken = value.telegramBotToken?.trim();
      const telegramChatId = value.telegramChatId?.trim();

      if (value.providerType === "telegram") {
        if (!telegramBotToken) {
          ctx.addIssue({
            code: z.ZodIssueCode.custom,
            message: t("notifications.targetDialog.errors.telegramBotTokenRequired"),
            path: ["telegramBotToken"],
          });
        }
        if (!telegramChatId) {
          ctx.addIssue({
            code: z.ZodIssueCode.custom,
            message: t("notifications.targetDialog.errors.telegramChatIdRequired"),
            path: ["telegramChatId"],
          });
        }
        return;
      }

      if (!webhookUrl) {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: t("notifications.targetDialog.errors.webhookUrlRequired"),
          path: ["webhookUrl"],
        });
        return;
      }

      try {
        // eslint-disable-next-line no-new
        new URL(webhookUrl);
      } catch {
        ctx.addIssue({
          code: z.ZodIssueCode.custom,
          message: t("notifications.targetDialog.errors.webhookUrlInvalid"),
          path: ["webhookUrl"],
        });
      }

      if (value.providerType === "custom") {
        const template = value.customTemplate?.trim();
        if (!template) {
          ctx.addIssue({
            code: z.ZodIssueCode.custom,
            message: t("notifications.targetDialog.errors.customTemplateRequired"),
            path: ["customTemplate"],
          });
          return;
        }
        try {
          const parsed = JSON.parse(template) as unknown;
          if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
            ctx.addIssue({
              code: z.ZodIssueCode.custom,
              message: t("notifications.targetDialog.errors.customTemplateMustBeObject"),
              path: ["customTemplate"],
            });
          }
        } catch {
          ctx.addIssue({
            code: z.ZodIssueCode.custom,
            message: t("notifications.targetDialog.errors.customTemplateInvalidJson"),
            path: ["customTemplate"],
          });
        }
      }

      const headers = value.customHeaders?.trim();
      if (headers) {
        try {
          const parsed = JSON.parse(headers) as unknown;
          if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
            ctx.addIssue({
              code: z.ZodIssueCode.custom,
              message: t("notifications.targetDialog.errors.headersMustBeObject"),
              path: ["customHeaders"],
            });
          }
        } catch {
          ctx.addIssue({
            code: z.ZodIssueCode.custom,
            message: t("notifications.targetDialog.errors.headersInvalidJson"),
            path: ["customHeaders"],
          });
        }
      }
    });

export type WebhookTargetFormValues = z.input<ReturnType<typeof createWebhookTargetFormSchema>>;
