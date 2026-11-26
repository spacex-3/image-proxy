import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';
import { test } from 'node:test';

const plugin = await import('data:text/javascript;base64,' + Buffer.from(readFileSync(new URL('./plugin.js', import.meta.url))).toString('base64'));
const decode = (body, operation = 'generate') => plugin.protocols.openai_image.decodeRequest({ model: body.model || 'gemini-3.1-flash-image-preview', operation, body: { kind: 'json', value: { model: 'gemini-3.1-flash-image-preview', prompt: 'A circle', ...body } } });

test('Cangyuan size and quality normalize to the billed resolution', () => {
  for (const [size, quality, expected, resolution] of [['1:1', '2k', '2048x2048', '2K'], ['3:4', '4k', '3072x4096', '4K'], [undefined, undefined, '1024x1024', '1K']]) {
    const intent = decode({ size, quality, async: true });
    assert.equal(intent.requestBody.size, expected);
    const ctx = { ...intent, apiKey: 'test', baseUrl: 'http://ter-proxy:8080', upstreamModel: 'gemini-3.1-flash-image' };
    const request = plugin.buildSubmitRequest(ctx);
    assert.equal(request.url, 'http://ter-proxy:8080/v1/images/generations/async');
    assert.equal(request.body.model, 'gemini-3.1-flash-image');
    assert.deepEqual(plugin.extractUsage(ctx), { image_count: 1, resolution });
  }
});

test('reference aliases, nested image_url, Base64 and file uploads', () => {
  const ref = 'https://example.com/reference.png';
  for (const images of [[{ url: ref }], [{ image_url: ref }], [{ image_url: { url: ref } }], [ref]]) {
    assert.equal(decode({ images }, 'edit').requestBody.images[0].image_url, ref);
  }
  const data = 'data:image/png;base64,YWJj';
  assert.equal(decode({ reference: data }, 'edit').requestBody.images[0].image_url, data);
  const file = { field: 'image', ref: 'request_file:image', mimeType: 'image/png', size: 1234 };
  const intent = plugin.native.editImage({ model: 'gemini-3.1-flash-image', body: { kind: 'multipart', fields: { model: ['gemini-3.1-flash-image'], prompt: ['Blue circle'], quality: ['2k'] }, files: [file] } });
  assert.equal(intent.requestBody.images[0].image_url.__fileRef, file.ref);
  assert.equal(plugin.buildSubmitRequest({ ...intent, files: [file], baseUrl: 'http://ter-proxy:8080' }).body.images[0].image_url.encoding, 'dataUrl');
});

test('reject unsupported counts and formats before upstream work', () => {
  for (const n of [-1, 0, 2, 1.5, '1', 1e100]) assert.throws(() => decode({ n }));
  assert.throws(() => decode({ quality: 'high' }));
  assert.throws(() => decode({}, 'edit'));
  assert.throws(() => decode({ images: [{ url: 'https://user:password@example.com/image' }] }, 'edit'));
  assert.throws(() => decode({ images: Array(11).fill('https://example.com/image') }, 'edit'));
});

test('failure codes survive without duplicate prefixes; image metadata and URLs are correct', () => {
  const failed = plugin.parseTaskResult({}, { status: 'failed', error: { code: 'insufficient_quota', type: 'upstream_error', message: 'Insufficient balance' } });
  assert.equal(failed.status, 'FAILURE');
  assert.match(failed.reason, /insufficient_quota/);
  assert.equal(plugin.parseTaskResult({}, { status: 'unexpected' }).status, 'UNKNOWN');
  assert.equal(plugin.parseTaskResult({}, { status: 'completed', data: [] }).status, 'UNKNOWN');
  const task = { task_id: 'task_public', status: 'SUCCESS', data: { data: [{ url: 'https://r2.example/image.jpg', mime_type: 'image/jpeg' }] } };
  assert.deepEqual(plugin.listArtifacts(task), [{ key: 'image-1', type: 'image', mimeType: 'image/jpeg' }]);
  assert.deepEqual(plugin.extractUsageOnComplete({}, { status: 'SUCCESS' }, task.data), { image_count: 1 });
  const response = plugin.protocols.openai_image.render({ body: { value: {} }, artifacts: { 'image-1': { url: 'https://gateway.example/content' } } }, task);
  assert.equal(response.data[0].url, 'https://gateway.example/content');
  assert.deepEqual(plugin.buildContentRequest({ artifactKey: 'image-1', data: task.data, clientRequest: { method: 'GET' } }), { url: 'https://r2.example/image.jpg', method: 'GET', credentialless: true });
});
