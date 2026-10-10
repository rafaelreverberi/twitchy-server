# Recorded playback and client downloads

Version 0.2.1 adds public VOD and clip playback. Session creation accepts `content_type: vod` with a numeric Twitch `content_id`, or `content_type: clip` with the case-sensitive clip slug. `requested_quality: auto` means Best: the server resolves the highest available supported rendition and returns `selected_quality`. Missing, private, restricted or deleted content remains unavailable; no recovery or DRM decryption is implemented.

VOD playback fetches a finite playlist and keeps a private resource map in memory. The signed client playlist includes the full timeline and ENDLIST for seeking. Complete segments and initialization data pass through bounded buffers on demand; the server does not prepare or retain an entire VOD. A still-growing broadcast is a snapshot of the archive at opening; reopen for newly archived content. Encrypted and byte-range playlists fail closed. Clip playback temporarily remuxes a bounded clip to local HLS segments, removed with the session. Neither path transcodes; clients decode the media.

The updated app exposes Download on individual videos and clips, using its default quality. Downloads begin only when explicitly requested. The Downloads tab shows transferred bytes, speed, cancellation, retry, local deletion and offline playback. Unknown file length uses an indeterminate progress indicator. Completed MP4 files and their index live in the app's local Downloads directory, not on the server. Retry restarts the transfer; no partial-file resume or automatic recording is claimed. Apple TV supports playback but not persistent downloads.

## Capability extension to the imported v1 contract

The original contract under `docs/contract/` remains an immutable reference. Its prepared-file download model is extended explicitly by `client_side_downloads: true` together with `downloads: true` and `authorized_downloads_only: false`. Only updated clients opt into this mode. Legacy sources requiring independently issued permission references retain the existing client checks; an arbitrary client string is never presented as rights proof.

In this public recorded-media mode POST `/v1/downloads` accepts `content_type` (vod/clip), `content_id`, and optional `requested_quality` (default auto). `authorization_reference` is unnecessary. `ready` means the authenticated owner can start a transfer, not that a complete file is stored. GET `/file` launches filtered acquisition and FFmpeg copy-remux into a fragmented H.264/AAC MP4 response. Content-Disposition and video/mp4 are supplied; unknown Content-Length is omitted. No upstream URL or token reaches the client. No redirects are allowed. Late failures abort HTTP so the client cannot mark a partial MP4 complete.

Every job operation checks principal ownership. Metadata jobs expire after one hour (cancelled jobs after one minute); capacity is 32 globally and eight per principal. Two concurrent clip acquisitions/download transfers share a gate; active downloads also count toward configured session limits. Cancellation, disconnect and server shutdown terminate process groups. Download bytes pass through pipes and bounded worker buffers, with no full video file on server disk. Older clips with trailing MP4 metadata use a bounded anonymous Linux RAM file to provide seekable FFmpeg input; it is closed after remux.

With ADBLOCK on, the same conservative marker parser removes recognized ad segments from VOD playback and downloading and inserts a discontinuity where needed. Clips use the resolved clip media without a separate ad-insertion player. Unmarked or creator-embedded sponsorship cannot be automatically identified or removed. Existing muted sections are preserved.

## Small Linux hosts

Both linux/amd64 (including 64-bit Intel NUC Linux) and linux/arm64 are targeted. No GPU is required. Start with two total sessions and observe CPU, memory and bandwidth; no physical NUC capacity has been measured. Clip input is capped at 64 MiB, remux output at 80 MiB, and concurrent recorded acquisitions at two. VOD playlists are capped at 20,000 segments; on-demand media has at most four simultaneous buffers of 32 MiB (initialization 1 MiB). The 768 MiB container ceiling remains enforced. See [host guidance](SMALL_LINUX_HOST.md).

Archive file responses refresh their write deadline with progress instead of inheriting the short playback startup deadline. A blocked socket write has a 30-second budget; the overall transfer is bounded to 12 hours, matching the updated client resource timeout.
