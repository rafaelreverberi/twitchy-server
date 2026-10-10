"""Private media-byte worker. Never returns upstream URLs or credentials to Go/clients."""
import argparse
import ipaddress
import json
import logging
import os
import re
import socket
import sys
from urllib.parse import urlsplit

from streamlink import Streamlink
from streamlink.exceptions import StreamError
from streamlink.plugins.twitch import TwitchHLSStream, TwitchHLSStreamReader, TwitchHLSStreamWorker, TwitchHLSStreamWriter, TwitchM3U8Parser
from streamlink.stream.hls import HLSStream, HLSStreamReader, HLSStreamWorker
import urllib3.util.connection

logging.disable(logging.CRITICAL)
LOGIN = re.compile(r"[a-zA-Z0-9_]{1,25}\Z")
DOMAINS = ("twitch.tv", "ttvnw.net", "twitchcdn.net", "jtvnw.net")


def allowed_url(url):
    p = urlsplit(url)
    return (p.scheme == "https" and not p.username and not p.password
            and p.port in (None, 443) and p.hostname is not None
            and any(p.hostname == d or p.hostname.endswith("." + d) for d in DOMAINS))


def secure_network(session):
    """Validate each redirect at send, and pin a checked IP at connection time."""
    original_send = session.http.send
    session.upstream_status = 0
    session.upstream_retry_after = 30
    session.http.trust_env = False
    def send(request, **kwargs):
        if not allowed_url(request.url):
            raise StreamError("upstream destination denied")
        response = original_send(request, **kwargs)
        if response.status_code in (403, 429) or response.status_code >= 500:
            session.upstream_status = response.status_code
            try: session.upstream_retry_after = max(1, min(300, int(response.headers.get('Retry-After', '30'))))
            except ValueError: session.upstream_retry_after = 30
        return response
    session.http.send = send
    def connect(address, timeout=socket._GLOBAL_DEFAULT_TIMEOUT, source_address=None, socket_options=None):
        host, port = address
        if not any(host == d or host.endswith("." + d) for d in DOMAINS) or port != 443:
            raise OSError("upstream destination denied")
        candidates = socket.getaddrinfo(host, port, type=socket.SOCK_STREAM)
        if not candidates or any(not ipaddress.ip_address(c[4][0]).is_global or ipaddress.ip_address(c[4][0]).is_multicast for c in candidates):
            raise OSError("non-public upstream address denied")
        last = None
        for family, typ, proto, _, addr in candidates:
            sock = socket.socket(family, typ, proto)
            try:
                if timeout is not socket._GLOBAL_DEFAULT_TIMEOUT:
                    sock.settimeout(timeout)
                for option in socket_options or []:
                    sock.setsockopt(*option)
                if source_address:
                    sock.bind(source_address)
                sock.connect(addr)
                return sock
            except OSError as error:
                last = error
                sock.close()
        raise last or OSError("connect failed")
    urllib3.util.connection.create_connection = connect


class ConservativeParser(TwitchM3U8Parser):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, **kwargs)
        self.cue_ad = False

    def parse_line(self, line):
        if line.startswith(("#EXT-X-CUE-OUT:", "#EXT-X-CUE-OUT-CONT:")) or line == "#EXT-X-CUE-OUT":
            self.cue_ad = True
        elif line == "#EXT-X-CUE-IN":
            self.cue_ad = False
        return super().parse_line(line)

    def _is_segment_ad(self, date, title=None):
        # The legacy Twitch EXTINF marker is Amazon, not arbitrary filename text.
        marked = bool(title and (title == "Amazon" or title.startswith("Amazon|")))
        ranges = self.m3u8.dateranges_ads
        return self.cue_ad or marked or (bool(ranges) and date is None) or any(
            self.m3u8.is_date_in_daterange(date, r) for r in ranges)

    def parse(self, data, *args, **kwargs):
        # Own TS output only. Reject unsupported formats rather than exposing an
        # unfiltered init/partial/rendition/key URL or incoherent fMP4 timeline.
        text = data if isinstance(data, str) else data.text
        for line in text.splitlines():
            if line.startswith(("#EXT-X-MAP:", "#EXT-X-PART:", "#EXT-X-PRELOAD-HINT:", "#EXT-X-SKIP:")):
                raise StreamError("unsupported upstream HLS feature")
        return super().parse(data, *args, **kwargs)


