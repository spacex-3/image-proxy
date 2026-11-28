# Image Proxy

Independent image relay services and compatibility sources.

## Layout

- `az-proxy/`: deployable AZ image relay. Its Docker build context contains only this directory.
- `cangyuan/`: image-only Cangyuan task plugin snapshot. It is source-only and is not included in the AZ image.
- `ter-proxy/`: independent Terrakinc synchronous-to-asynchronous image relay and `ter` task plugin, with its own image and workflow.

- `zerofall-proxy/`: independent ZeroFall Gemini-to-asynchronous image relay and `zerofall` task plugin, with its own image and workflow.

Future providers must use a separate top-level directory, build context, image name, and workflow path filter. Changes for one provider must not alter another provider's request handling or release artifact.

Deployment policy: all customer HTTPS reference downloads for AZ, Terrakinc and ZeroFall use the existing VMess egress via its internal HTTP proxy (`REFERENCE_PROXY_URL=http://az-egress:7890`). Ter/ZeroFall provide `compose.az-egress.yaml` to join that existing network. Keep the endpoint in private deployment configuration and verify node reachability. Do not silently fall back to direct reference downloads. Base64 inputs require no download; each provider retains its upstream conversion (AZ uses R2 reference URLs, Ter/ZeroFall use inline Base64). Generation and result-download routing remain separately configured.

## AZ image

The `Publish AZ proxy image` workflow publishes:

```text
ghcr.io/spacex-3/image-proxy-az-proxy:latest
ghcr.io/spacex-3/image-proxy-az-proxy:sha-<commit>
```

The runtime configuration stays outside the repository. Never commit API keys, R2 credentials, notification URLs, task data, or production Compose environment files.
