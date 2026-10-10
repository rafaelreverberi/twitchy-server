#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
mkdir -p .local/sources
# Disposable root build helper with no credentials mounted. Only downloads the
# exact Debian source version; does not touch the running server.
docker run --rm --user 0 --entrypoint sh -v "$PWD/.local/sources:/sources" twitchy-server:local -c '
set -eu
printf "deb-src [check-valid-until=no] https://snapshot.debian.org/archive/debian/20261009T000000Z/ bookworm main\ndeb-src [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20261009T000000Z/ bookworm-security main\n" > /etc/apt/sources.list
apt-get update >/dev/null
cd /sources
apt-get source --download-only ffmpeg=7:5.1.9-0+deb12u1 >/dev/null
sha256sum *.dsc *.tar.* > SHA256SUMS
'
tar -czf .local/ffmpeg-5.1.9-debian-source.tar.gz -C .local/sources .
