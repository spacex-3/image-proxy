# Image Proxy

Independent image relay services and compatibility sources.

## Layout

- `az-proxy/`: deployable AZ image relay. Its Docker build context contains only this directory.
- `cangyuan/`: image-only Cangyuan task plugin snapshot. It is source-only and is not included in the AZ image.
- `ter-proxy/`: independent Terrakinc synchronous-to-asynchronous image relay and `ter` task plugin, with its own image and workflow.

- `zerofall-proxy/`: independent ZeroFall Gemini-to-asynchronous image relay and `zerofall` task plugin, with its own image and workflow.

Future providers must use a separate top-level directory, build context, image name, and workflow path filter. Changes for one provider must not alter another provider's request handling or release artifact.

Reference download policy: AZ, Ter and ZeroFall support the independent `reference-worker/` Cloudflare Worker. Configure `REFERENCE_FETCH_URL` and the private `REFERENCE_FETCH_KEY` together to use it for all customer HTTPS reference downloads without VMess. This mode overrides the reference proxy only; generation/result routing remains independent. Existing VMess deployments can still use `REFERENCE_PROXY_URL` and the Ter/ZeroFall `compose.az-egress.yaml` overrides. Neither mode silently falls back to direct reference fetching. Base64 inputs require no download; each provider retains its upstream conversion (AZ uses R2 reference URLs, Ter/ZeroFall use inline Base64).

## AZ image

The `Publish AZ proxy image` workflow publishes:

```text
ghcr.io/spacex-3/image-proxy-az-proxy:latest
ghcr.io/spacex-3/image-proxy-az-proxy:sha-<commit>
```

The runtime configuration stays outside the repository. Never commit API keys, R2 credentials, notification URLs, task data, or production Compose environment files.
