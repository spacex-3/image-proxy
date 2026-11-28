import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHmac, webcrypto } from 'node:crypto';
import { handle } from './worker.mjs';
if (!globalThis.crypto) globalThis.crypto = webcrypto;
const env = { REFERENCE_FETCH_KEY: 'test-only-secret-'.repeat(3) };
const now = 1795840000000;
function request(url, options = {}) {
  const body = JSON.stringify({ url });
  const timestamp = String(Math.floor(now / 1000) + (options.offset || 0));
  const sig = createHmac('sha256', options.key || env.REFERENCE_FETCH_KEY).update(timestamp + '\n' + body).digest('hex');
  return new Request('https://worker.example/v1/reference', { method: 'POST', body: options.tamper ? body + ' ' : body, headers: { 'X-Reference-Timestamp': timestamp, 'X-Reference-Signature': sig, Authorization: 'Bearer must-not-forward' } });
}
test('signed request streams image bytes without forwarding secrets or cookies', async () => {
  let calls = 0;
  const result = await handle(request('https://images.example/a.png'), env, async (url, init) => {
    calls++;
    assert.equal(url, 'https://images.example/a.png');
    assert.equal(init.redirect, 'manual');
    assert.deepEqual(Object.keys(init.headers), ['Accept']);
    return new Response(new Uint8Array([1, 2, 3]), { headers: { 'Content-Type': 'image/png', 'Set-Cookie': 'private' } });
  }, () => now);
  assert.equal(result.status, 200);
  assert.equal(result.headers.get('Set-Cookie'), null);
  assert.deepEqual([...new Uint8Array(await result.arrayBuffer())], [1, 2, 3]);
  assert.equal(calls, 1);
});
test('expired, forged and tampered requests never fetch', async () => {
  for (const options of [{ offset: -61 }, { offset: 61 }, { key: 'wrong' }, { tamper: true }]) {
    const result = await handle(request('https://images.example/a', options), env, () => { throw new Error('must not fetch'); }, () => now);
    assert.equal(result.status, 401);
  }
});
test('reject private targets, credentials, non-HTTPS and unsafe redirects', async () => {
  for (const url of ['http://images.example/a', 'https://127.0.0.1/a', 'https://[::1]/a', 'https://localhost/a', 'https://a.internal/a', 'https://user:pass@images.example/a', 'https://worker.example/a']) {
    const r = await handle(request(url), env, () => { throw new Error('must not fetch'); }, () => now);
    assert.equal(r.status, 400, url);
  }
  let calls = 0;
  const r = await handle(request('https://images.example/a'), env, async () => { calls++; return new Response(null, { status: 302, headers: { Location: 'http://127.0.0.1/secret' } }); }, () => now);
  assert.equal(r.status, 502);
  assert.equal(calls, 1);
});
test('reject non-images and oversized responses, including unknown-length streams', async () => {
  for (const headers of [{ 'Content-Type': 'text/html' }, { 'Content-Type': 'image/png', 'Content-Length': String(21 * 1024 * 1024) }]) {
    const r = await handle(request('https://images.example/a'), env, async () => new Response('x', { headers }), () => now);
    assert.equal(r.status, 502);
  }
  const r = await handle(request('https://images.example/a'), env, async () => new Response(new Uint8Array(20 * 1024 * 1024 + 1), { headers: { 'Content-Type': 'image/png' } }), () => now);
  await assert.rejects(r.arrayBuffer());
});
