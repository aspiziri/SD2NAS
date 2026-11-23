// fetch wrapper for the SD2NAS API. Every request that isn't GET or HEAD
// carries X-SD2NAS: 1, without which the server's request guard refuses it.
// Failures become ApiError with the server's message.
export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.name = 'ApiError';
    this.status = status; // 0 = no response (network failure)
  }
}

function parseJSON(text) {
  try {
    return { ok: true, value: JSON.parse(text) };
  } catch (error) {
    return { ok: false, error };
  }
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
  const parsed = parseJSON(await res.text());
  if (!res.ok) {
    const msg = parsed.ok && parsed.value && typeof parsed.value.error === 'string' ? parsed.value.error : `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg);
  }
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
