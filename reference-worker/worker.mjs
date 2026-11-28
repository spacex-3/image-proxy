const MAX_BODY = 16 * 1024;
const MAX_IMAGE = 20 * 1024 * 1024;
const TIMEOUT_MS = 90_000;
const TYPES = new Set(['image/png', 'image/jpeg', 'image/webp', 'image/gif']);
const utf8 = new TextEncoder();

function error(status, code) {
  return Response.json({ error: { code } }, { status, headers: { 'Cache-Control': 'no-store' } });
}

function targetURL(value, selfHost) {
  const u = new URL(value);
  const host = u.hostname.toLowerCase();
  // Public HTTPS DNS names only. The deployed Worker also uses
  // global_fetch_strictly_public, has no private-network/service bindings,
  // and revalidates every redirect. Credentials are never forwarded.
  if (u.protocol !== 'https:' || u.username || u.password || u.hash ||
      !host.includes('.') || host.endsWith('.') || host.includes(':') || /^[0-9.]+$/.test(host) ||
      /(^|\.)(localhost|local|internal|invalid|test)$/.test(host) || host === selfHost) {
    throw new Error('invalid_reference_url');
  }
  return u;
}

async function limitedBody(request) {
  if (!request.body) throw new Error('invalid_request');
  const reader = request.body.getReader();
  let size = 0;
  const chunks = [];
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.length;
      if (size > MAX_BODY) throw new Error('request_too_large');
      chunks.push(value);
    }
  } catch (e) { await reader.cancel().catch(() => {}); throw e; }
  const data = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { data.set(chunk, offset); offset += chunk.length; }
  return new TextDecoder('utf-8', { fatal: true }).decode(data);
}

export async function handle(request, env, fetcher = fetch, now = Date.now) {
  const route = new URL(request.url);
  if (route.pathname === '/healthz' && request.method === 'GET') return new Response('ok');
  if (route.pathname !== '/v1/reference' || request.method !== 'POST' || route.search) return error(404, 'not_found');
  if (typeof env.REFERENCE_FETCH_KEY !== 'string' || env.REFERENCE_FETCH_KEY.length < 32) return error(503, 'not_configured');
  const timestamp = request.headers.get('X-Reference-Timestamp') || '';
  const signature = request.headers.get('X-Reference-Signature') || '';
  if (!/^\d{10}$/.test(timestamp) || Math.abs(now() / 1000 - Number(timestamp)) > 60 || !/^[a-f0-9]{64}$/.test(signature)) return error(401, 'unauthorized');
  let raw;
  try { raw = await limitedBody(request); } catch { return error(400, 'invalid_request'); }
  const key = await crypto.subtle.importKey('raw', utf8.encode(env.REFERENCE_FETCH_KEY), { name: 'HMAC', hash: 'SHA-256' }, false, ['verify']);
  const sig = Uint8Array.from(signature.match(/../g), (v) => parseInt(v, 16));
  if (!await crypto.subtle.verify('HMAC', key, sig, utf8.encode(timestamp + '\n' + raw))) return error(401, 'unauthorized');
  let target;
  try {
    const body = JSON.parse(raw);
    if (!body || typeof body.url !== 'string' || body.url.length > 8192 || Object.keys(body).some((k) => k !== 'url')) throw new Error();
    target = targetURL(body.url, route.hostname);
  } catch { return error(400, 'invalid_reference_url'); }
  const abort = new AbortController();
  const timer = setTimeout(() => abort.abort(), TIMEOUT_MS);
  let response;
  try {
    for (let redirects = 0; ; redirects++) {
      response = await fetcher(target.toString(), {
        method: 'GET', redirect: 'manual', signal: abort.signal, cache: 'no-store',
        headers: { Accept: 'image/png,image/jpeg,image/webp,image/gif' },
      });
      if (![301, 302, 303, 307, 308].includes(response.status)) break;
      await response.body?.cancel();
      if (redirects >= 4 || !response.headers.get('Location')) throw new Error('invalid_redirect');
      target = targetURL(new URL(response.headers.get('Location'), target).toString(), route.hostname);
    }
    if (response.status !== 200 || !response.body) {
      await response.body?.cancel();
      clearTimeout(timer);
      return error(502, 'reference_http_' + response.status);
    }
    const mime = (response.headers.get('Content-Type') || '').split(';')[0].trim().toLowerCase();
    if (!TYPES.has(mime)) throw new Error('unsupported_image_type');
    if (Number(response.headers.get('Content-Length')) > MAX_IMAGE) throw new Error('image_too_large');
    // Fetch may decode a compressed response, so its wire length is usable only
    // for identity encoding. Preserve it outside Content-Length because edge
    // streaming can otherwise hide a short body behind a normal EOF.
    const lengthHeader = response.headers.get('Content-Length');
    const encoding = (response.headers.get('Content-Encoding') || 'identity').toLowerCase();
    const expected = encoding === 'identity' && /^\d+$/.test(lengthHeader || '') ? Number(lengthHeader) : null;
    const headers = { 'Content-Type': mime, 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' };
    if (expected !== null) headers['X-Reference-Content-Length'] = String(expected);
    const reader = response.body.getReader();
    let bytes = 0;
    const stream = new ReadableStream({
      async pull(controller) {
        try {
          const { done, value } = await reader.read();
          if (abort.signal.aborted || (done && expected !== null && bytes !== expected)) throw new Error('incomplete_image');
          if (done) { clearTimeout(timer); controller.close(); return; }
          bytes += value.byteLength;
          if (bytes > MAX_IMAGE || (expected !== null && bytes > expected)) throw new Error('image_too_large');
          controller.enqueue(value);
        } catch {
          clearTimeout(timer); abort.abort(); await reader.cancel().catch(() => {});
          controller.error(new Error('reference_stream_failed'));
        }
      },
      async cancel() { clearTimeout(timer); abort.abort(); await reader.cancel().catch(() => {}); },
    });
    return new Response(stream, { headers });
  } catch (e) {
    clearTimeout(timer); abort.abort(); await response?.body?.cancel().catch(() => {});
    const code = ['invalid_redirect', 'unsupported_image_type', 'image_too_large'].includes(e.message) ? e.message : 'reference_fetch_failed';
    return error(502, code);
  }
}

export default { fetch(request, env) { return handle(request, env); } };
