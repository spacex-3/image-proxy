# AZ Image Proxy

The AZ relay accepts OpenAI-compatible asynchronous image generation and edit requests. It converts reference images to public R2 objects, polls the AZ upstream, converts Base64 results to R2 URLs, and exposes stable task results to new-api.

Supported routes:

```text
POST /v1/images/generations/async
POST /v1/images/edits/async
GET  /v1/images/tasks/{task_id}
```

The service also retains the existing generation/edit aliases used by the AZ task plugin. Runtime settings are supplied through environment variables. `UPSTREAM_PROXY_URL` optionally routes AZ submission and result requests through an HTTP, HTTPS, or SOCKS5 proxy. `REFERENCE_PROXY_URL` independently routes validated public reference-image downloads through a proxy. R2 storage always stays direct. Large completed results use `UPSTREAM_RESULT_TIMEOUT_SECONDS` and, after an HTTP 200 response is truncated, up to `UPSTREAM_RESULT_RACE_REQUESTS` concurrent HTTP/1.1 and HTTP/2 recovery reads.

## Cloudflare reference downloads

Set `REFERENCE_FETCH_URL` and `REFERENCE_FETCH_KEY` (or its `_FILE` variant) together to use the independent `../reference-worker/` for HTTPS references. Worker mode bypasses VMess for reference downloads only, streams binary images and preserves the existing upstream image format and R2 flow. It takes precedence over `REFERENCE_PROXY_URL`; Base64 input is unchanged. No new-api plugin or core update is required.
