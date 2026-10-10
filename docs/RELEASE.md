# v0.1.1 release evidence

Source commit: `9ec0f9f3ad5ae4519c4f8e309417e926263f277c` (tag `v0.1.1`). Repository and GHCR package remain private.

Verified immutable image:

```
ghcr.io/rafaelreverberi/twitchy-server:0.1.1@sha256:79cf103481bad77c4dc187004658a5984b1869102f5da2d6a82bd0144ba103a7
```

[Release CI](https://github.com/rafaelreverberi/twitchy-server/actions/runs/38039397819) passed all Go/worker/contract tests and the controlled Docker HLS smoke, verified both linux/amd64 and linux/arm64 manifests with SBOM/provenance, pulled the exact published image on native amd64, ran health/capabilities checks and confirmed private package visibility. Local arm64 Docker and actual Twitchy macOS playback/720p60 switching passed. Detailed evidence and limits are in [VERIFICATION.md](VERIFICATION.md). The app changes are local commit `d27f823`, not a published app release.

The local running server is a v0.1.1 build. For an authorized private GHCR login with read:packages, set `TWITCHY_IMAGE` to the reference above and run `docker compose pull server` followed by `docker compose up -d --no-build server`. Keep the previous digest for rollback. Release assets contain exact Debian FFmpeg corresponding source, verification metadata and SHA-256 checksums.

# v0.1.0 release evidence

Source commit: `929329e652a8c13ee5aa017916fc16bd53d6fa97` (tag `v0.1.0`).
Repository and container package are **private**; visibility was not widened.

Verified immutable GHCR reference:

```
ghcr.io/rafaelreverberi/twitchy-server:0.1.0@sha256:3ace49d2400609d4cb8f6b0414bf82c21b7e44beccadd537d75221b2a828a2a5
```

[Release-tag CI and publication](https://github.com/rafaelreverberi/twitchy-server/actions/runs/38036846328) completed successfully on 2026-10-10. The publisher inspected the remote OCI index, confirmed linux/amd64 and linux/arm64 manifests plus SBOM/provenance attestations, pulled the exact index digest on native amd64, started the actual published image and passed health/capabilities and zero-media-directory checks. This registry check is separate from the complete controlled-HLS container smoke passed on native amd64 CI and arm64 local Docker, and the additional local amd64 emulation smoke.

The local running source uses the same release source as a local arm64 build. Live Twitch HLS and macOS AVPlayer checks passed; no real ad break was observed. Detailed boundaries are in [VERIFICATION.md](VERIFICATION.md). No remote production domain/host was provisioned.

Release assets contain verification metadata, exact Debian FFmpeg corresponding source and SHA-256 checksums. The source archive itself is available from the release tag. After a private GHCR login with `read:packages`, put the reference above in `TWITCHY_IMAGE` in `.env` and run:

```sh
docker compose pull server
docker compose up -d --no-build server
```

Keep the previous digest before updating; restore it and recreate for rollback. For this first release, the tested local build is also available as `twitchy-server:local`.
