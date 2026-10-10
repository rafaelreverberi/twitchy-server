# Architecture and configuration

`Provider` resolves qualities and produces media bytes. `StreamlinkProvider` invokes a private Python worker without a shell. Twitch URLs are constructed solely from a validated channel login. `AdPolicy` selects filtering on/off; default `FilterPause` uses a conservative Twitch-aware parser and Streamlink's actual segment writer. `BackupSelector` defines a future verified-feed boundary; no selector is wired into this release. `SessionManager` reserves principal/global capacity before acquisition, starts one continuous FFmpeg remuxer per session, tracks inactivity/absolute expiry, and cancels whole child process groups. `MediaGateway` is the signed-file boundary in the Go HTTP handler. No external playlist is forwarded.

Each session resolves upstream qualities, selects `auto` as the highest available H.264 rendition or an exact requested rendition, and waits for a complete **normal** output segment before returning 201/playing/enabled. No empty success URL is returned during an all-ad preroll; bounded startup can return 503 if it lasts too long. Initial state is not a live event feed. Private anonymous worker logs only report `ad_break`/`playing`. The app observes buffering through AVPlayer; v1 has no dynamic state endpoint.

Worker output → stdin pipe → persistent FFmpeg → atomic MPEG-TS segments and rolling HLS playlist → signed Go URLs. FFmpeg never receives an upstream URL; its input protocol is pipe-only. TS timestamp discontinuities are repaired by its demuxer; AAC DTS/PTS are clamped to advance by packet duration at an overlap. Codec/initialization changes are not spliced: fMP4 maps, partial/delta HLS and LL-HLS are rejected or disabled. The output has no remote audio, init or key references. The initial discontinuity is represented in the playlist; the gateway advances discontinuity-sequence after it leaves the rolling window. Own media sequences increase within a session. Reacquisition retains the remuxer and selected quality; restart creates a fresh session/timeline.

A worker refreshes the playlist through Streamlink's bounded reload/segment retries. EOF/failure causes up to three additional token/playlist acquisitions with 2/4/8-second backoff. There is no hidden infinite mutation retry. A request can wait at most startup timeout; cancellation closes a pending session. Following 201, media requests maintain idle activity, not the control request. `DELETE` checks ownership, kills process groups, waits for shutdown, removes files and revokes all subsequent media requests. Already-started bounded segment responses may finish. A failed worker is removed by the sweeper. Server shutdown stops acceptance and cancels sessions; stale session directories are removed at startup. In-memory sessions do not survive restart.

No cross-principal upstream sharing is implemented. This deliberately avoids ownership/cache leaks at the cost of one remuxer and one bounded worker per active session. Global request concurrency is capped at 64; session-creation attempts at 10/minute/principal. Pending acquisitions count toward capacity. HTTP headers, JSON bodies, metadata output, ring buffers, per-session disk and process resources are bounded. Disk is checked every second; the 10-segment rolling window plus 3 retained segments normally stays far below the cap. The cap may briefly overshoot by data produced between checks; crossing it cancels the session. Slow output readers apply pipe/ring-buffer backpressure. No durable media cache or database is necessary.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Go listener; Compose publishes loopback only |
| `PUBLIC_BASE_URL` | `http://localhost:8080` | Explicit player-facing base including optional path prefix |
| `ALLOW_HTTP` | `false` in binary; `true` in local Compose | Explicit development HTTP exception |
| `TOKEN_FILE` | `secrets/tokens.json`; `/state/credentials/tokens.json` in image | Principal → independent bearer token map |
| `SIGNING_KEY_FILE` | `secrets/signing.key`; `/state/credentials/signing.key` in image | At least 32 random bytes |
| `MEDIA_DIR` | `state/media`; `/state/media` in image | Dedicated session files; avoid sharing with other software |
| `ADBLOCK` | `on` | `on` = filter/pause; `off` = explicitly unfiltered |
| `MAX_SESSIONS` | `8` | Global limit including pending acquisition |
| `SESSIONS_PER_PRINCIPAL` | `2` | Principal limit |
| `SESSION_LIFETIME` | `12h` | Fixed signed-media lifetime, maximum allowed 24h |
| `SESSION_IDLE_TTL` | `2m` | No valid media access → cleanup |
| `SESSION_STARTUP_TIMEOUT` | `90s` | Bounded acquisition/preroll/startup |
| `SESSION_MAX_DISK_BYTES` | `268435456` | Per-session disk guard |
| `PYTHON`, `WORKER_PATH`, `FFMPEG` | `python3`, `worker/reader.py`, `ffmpeg` | Operator-only executable paths |
| `TWITCHY_IMAGE` | `twitchy-server:local` | Compose image reference |
| `BIND_ADDR`, `PORT` | `127.0.0.1`, `8080` | Compose host binding |

Signatures bind session, resource and absolute expiry using HMAC-SHA256. Twelve hours is bounded, long enough for ordinary multi-hour viewing and necessary because the current client has no renewal endpoint. After twelve hours reopen the channel; activity only extends idle lifetime, never the signed expiry. Shorter configuration values can intentionally interrupt long viewing. Pause longer than idle TTL can end a session if AVPlayer stops requesting playlists.

## Network and privacy boundary

Public HTTPS upstreams are restricted to exact/subdomains of `twitch.tv`, `ttvnw.net`, `twitchcdn.net` and `jtvnw.net`, port 443, without userinfo. Every redirect passes the same check. At connection time all DNS addresses must be globally routable, then a checked address is connected directly; this closes the DNS-check/connect rebinding gap. Ambient proxy configuration is ignored. Local/metadata/link-local destinations, other schemes and unexpected ports are denied. There is no API field for a URL, proxy, token or arbitrary file. Fixture HTTP is only available through explicit test injection in the separate test image. Normal mode fails closed on resolver/format failures.

Source tokens are fixed-length-hashed and compared in constant time. Media signatures never contain those tokens or Twitch access tokens. API errors and worker logs suppress raw upstream exception text, which may contain sensitive URLs. Health/capabilities reveal only readiness/feature flags. No browser cookies, Apple Keychain data or app OAuth data are read. Forwarded headers are ignored. The app checkout is not modified.
