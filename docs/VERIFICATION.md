# Verification evidence

Recorded 2026-10-10, Europe/Zurich. This separates offline tests, local containers, actual Twitch acquisition and Apple playback. It does not claim a production deployment or a universal adblocking guarantee.

## Contract and isolated tests

- Imported OpenAPI 3.1 unchanged from Twitchy commit `f29ea979a08ccfa152309b01c73f7f29df1e3913`; SHA-256 `afd46e988fc51ee07f64a08966497c2fdf0f5fb6fa89a40eff1a96e5d611dfde`. Local app copy and GitHub raw snapshot matched.
- `go test -race ./...` and `go vet ./...` passed. Auth/input limits, principal/pending isolation, startup/client cancellation, signed-resource tampering/expiry, path traversal, request/creation limits, DELETE revocation, idle/absolute TTL and file cleanup exercised. Go test integration generates real H.264/AAC fixtures and decodes the full signed HTTP HLS chain with FFmpeg.
- 18 recorded API responses passed JSON Schema validation against the unchanged OpenAPI snapshot. Includes required successful discovery/playback fields, disabled download-route errors, auth/validation/limit/failure/ownership errors and bodyless DELETE. All implementation responses use the same error envelope. This is schema validation of recorded responses, not an assertion that optional download success routes exist.
- 17 nonempty responses decoded with exact original Twitchy Swift model definitions and the original snake_case/ISO8601 `JSONCoding` decoder. Optional fields were not removed to make decoding succeed.
- Ten Python worker tests passed against a controlled HTTP HLS upstream using the real pinned Streamlink parser/writer. Preroll, midroll, all-ad pause → refreshed normal media, daterange ID/class/end semantics, missing dates, cue end, misleading titles, ignored prefetch, unsupported map/partial/delta features, destination/DNS restrictions, exact byte exclusion, and red-ad-frame exclusion after HLS remux exercised. FFprobe checks monotone stream DTS and A/V presentation-end difference below 150 ms in the generated discontinuity fixture. That endpoint test does not prove long-term A/V synchronization for every upstream.
- The offline Go path-A spike removes marked fixture URIs/inserts a recovery discontinuity and rejects unsafe backup candidates; it is not linked into the server. No live backup switching is implemented or claimed.

## Container and native Apple checks

- Docker Engine/Desktop 29.7.2. Non-root Linux arm64 release image built and started through real idempotent setup; protected local credentials, named state volume, loopback publishing and healthy status verified.
- Separate smoke image has an explicit controlled provider, `live:false`, and no production fixture option. From outside the container: health → capabilities → session → master → variant → TS bytes → 10-second FFmpeg decode → DELETE → invalid media URL → zero media directories passed. Request signing is exercised without an Authorization header on media.
- Public live `eslcs`, Streamlink 8.6.2 / container FFmpeg 5.1.9, default filter/pause: qualities `160p30`, `360p30`, `480p30`, `720p60`, `1080p60` and `auto` resolved. Explicit `360p30`, ten seconds of decoded audio/video and 30 seconds of playlist refresh passed. A separate `auto` acquisition and 180-second sequence/playlist observation passed, followed by DELETE/revocation.
- macOS AVPlayer played that public feed from `http://localhost:8080`, reached `readyToPlay` and advanced its media clock five seconds; source deletion passed. `scripts/avplayer.swift` uses independently generated server credentials, never Twitchy's Keychain. It does not change the app's stored sources/accounts. This proves the AVFoundation playback path on this Mac, not a click-through of Twitchy's Sources UI.
- Initial channel-resolution experiments returned empty results. Investigation found that Streamlink's dynamic plugin loading creates a distinct Twitch class identity; checking an independently imported Twitch class with `isinstance` incorrectly discarded resolved streams. The provider now checks the common HLS base plus the expected plugin stream name. `monstercat`/`xqc` also genuinely returned upstream 404 in the inspection. Those failures were not reported as playable feeds.
- After deletion and no sessions: zero media directories; observed server usage approximately 0.01% CPU / 12.5 MiB RAM under the 768 MiB Compose limit. These are idle measurements, not capacity benchmarks.

## Live-ad, device and deployment boundaries

**No actual live ad transition was observed during the 180-second run. Live ad filtering/recovery therefore remains unverified.** Controlled ad fixtures passed; anonymous diagnostic state transitions are available for a longer operator observation. The three-minute run did not force token expiry or simulate upstream 403/429, and is not a multi-hour soak. Reacquisition/backoff code exists, but actual token renewal across Twitch expiry and prolonged live player stalls were not proved by this smoke.

No physical iPhone/iPad/Apple TV test, no Twitchy app-source UI click-through, and no remote HTTPS host/domain deployment. No optional Chromium/client-integrity profile was required in this environment or delivered. No same-channel live backup transition. Unsupported initialization/partial formats are deliberately rejected rather than spliced. The source only implements public live content and does not claim authorization for VOD/clip downloads.

The local source URL actually used is `http://localhost:8080`; token is in `secrets/tokens.json` (never in this document). Production requires your own authorized Docker host and trusted HTTPS setup.

## Reproduce

Use the commands in README for offline tests, contract/Swift decode and separate container smoke. Live tests depend on current channel availability:

```sh
python3 scripts/smoke.py --channel A_CURRENTLY_LIVE_CHANNEL --quality auto --decode --hold 180
# macOS framework playback without touching app configuration:
swiftc -parse-as-library scripts/avplayer.swift -o .local/avplayer-smoke
.local/avplayer-smoke http://localhost:8080 A_CURRENTLY_LIVE_CHANNEL
# Review anonymous ad transition diagnostics (never raw upstream URL logs):
docker compose logs server
```

The AVPlayer helper currently chooses `360p30`; choose a channel offering it or adjust that local test's quality. Release build/digest and architecture evidence are recorded in `RELEASE.md` once publication and pull verification complete. A multiarch manifest alone is not a runtime test.
