"""Private media-byte worker. Public URLs stay private to the source; never sent to clients or logs."""
import argparse
import pathlib
import tempfile
import time
import subprocess
import hashlib
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
from streamlink.stream.http import HTTPStream
from streamlink.stream.hls import HLSStream, HLSStreamReader, HLSStreamWorker, HLSStreamWriter
import urllib3.util.connection

logging.disable(logging.CRITICAL)
LOGIN = re.compile(r"[a-zA-Z0-9_]{1,25}\Z")
DOMAINS = ("twitch.tv", "ttvnw.net", "twitchcdn.net", "jtvnw.net", "cloudfront.net")


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
        previous_map=self._map
        result=super().parse_line(line)
        # EXT-X-MAP remains applicable until replaced (RFC 8216 §4.3.2.5).
        # Keep it across a discontinuity; reject an actual init-byte change.
        if line=='#EXT-X-DISCONTINUITY': self._map=previous_map
        return result

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
            if line.startswith(("#EXT-X-PART:", "#EXT-X-PRELOAD-HINT:", "#EXT-X-SKIP:")):
                raise StreamError("unsupported upstream HLS feature")
        return super().parse(data, *args, **kwargs)


class FragmentRemuxMixin:
    init_bytes = None
    init_uri = None
    init_hash = None
    def bounded_body(self, response, limit):
        body=bytearray()
        for chunk in response.iter_content(65536):
            if len(body)+len(chunk)>limit: raise StreamError('upstream fragment size limit')
            body.extend(chunk)
        return bytes(body)

    def accept_init(self, segment, response):
        data=self.bounded_body(response,1024*1024)
        digest=hashlib.sha256(data).digest()
        if self.init_hash is not None and self.init_hash!=digest:
            raise StreamError('unsupported initialization change')
        self.init_bytes=data;self.init_uri=segment.map.uri;self.init_hash=digest

    def _write(self, segment, response, is_map):
        if not segment.map: return super()._write(segment,response,is_map)
        try:
            if segment.key and segment.key.method!='NONE': raise StreamError('unsupported encrypted fragment')
            if segment.map.key and segment.map.key.method!='NONE': raise StreamError('unsupported encrypted initialization')
            if is_map:
                if self.init_bytes is None or self.init_uri!=segment.map.uri: self.accept_init(segment,response)
                return
            if self.init_bytes is None or self.init_uri!=segment.map.uri:
                self.accept_init(segment,self.fetch_map(segment))
            fragment=self.bounded_body(response,32*1024*1024)
            # Only normal segments reach this method: the outer Twitch writer
            # filters both advertising init and fragment responses first.
            # Convert one bounded fMP4 fragment to TS without transcoding. The
            # long-lived Go remuxer then repairs TS discontinuities as usual.
            result=subprocess.run([os.environ.get('FFMPEG','ffmpeg'),'-hide_banner','-loglevel','error','-protocol_whitelist','pipe','-copyts','-i','pipe:0','-map','0:v:0','-map','0:a:0?','-c','copy','-mpegts_copyts','1','-muxdelay','0','-muxpreload','0','-f','mpegts','pipe:1'],input=self.init_bytes+fragment,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=12)
            if result.returncode or not result.stdout or len(result.stdout)>40*1024*1024: raise StreamError('fragment remux failed')
            self._write_into_buffer(iter([result.stdout]))
        except Exception:
            self.reader.fragment_failed=True
            self.close()


class DiagnosticWriter(FragmentRemuxMixin,TwitchHLSStreamWriter):
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


class UnfilteredWriter(FragmentRemuxMixin, HLSStreamWriter): pass
class UnfilteredReader(HLSStreamReader):
    __writer__ = UnfilteredWriter
class UnfilteredStream(HLSStream):
    __parser__ = ConservativeParser
    __reader__ = UnfilteredReader


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


def streams(s, login, cache_dir=None, fresh=False):
    url='https://www.twitch.tv/'+login.lower()
    if login.startswith('vod:') and re.fullmatch(r'[0-9]{1,20}',login[4:]): url='https://www.twitch.tv/videos/'+login[4:]
    elif login.startswith('clip:') and re.fullmatch(r'[a-zA-Z0-9_-]{1,199}',login[5:]): url='https://clips.twitch.tv/'+login[5:]
    elif not LOGIN.fullmatch(login): raise ValueError('invalid content identifier')
    cache = pathlib.Path(cache_dir) if cache_dir else None
    path = cache/(login.lower()+'.json') if cache else None
    if path and not fresh and not login.startswith("clip:"):
        try:
            data=json.loads(path.read_text())
            if 0 <= time.time()-data['created'] <= 15 and data['channel']==login.lower():
                urls=data['streams']
                if 0 < len(urls) <= 40 and all(re.fullmatch(r"\d+p(?:\d+)?",q) and allowed_url(url) for q,url in urls.items()):
                    return {q:TwitchHLSStream(s,url) for q,url in urls.items()}
        except (OSError,ValueError,KeyError,TypeError): pass
    # Enumerate the master once. Do not serially fetch every rendition: the
    # selected playlist is validated by its real reader before any API success.
    _, plugin_class, url = s.resolve_url(url)
    plugin=plugin_class(s,url)
    def master(url, restricted_bitrates, **params):
        try:
            return TwitchHLSStream.parse_variant_playlist(s,url,check_streams=False,low_latency=False,**params)
        except OSError as error:
            from requests.exceptions import HTTPError
            cause=error.__context__
            if isinstance(cause,HTTPError) and cause.response is not None and cause.response.status_code>=400:
                return {}
            raise
    plugin._get_hls_streams=master
    found = plugin.streams()
    result = {q: stream for q, stream in found.items()
            if re.fullmatch(r"\d+p(?:\d+)?", q) and ((isinstance(stream, HLSStream) and type(stream).__name__ == "TwitchHLSStream") or (login.startswith("clip:") and isinstance(stream, HTTPStream)))}
    if path and result and not login.startswith("clip:"):
        cache.mkdir(mode=0o700,parents=True,exist_ok=True)
        # Bounded public-channel acquisition cache, not a media/session cache.
        for entry in cache.iterdir():
            try:
                if time.time()-entry.stat().st_mtime>15: entry.unlink()
            except OSError: pass
        entries=[]
        for entry in cache.glob('*.json'):
            try: entries.append((entry.stat().st_mtime,entry))
            except OSError: pass
        for _,entry in sorted(entries)[:-31]:
            try: entry.unlink()
            except OSError: pass
        fd,tmp=tempfile.mkstemp(dir=cache)
        try:
            with os.fdopen(fd,'w') as f: json.dump({'created':time.time(),'channel':login.lower(),'streams':{q:st.url for q,st in result.items()}},f)
            os.replace(tmp,path)
        finally:
            try: os.unlink(tmp)
            except FileNotFoundError: pass
    return result


