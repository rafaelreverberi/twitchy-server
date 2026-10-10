"""Real Streamlink writer tests against a controlled HTTP HLS upstream."""
import concurrent.futures
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import pathlib
import socket
import subprocess
import tempfile
import threading
import time
import unittest
from unittest.mock import patch
import sys
sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))
from worker.reader import ConservativeParser, FilteredStream, UnfilteredStream, allowed_url, secure_network, session, streams, vod_plan

HEADER = '#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n'
AD = '#EXT-X-DATERANGE:ID="stitched-ad-fixture",CLASS="twitch-stitched-ad",START-DATE="2026-10-10T00:00:02Z",DURATION=2\n'

def segment(i, title='live', date=True, discontinuity=False):
    return ('#EXT-X-DISCONTINUITY\n' if discontinuity else '') + (f'#EXT-X-PROGRAM-DATE-TIME:2026-10-10T00:00:{i*2:02d}Z\n' if date else '') + f'#EXTINF:2,{title}\n{i}.ts\n'


class Upstream:
    def __init__(self, playlist, bodies=None):
        self.playlist = playlist
        self.bodies = bodies or {0: b'NORMAL-BEFORE', 1: b'AD-RED', 2: b'NORMAL-AFTER'}
        self.hits = []
        self.status = 200
        upstream = self
        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args): pass
            def do_GET(self):
                upstream.hits.append(self.path)
                if upstream.status != 200:
                    self.send_response(upstream.status); self.end_headers(); return
                if self.path == '/index.m3u8':
                    data = upstream.playlist.encode()
                else:
                    try: data = upstream.bodies[self.path[1:]] if self.path[1:] in upstream.bodies else upstream.bodies[int(self.path[1:-3])]
                    except (KeyError, ValueError):
                        self.send_response(404); self.end_headers(); return
                self.send_response(200); self.send_header('Content-Length', str(len(data))); self.end_headers(); self.wfile.write(data)
        self.http = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.thread = threading.Thread(target=self.http.serve_forever, daemon=True)
        self.thread.start()
        self.url = f'http://127.0.0.1:{self.http.server_port}/index.m3u8'
    def close(self): self.http.shutdown(); self.http.server_close(); self.thread.join()


def read(upstream, filtered=True):
    s = session(test=True); s.set_option('hls-live-edge', 10); s.set_option('stream-timeout', 3)
    stream = (FilteredStream if filtered else UnfilteredStream)(s, upstream.url)
    data = b''
    with stream.open() as reader:
        while True:
            chunk = reader.read(65536)
            if not chunk: break
            data += chunk
    return data


