/**
 * 唯一的后端请求客户端。协议见 frontend/前端设计.md 第 3 节：
 * - 同源 /api/v1，浏览器自动携带 HttpOnly 会话 Cookie，不使用 Authorization。
 * - 非 GET 请求携带 X-CSRF-Token；csrf_failed 时重取 token 并用同一幂等键重发一次。
 * - 幂等键由调用方按"一次用户意图"生成并在重试时复用。
 * - If-Match 由调用方显式传入，取自详情 ETag 或列表 revision。
 */
import type { AxiosError, AxiosRequestConfig } from 'axios';

import axios from 'axios';
import { z } from 'zod';

import { useAppConfig } from '@vben/hooks';

const { apiURL } = useAppConfig(import.meta.env, import.meta.env.PROD);

export const API_BASE = apiURL || '/api/v1';

export type ErrorDetails = Record<string, unknown>;

/** 后端统一信封：{data, request_id} 或 {error:{code,message,details}, request_id}。 */
const Envelope = z
  .object({
    data: z.unknown().optional(),
    error: z
      .object({
        code: z.string().catch(''),
        details: z.record(z.unknown()).nullable().catch(null),
        message: z.string().catch(''),
      })
      .optional(),
    request_id: z.string().catch(''),
  })
  .partial()
  .catch({});

type ErrorBody = NonNullable<z.infer<typeof Envelope>['error']>;

const CsrfPayload = z.object({ csrf_token: z.string() });

/** 后端错误：保留 HTTP 状态、错误码、request_id 与 Retry-After。 */
export class ApiError extends Error {
  code: string;
  details: ErrorDetails | null;
  requestId: string;
  retryAfter: null | number;
  status: number;

  constructor(
    status: number,
    body: ErrorBody | undefined,
    requestId: string,
    retryAfter: null | number,
  ) {
    super(body?.message || `HTTP ${status}`);
    this.status = status;
    this.code = body?.code || (status === 0 ? 'network_error' : 'http_error');
    this.details = body?.details ?? null;
    this.requestId = requestId;
    this.retryAfter = retryAfter;
  }
}

export interface ApiResult<T> {
  data: T;
  etag: null | string;
  location: null | string;
  requestId: string;
  status: number;
}

export interface RequestOptions {
  /** 写请求的 body；无参数动作传 {}。 */
  body?: unknown;
  /** If-Match，格式 "rev-N"（含双引号）。 */
  ifMatch?: null | string;
  /** Idempotency-Key；POST/DELETE（登录退出除外）必需。 */
  idempotencyKey?: string;
  params?: Record<string, boolean | number | string | undefined>;
  /** 401 时不触发全局登录过期处理（登录、启动探测等）。 */
  silentAuth?: boolean;
  timeout?: number;
}

const client = axios.create({
  baseURL: API_BASE,
  timeout: 30_000,
  withCredentials: true,
});

let csrfToken = '';
let csrfPromise: null | Promise<string> = null;
let unauthenticatedHandler: (() => void) | null = null;

export function onUnauthenticated(handler: () => void) {
  unauthenticatedHandler = handler;
}

/** 获取（或复用进行中的）CSRF token。 */
export async function refreshCsrf(): Promise<string> {
  if (!csrfPromise) {
    csrfPromise = client
      .get<unknown>('/auth/csrf')
      .then((r) => {
        const parsed = CsrfPayload.safeParse(Envelope.parse(r.data).data);
        csrfToken = parsed.success ? parsed.data.csrf_token : '';
        return csrfToken;
      })
      .finally(() => {
        csrfPromise = null;
      });
  }
  return csrfPromise;
}

export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}

/** 由列表 revision 构造 If-Match。 */
export function revisionTag(revision: number | string | undefined): string {
  return `"rev-${String(revision)}"`;
}