def vod_plan(s, chosen, ads):
    response=s.http.get(chosen.url,stream=True,timeout=15)
    response.raise_for_status()
    body=bytearray()
    for chunk in response.iter_content(65536):
        if len(body)+len(chunk)>8*1024*1024: raise StreamError('playlist size limit')
        body.extend(chunk)
    response.close()
    playlist=ConservativeParser(chosen.url).parse(body.decode('utf-8'))
    if not playlist.segments or len(playlist.segments)>20000: raise StreamError('playlist segment limit')
    resources={};lines=['#EXTM3U','#EXT-X-VERSION:7','#EXT-X-PLAYLIST-TYPE:VOD','#EXT-X-MEDIA-SEQUENCE:0']
    maximum=1;last_map=None;removed=False;duration=0
    for segment in playlist.segments:
        if ads and segment.ad: removed=True;continue
        if segment.key and segment.key.method!='NONE': raise StreamError('encrypted recorded playlist unsupported')
        if segment.byterange: raise StreamError('byte-range recorded playlist unsupported')
        if not allowed_url(segment.uri): raise StreamError('destination denied')
        if removed or segment.discontinuity: lines.append('#EXT-X-DISCONTINUITY')
        removed=False
        if segment.map:
            if segment.map.byterange or (segment.map.key and segment.map.key.method!='NONE'): raise StreamError('unsupported recorded initialization')
            if segment.map.uri!=last_map:
                if not allowed_url(segment.map.uri): raise StreamError('destination denied')
                name='init%09d.mp4'%len(resources);resources[name]=segment.map.uri
                lines.append('#EXT-X-MAP:URI="'+name+'"');last_map=segment.map.uri
        name='seg%09d.ts'%len(resources);resources[name]=segment.uri
        length=float(segment.duration);maximum=max(maximum,length);duration+=length
        lines.extend(['#EXTINF:%.6f,'%length,name])
    if not resources or duration<=0: raise StreamError('no normal recorded content')
    import math
    lines.insert(2,'#EXT-X-TARGETDURATION:'+str(math.ceil(maximum)))
    lines.append('#EXT-X-ENDLIST')
    return {'playlist':'\n'.join(lines)+'\n','resources':resources,'duration':duration}


def run(args):
    s = session()
    found = streams(s, args.channel, os.environ.get("RESOLVER_CACHE_DIR"), args.fresh)
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
    if args.mode == 'plan':
        plan=vod_plan(s, chosen, args.adblock=='on')
        print(json.dumps(plan))
        return 0
    if args.channel.startswith('vod:'): s.set_option('hls-live-restart',True)
    if args.channel.startswith("clip:") and isinstance(chosen,HTTPStream):
        body=bytearray()
        with chosen.open() as reader:
            while True:
                chunk=reader.read(65536)
                if not chunk: break
                if len(body)+len(chunk)>64*1024*1024: return 7
                body.extend(chunk)
        result=subprocess.run([os.environ.get('FFMPEG','ffmpeg'),'-v','error','-protocol_whitelist','pipe','-i','pipe:0','-map','0:v:0','-map','0:a:0?','-c','copy','-f','mpegts','pipe:1'],input=body,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=30)
        if result.returncode or len(result.stdout)>80*1024*1024: return 7
        sys.stdout.buffer.write(result.stdout)
        return 0
    cls = FilteredStream if args.adblock == "on" else UnfilteredStream
    stream = cls(s, chosen.url)  # Never stream.to_url() as the playback output.
    with stream.open() as reader:
        while True:
            data = reader.read(64 * 1024)
            if not data:
                if getattr(reader,"fragment_failed",False): return 7
                break
            sys.stdout.buffer.write(data)
            sys.stdout.buffer.flush()
    return 0


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["check", "probe", "run", "plan"])
    parser.add_argument("--channel")
    parser.add_argument("--fresh", action="store_true")
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