class WorkerTests(unittest.TestCase):
    def test_run_hls_is_not_mistaken_for_clip_http_stream(self):
        import io
        from types import SimpleNamespace
        from streamlink.plugins.twitch import TwitchHLSStream
        from worker.reader import run
        s = session(test=True)
        chosen = TwitchHLSStream(s, 'https://cdn.ttvnw.net/index.m3u8')
        for target in ['example', 'vod:123']:
            output = io.BytesIO()
            fake = SimpleNamespace(open=lambda: io.BytesIO(b'NORMAL-HLS'))
            args = SimpleNamespace(channel=target, mode='run', quality='auto', fresh=False, adblock='on')
            with patch('worker.reader.session', return_value=s), patch('worker.reader.streams', return_value={'360p30':chosen}), patch('worker.reader.FilteredStream', return_value=fake), patch('worker.reader.sys.stdout', SimpleNamespace(buffer=output)):
                self.assertEqual(run(args), 0)
            self.assertEqual(output.getvalue(), b'NORMAL-HLS')

    @unittest.skipUnless(hasattr(__import__('os'), 'memfd_create'), 'Linux anonymous media file')
    def test_clip_tail_moov_remux_has_full_video_and_audio(self):
        from worker.reader import remux_clip
        with tempfile.TemporaryDirectory() as tmp:
            path = pathlib.Path(tmp)/'clip.mp4'
            subprocess.run(['ffmpeg','-v','error','-f','lavfi','-i','color=c=blue:s=160x90:r=25:d=2','-f','lavfi','-i','sine=frequency=440:sample_rate=48000:duration=2','-c:v','libx264','-threads','1','-c:a','aac',str(path)],check=True)
            body = path.read_bytes()
            self.assertGreater(body.find(b'moov'), body.find(b'mdat'))
            data = remux_clip(body)
            pixels = subprocess.check_output(['ffmpeg','-v','error','-f','mpegts','-i','pipe:0','-vf','scale=1:1','-pix_fmt','rgb24','-f','rawvideo','pipe:1'],input=data)
            self.assertGreaterEqual(len(pixels), 45*3)

    def test_clip_short_reads_are_collected_until_eof(self):
        import io
        from types import SimpleNamespace
        from streamlink.stream.http import HTTPStream
        from worker.reader import run
        s = session(test=True)
        chosen = HTTPStream(s, 'https://cdn.twitchcdn.net/clip.mp4')
        class ShortReader(io.BytesIO):
            def read(self, size=-1): return super().read(min(size, 3))
        output = io.BytesIO()
        args = SimpleNamespace(channel='clip:Example', mode='run', quality='auto', fresh=False, adblock='on')
        with patch('worker.reader.session', return_value=s), patch('worker.reader.streams', return_value={'360p30':chosen}), patch.object(chosen, 'open', return_value=ShortReader(b'COMPLETE-MP4')), patch('worker.reader.remux_clip', return_value=b'TS-BYTES') as remux, patch('worker.reader.sys.stdout', SimpleNamespace(buffer=output)):
            self.assertEqual(run(args), 0)
            self.assertEqual(remux.call_args.args[0], b'COMPLETE-MP4')
        self.assertEqual(output.getvalue(), b'TS-BYTES')

    def test_recorded_plan_filters_ads_and_retains_full_seek_timeline(self):
        upstream = Upstream(HEADER + AD + segment(0) + segment(1) + segment(2) + '#EXT-X-ENDLIST\n')
        try:
            chosen = type('Chosen', (), {'url': upstream.url})()
            with patch('worker.reader.allowed_url', return_value=True):
                plan = vod_plan(session(test=True), chosen, True)
                self.assertEqual(plan['duration'], 4)
                self.assertEqual(len(plan['resources']), 2)
                self.assertTrue(all(not u.endswith('/1.ts') for u in plan['resources'].values()))
                self.assertIn('#EXT-X-DISCONTINUITY', plan['playlist'])
                self.assertIn('#EXT-X-ENDLIST', plan['playlist'])
                self.assertNotIn(upstream.url.split('/index')[0], plan['playlist'])
                unfiltered = vod_plan(session(test=True), chosen, False)
                self.assertEqual(unfiltered['duration'], 6)
                self.assertEqual(len(upstream.hits), 2)  # Only manifests; no archived media fetched.
        finally: upstream.close()

    def test_recorded_plan_rejects_encryption_ranges_and_bad_destinations(self):
        for tag in ['#EXT-X-KEY:METHOD=AES-128,URI="key"', '#EXT-X-BYTERANGE:188@0', '']:
            upstream = Upstream(HEADER + tag + '\n' + segment(0) + '#EXT-X-ENDLIST\n')
            try:
                chosen = type('Chosen', (), {'url': upstream.url})()
                with patch('worker.reader.allowed_url', return_value=bool(tag)):
                    with self.assertRaises(Exception): vod_plan(session(test=True), chosen, True)
            finally: upstream.close()

    def test_midroll_actual_bytes_and_raw_url_does_not_filter(self):
        upstream = Upstream(HEADER + AD + segment(0) + segment(1) + segment(2, discontinuity=True) + '#EXT-X-ENDLIST\n')
        try:
            self.assertEqual(read(upstream), b'NORMAL-BEFORENORMAL-AFTER')
            self.assertIn(b'AD-RED', read(upstream, filtered=False))
        finally: upstream.close()

    def test_preroll_title_marker_and_no_false_positive(self):
        upstream = Upstream(HEADER + segment(0, 'Amazon|preroll') + segment(1, 'Amazon rainforest documentary') + segment(2, 'road adventure') + '#EXT-X-ENDLIST\n')
        try: self.assertEqual(read(upstream), b'AD-REDNORMAL-AFTER')
        finally: upstream.close()

    def test_all_ad_refresh_pause_and_resume(self):
        # All-ad initial playlist; no output until normal media arrives.
        ad = AD.replace('00:00:02Z', '00:00:00Z').replace('DURATION=2', 'DURATION=4')
        upstream = Upstream(HEADER + ad + segment(0) + segment(1))
        try:
            with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
                future = pool.submit(read, upstream)
                time.sleep(.5)
                self.assertFalse(future.done())
                upstream.playlist = HEADER + ad + segment(0) + segment(1) + segment(2, discontinuity=True) + '#EXT-X-ENDLIST\n'
                self.assertEqual(future.result(timeout=10), b'NORMAL-AFTER')
        finally: upstream.close()

    def test_missing_dates_fail_closed_and_unknown_metadata(self):
        p = ConservativeParser('https://cdn.ttvnw.net/index.m3u8')
        parsed = p.parse(HEADER + AD + segment(0, date=False) + segment(1, date=False))
        self.assertTrue(all(s.ad for s in parsed.segments))
        parsed = ConservativeParser().parse(HEADER + AD.replace('stitched-ad-fixture', 'road-trip').replace('twitch-stitched-ad', 'chapter') + segment(0) + segment(1))
        self.assertTrue(all(not s.ad for s in parsed.segments))

    def test_prefetch_never_reaches_output(self):
        upstream = Upstream(HEADER + segment(0) + '#EXT-X-TWITCH-PREFETCH:1.ts\n#EXT-X-ENDLIST\n')
        try:
            self.assertEqual(read(upstream), b'NORMAL-BEFORE')
            self.assertNotIn('/1.ts', upstream.hits)
        finally: upstream.close()

    def test_upstream_403_429_and_broken_feed_fail_without_bytes(self):
        for status in [403,429,503]:
            upstream = Upstream(HEADER+segment(0)+'#EXT-X-ENDLIST\n');upstream.status=status
            try:
                try: data = read(upstream)
                except Exception: data = b''
                self.assertEqual(data, b'')
                self.assertLessEqual(len(upstream.hits), 5)
            finally: upstream.close()

    def test_unsupported_partial_init_delta_fail_closed(self):
        for tag in ['#EXT-X-PART:DURATION=1,URI="1.ts"', '#EXT-X-PRELOAD-HINT:TYPE=PART,URI="1.ts"', '#EXT-X-SKIP:SKIPPED-SEGMENTS=1']:
            with self.assertRaises(Exception): ConservativeParser().parse(HEADER + tag + '\n' + segment(0))

    def test_ssrf_allowlist_redirect_and_dns(self):
        for url in ['http://gql.twitch.tv/x','https://127.0.0.1/x','https://gql.twitch.tv.evil.test/x','https://user:pass@gql.twitch.tv/x','https://gql.twitch.tv:444/x','file:///etc/passwd','https://169.254.169.254/x']:
            self.assertFalse(allowed_url(url))
        self.assertTrue(allowed_url('https://gql.twitch.tv/gql'))
        import urllib3.util.connection
        original_connect = urllib3.util.connection.create_connection
        self.addCleanup(setattr, urllib3.util.connection, "create_connection", original_connect)
        s = session(test=True); secure_network(s)
        import requests
        with self.assertRaises(Exception): s.http.send(requests.Request('GET','https://127.0.0.1/x').prepare())
        import urllib3.util.connection
        with patch('socket.getaddrinfo', return_value=[(socket.AF_INET, socket.SOCK_STREAM, 6, '', ('127.0.0.1', 443))]):
            with self.assertRaises(OSError): urllib3.util.connection.create_connection(('gql.twitch.tv', 443))

    def test_cue_end_and_saved_provenance_example(self):
        upstream = Upstream(HEADER + segment(0) + '#EXT-X-CUE-OUT:2\n' + segment(1) + '#EXT-X-CUE-IN\n' + segment(2) + '#EXT-X-ENDLIST\n')
        try: self.assertEqual(read(upstream), b'NORMAL-BEFORENORMAL-AFTER')
        finally: upstream.close()
        example = pathlib.Path(__file__).parent/'fixtures/twitch-ad-markers.m3u8'
        parsed = ConservativeParser('https://cdn.ttvnw.net/index.m3u8').parse(example.read_text())
        self.assertEqual([s.ad for s in parsed.segments], [False,True,False])

    def test_daterange_id_only_and_open_end(self):
        marker = AD.replace('twitch-stitched-ad','chapter')
        parsed = ConservativeParser().parse(HEADER + marker + segment(0) + segment(1) + segment(2))
        self.assertEqual([s.ad for s in parsed.segments], [False,True,False])
        marker = AD.replace(',DURATION=2', '')
        parsed = ConservativeParser().parse(HEADER + marker + segment(0) + segment(1) + segment(2))
        self.assertEqual([s.ad for s in parsed.segments], [False,True,True])

    def test_fragmented_mp4_init_filter_remux_and_ad_pixels(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=pathlib.Path(tmp)
            cmd=['ffmpeg','-v','error']
            for color in ['green','red','blue']: cmd+=['-f','lavfi','-i',f'color=c={color}:s=160x90:r=25:d=2']
            cmd+=['-f','lavfi','-i','sine=frequency=440:sample_rate=48000','-filter_complex','[0:v][1:v][2:v]concat=n=3:v=1:a=0[v]','-map','[v]','-map','3:a','-t','6','-c:v','libx264','-pix_fmt','yuv420p','-threads','1','-g','50','-c:a','aac','-f','hls','-hls_time','2','-hls_segment_type','fmp4','-hls_segment_filename',str(root/'%d.m4s'),str(root/'index.m3u8')]
            subprocess.run(cmd,check=True)
            bodies={p.name:p.read_bytes() for p in root.iterdir()}
            manifest=HEADER+'#EXT-X-MAP:URI="init.mp4"\n'+AD+segment(0)+segment(1)+segment(2,discontinuity=True)+'#EXT-X-ENDLIST\n'
            manifest=manifest.replace('.ts','.m4s')
            upstream=Upstream(manifest,bodies)
            try: data=read(upstream)
            finally: upstream.close()
            self.assertTrue(data and data[0]==0x47)
            pixels=subprocess.check_output(['ffmpeg','-v','error','-fflags','+genpts','-dts_delta_threshold','1','-f','mpegts','-i','pipe:0','-vf','scale=1:1','-f','rawvideo','-pix_fmt','rgb24','pipe:1'],input=data)
            frames=list(zip(pixels[::3],pixels[1::3],pixels[2::3]))
            self.assertTrue(any(g>r*2 and g>b for r,g,b in frames))
            self.assertTrue(any(b>r*2 and b>g for r,g,b in frames))
            self.assertFalse(any(r>max(g,b)*2 for r,g,b in frames))

    def test_resolver_cache_reuses_only_fresh_same_channel_allowed_urls(self):
        import json, os
        from streamlink.plugins.twitch import TwitchHLSStream
        s=session(test=True)
        class Plugin:
            def __init__(self, *args): pass
            def streams(self): return self._get_hls_streams('https://usher.ttvnw.net/master.m3u8', [])
        with tempfile.TemporaryDirectory() as tmp, patch.object(s,'resolve_url',return_value=('twitch',Plugin,'https://www.twitch.tv/example')), patch.object(TwitchHLSStream,'parse_variant_playlist',return_value={'720p60':TwitchHLSStream(s,'https://cdn.ttvnw.net/index.m3u8')}) as parser:
            self.assertEqual(list(streams(s,'example',tmp)),['720p60'])
            self.assertFalse(parser.call_args.kwargs['check_streams'])
            self.assertEqual(list(streams(s,'example',tmp)),['720p60']);self.assertEqual(parser.call_count,1)
            path=pathlib.Path(tmp)/'example.json'
            self.assertEqual(path.stat().st_mode & 0o777,0o600)
            data=json.loads(path.read_text());data['streams']['720p60']='http://127.0.0.1/secret';path.write_text(json.dumps(data))
            streams(s,'example',tmp);self.assertEqual(parser.call_count,2)
            streams(s,'example',tmp,fresh=True);self.assertEqual(parser.call_count,3)
            data=json.loads(path.read_text());data['created']-=20;path.write_text(json.dumps(data))
            streams(s,'example',tmp);self.assertEqual(parser.call_count,4)

    def test_remux_decodable_timeline_and_ad_pixels_absent(self):
        # Distinct RGB content, H264/AAC. Prove the client-visible remux contains
        # green/blue frames, never red advertising, and monotone packet DTS.
        with tempfile.TemporaryDirectory() as tmp:
            bodies = {}
            for i, color in enumerate(['green', 'red', 'blue']):
                path = pathlib.Path(tmp) / f'{i}.ts'
                subprocess.run(['ffmpeg','-v','error','-f','lavfi','-i',f'color=c={color}:s=160x90:r=25','-f','lavfi','-i','sine=frequency=440:sample_rate=48000','-t','2','-c:v','libx264','-pix_fmt','yuv420p','-g','25','-threads','1','-c:a','aac','-output_ts_offset',str(i*2),'-f','mpegts',str(path)],check=True)
                bodies[i] = path.read_bytes()
            upstream = Upstream(HEADER + AD + segment(0) + segment(1) + segment(2, discontinuity=True) + '#EXT-X-ENDLIST\n', bodies)
            try:
                filtered = read(upstream)
                self.assertEqual(filtered, bodies[0] + bodies[2])
            finally: upstream.close()
            out = pathlib.Path(tmp)/'out.m3u8'
            subprocess.run(['ffmpeg','-v','error','-fflags','+genpts+discardcorrupt','-dts_delta_threshold','1','-f','mpegts','-i','pipe:0','-c','copy','-bsf:a',"setts=ts='if(eq(N,0),TS,max(TS,PREV_OUTDTS+DURATION))'",'-avoid_negative_ts','make_zero','-f','hls','-hls_time','1','-hls_flags','discont_start',str(out)],input=filtered,check=True)
            pixels = subprocess.check_output(['ffmpeg','-v','error','-i',str(out),'-vf','scale=1:1','-f','rawvideo','-pix_fmt','rgb24','pipe:1'])
            frames = list(zip(pixels[::3],pixels[1::3],pixels[2::3]))
            self.assertTrue(any(g > r*2 and g > b for r,g,b in frames))
            self.assertTrue(any(b > r*2 and b > g for r,g,b in frames))
            self.assertFalse(any(r > max(g,b)*2 for r,g,b in frames))
            import json
            packets = json.loads(subprocess.check_output(['ffprobe','-v','error','-show_packets','-of','json',str(out)]))['packets']
            last = {}; first = {}; pts_end = {}
            for pkt in packets:
                if 'pts_time' in pkt:
                    idx = pkt['stream_index']
                    pts_end[idx] = max(pts_end.get(idx, -1e9), float(pkt['pts_time']) + float(pkt.get('duration_time', 0)))
                if 'dts_time' in pkt:
                    idx, dts = pkt['stream_index'], float(pkt['dts_time'])
                    first.setdefault(idx, dts)
                    self.assertGreaterEqual(dts, last.get(idx, -1e9)); last[idx] = dts
            self.assertEqual(set(last), {0,1})
            # The ad gap is removed from the output timeline, not silently
            # represented by the ad's source video/audio bytes.
            self.assertLess(max(last.values()) - min(first.values()), 4.5)
            self.assertLess(abs(pts_end[0] - pts_end[1]), .15)

if __name__ == '__main__': unittest.main()
