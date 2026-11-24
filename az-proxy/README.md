# AZ Image Proxy

The AZ relay accepts OpenAI-compatible asynchronous image generation and edit requests. It converts reference images to public R2 objects, polls the AZ upstream, converts Base64 results to R2 URLs, and exposes stable task results to new-api.

Supported routes:

```text
POST /v1/images/generations/async
POST /v1/images/edits/async
GET  /v1/images/tasks/{task_id}
```

The service also retains the existing generation/edit aliases used by the AZ task plugin. Runtime settings are supplied through environment variables. `UPSTREAM_PROXY_URL` optionally routes only AZ submission and result requests through an HTTP, HTTPS, or SOCKS5 proxy; reference downloads and R2 storage stay direct. Large completed results use `UPSTREAM_RESULT_TIMEOUT_SECONDS` and, after an HTTP 200 response is truncated, up to `UPSTREAM_RESULT_RACE_REQUESTS` concurrent HTTP/1.1 and HTTP/2 recovery reads.
