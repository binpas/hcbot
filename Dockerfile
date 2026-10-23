# HEALTH bot container image (linux/amd64). See README.md, "Run on TrueNAS".
#
# The image has no secrets and no data. Mount the bot folder (.env,
# hcbot.db, markov.db, backups/, cookies file) at /data.
#
# To update a tool, change its version and its checksum below. The
# checksums come from each project's release page.

# Stage 1: build hcbot and hcbot-train (pure Go, no C code).
FROM golang:1.27.1-trixie AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" -o /out/hcbot ./cmd/hcbot \
 && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" -o /out/hcbot-train ./cmd/hcbot-train

# Stage 2: get the /download tools (yt-dlp and deno). The checksums
# make sure that each file is the expected one.
FROM debian:trixie-slim AS tools
ARG YTDLP_VERSION=2026.10.21
ARG YTDLP_SHA256=d8bd33fca9d5b23bd5b9952b3fca36db5c46331933fda9f598a3da14ce2c243f
ARG DENO_VERSION=v2.9.8
ARG DENO_SHA256=e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl unzip \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /tools
RUN curl -fsSL -o yt-dlp \
        "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp_linux" \
 && echo "${YTDLP_SHA256}  yt-dlp" | sha256sum -c - \
 && chmod 0755 yt-dlp \
 && curl -fsSL -o deno.zip \
        "https://github.com/denoland/deno/releases/download/${DENO_VERSION}/deno-x86_64-unknown-linux-gnu.zip" \
 && echo "${DENO_SHA256}  deno.zip" | sha256sum -c - \
 && unzip -q deno.zip deno \
 && rm deno.zip \
 && chmod 0755 deno

# Stage 3: the run image. ffmpeg and ffprobe come from Debian, so a
# rebuild later still finds the same packages.
FROM debian:trixie-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates ffmpeg tzdata \
 && rm -rf /var/lib/apt/lists/*
COPY --from=tools /tools/yt-dlp /tools/deno /usr/local/bin/
COPY --from=build /out/hcbot /out/hcbot-train /usr/local/bin/
COPY README.md /usr/share/doc/hcbot/README.md

# UID and GID 568 are the TrueNAS "apps" user. The bot folder on the
# host must belong to this user.
USER 568:568
WORKDIR /data
# The root file system can be read-only, so the tool caches go to /tmp.
ENV HOME=/tmp XDG_CACHE_HOME=/tmp/.cache DENO_DIR=/tmp/deno
# The bot stops cleanly on SIGTERM: it closes the gateway and the database.
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/hcbot"]
