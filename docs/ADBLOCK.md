# Server-side ad handling

## Primary-source review (2026-10-10)

- [pixeltris/TwitchAdSolutions](https://github.com/pixeltris/TwitchAdSolutions): GitHub API confirms archived; last push 2026-03-05. VAFT browser hooks are not executed on this server.
- [ryanbr/TwitchAdSolutions](https://github.com/ryanbr/TwitchAdSolutions): not archived, last push 2026-08-27; reviewed commit `e4dfb26755cb440f9ec6a2430a9b131fa1de062a`. Its README describes fallback playlists and quality reduction. This is maintainer documentation, not our proof of a successful backup switch.
- [Streamlink Twitch documentation](https://streamlink.github.io/cli/plugins/twitch.html): 8.6.2 automatically filters embedded preroll/midroll ads when **reading a stream**, potentially pausing output. Passing a resolved raw URL to another player does not run that filter. The obsolete `--twitch-disable-ads` is not used.
- [Streamlink source](https://github.com/streamlink/streamlink/tree/8.6.2): pinned 8.6.2, tag object `f33007410e7a2b5717a095a23b4978f774a1493a`. Actual parsed fields and `TwitchHLSStreamWriter.should_filter_segment` inspected. BSD-2-Clause.
- [streamlink-ttvlol](https://github.com/2bc4/streamlink-ttvlol): considered as a playlist proxy comparison. No public proxy, external plugin download or optional proxy dependency is installed. Operators should not assume free proxies are private or trustworthy.

## Spike A versus B

| Path | Executed evidence | Decision |
|---|---|---|
| A: Go HLS gateway / backup playlists | Offline Go gateway prototype tested against saved Twitch marker fixtures: ad URIs removed, discontinuity inserted; wrong-channel/stale/codec-mismatched/unaligned backup candidates rejected. No independently verified live same-channel backup/timeline switch available | Not implemented as a playable provider. The prototype handles a fixture subset only; a URL swap would not prove codec/timestamp continuity. It is not imported by the release server. Keep the `BackupSelector` interface, no runtime backup flag |
| B: Streamlink filtered bytes → FFmpeg remux → own HLS | Controlled HTTP playlists through the real Streamlink reader, actual bytes compared; red ad frames absent after HLS remux; green/blue content resumes; monotone A/V DTS and bounded presentation-end difference; real container/live smoke | Default, `ad_handling:"pause"` |

This is a conservative decision based on the path actually exercised. A production backup implementation remains future work, not a tested feature or experimental switch available today. No feed is selected merely because a URL resolves. Future selection must verify same channel, recent healthy content, compatible codec/quality and a safe timeline boundary. Without that evidence the safe policy is to pause.

## Recognition and fail-closed behavior

Structured `EXT-X-DATERANGE` markers with `CLASS="twitch-stitched-ad"` or IDs starting `stitched-ad-` use the upstream parser's date-range classification. The legacy `EXTINF` title `Amazon`/`Amazon|…` is also recognized; random `ad` filenames and normal metadata, including an Amazon rainforest documentary title, do not trigger a substring filter. An ad range without timestamps is classified conservatively as advertising. Structured `EXT-X-CUE-OUT`/`CUE-OUT-CONT` continues ad classification until `CUE-IN` within a playlist. Twitch's original parser handles daterange end semantics.

The filtering writer discards classified segment bodies before emitting any media bytes; it may still fetch them upstream. No classified ad bytes are delivered to FFmpeg or AVPlayer. Unrecognized/unmarked advertising is outside this classification guarantee. Missing every identifying marker cannot be reconstructed into a reliable ad decision; do not claim 100% filtering.

LL prefetch segments never enter the normal reader. Complete H.264/AAC fMP4 segments with a stable `EXT-X-MAP` are filtered first, then privately remuxed to MPEG-TS without transcoding. Initialization applies across discontinuities; a different initialization body, encrypted fMP4, partial segments, preload hints and delta playlists fail closed. Initialization is capped at 1 MiB, fragments at 32 MiB, and each fragment remux has a 12-second timeout. Supported output remains complete MPEG-TS segments. Alternate renditions and encrypted MPEG-TS resources are consumed privately by the resolver/reader when supported; there is no upstream key/audio/playlist URL in output. Unsupported format failures stop acquisition, not an unfiltered fallback. Encryption is removed upstream; own TS HLS is unencrypted but access-controlled by signed URLs.

At a preroll the API waits for normal output, then returns playing. A long preroll may exhaust 90-second startup and return 503. At a midroll it keeps the remuxer/rolling manifest, stops producing segments, and resumes on normal material. The decoder sees buffering; the API's initial `playing` does not become an app-visible ad indicator. Recovery repairs TS gaps while retaining a continuous own output timeline. Some clients may eventually give up on an unusually long stall; reopening the channel is the v1 recovery mechanism.

`ADBLOCK=off` explicitly disables the filtering writer and reports `ad_handling:none`/`filtering_status:disabled`. It never continues to claim enabled. Resolver failures do not silently switch it off. Browser/client-integrity acquisition is disabled in this release; if Twitch requires it for a particular operator/channel, session creation returns a structured acquisition error. No fabricated integrity token or cookie borrowing is used. Public acquisition succeeded in the verified environment, so a Chromium profile was not necessary or delivered.

## Fixture provenance

[Streamlink's 8.6.2 tests](https://github.com/streamlink/streamlink/blob/8.6.2/tests/plugins/test_twitch.py), especially `TagDateRangeAd`, `test_hls_daterange_by_class` and `test_hls_has_preroll`, establish the structure of synthetic marker examples. [Saved example](../tests/fixtures/twitch-ad-markers.m3u8) uses invented IDs/dates/relative files and contains no real upstream token. It is **not a captured live ad break**. Its source SHA and origin are recorded beside it. Tests generate green normal / red ad / blue resumed H.264/AAC media locally.

Live ad-transition verification requires observing both `ad_break` and subsequent `playing`, then checking media output and the player's recovery. A successful short live smoke without that transition proves acquisition/playback only. See [verification](VERIFICATION.md).
