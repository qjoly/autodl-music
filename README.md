# autodl-music

Downloads a YouTube playlist as MP3, removes sponsor segments using [SponsorBlock](https://sponsor.ajay.app), and embeds metadata + cover art.

**Dependencies (bare-metal):** `yt-dlp`, `ffmpeg`, `node` or `deno` (for YouTube n-challenge). Or just use the Docker image — batteries included.

---

## Quick start

### Web UI (recommended)

```bash
autodl-music -web
# → open http://localhost:8080, register a passkey, configure and start
```

On first visit you will be prompted to **register a passkey**. After that, the passkey is used to authenticate every subsequent visit.

### CLI

```bash
# Public playlist
autodl-music -url "https://youtube.com/playlist?list=PLxxx"

# Private playlist
autodl-music -url "https://youtube.com/playlist?list=PLxxx" -cookies cookies.txt

# Periodic sync every hour
autodl-music -url "..." -interval 1h
```

---

## All flags

| Flag | Default | Description |
|------|---------|-------------|
| `-url` | *(none)* | YouTube playlist or video URL |
| `-output` | `./music` | Output directory |
| `-categories` | `sponsor,outro,selfpromo,interaction,music_offtopic` | SponsorBlock categories to cut |
| `-cookies` | *(none)* | Path to `cookies.txt` (needed for private playlists) |
| `-interval` | *(none)* | Re-run interval, e.g. `1h`, `30m` |
| `-web` | `false` | Enable web UI |
| `-port` | `8080` | Web UI port |
| `-host` | `localhost` | Hostname used for WebAuthn RPID — **must match the hostname you browse to** |
| `-origin` | *(derived)* | Full WebAuthn origin, e.g. `https://mydomain.com` — overrides the default `http://<host>:<port>` |
| `-passkey` | `passkey.json` | Path to the passkey credential file |
| `-config` | `autodl-music.json` | Path to the persistent config file |

`visitor_data` and `po_token` are not CLI flags — set them via the web UI settings panel or edit the config JSON directly.

CLI flags override values stored in the config file. The config file is written by the web UI.

Available SponsorBlock categories: `sponsor`, `intro`, `outro`, `selfpromo`, `interaction`, `music_offtopic`, `preview`, `filler`.

---

## Web UI — passkey troubleshooting

The passkey implementation uses [WebAuthn](https://webauthn.io/). WebAuthn enforces that the **RP ID** (Relying Party ID) matches the **effective domain** of the origin your browser uses. If they don't match, you'll get:

> `rp.id cannot be used with the current origin`

### Accessing from localhost (default)

No extra flags needed:

```bash
autodl-music -web
# browse to http://localhost:8080
```

### Accessing from a LAN IP or hostname

```bash
autodl-music -web -host 192.168.1.10
# browse to http://192.168.1.10:8080
```

> The `-host` value becomes the WebAuthn RPID. It must exactly match the hostname in your browser's address bar (IP addresses are valid).

### Accessing behind a reverse proxy with HTTPS

```bash
autodl-music -web -host mydomain.com -origin https://mydomain.com
# browse to https://mydomain.com  (proxy forwards to :8080)
```

The `-origin` flag sets the full origin that the browser will report during the WebAuthn ceremony. Use it whenever the origin your browser sees differs from `http://<host>:<port>`.

### Re-registering a passkey

Delete the passkey file and refresh the login page — you will be prompted to register again:

```bash
rm passkey.json   # or /config/passkey.json in Docker
```

---

## Private playlists — getting cookies.txt

YouTube requires authentication for private or unlisted playlists. `yt-dlp` accepts cookies in [Netscape cookie format](http://www.cookieparser.com/netscape-cookies/).

### Option 1 — browser extension (recommended)

1. Install while logged into YouTube:
   - Chrome/Edge: [Get cookies.txt LOCALLY](https://chromewebstore.google.com/detail/get-cookiestxt-locally/cclelndahbckbenkjhflpdbgdldlbecc)
   - Firefox: [cookies.txt](https://addons.mozilla.org/en-US/firefox/addon/cookies-txt/)
2. Navigate to `https://www.youtube.com`
3. Click the extension → **Export** (scope: `youtube.com`)
4. Save as `cookies.txt`

### Option 2 — yt-dlp export (local only)

```bash
yt-dlp --cookies-from-browser chrome --cookies cookies.txt --skip-download \
  "https://www.youtube.com"
```

Replace `chrome` with `firefox`, `edge`, `safari`, or `brave`.

> **Note:** close the browser first on some platforms (Chrome locks the cookie DB while running).

### Keeping cookies fresh

YouTube session cookies expire. If downloads fail with `Sign in to confirm you're not a bot` or `HTTP Error 403`, re-export your cookies.

---

## Docker

The image includes `yt-dlp`, `ffmpeg`, `mutagen`, and `node` (for n-challenge solving).

```bash
docker build -t autodl-music .
```

### Web UI mode (default ENTRYPOINT)

Mount a `/config` directory for persistent state (config file, passkey):

```bash
docker run -d \
  -p 8080:8080 \
  -v "$(pwd)/config:/config" \
  -v "$(pwd)/music:/music" \
  autodl-music
```

Then open `http://localhost:8080` and configure via the UI.

To pass cookies for private playlists, place `cookies.txt` in the config directory and set the **Cookies file path** to `/config/cookies.txt` in the web UI settings.

### CLI mode

```bash
docker run --rm \
  -v "$(pwd)/music:/music" \
  -v "$(pwd)/cookies.txt:/config/cookies.txt:ro" \
  autodl-music \
  -url "https://youtube.com/playlist?list=PLxxx" \
  -cookies /config/cookies.txt
```

---

## How it works

1. `yt-dlp --flat-playlist` fetches all video IDs without downloading
2. For each video, the SponsorBlock API returns segment timestamps
3. `yt-dlp -x --audio-format mp3 --embed-metadata --embed-thumbnail` downloads audio with cover art and tags
4. `ffmpeg` uses `atrim` + `aconcat` to surgically remove the flagged intervals
5. Already-downloaded files (`Title [videoID].mp3`) are skipped on re-runs
6. Failed downloads appear in a table in the web UI — with **retry** and **remove** buttons
