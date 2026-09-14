/**
 * error-rules 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/repository/error-rules.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */
export interface ErrorRule {
  id: number;
  pattern: string;
  matchType: "regex" | "contains" | "exact";
  category: string;
  description: string | null;
  /** 覆写响应体（JSON）：匹配成功时用此响应替换原始错误响应，null 表示不覆写 */
  overrideResponse: ErrorOverrideResponse | null;
  /** 覆写状态码：null 表示透传上游状态码 */
  overrideStatusCode: number | null;
  isEnabled: boolean;
  isDefault: boolean;
  priority: number;
  createdAt: Date;
  updatedAt: Date;
}

/**
 * 错误覆写响应体类型（支持 Claude、Gemini、OpenAI 三种格式）
 */
export type ErrorOverrideResponse = ClaudeErrorResponse | GeminiErrorResponse | OpenAIErrorResponse;

/**
 * Claude API 错误格式
 * 参考: https://platform.claude.com/docs/en/api/errors
 */
export interface ClaudeErrorResponse {
  type: "error";
  error: {
    type: string; // 错误类型，如 "invalid_request_error"
    message: string; // 错误消息
    [key: string]: unknown; // 其他可选字段
  };
  request_id?: string; // 请求 ID（会自动从上游注入）
  [key: string]: unknown; // 其他可选字段
}

/**
 * Gemini API 错误格式
 * 参考: Google gRPC Status 标准
 */
export interface GeminiErrorResponse {
  error: {
    code: number; // HTTP 状态码，如 400
    message: string; // 错误消息
    status: string; // 错误状态，如 "INVALID_ARGUMENT"
    details?: unknown[]; // 可选的错误详情
    [key: string]: unknown; // 其他可选字段
  };
  [key: string]: unknown; // 其他可选字段
}

/**
 * OpenAI API 错误格式
 * 参考: https://platform.openai.com/docs/guides/error-codes
 */
export interface OpenAIErrorResponse {
  error: {
    message: string; // 错误消息
    type: string; // 错误类型，如 "invalid_request_error"
    param?: string | null; // 可选的参数名（指向出错的请求参数）
    code?: string | null; // 可选的错误代码，如 "model_not_found"
    [key: string]: unknown; // 其他可选字段
  };
  [key: string]: unknown; // 其他可选字段
}
