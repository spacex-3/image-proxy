# Terrakinc image relay

Independent service, task plugin (`ter`) and image build. It never imports, builds or changes `az-proxy/` or `cangyuan/`. Keep the existing native Gemini channel; create a separate type-61 channel for this plugin.

The relay returns HTTP 202 after persisting a task. Background workers call Terrakinc's synchronous OpenAI Images API once, then store the returned Base64 or downloaded image in R2. Polling never resubmits a generation. The new-api plugin preserves synchronous Images calls as well as the installed host's `async: true` mode.

## Requests

Submit to `/v1/images/generations` or `/v1/images/edits` with `async: true`, as with Cangyuan. The plugin also provides `/ter/v1/images/generations` and `/ter/v1/images/edits` for explicit asynchronous submission. Poll `/v1/images/generations/{task_id}` or `/v1/images/edits/{task_id}` with the same new-api token.

```json
{
  "model": "gemini-3.1-flash-image-preview",
  "prompt": "Change the background to a beach",
  "size": "1:1",
  "quality": "2k",
  "n": 1,
  "async": true,
  "images": [{"url": "https://example.com/reference.png"}]
}
```

- Flash aliases: `gemini-3.1-flash-image-preview` → `gemini-3.1-flash-image`.
- Pro aliases: `gemini-3-pro-image-preview` → `gemini-3.0-pro-image`.
- `n=1`, up to 10 references, 20 MiB combined decoded references.
- Resolutions: `1k`, `2k`, `4k`; `size` accepts ratios or pixel dimensions. Default: square 1K. `quality=2k, size=1:1` becomes `2048x2048`.
- Reference strings, `{url}`, `{image_url}`, nested `{image_url:{url}}`, data URLs and plugin multipart file uploads are accepted. Reference URLs are downloaded by the worker, validated and sent upstream as Base64 data URLs in JSON. Credentials are never forwarded to image hosts.
- `output_format` accepts common compatibility hints; it does not transcode images. Results preserve actual MIME types. Both upstream Base64 and URL responses are stored in R2.
- The new-api plugin uses gateway artifact URLs, which follow the site's existing media/R2 delivery policy.

## Configuration and deployment

Build and publish through `.github/workflows/publish-ter-proxy.yml`. Images support amd64 and arm64. ARM runs published images only. Use an independent Compose project with `compose.yaml`; it attaches only to the existing new-api network and publishes no host port. The image initializes its named data volume with UID 65532; a custom bind mount must grant that UID write access.

Configure outside the repository in a mode-0600 `ter-proxy.env`:

- `UPSTREAM_BASE_URL`, `UPSTREAM_API_KEY`: Terrakinc endpoint and channel credential.
- `PROXY_API_KEY`: an independent randomly generated key of at least 32 characters; use it as the new type-61 channel key with Base URL `http://ter-proxy:8080`.
- `PUBLIC_MEDIA_URL`, `R2_ENDPOINT`, `R2_BUCKET`, `R2_ACCESS_KEY`, `R2_SECRET_KEY`; optional `R2_REGION` (default `auto`). The dedicated object prefix is `ter-proxy/`.
- Optional `BARK_URL`: notify on terminal failures, including quota codes and processing stage. Never include it in public configuration.
- Optional `UPSTREAM_PROXY_URL`, `REFERENCE_PROXY_URL`: independent proxy settings, default direct.
- `UPSTREAM_WORKERS` (default 2), `QUEUE_LIMIT` (default 16 active plus queued tasks), `REFERENCE_TIMEOUT_SECONDS` (120), `TASK_TIMEOUT_SECONDS` (900), `TASK_RETENTION_SECONDS` (604800).
- Secret variables also support `_FILE` paths. Never put upstream or storage credentials in the plugin.

Configure provider-specific billing under `ter::<model>` using `image_count` and `resolution` (`1K`, `2K`, `4K`). Verify current upstream cost and site group ratios before enabling a customer channel; prices are deliberately not hardcoded. Failure status and quota codes reach the existing host refund and alert paths.

Task metadata records queue, reference preparation, upstream headers/body, output download and R2 upload timings. Prompts and references are private spool files, deleted at terminal status. Queued tasks resume after restart; started synchronous requests become `upstream_interrupted` and are never automatically replayed because completion and upstream charging may be unknown. Terminal tasks and their output objects expire after retention. No other provider's objects are deleted.

Bearer authentication uses constant-time digest comparison and applies to every submit/query route. Only `/healthz` is public. Deploy on the private Docker network behind new-api, which owns customer authentication and TLS. Upstream redirects are not followed; reference downloads reject private addresses, credentials and HTTPS downgrades. Error logs redact configured secrets and URL paths. Security references: OWASP ASVS 5.0 authentication/access controls, Authentication Cheat Sheet and Session Management Cheat Sheet. This is not a claim of full ASVS certification.

Validation: `go test ./...`, `go vet ./...`, `node --test plugin.test.mjs`.
