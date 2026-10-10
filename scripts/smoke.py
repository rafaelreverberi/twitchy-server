#!/usr/bin/env python3
"""Public API/media smoke. Reads credentials locally; prints no URLs/signatures."""
import argparse
import json
import os
import pathlib
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs): raise RuntimeError('Redirect denied')

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--base-url')
    parser.add_argument('--channel')
    parser.add_argument('--quality', default='auto')
    parser.add_argument('--health-only', action='store_true')
    parser.add_argument('--decode', action='store_true')
    parser.add_argument('--hold', type=int, default=0)
    args=parser.parse_args()
    cfg={}
    if pathlib.Path('.env').exists():
        for line in pathlib.Path('.env').read_text().splitlines():
            if line and not line.startswith('#') and '=' in line:
                k,v=line.split('=',1); cfg[k]=v
    base=(args.base_url or os.environ.get('PUBLIC_BASE_URL') or cfg.get('PUBLIC_BASE_URL','http://localhost:8080')).rstrip('/')
    token_file=os.environ.get('TOKEN_FILE','secrets/tokens.json')
    token=next(iter(json.loads(pathlib.Path(token_file).read_text()).values()))
    opener=urllib.request.build_opener(NoRedirect())
    def request(path,method='GET',data=None,auth=True):
        headers={'Authorization':'Bearer '+token} if auth else {}
        if data is not None: headers['Content-Type']='application/json'
        req=urllib.request.Request(path if path.startswith('http') else base+path,method=method,data=json.dumps(data).encode() if data is not None else None,headers=headers)
        with opener.open(req,timeout=110) as res: return res.status,res.read()
    for i in range(30):
        try:
            status,raw=request('/v1/health',auth=False)
            if json.loads(raw)['status']=='ok': break
        except (OSError,ValueError): pass
        time.sleep(1)
    else: raise RuntimeError('health did not become ok')
    _,caps=request('/v1/capabilities',auth=False); caps=json.loads(caps)
    assert caps['api_version']=='v1'
    print('Health/capabilities passed; live='+str(caps['live']))
    if args.health_only: return
    if not args.channel: raise RuntimeError('--channel is required for live smoke')
    status,raw=request('/v1/playback/sessions','POST',{'content_type':'live','channel_login':args.channel,'requested_quality':args.quality,'preferences':{'low_latency':True}})
    assert status==201
    session=json.loads(raw);id=session['session_id'];url=session['hls_url']
    try:
        status,raw=request(url,auth=False); assert status==200
        variant=next(l for l in raw.decode().splitlines() if l and not l.startswith('#'))
        status,raw=request(variant,auth=False); assert status==200
        lines=[l for l in raw.decode().splitlines() if l and not l.startswith('#')]
        assert lines
        for segment in lines[:2]:
            status,data=request(segment,auth=False);assert status==200 and data[0]==0x47 and len(data)>=188
        print('Session/master/variant/signed TS bytes passed; qualities='+','.join(session['qualities']))
        if args.decode:
            result=subprocess.run(['ffmpeg','-v','error','-i',url,'-t','10','-f','null','-'],stdout=subprocess.DEVNULL,stderr=subprocess.PIPE,timeout=60)
            assert result.returncode==0, 'decoder failed (upstream details withheld)'
            print('10 seconds of HLS audio/video decoded')
        end=time.monotonic()+args.hold
        previous=-1
        while time.monotonic()<end:
            _,raw=request(variant,auth=False)
            seq=int(next(l.split(':')[1] for l in raw.decode().splitlines() if l.startswith('#EXT-X-MEDIA-SEQUENCE:')))
            assert seq>=previous;previous=seq
            time.sleep(2)
        if args.hold: print(f'{args.hold}s media refresh/sequence observation completed; does not prove an ad break')
    finally:
        status,body=request('/v1/playback/sessions/'+id,'DELETE'); assert status==204 and body==b''
    try:
        request(url,auth=False)
        raise RuntimeError('deleted session still accessible')
    except urllib.error.HTTPError as e: assert e.code in (403,404)
    print('DELETE/revocation passed')

if __name__=='__main__':
    try: main()
    except urllib.error.HTTPError as e:
        # Error envelopes are ours, but print only stable code/status.
        try: code=json.loads(e.read())['error']['code']
        except Exception: code='http_error'
        print(f'Smoke FAILED: HTTP {e.code}, {code}'); raise SystemExit(1)
    except Exception as e:
        print('Smoke FAILED: '+type(e).__name__);raise SystemExit(1)
