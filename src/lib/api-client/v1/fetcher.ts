import { CSRF_HEADER } from "@/lib/api/v1/_shared/constants";
import { ApiError } from "./errors";

type ProblemBody = {
  status?: number;
  detail?: string;
  errorCode?: string;
  errorParams?: Record<string, unknown>;
};

const CSRF_TOKEN_CACHE_TTL_MS = 25 * 60 * 1000;

export type ApiFetchOptions = Omit<RequestInit, "body"> & {
  body?: unknown;
  apiKey?: string;
  onResponse?: (response: Response) => void;
  skipCsrf?: boolean;
};

let csrfTokenPromise: Promise<string | null> | null = null;
let csrfTokenExpiresAt = 0;

export async function apiFetch<T>(path: string, options: ApiFetchOptions = {}): Promise<T> {
  const headers = new Headers(options.headers);
  headers.set("Accept", "application/json");

  if (options.apiKey) {
    headers.set("X-Api-Key", options.apiKey);
  }

  const method = (options.method ?? "GET").toUpperCase();
  const hasBody = options.body !== undefined;
  if (hasBody) {
    headers.set("Content-Type", "application/json");
  }

  if (!options.skipCsrf && isMutation(method) && !options.apiKey && !headers.has(CSRF_HEADER)) {
    const csrfToken = await getCsrfToken();
    if (csrfToken) headers.set(CSRF_HEADER, csrfToken);
  }

  const response = await fetch(path, {
    ...options,
    method,
    credentials: options.credentials ?? "include",
    headers,
    body: hasBody ? JSON.stringify(options.body) : undefined,
  });

  if (!response.ok) {
    const error = await decodeApiError(response);
    if (error.errorCode === "auth.csrf_invalid") {
      clearCsrfTokenCache();
    }
    throw error;
  }

  options.onResponse?.(response);

  if (response.status === 204) {
    return undefined as T;
  }

  return (await response.json()) as T;
}

/**
 * 向根级 `/api/admin/*` 端点发写请求（POST），附加 CSRF 头。
 *
 * 为何 cookie 会话下必须带它：`go/internal/adminapi/auth.go` 对「cookie 凭据 + 写方法」统一校验
 * `X-CCH-CSRF`，缺失即 403 `auth.csrf_invalid`；而 `X-CCH-CSRF` 的签发端点是
 * `GET /api/v1/auth/csrf`。裸 `fetch` 不会带这个头（生产实证：设置页改日志级别 403）。
 *
 * 为何不复用 apiFetch：这几个根级端点的失败体是 `{ error, details }`（见 `go/internal/adminapi/`
 * 的 admin_ops.go 与 system_config.go），而 apiFetch 的错误契约是 problem+json → ApiError：
 * 走它会把后端 `error` 文案换成 ApiError 的通用 detail（statusText），属用户可见变化。
 * 故此处只借**同一份** CSRF 取值实现（getCsrfToken 的缓存与 TTL），请求与响应原样交给调用方。
 */
export async function postAdminMutation(path: string, body: unknown): Promise<Response> {
  const headers = new Headers();
  headers.set("Content-Type", "application/json");
  const csrfToken = await getCsrfToken();
  if (csrfToken) headers.set(CSRF_HEADER, csrfToken);

  const response = await fetch(path, {
    method: "POST",
    credentials: "include",
    headers,
    body: JSON.stringify(body),
  });

  // 403 时清 token 缓存：否则调用方会一直用这份被拒的 token 直到 TTL（25 分钟）到期，
  // 表现为「点了没反应」。这里不解析错误体（本函数不消费它），故比 apiFetch 的
  // 「仅 csrf_invalid 才清」略宽——多付一次 /api/v1/auth/csrf 的代价换取不会卡死。
  if (response.status === 403) clearCsrfTokenCache();

  return response;
}

/**
 * 取 CSRF token（带缓存与 TTL）；取不到时返回 null。
 *
 * 导出是为了让根级 `/api/admin/*` 的写请求（postAdminMutation）复用同一份实现——
 * 组件侧不得自行硬编码头名或另写一份取 token 的 fetch。
 */
export async function getCsrfToken(): Promise<string | null> {
  const now = Date.now();
  if (csrfTokenPromise && now < csrfTokenExpiresAt) return csrfTokenPromise;

  csrfTokenExpiresAt = now + CSRF_TOKEN_CACHE_TTL_MS;
  csrfTokenPromise = fetch("/api/v1/auth/csrf", {
    credentials: "include",
    headers: { Accept: "application/json" },
  })
    .then(async (response) => {
      if (!response.ok) {
        clearCsrfTokenCache();
        return null;
      }
      const body = (await response.json()) as { csrfToken?: string | null };
      const csrfToken = body.csrfToken ?? null;
      if (!csrfToken) clearCsrfTokenCache();
      return csrfToken;
    })
    .catch((error) => {
      // Propagate transport/network failures: swallowing them as `null` would cause the
      // subsequent mutation to omit the X-CCH-CSRF header, returning auth.csrf_invalid which
      // the UI surfaces as PERMISSION_DENIED — masking the real cause as an auth problem.
      clearCsrfTokenCache();
      throw error;
    });

  return csrfTokenPromise;
}

export function clearCsrfTokenCache(): void {
  csrfTokenPromise = null;
  csrfTokenExpiresAt = 0;
}

async function decodeApiError(response: Response): Promise<ApiError> {
  const contentType = response.headers.get("content-type") ?? "";
  if (
    contentType.includes("application/problem+json") ||
    contentType.includes("application/json")
  ) {
    const bodyText = await response.text();
    let body: ProblemBody = {};
    try {
      body = bodyText ? (JSON.parse(bodyText) as ProblemBody) : {};
    } catch {
      return new ApiError({
        status: response.status,
        errorCode: "api.malformed_error_body",
        detail: bodyText.slice(0, 500) || response.statusText || "Request failed",
      });
    }
    return new ApiError({
      status: response.status,
      errorCode: body.errorCode ?? "api.error",
      detail: body.detail ?? response.statusText,
      errorParams: body.errorParams,
    });
  }

  return new ApiError({
    status: response.status,
    errorCode: "api.error",
    detail: response.statusText || "Request failed",
  });
}

function isMutation(method: string): boolean {
  return ["POST", "PUT", "PATCH", "DELETE"].includes(method);
}
