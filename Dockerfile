# Build stage
FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o autodl-music .

# Runtime stage
FROM python:3.14-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    ffmpeg ca-certificates nodejs curl unzip \
 && ARCH=$(uname -m) \
 && curl -fsSL "https://github.com/denoland/deno/releases/latest/download/deno-${ARCH}-unknown-linux-gnu.zip" -o /tmp/deno.zip \
 && unzip /tmp/deno.zip -d /usr/local/bin \
 && chmod +x /usr/local/bin/deno \
 && rm /tmp/deno.zip \
 && apt-get purge -y curl unzip \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*
RUN pip install --no-cache-dir yt-dlp mutagen

COPY --from=builder /app/autodl-music /usr/local/bin/autodl-music

# /music  — downloaded MP3s
# /config — persistent config: cookies.txt, passkey.json
VOLUME ["/music", "/config"]
WORKDIR /music

ENTRYPOINT ["autodl-music", "-web", "-passkey", "/config/passkey.json", "-config", "/config/autodl-music.json", "-output", "/music"]
