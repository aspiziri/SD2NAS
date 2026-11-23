import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { api, getHealth, ApiError } from './api.js';

let calls;
function respond(body, init) {
  globalThis.fetch = async (url, opts) => {
    calls.push({ url, opts });
    return new Response(body, init);
  };
}
beforeEach(() => { calls = []; });

test('GET sends no guard header', async () => {
  respond('{"ok":true}', { status: 200 });
  assert.deepEqual(await api('/api/staging'), { ok: true });
  assert.equal(calls[0].opts.method, 'GET');
  assert.equal(calls[0].opts.headers['X-SD2NAS'], undefined);
});

test('every non-GET sends X-SD2NAS: 1', async () => {
  for (const method of ['POST', 'PATCH', 'PUT', 'DELETE']) {
    respond('{}', { status: 200 });
    await api('/api/x', { method, body: { a: 1 } });
    const { opts } = calls.at(-1);
    assert.equal(opts.headers['X-SD2NAS'], '1', method);
    assert.equal(opts.headers['Content-Type'], 'application/json');
    assert.equal(opts.body, '{"a":1}');
  }
});

test('a JSON error becomes ApiError with the server message', async () => {
  respond('{"error":"no ingest 9"}', { status: 404 });
  await assert.rejects(api('/api/ingests/9'), (err) => err instanceof ApiError && err.status === 404 && err.message === 'no ingest 9');
});

test('a non-JSON error uses the status line', async () => {
  respond('oops', { status: 500, statusText: 'Internal Server Error' });
  await assert.rejects(api('/api/x'), { name: 'ApiError', status: 500, message: '500 Internal Server Error' });
});

test('a 200 that is not JSON is an error', async () => {
  respond('<!doctype html>', { status: 200 });
  await assert.rejects(api('/api/x'), { name: 'ApiError', status: 200 });
});

test('a network failure is status 0', async () => {
  globalThis.fetch = async () => { throw new TypeError('fetch failed'); };
  await assert.rejects(api('/api/x'), (err) => err.status === 0 && err.message.startsWith('SD2NAS is not reachable'));
});

test('getHealth reports a network failure as "not reachable", which the banner shows as is', async () => {
  globalThis.fetch = async () => { throw new TypeError('Failed to fetch'); };
  await assert.rejects(getHealth(), (err) =>
    err instanceof ApiError && err.status === 0 && err.message.startsWith('SD2NAS is not reachable:') && err.message.includes('Failed to fetch'));
});

test('getHealth returns the report for 503 too', async () => {
  respond('{"status":"degraded","problems":["catalog: boom"]}', { status: 503 });
  assert.deepEqual(await getHealth(), { status: 'degraded', problems: ['catalog: boom'] });
});
