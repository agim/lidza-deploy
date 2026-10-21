export class APIError extends Error {
  status: number;
  code: string;
  constructor(message: string, status: number, code = '') {
    super(message);
    this.status = status;
    this.code = code;
  }
}

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(`/api/${path.replace(/^\//, '')}`, {
    cache: 'no-store',
    ...init,
    headers: { 'Content-Type': 'application/json', ...init.headers },
  });
  if (response.status === 401) {
    location.href = `/login.html?next=${encodeURIComponent(location.pathname + location.search + location.hash)}`;
    throw new APIError('Please sign in.', 401);
  }
  let data: unknown = {};
  try { data = await response.json(); } catch { /* empty response */ }
  if (!response.ok) {
    const value = data as { error?: string | { message?: string; code?: string }; code?: string };
    const message = typeof value.error === 'string' ? value.error : value.error?.message;
    throw new APIError(message || `Request failed (${response.status})`, response.status, value.code || (typeof value.error === 'object' ? value.error?.code : '') || '');
  }
  return data as T;
}

export const get = <T>(path: string) => api<T>(path);
export const post = <T>(path: string, body: unknown = {}) => api<T>(path, { method: 'POST', body: JSON.stringify(body) });
export const put = <T>(path: string, body: unknown = {}) => api<T>(path, { method: 'PUT', body: JSON.stringify(body) });
export const patch = <T>(path: string, body: unknown = {}) => api<T>(path, { method: 'PATCH', body: JSON.stringify(body) });
export const del = <T>(path: string, body?: unknown) => api<T>(path, { method: 'DELETE', body: body === undefined ? undefined : JSON.stringify(body) });
