# Build stage
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -o autodl-music .

# Runtime stage
FROM python:3.14-alpine
RUN apk add --no-cache ffmpeg ca-certificates nodejs
RUN pip install --no-cache-dir yt-dlp mutagen

COPY --from=builder /app/autodl-music /usr/local/bin/autodl-music

# /music  — output directory
# /cookies.txt — optional: mount your browser cookies file for private playlists
VOLUME ["/music"]
WORKDIR /music

ENTRYPOINT ["autodl-music", "-output", "/music"]
