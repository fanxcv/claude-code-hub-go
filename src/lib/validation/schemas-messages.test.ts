import { describe, expect, test } from "vitest";
import { setZodErrorMap } from "@/lib/utils/zod-i18n";
import { CreateUserSchema, KeyFormSchema } from "./schemas";

describe("schema messages", () => {
  test("未装配 translator 时消息保持为词表键（不再是硬编码文案）", () => {
    const parsed = CreateUserSchema.safeParse({ name: "" });

    expect(parsed.success).toBe(false);
    expect(parsed.error?.issues[0].message).toBe("USER_NAME_REQUIRED");
  });

  test("装配 translator 后按词表本地化并插值参数", () => {
    setZodErrorMap((key, params) => (params ? `${key}:${JSON.stringify(params)}` : key));

    const tooLong = CreateUserSchema.safeParse({ name: "x".repeat(65) });
    expect(tooLong.error?.issues[0].message).toBe('USER_NAME_MAX_LENGTH:{"max":64}');

    const keyName = KeyFormSchema.safeParse({ name: "" });
    expect(keyName.error?.issues[0].message).toBe("KEY_NAME_REQUIRED");
  });
});
