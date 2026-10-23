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
ARG YTDLP_SHA256=dcddc44b6151ccd855a9a3c8fcdb00e897bb872ec9f4908b1b708b16eaaaa114
ARG DENO_VERSION=v2.9.8
ARG DENO_SHA256=e890975bc2549807bbcc0b584ffafda72cc7c93349dd99c80483dd784f6c4609
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl unzip \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /tools
# yt-dlp_linux.zip is the unpacked release. The one-file yt-dlp_linux
# unpacks itself into /tmp at each start, and that fails when /tmp is
# noexec (as in the TrueNAS app).
RUN curl -fsSL -o yt-dlp.zip \
        "https://github.com/yt-dlp/yt-dlp/releases/download/${YTDLP_VERSION}/yt-dlp_linux.zip" \
 && echo "${YTDLP_SHA256}  yt-dlp.zip" | sha256sum -c - \
 && unzip -q yt-dlp.zip -d yt-dlp \
 && rm yt-dlp.zip \
 && mv yt-dlp/yt-dlp_linux yt-dlp/yt-dlp \
 && chmod 0755 yt-dlp/yt-dlp \
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
COPY --from=tools /tools/yt-dlp /opt/yt-dlp
COPY --from=tools /tools/deno /usr/local/bin/
# yt-dlp finds its _internal folder next to the program, so it stays in
# its own folder.
ENV PATH=/opt/yt-dlp:$PATH
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
