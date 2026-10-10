# Setup and operation

## Local Docker

Run `./scripts/setup.sh` from the checkout. It requires Docker/Compose/Python 3, preserves `.env`, creates missing independent random 256-bit bearer credentials and signing key, builds, initializes the secret volume, starts and probes discovery. Token values never enter build arguments, images, logs or command-line arguments. The setup-only root helper has no network, reads the two host-owned secret files and writes a 0700/0600 directory for UID 10001; the running server has no capabilities and runs non-root. Re-running setup does not rotate credentials. Do not remove the volume when stopping.

For a previously published image, set `TWITCHY_IMAGE` to the release's immutable digest in `.env`, then `./scripts/setup.sh --image`. This pulls rather than builds. When initializing manually:

```sh
docker compose pull server
docker compose --profile setup run --rm --no-deps secrets-init
docker compose up -d --no-build server
```

Generate the local secret files with setup first. They are intentionally not shown by `docker compose config`. For multiple principals, add unique high-entropy token strings to `secrets/tokens.json`, run the setup helper to synchronize protected copies, then restart the server. Rotating the signing key invalidates all media; restarting also removes sessions. Keep credentials out of source control and backups available to other users.

`PUBLIC_BASE_URL` must be the address reachable **from the player**. It is never derived from Host, Forwarded or X-Forwarded-* headers. Those headers are ignored; no trusted-proxy configuration is needed. Media access is a bearer URL scoped to one session and one filename. The URL can be used by anyone possessing it until expiry or revocation; treat it as a secret. Disable URL access logging at any proxy.

## HTTPS with your existing domain

Point an existing domain's DNS to your own authorized host, arrange inbound TCP 80/443 and UDP 443, and use the optional Caddy profile. Do not expose Docker's API/socket. Set `.env`:

```dotenv
SOURCE_DOMAIN=your-existing-domain
PUBLIC_BASE_URL=https://your-existing-domain
ALLOW_HTTP=false
BIND_ADDR=127.0.0.1
```

```sh
docker compose --profile https up -d --no-build server caddy
```

Caddy obtains and renews a publicly trusted certificate. The server's internal Docker port remains 8080. The supplied proxy preserves any `/source` prefix; configure the same prefix in `PUBLIC_BASE_URL`. There is no access log. If your existing proxy already handles TLS, forward the prefix unchanged to port 8080 instead. Protect secrets, the state volume and administrative host access. Do not put a redirect in front of authenticated control requests or download routes: Twitchy refuses those redirects.

## Tailscale alternative

On your own Tailscale-configured host, use Tailscale Serve with HTTPS and its assigned fully-qualified `*.ts.net` hostname. Enable HTTPS certificates in your tailnet first. Run `tailscale serve --bg http://127.0.0.1:8080`, set `PUBLIC_BASE_URL=https://YOUR_ASSIGNED_NAME.YOUR_TAILNET.ts.net` and `ALLOW_HTTP=false`, then recreate the server. Confirm the current `tailscale serve --help` and `tailscale serve status` on the host. Apple clients must have tailnet connectivity. These certificates use ordinary system trust; never bypass certificate validation or substitute an untrusted self-signed certificate. No Tailscale hostname has been provisioned by this project.

## Private GHCR access and updates

The repository is created private. Publishing uses the repository-scoped Actions `GITHUB_TOKEN`, with `contents:read` and `packages:write` only in the publish job. Pull with your own classic PAT with `read:packages` and package/repository access; authorize SSO if your account's organization requires it. No runtime Twitchy source token is a registry credential. Keep the package private and review inherited repository permissions. See [GitHub registry authentication](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry).

Use `docker login --password-stdin`, an OS-backed credential helper where available, and the exact published version plus digest. Update `TWITCHY_IMAGE`, `docker compose pull server`, then `docker compose up -d --no-build server`. Restore the previous image reference for rollback. Keep old image digests available. Setup's build mode uses local source; `--image` uses the pinned image. A version tag alone is not an immutable update policy.

## Native development

After setup generates credentials, install hash-locked requirements in `.venv`, then:

```sh
ALLOW_HTTP=true PUBLIC_BASE_URL=http://localhost:18081 LISTEN_ADDR=127.0.0.1:18081 \
PYTHON="$PWD/.venv/bin/python" MEDIA_DIR="$PWD/.local/native-media" \
go run ./cmd/twitchy-server
```

Runtime dependencies are Python 3.12+/Streamlink 8.6.2 and FFmpeg with MPEG-TS, HLS, H.264/AAC support and the `setts` bitstream filter. The Go binary has no external module dependency. The container pins Go/Python base-image indices, a Debian package snapshot, and all Python wheels with hashes. Updating pins requires repeating filtering, timeline and container checks. TLS acquisition uses the standard CA bundle.