function toApiError(error: AxiosError<unknown>): ApiError {
  const r = error.response;
  if (!r) {
    return new ApiError(
      0,
      { code: 'network_error', details: null, message: '网络连接失败' },
      '',
      null,
    );
  }
  const retry = Number(r.headers?.['retry-after']);
  const envelope = Envelope.parse(r.data);
  return new ApiError(
    r.status,
    envelope.error,
    envelope.request_id ?? '',
    Number.isFinite(retry) ? retry : null,
  );
}

async function send<T>(
  method: string,
  url: string,
  options: RequestOptions,
): Promise<ApiResult<T>> {
  const headers: Record<string, string> = {};
  const isWrite = method !== 'GET';
  if (isWrite) {
    if (!csrfToken) {
      await refreshCsrf();
    }
    headers['Content-Type'] = 'application/json';
    headers['X-CSRF-Token'] = csrfToken;
  }
  if (options.idempotencyKey) {
    headers['Idempotency-Key'] = options.idempotencyKey;
  }
  if (options.ifMatch) {
    headers['If-Match'] = options.ifMatch;
  }
  const config: AxiosRequestConfig = {
    data: isWrite ? JSON.stringify(options.body ?? {}) : undefined,
    headers,
    method,
    params: options.params,
    timeout: options.timeout,
    url,
  };
  const r = await client.request<unknown>(config);
  const envelope = Envelope.parse(r.data);
  const csrf = CsrfPayload.safeParse(envelope.data);
  if (csrf.success) {
    csrfToken = csrf.data.csrf_token;
  }
  const etag = r.headers?.etag;
  const location = r.headers?.location;
  return {
    // Payload shapes are fixed per endpoint by the backend contract; typed API modules declare T.
    data: envelope.data as T,
    etag: typeof etag === 'string' ? etag : null,
    location: typeof location === 'string' ? location : null,
    requestId: envelope.request_id ?? '',
    status: r.status,
  };
}

export async function request<T = unknown>(
  method: 'DELETE' | 'GET' | 'PATCH' | 'POST' | 'PUT',
  url: string,
  options: RequestOptions = {},
): Promise<ApiResult<T>> {
  const needsKey =
    (method === 'POST' || method === 'DELETE') && !url.startsWith('/auth/');
  if (needsKey && !options.idempotencyKey) {
    throw new Error(`Idempotency-Key required for ${method} ${url}`);
  }
  try {
    return await send<T>(method, url, options);
  } catch (error) {
    if (!axios.isAxiosError(error)) throw error;
    let err = toApiError(error);
    // CSRF 中间件在业务逻辑前拒绝，重取 token 后用同一个幂等键重发一次是安全的。
    if (err.code === 'csrf_failed' && method !== 'GET') {
      await refreshCsrf();
      try {
        return await send<T>(method, url, options);
      } catch (retryError) {
        if (!axios.isAxiosError(retryError)) throw retryError;
        err = toApiError(retryError);
      }
    }
    if (
      err.status === 401 &&
      err.code === 'unauthenticated' &&
      !options.silentAuth
    ) {
      unauthenticatedHandler?.();
    }
    throw err;
  }
}

export const api = {
  delete: <T = unknown>(url: string, o: RequestOptions = {}) =>
    request<T>('DELETE', url, o),
  get: <T = unknown>(url: string, o: RequestOptions = {}) =>
    request<T>('GET', url, o),
  patch: <T = unknown>(url: string, o: RequestOptions = {}) =>
    request<T>('PATCH', url, o),
  post: <T = unknown>(url: string, o: RequestOptions = {}) =>
    request<T>('POST', url, o),
  put: <T = unknown>(url: string, o: RequestOptions = {}) =>
    request<T>('PUT', url, o),
};

/** WebSocket 地址：后端返回的 websocket_path 已含 /api/v1 前缀。 */
export function websocketURL(path: string): string {
  const scheme = location.protocol === 'https:' ? 'wss://' : 'ws://';
  return scheme + location.host + path;
}
