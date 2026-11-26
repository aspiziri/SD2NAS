// fetch wrapper for the SD2NAS API. Every request that isn't GET or HEAD
// carries X-SD2NAS: 1, without which the server's request guard refuses it.
// Failures become ApiError with the server's message.
export class ApiError extends Error {
  constructor(status, message, data = null) {
    super(message);
    this.name = 'ApiError';
    this.status = status; // 0 = no response (network failure)
    this.data = data; // the parsed JSON error body ({error, code, …}) or null
  }
}

function parseJSON(text) {
  try {
    return { ok: true, value: JSON.parse(text) };
  } catch (error) {
    return { ok: false, error };
  }
}

let onUnauthorized = null;

// setUnauthorizedHandler registers fn to run when the server answers 401
// token_required (the cookie expired or the password changed).
export function setUnauthorizedHandler(fn) {
  onUnauthorized = fn;
}

async function send(path, init) {
  try {
    return await fetch(path, init);
  } catch (err) {
    if (err.name === 'AbortError') throw err;
    throw new ApiError(0, `SD2NAS is not reachable: ${err.message}`);
  }
}

export async function api(path, { method = 'GET', body, signal } = {}) {
  const headers = { Accept: 'application/json' };
  if (method !== 'GET' && method !== 'HEAD') headers['X-SD2NAS'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await send(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal });
  const text = await res.text();
  if (!res.ok) {
    const parsed = parseJSON(text);
    const data = parsed.ok && parsed.value && typeof parsed.value === 'object' ? parsed.value : null;
    const msg = data && typeof data.error === 'string' ? data.error : `${res.status} ${res.statusText}`;
    if (res.status === 401 && data?.code === 'token_required') onUnauthorized?.();
    throw new ApiError(res.status, msg, data);
  }
  // 204, or an endpoint that answers success with no body.
  if (res.status === 204 || text.trim() === '') return null;
  const parsed = parseJSON(text);
  if (!parsed.ok) throw new ApiError(res.status, `${path} did not answer with JSON: ${parsed.error.message}`);
  return parsed.value;
}

// getHealth returns /healthz's report: it is JSON for 200 (ok) and 503 (degraded).
export async function getHealth() {
  const res = await send('/healthz', { headers: { Accept: 'application/json' } });
  const parsed = parseJSON(await res.text());
  if (!parsed.ok) throw new ApiError(res.status, `/healthz answered ${res.status} without JSON`);
  return parsed.value;
}