class DiagnosticWriter(TwitchHLSStreamWriter):
    was_ad = False
    def should_filter_segment(self, segment):
        ad = super().should_filter_segment(segment)
        if ad != self.was_ad:
            if os.environ.get('WORKER_DIAGNOSTICS', 'on') == 'on':
                print(json.dumps({'state': 'ad_break' if ad else 'playing'}), file=sys.stderr, flush=True)
            self.was_ad = ad
        return ad


class FilteredReader(TwitchHLSStreamReader):
    __writer__ = DiagnosticWriter


class FilteredStream(TwitchHLSStream):
    __parser__ = ConservativeParser
    __reader__ = FilteredReader


class UnfilteredStream(HLSStream):
    # Same supported-format checks, without the Twitch filtering writer.
    __parser__ = ConservativeParser


def session(test=False):
    s = Streamlink()
    for key, value in {
        "http-timeout": 8, "http-query-params": {}, "hls-live-edge": 3,
        "hls-segment-timeout": 8, "hls-segment-attempts": 2,
        "hls-playlist-reload-attempts": 3, "stream-timeout": 30,
        "ringbuffer-size": 4 * 1024 * 1024, "webbrowser": False,
        "hls-segment-threads": 1, "hls-segment-stream-data": True,
    }.items():
        s.set_option(key, value)
    if not test:
        secure_network(s)
    return s


def streams(s, login):
    if not LOGIN.fullmatch(login):
        raise ValueError("invalid login")
    found = s.streams("https://www.twitch.tv/" + login.lower())
    return {q: stream for q, stream in found.items()
            if re.fullmatch(r"\d+p(?:\d+)?", q) and isinstance(stream, HLSStream) and type(stream).__name__ == "TwitchHLSStream"}


def run(args):
    s = session()
    found = streams(s, args.channel)
    qualities = sorted(found, key=lambda q: tuple(int(n) for n in re.findall(r"\d+", q)))
    if not qualities:
        status = getattr(s, 'upstream_status', 0)
        error = 'rate_limited' if status == 429 else 'source_unavailable' if status else 'content_unavailable'
        if args.mode == 'probe':
            print(json.dumps({'error': error, 'retry_after': getattr(s, 'upstream_retry_after', 30)}))
        return 4
    if args.mode == "probe":
        print(json.dumps({"qualities": ["auto"] + qualities}))
        return 0
    q = qualities[-1] if args.quality == "auto" else args.quality
    if q not in found:
        return 5
    chosen = found[q]
    cls = FilteredStream if args.adblock == "on" else UnfilteredStream
    stream = cls(s, chosen.url)  # Never stream.to_url() as the playback output.
    with stream.open() as reader:
        while True:
            data = reader.read(64 * 1024)
            if not data:
                break
            sys.stdout.buffer.write(data)
            sys.stdout.buffer.flush()
    return 0


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["check", "probe", "run"])
    parser.add_argument("--channel")
    parser.add_argument("--quality", default="auto")
    parser.add_argument("--adblock", choices=["on", "off"], default="on")
    args = parser.parse_args()
    if args.mode == "check":
        from importlib.metadata import version
        sys.exit(0 if version("streamlink") == "8.6.2" else 1)
    try:
        sys.exit(run(args))
    except BrokenPipeError:
        sys.exit(0)
    except Exception:
        # No exception text: upstream exceptions may contain token-bearing URLs.
        if args.mode == "probe":
            print(json.dumps({"error": "source_unavailable"}))
        sys.exit(6)
