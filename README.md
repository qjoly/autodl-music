# autodl-music

Downloads a YouTube playlist as MP3 and removes sponsor segments, outros, and other junk using the [SponsorBlock](https://sponsor.ajay.app) API.

**Dependencies:** `yt-dlp`, `ffmpeg` (or use the Docker image — batteries included).

## Usage

```bash
# Public playlist
autodl-music -url "https://youtube.com/playlist?list=PLxxx"

# Private playlist
autodl-music -url "https://youtube.com/playlist?list=PLxxx" -cookies cookies.txt

# Custom output directory and categories
autodl-music -url "..." -output ~/Music -categories "sponsor,outro,intro"
```

### All flags

| Flag | Default | Description |
|------|---------|-------------|
| `-url` | *(required)* | YouTube playlist or video URL |
| `-output` | `./music` | Output directory |
| `-categories` | `sponsor,outro,selfpromo,interaction,music_offtopic` | SponsorBlock categories to cut |
| `-cookies` | *(none)* | Path to a `cookies.txt` file (needed for private playlists) |

Available SponsorBlock categories: `sponsor`, `intro`, `outro`, `selfpromo`, `interaction`, `music_offtopic`, `preview`, `filler`.

---

## Private playlists — getting cookies.txt

YouTube requires you to be logged in to access private or unlisted playlists.  
`yt-dlp` accepts cookies exported from your browser in the [Netscape cookie format](http://www.cookieparser.com/netscape-cookies/).

### Option 1 — browser extension (recommended)

1. Install one of these extensions while logged into YouTube:
   - Chrome/Edge: [Get cookies.txt LOCALLY](https://chromewebstore.google.com/detail/get-cookiestxt-locally/cclelndahbckbenkjhflpdbgdldlbecc)
   - Firefox: [cookies.txt](https://addons.mozilla.org/en-US/firefox/addon/cookies-txt/)
2. Navigate to `https://www.youtube.com`
3. Click the extension icon → **Export** (scope: current site / `youtube.com`)
4. Save the file as `cookies.txt`

### Option 2 — yt-dlp export from your browser (local only)

If you have `yt-dlp` installed locally you can export cookies directly without an extension.  
Replace `chrome` with `firefox`, `edge`, `safari`, or `brave` as needed.

```bash
yt-dlp --cookies-from-browser chrome --cookies cookies.txt --skip-download \
  "https://www.youtube.com"
```

This reads cookies straight from the browser's profile and writes `cookies.txt`.  
> **Note:** close the browser first on some platforms (Chrome locks the cookie DB while running).

### Option 3 — browser DevTools (manual, no extension)

1. Open `https://www.youtube.com` in your browser
2. Press `F12` → **Application** tab → **Cookies** → `https://www.youtube.com`
3. You need at minimum these cookies: `SID`, `HSID`, `SSID`, `LOGIN_INFO`, `SAPISID`, `__Secure-1PSID`, `__Secure-3PSID`
4. Paste them into a file following the Netscape format:

```
# Netscape HTTP Cookie File
.youtube.com	TRUE	/	FALSE	<expiry-unix>	<name>	<value>
```

> This is tedious — prefer option 1 or 2.

### Keeping cookies fresh

YouTube session cookies expire. If downloads start failing with `Sign in to confirm you're not a bot` or `HTTP Error 403`, re-export your cookies and replace the file.

---

## Docker

```bash
docker build -t autodl-music .

# Public playlist
docker run --rm -v "$(pwd)/music:/music" autodl-music \
  -url "https://youtube.com/playlist?list=PLxxx"

# Private playlist — mount cookies.txt read-only
docker run --rm \
  -v "$(pwd)/music:/music" \
  -v "$(pwd)/cookies.txt:/cookies.txt:ro" \
  autodl-music \
  -url "https://youtube.com/playlist?list=PLxxx" \
  -cookies /cookies.txt
```

> Inside the container, export `/cookies.txt` as read-only (`:ro`) so the file cannot be modified by the process.

---

## How it works

1. `yt-dlp --flat-playlist` fetches all video IDs without downloading
2. For each video, the SponsorBlock API is queried for segment timestamps
3. `yt-dlp -x --audio-format mp3` downloads the audio
4. `ffmpeg` uses `atrim` + `aconcat` to surgically remove the flagged intervals
5. Already-downloaded files (`<videoID>.mp3`) are skipped on re-runs
