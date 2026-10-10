# twitchy-server

A self-hosted live HLS source for [Twitchy](https://github.com/rafaelreverberi/Twitchy), with independent source authentication and server-side ad filtering. Go owns the control API, sessions and signed media gateway. Streamlink **8.6.2 reads the stream bytes** and filters marked ads; FFmpeg remuxes H.264/AAC into server-owned MPEG-TS HLS. During ads, output pauses until normal content returns. No Apple browser extension is required.

**Default:** filtering on, filter/pause mode. Backup playlist switching is disabled. Public live streams are supported; VODs, clips and downloads are disabled. `auto` selects the highest available H.264 video quality at session creation; an explicit quality starts a new session. LL-HLS is disabled even if requested. Unmarked advertising cannot be reliably identified. No promise of uninterrupted or universally ad-free playback.

GitHub stores the code; GHCR stores the image. Neither hosts the running video server. Run it on your own Docker-capable Mac, Linux computer, NAS or authorized VPS. No production host or domain has been deployed.

## Start locally

Requirements: Docker Engine/Desktop with Compose, Python 3 for local secret generation, free TCP port 8080. Setup only creates this project's containers and volumes; it does not install Docker or stop unrelated containers.

```sh
git clone https://github.com/rafaelreverberi/twitchy-server.git
cd twitchy-server
./scripts/setup.sh
python3 scripts/smoke.py --channel A_CURRENTLY_LIVE_CHANNEL --decode
```

Setup preserves `.env` and existing secrets. It builds the image, uses a one-shot offline helper to install protected credentials into the project volume, starts the non-root server, and checks health/capabilities. A live channel is required for the **separate media check**; an offline channel is not a broken server. Credentials are in `secrets/tokens.json`, principal `local`. Never paste them into issues or logs. Original files remain mode 0600, directory 0700. The server reads a UID-10001-owned copy from the Docker volume.

In Twitchy: **Sources → Add**. Name: `Local Twitchy Server`; Base URL: `http://localhost:8080`; enable the local HTTP development exception; select **Bearer** authentication and enter the `local` value from the secret file. **Test → Select → open a live channel**. The Source password is separate from Twitch login. No Twitch access/refresh token is needed or accepted as an intentional credential.

On iPhone, iPad or Apple TV, `localhost` means that device. For LAN development, set `BIND_ADDR=0.0.0.0` and `PUBLIC_BASE_URL=http://YOUR_SERVER_LAN_IP:8080` in `.env`, then `docker compose up -d server`. Use that exact base URL in Twitchy. Use HTTPS for regular use; see [HTTPS setup](docs/SETUP.md). Base paths such as `https://your-domain/source` are preserved on all control and signed media URLs. No bearer header is needed on media requests.

## Start, update and roll back

```sh
# Start an already configured local build
docker compose up -d --no-build server
# Build updated source, retaining secrets and configuration
git pull --ff-only
./scripts/setup.sh
# Stop only this project's runtime; retain credentials and volumes
docker compose stop server
```

Release images are published as `ghcr.io/rafaelreverberi/twitchy-server:VERSION@sha256:DIGEST`. Obtain the **verified digest** from the release and Actions publish summary. Do not use `latest`.

```sh
# For a private package, use your own GitHub PAT (classic), read:packages only,
# with access to the private repository/package. Token goes to stdin.
read -r GHCR_READ_TOKEN
printf '%s' "$GHCR_READ_TOKEN" | docker login ghcr.io -u YOUR_GITHUB_USER --password-stdin
unset GHCR_READ_TOKEN
# Save the current image reference before an update.
# Set TWITCHY_IMAGE in .env to the exact VERSION@sha256:DIGEST, then:
docker compose pull server
docker compose up -d --no-build server
# Rollback: restore the previous exact TWITCHY_IMAGE reference in .env, then:
docker compose up -d --no-build server
```

A restart ends current sessions. Twitchy must open the channel again. To initialize a release image on a new machine, copy `.env.example` to `.env`, set `TWITCHY_IMAGE`, and run `./scripts/setup.sh --image`; see [setup details](docs/SETUP.md).

## Verify

```sh
python3 -m venv .venv
.venv/bin/pip install --require-hashes -r requirements-test.lock
mkdir -p .local
CONTRACT_REPORT="$PWD/.local/contract-responses.json" go test -race ./...
go vet ./...
WORKER_DIAGNOSTICS=off .venv/bin/python -m unittest discover -s tests -v
.venv/bin/python scripts/check_contract.py .local/contract-responses.json
# macOS with Swift installed:
./scripts/check_swift.sh
# Isolated test image: controlled provider, honest live:false; no Twitch access.
docker build --target smoke -t twitchy-server:smoke .
./scripts/container_smoke.sh
```

The release binary cannot enable the fixture provider. The separate smoke target exercises HTTP → signed HLS → FFmpeg decode → DELETE → cleanup. Worker tests additionally prove ad bytes/pixels are absent, pause/resume works, and a raw URL does not activate filtering. See [verification evidence and boundaries](docs/VERIFICATION.md), [ad handling](docs/ADBLOCK.md), [architecture/configuration](docs/ARCHITECTURE.md), [troubleshooting](docs/TROUBLESHOOTING.md), and [third-party notices](THIRD_PARTY_NOTICES.md).

Small Linux hosts / Intel NUC and the Best quality selector: [deployment notes](docs/SMALL_LINUX_HOST.md).
