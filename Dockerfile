# Immutable multiarch base indices; Python dependencies are hash-locked.
FROM --platform=$BUILDPLATFORM golang:1.26.5-bookworm@sha256:53eeac89074db483fdf0ab3be1df32bf6e47562263d2d0d6baa7f26acb4957dd AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/twitchy-server ./cmd/twitchy-server

FROM python:3.12.12-slim-bookworm@sha256:593bd06efe90efa80dc4eee3948be7c0fde4134606dd40d8dd8dbcade98e669c AS runtime
# Debian security packages are fixed to a reviewed snapshot, not moving apt.
RUN printf 'deb [check-valid-until=no] https://snapshot.debian.org/archive/debian/20261009T000000Z/ bookworm main\ndeb [check-valid-until=no] https://snapshot.debian.org/archive/debian-security/20261009T000000Z/ bookworm-security main\n' > /etc/apt/sources.list \
    && rm -f /etc/apt/sources.list.d/debian.sources \
    && apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY requirements.lock ./
RUN pip install --no-cache-dir --require-hashes -r requirements.lock
COPY --from=build /out/twitchy-server /app/twitchy-server
COPY worker /app/worker
COPY scripts/init_secrets.py /app/init_secrets.py
COPY LICENSE THIRD_PARTY_NOTICES.md /app/notices/
COPY docs/contract/*LICENSE* /app/notices/
RUN groupadd -g 10001 twitchy && useradd -u 10001 -g twitchy -d /state -s /usr/sbin/nologin twitchy \
    && mkdir -p /state/media && chown -R 10001:10001 /state
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 HOME=/state XDG_CACHE_HOME=/state/cache MEDIA_DIR=/state/media TOKEN_FILE=/state/credentials/tokens.json SIGNING_KEY_FILE=/state/credentials/signing.key WORKER_PATH=/app/worker/reader.py
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s CMD python -c "import urllib.request,json; assert json.load(urllib.request.urlopen('http://127.0.0.1:8080'+__import__('urllib.parse',fromlist=['urlsplit']).urlsplit(__import__('os').environ.get('PUBLIC_BASE_URL','')).path.rstrip('/')+'/v1/health',timeout=3))['status']=='ok'"
ENTRYPOINT ["/app/twitchy-server"]

FROM build AS fixture-build
COPY tests/container ./tests/container
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /out/twitchy-fixture ./tests/container
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go test -c -o /out/server-tests ./internal/server

FROM runtime AS smoke
USER root
RUN ffmpeg -v error -f lavfi -i testsrc2=size=160x90:rate=25 -f lavfi -i sine=frequency=440 -t 4 -c:v libx264 -threads 1 -g 25 -c:a aac -f mpegts /app/fixture.ts
COPY --from=fixture-build /out/twitchy-fixture /app/twitchy-fixture
COPY --from=fixture-build /out/server-tests /app/server-tests
USER 10001:10001
ENTRYPOINT ["/app/twitchy-fixture"]

FROM runtime AS release
