# Image Proxy

Independent image relay services and compatibility sources.

## Layout

- `az-proxy/`: deployable AZ image relay. Its Docker build context contains only this directory.
- `cangyuan/`: image-only Cangyuan task plugin snapshot. It is source-only and is not included in the AZ image.
- `ter-proxy/`: independent Terrakinc synchronous-to-asynchronous image relay and `ter` task plugin, with its own image and workflow.

- `zerofall-proxy/`: independent ZeroFall Gemini-to-asynchronous image relay and `zerofall` task plugin, with its own image and workflow.

Future providers must use a separate top-level directory, build context, image name, and workflow path filter. Changes for one provider must not alter another provider's request handling or release artifact.

## AZ image

The `Publish AZ proxy image` workflow publishes:

```text
ghcr.io/spacex-3/image-proxy-az-proxy:latest
ghcr.io/spacex-3/image-proxy-az-proxy:sha-<commit>
```

The runtime configuration stays outside the repository. Never commit API keys, R2 credentials, notification URLs, task data, or production Compose environment files.
