# Reference image fetch Worker

An independent Cloudflare Worker shared by AZ, Ter and ZeroFall. It downloads a customer's public HTTPS reference and streams image bytes back to the private relay. Ter/ZeroFall encode those bytes for their upstreams; AZ preserves its existing R2 reference-URL conversion. It does not generate images, touch customer billing, store images, or replace the task artifact Worker.

Deploy `worker.mjs` with `wrangler.toml`, then set `REFERENCE_FETCH_KEY` as a Worker secret. Use a randomly generated secret of at least 32 bytes; never commit it or expose it to customers. The only binding is this secret. Do not add private-network or service bindings. Keep `global_fetch_strictly_public` enabled.

Configure each relay outside Git:

- `REFERENCE_FETCH_URL=https://<worker-host>/v1/reference`
- `REFERENCE_FETCH_KEY` or `REFERENCE_FETCH_KEY_FILE` with the same secret

Both settings must be configured together. Worker mode takes precedence over `REFERENCE_PROXY_URL`, uses direct HTTPS to the Worker, and never silently falls back to VMess or direct image fetching. Base64 references bypass the Worker. Generation/result-download routing remains independent; in particular AZ's upstream proxy is not removed by this change.

Requests use POST JSON `{ "url": "https://image-host/path" }` and HMAC-SHA256 over `timestamp + "\n" + exactBody`. Send the Unix-seconds timestamp and hex signature in `X-Reference-Timestamp` and `X-Reference-Signature`. Signatures expire after 60 seconds (including clock skew); an identical request can be replayed within this short window. Fetching is read-only and does not create billable generation work. HTTPS prevents network disclosure; application logs do not record request bodies or signed URLs. Worker observability is disabled. `/healthz` is public; image fetch requires a valid signature.

Only public HTTPS DNS targets are accepted, including explicit HTTPS ports such as 8443 or 8888. Reject IP literals, credentials, fragments and private-style hostnames; revalidate up to four redirects. Cloudflare's public fetch boundary is used without private bindings. Forward only the image Accept header, never a customer token, upstream key, signature, cookie or Referer. Enforce a 45-second fetch deadline, 20 MiB image limit, and PNG/JPEG/WebP/GIF MIME allowlist. The relay validates actual image bytes and its own size limit. Bodies stream to avoid Worker memory amplification; converting Base64 in the relay avoids ~33% extra transfer.

Verification: `node --test worker.test.mjs`; each relay's Go tests cover signatures, expiry/configuration boundaries, no proxy fallback, no credential forwarding, redirects and size bounds. These controls follow the applicable OWASP ASVS 5.0 authentication/access-control guidance and Authentication/Session Management Cheat Sheets; this is not a full ASVS compliance claim. Source-host anti-bot rules, expired signed links or source outages can still prevent downloads. Benchmark actual customer URLs before claiming an incident resolved.

References: https://developers.cloudflare.com/workers/runtime-apis/streams/ and https://developers.cloudflare.com/workers/platform/limits/.
