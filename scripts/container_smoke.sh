#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
image_name=${1:-twitchy-server:smoke}
port=${2:-18080}
work=$(mktemp -d)
name="twitchy-smoke-$$"
trap 'docker rm -f "$name" >/dev/null 2>&1 || true; rm -rf "$work"' EXIT
python3 - "$work" <<'PY'
import json,secrets,sys,pathlib,os
p=pathlib.Path(sys.argv[1]);(p/'tokens.json').write_text(json.dumps({'test':secrets.token_urlsafe(32)}));(p/'signing.key').write_bytes(secrets.token_bytes(32));os.chmod(p,0o755)
# Disposable test credentials only, accessible within this temporary smoke mount.
os.chmod(p/'tokens.json',0o644);os.chmod(p/'signing.key',0o644)
PY
docker run -d --name "$name" --read-only --tmpfs /tmp:size=32m --tmpfs /state:uid=10001,gid=10001,size=32m --cap-drop ALL --security-opt no-new-privileges --memory 384m --pids-limit 96 -p "127.0.0.1:$port:8080" -v "$work:/test-secrets:ro" -e TOKEN_FILE=/test-secrets/tokens.json -e SIGNING_KEY_FILE=/test-secrets/signing.key -e ALLOW_HTTP=true -e "PUBLIC_BASE_URL=http://127.0.0.1:$port" "$image_name" >/dev/null
TOKEN_FILE="$work/tokens.json" python3 scripts/smoke.py --base-url "http://127.0.0.1:$port" --channel fixture --decode
# Every worker and media directory must be gone after DELETE.
count=$(docker exec "$name" python -c 'import os; print(len(os.listdir("/state/media")))')
[ "$count" = 0 ]
echo 'Container media cleanup passed'
