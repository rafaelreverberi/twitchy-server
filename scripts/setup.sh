#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
command -v docker >/dev/null || { echo 'Docker is required; install/start it yourself.' >&2; exit 1; }
docker compose version >/dev/null
docker info >/dev/null
command -v python3 >/dev/null || { echo 'Python 3 is required for local secret generation.' >&2; exit 1; }
umask 077
[ -f .env ] || cp .env.example .env
mkdir -p secrets
chmod 700 secrets
python3 - <<'PY'
import json, os, secrets
for path, data in [('secrets/tokens.json', json.dumps({'local': secrets.token_urlsafe(32)})+'\n'), ('secrets/signing.key', secrets.token_bytes(32))]:
    try:
        with open(path, 'xb') as f: f.write(data.encode() if isinstance(data,str) else data)
    except FileExistsError: pass
    os.chmod(path, 0o600)
PY
case "${1:-}" in
    "") docker compose build server ;;
    --image) docker compose pull server ;;
    *) echo "Usage: scripts/setup.sh [--image]" >&2; exit 1 ;;
esac
docker compose --profile setup run --rm --no-deps secrets-init
docker compose up -d --no-build server
python3 scripts/smoke.py --health-only
echo 'Server started. Source credentials: secrets/tokens.json (not printed).'
echo 'For a full media smoke run: python3 scripts/smoke.py --channel A_CURRENTLY_LIVE_CHANNEL'
