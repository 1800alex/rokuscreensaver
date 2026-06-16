# Roku Photo Screensaver

[![release](https://github.com/1800alex/rokuscreensaver/actions/workflows/release.yml/badge.svg)](https://github.com/1800alex/rokuscreensaver/actions/workflows/release.yml)
[![license](https://img.shields.io/github/license/1800alex/rokuscreensaver)](LICENSE)

A two-part family-photo screensaver for Roku. All logic for cycling photos and
periodically inserting a calendar image lives in a small Go backend; the Roku
app is a dead-simple screensaver that just downloads and shows the current
image every few seconds.

```
rokuscreensaver/
├── backend/        Go HTTP server (Docker) -> serves /feed.jpg at 1920x1080
├── calrender/      Go service (Docker) -> renders the weekly .ics into calendar.jpg
├── roku/           BrightScript screensaver channel + Docker packager
├── Makefile        builds all three with only Docker + make on the host
└── dist/           build output (screensaver.zip) — created by `make roku`
```

You only need **Docker** and **make** on your host. Nothing else (no Go, no
ImageMagick, no Roku tooling) is installed locally.

## Build

```sh
make          # builds the backend image and the Roku zip
make backend  # just the backend Docker image
make roku     # just dist/screensaver.zip
```

## Prebuilt images & releases

Every tag matching `v*` (e.g. `v1.0.0`) triggers a [GitHub Actions
workflow](.github/workflows/release.yml) that:

- builds and pushes the two Docker images (multi-arch `linux/amd64` +
  `linux/arm64`, so they run on a Raspberry Pi too) to the GitHub Container
  Registry, and
- builds the Roku channel and attaches **`screensaver.zip`** to the matching
  [GitHub Release](https://github.com/1800alex/rokuscreensaver/releases).

So you don't have to build anything yourself:

```sh
docker pull ghcr.io/1800alex/rokuscreensaver-backend:latest
docker pull ghcr.io/1800alex/rokuscreensaver-calrender:latest
```

The bundled [docker-compose.yml](docker-compose.yml) already references these
images — copy it, drop your photos into `./photos`, point `CAL_URL_TEMPLATE` at
your own calendar feed, and `docker compose up -d`. Grab the latest
`screensaver.zip` from the [Releases
page](https://github.com/1800alex/rokuscreensaver/releases) to sideload onto the
Roku (see [Sideload](#sideload) below). Pin a specific version by replacing
`:latest` with a tag like `:1.0.0`.

To cut a release, push a tag:

```sh
git tag v1.0.0 && git push origin v1.0.0
```

> **One-time CI setup.** The workflow uses the built-in `GITHUB_TOKEN`, so no
> secrets are needed. After the first tagged push, the new packages default to
> private — make them public under the repo's *Packages* settings if you want
> anonymous `docker pull`.

## Backend

A Go container that:

- pulls photos from one of two sources (see below) and advances to the next one
  every `IMAGE_DURATION_SECONDS`,
- shows `CALENDAR_PHOTO` after every `CALENDAR_INTERVAL` photos (the calendar is
  always a local file, in either mode),
- converts every image to **1920×1080** with ImageMagick's `convert`
  (installed in the Dockerfile), letterboxed on black so nothing is cropped,
- serves the current frame at **`/feed.jpg`** (and `/feed`) with no-cache
  headers and an `ETag` content hash. **`/feed.id`** returns just that hash
  (cheap) so the client can tell when the image actually changed. `/healthz` is
  a health check.

### Photo source

- **Directory mode** (default): cycles images in `PHOTO_DIR`.
- **Immich album mode**: set `IMMICH_ALBUM_ID` (plus `IMMICH_URL` and
  `IMMICH_API_KEY`) and the directory is ignored — it cycles the images in that
  Immich album instead. Just add photos to the album and they show up. Create an
  API key in Immich under *Account Settings → API Keys*, and grab the album ID
  from the album's URL (`/albums/<id>`).

### Configuration (env vars)

| Variable                 | Default                    | Meaning                                          |
| ------------------------ | -------------------------- | ------------------------------------------------ |
| `PHOTO_DIR`              | `/photos`                  | Directory of source photos (directory mode)      |
| `CALENDAR_PHOTO`         | `/calendar/calendar.jpg`   | Calendar image (leave unset to disable)          |
| `CALENDAR_INTERVAL`      | `10`                       | Show the calendar after this many photos (0=off) |
| `IMAGE_DURATION_SECONDS` | `10`                       | How long each image is the current frame         |
| `PREFETCH`               | `1`                        | How many converted frames to keep pre-built in memory (≥1) |
| `WIDTH` / `HEIGHT`       | `1920` / `1080`            | Output resolution in px (use `3840`/`2160` for 4K; aliases `SCREEN_WIDTH`/`SCREEN_HEIGHT`) |
| `BLUR_FILL`              | `1`                        | Fill letterbox bars with a zoomed, blurred copy of the image; `0`=plain black |
| `BLUR_SIGMA`             | `25`                       | Blur strength (px) for the fill background       |
| `BG_BRIGHTNESS`          | `70`                       | Fill background brightness % (`100`=no dimming)  |
| `PORT`                   | `9543`                     | Listen port                                      |
| `BATCH`                  | `0`                        | `1`/`on` to pre-render the whole source to `CACHE_DIR` and sync it (see below) |
| `CACHE_DIR`              | `/cache`                   | Where batch-rendered frames live — **must differ from `PHOTO_DIR`** (alias `BATCH_DIR`) |
| `IMMICH_ALBUM_ID`        | (unset)                  | Immich album UUID — **set this to use Immich mode** |
| `IMMICH_URL`             | (unset)                  | Immich base URL, e.g. `http://host:2283` (alias: `IMMICH_HOST`) |
| `IMMICH_API_KEY`         | (unset)                  | Immich API key (`x-api-key`)                     |
| `IMMICH_IMAGE_SIZE`      | `original`                 | `original`, `preview`, or `thumbnail`            |
| `IMMICH_REFRESH_MINUTES` | `1440`                     | How often to re-fetch the album's asset IDs      |

In **directory mode**, new photos dropped into `PHOTO_DIR` are picked up on the
next pass through the list. In **Immich mode**, the album's asset IDs are
re-fetched every `IMMICH_REFRESH_MINUTES` (default 24h), and the playback order
is **randomized** (reshuffled each pass). If no images are found, the calendar
is skipped too and a black frame is served.

### Batch pre-render & sync (`BATCH=1`)

By default the backend converts images **lazily** — only when a frame is
requested — keeping `PREFETCH` of them ready in memory. With a large album at high
resolution that conversion is slow, and the work repeats every time the in-memory
cache turns over.

Set **`BATCH=1`** to instead **pre-render the entire source up front** into
`CACHE_DIR` as finished JPEGs (already at the configured resolution, with the blur
fill baked in). Serving then becomes a plain file read — instant, with no repeated
conversion. On the **same refresh schedule** as the photo list
(`IMMICH_REFRESH_MINUTES` in Immich mode; daily in directory mode) it then
**re-syncs**:

- **renders** any source images not yet cached (e.g. photos newly added to the
  Immich album), and
- **deletes** cached frames whose source is **gone** (e.g. assets removed from the
  album) — keeping the cache in lock-step with the source.

Changing a render setting (`WIDTH`/`HEIGHT`/`BLUR_*`/`BG_BRIGHTNESS`) is part of
the cache key, so changed frames are re-rendered and the stale ones pruned
automatically on the next sync — no manual cache clear needed.

> ⚠️ **Your originals are never touched.** `CACHE_DIR` **must be a different
> directory from `PHOTO_DIR`** — the server refuses to start if they're equal.
> Sync only ever creates and deletes files matching its own hash scheme
> (`<sha256>.jpg`) inside `CACHE_DIR`; any other file there — and everything
> outside it — is left alone. Mount your photos read-only (`:ro`) for belt-and-
> braces safety and point `CACHE_DIR` at a small **writable** volume. The cache is
> fully derived: deleting it just triggers a re-render on the next sync.

```sh
docker run --rm -p 9543:9543 \
  -e BATCH=1 -e CACHE_DIR=/cache \
  -e IMMICH_HOST=http://host:2283 -e IMMICH_API_KEY=... -e IMMICH_ALBUM_ID=... \
  -v /srv/roku-cache:/cache \
  rokuscreensaver-backend
```

### Run

```sh
make run PHOTO_DIR=/srv/family-photos CALENDAR_PHOTO=/srv/cal/week.png
```

or directly:

```sh
docker run --rm -p 9543:9543 \
  -e CALENDAR_INTERVAL=10 -e IMAGE_DURATION_SECONDS=10 \
  -v /srv/family-photos:/photos:ro \
  -v /srv/cal:/calendar:ro \
  rokuscreensaver-backend
```

Test it: `curl -o frame.jpg http://localhost:9543/feed.jpg`.

The calendar image is just a file on disk. You can render your "next 7 days"
however you like and point `CALENDAR_PHOTO` at it — or use the bundled
**`calrender`** service below, which generates it from an `.ics` feed.

## Calendar renderer (`calrender`)

A second Go container that fetches an **iCalendar (`.ics`)** feed for the week
starting **today** and renders it into a single, TV-friendly **schedule JPEG**.
Drop its output onto the same volume the backend reads as `CALENDAR_PHOTO` and
the screensaver shows an always-current weekly schedule. It also serves the image
at **`http://host:9544/calendar.jpg`** (with `/healthz`).

It re-fetches and re-renders every `REFRESH_MINUTES` (default 60), which also
rolls the 7-day window forward as the date changes.

### Preview / testing endpoint

**`/preview.jpg`** renders a one-off image for an arbitrary window and serves it
inline — it does **not** write `OUTPUT_PATH` or disturb the scheduled
`calendar.jpg`, so it's safe to hit while the screensaver is live. All params are
optional and override the config for that render only:

| Param            | Example        | Meaning                              |
| ---------------- | -------------- | ------------------------------------ |
| `start`          | `2026-12-21`   | window start (default: today)        |
| `days`           | `7`            | days in the window (1–14)            |
| `width`/`height` | `1920`/`1080`  | output resolution                    |
| `tz`             | `America/New_York` | timezone                     |
| `title`          | `Holiday Week` | heading text                         |

```sh
# Test layout against a busier week:
curl -o preview.jpg 'http://localhost:9544/preview.jpg?start=2026-12-21&days=7'
```

### Feed URL template

The feed URL comes from **`CAL_URL_TEMPLATE`**, a [Go
`text/template`](https://pkg.go.dev/text/template) evaluated each render with the
date window computed from *today* in **`TZ`**:

| Token                                            | Example            | Meaning                              |
| ------------------------------------------------ | ------------------ | ------------------------------------ |
| `{{.Start}}` / `{{.End}}`                        | `2026-06-09` / `2026-06-16` | window start / end (`End` exclusive = `Start + DAYS`) |
| `{{.StartYear}}` `{{.StartMonth}}` `{{.StartDay}}` | `2026` `06` `09` | start components (zero-padded)        |
| `{{.EndYear}}` `{{.EndMonth}}` `{{.EndDay}}`     | `2026` `06` `16`   | end components (zero-padded)          |
| `{{.TZ}}`                                         | `America/New_York` | timezone, raw                     |
| `{{.TZEnc}}`                                      | `America%2FNew_York` | timezone, URL-encoded           |

The default template targets the example export endpoint:

```text
http://calendar.lan:8100/export/download?feeds=...&start={{.Start}}&end={{.End}}&tz={{.TZEnc}}
```

The feed is expected to have **already expanded recurring events** into concrete
instances for the requested window (export endpoints typically do); `RRULE` is
not interpreted.

### Too many events

The layout is one row per day, sized **proportionally to how busy each day is**.
Within a day, if the events don't fit, the font **shrinks** to a readable floor;
past that, the remainder collapses into a **"+N more"** line. So a packed day
degrades gracefully instead of overflowing.

### Weather (optional)

Set **`WEATHER=1`** to show current conditions in the header's **top-right**,
from [wttr.in](https://wttr.in)'s `?format=j1` JSON: an icon
(sunny/partly/cloudy/rain/snow/thunder/fog), the **temperature** and **feels
like**, plus a details line with **wind**, **chance of precipitation**, and **UV
index**. Pick units with `WEATHER_UNITS` (`F` or `C`). The fetch is best-effort —
if wttr.in is unreachable, the calendar still renders, just without the weather
block.

### Configuration (calrender env vars)

| Variable           | Default                  | Meaning                                            |
| ------------------ | ------------------------ | -------------------------------------------------- |
| `CAL_URL_TEMPLATE` | (example feed)           | Go template for the `.ics` URL (see tokens above)  |
| `TZ`               | `America/New_York`   | Timezone the "today" window is computed in         |
| `WIDTH` / `HEIGHT` | `1920` / `1080`          | Output resolution in px (match your screen)        |
| `DAYS`             | `7`                      | Days in the window, starting today (1–14)          |
| `TITLE`            | `This Week`              | Heading shown top-left                             |
| `OUTPUT_PATH`      | `/out/calendar.jpg`      | Where the JPEG is written (share with the backend) |
| `REFRESH_MINUTES`  | `60`                     | Re-fetch + re-render cadence                       |
| `PORT`             | `9544`                   | Listen port for `/calendar.jpg`                    |
| `WEATHER`          | `0`                      | `1`/`on` to show weather in the header             |
| `WEATHER_LOCATION` | `New York`               | wttr.in location                                   |
| `WEATHER_UNITS`    | `F`                      | `F` or `C`                                         |
| `WEATHER_URL`      | (built from location)    | Full override for the wttr.in JSON URL             |

Weather also appears in the `/preview.jpg` endpoint, so you can test it live.

### Run (calrender)

The easiest path is `docker compose up` — the bundled compose file wires
`calrender` to write into a shared `calendar` volume that the backend mounts
read-only as `/calendar`. Or run it standalone:

```sh
docker run --rm -p 9544:9544 \
  -e TZ=America/New_York \
  -e CAL_URL_TEMPLATE='http://calendar.lan:8100/export/download?feeds=...&start={{.Start}}&end={{.End}}&tz={{.TZEnc}}' \
  -v /srv/cal:/out \
  rokuscreensaver-calrender
```

Then point the backend at it: `CALENDAR_PHOTO=/calendar/calendar.jpg` with
`/srv/cal` mounted at `/calendar`. Preview directly with
`curl -o cal.jpg http://localhost:9544/calendar.jpg`.

## Roku screensaver

`make roku` produces `dist/screensaver.zip`, a sideloadable Roku channel.

### Settings (control panel)

Configure it **on the device**, no rebuild needed. **Open the "Photostream"
channel tile** from the Roku home screen — it launches straight into a **live
preview** of the feed (the screensaver running), with a *"Press \* for
Settings"* hint in the corner. Press the remote's **`*` (options)** button to
open the settings overlay; navigate with Up/Down, press OK on a row:

- **Protocol** — toggles `http` / `https`
- **Address** — opens a keyboard for the backend IP or hostname
- **Port** — opens a keyboard for the backend port
- **Interval** — cycles `10s` / `30s` / `60s`, then a keyboard for a custom value
- **Save** — writes the settings and returns to the preview (now using them)

**Back** also returns to the preview (discarding unsaved edits). Settings persist
in the device registry and are read each time the screensaver activates.

The `*`-for-settings hint only appears on a manual channel launch — the real
screensaver (on idle) is just the photos, never the hint or overlay.

> **Why the channel tile and not "Change screensaver settings"?** The OS menu
> *Settings → Screen saver → Photostream → Change screensaver settings* calls
> the channel's `RunScreenSaverSettings` entry point, but that path renders a
> blank screen on some firmware. The same UI is reached reliably by opening the
> channel tile (which runs `Main`), so that's the supported way in. The actual
> screensaver still activates normally on idle.

The out-of-the-box defaults live in
[roku/source/config.brs](roku/source/config.brs) (`ssDefaults`) — edit those if
you want a different default before building:

```brightscript
protocol: "http"          ' http | https
host:     "192.168.1.15"  ' backend IP or hostname
port:     "9543"          ' backend port
interval: 10              ' fetch interval in seconds
```

> **Why it downloads instead of pointing a Poster at the URL:** in the Roku
> *screensaver* context a `Poster` aimed straight at a remote `http://` URL
> comes up blank (it works as a normal channel, but not as a screensaver). So
> [PhotoView](roku/components/PhotoView.brs) downloads each frame to a local
> `tmp:/` file and the Poster shows that. `PhotoView` is shared by the
> screensaver and the settings **Preview**.
>
> Downloads run on a **single long-lived worker Task**
> ([ImageFetcher](roku/components/ImageFetcher.brs)) that reuses its
> `roUrlTransfer` connections and aborts a stalled request after a timeout.
> (Spawning a fresh task/connection per fetch leaked threads/sockets and wedged
> all networking after a few hours — the "can't reach the backend even though it's
> up" failure.)

### Sideload

1. Enable developer mode on the Roku (Home ×3, Up ×2, Right, Left, Right, Left,
   Right) and note the device IP + the password you set (default here:
   `rokudev` / `roku`).
2. Install the package. **Preferred — `make install`**, which POSTs the zip
   straight to the dev installer (digest auth) and avoids the browser:

   ```sh
   make install ROKU_IP=192.168.1.201 ROKU_USER=rokudev ROKU_PASS=roku
   ```

   A successful run prints `Install Success`. (The browser upload at
   `http://<roku-ip>` also works, but Chrome sometimes fails the POST with
   `net::ERR_ACCESS_DENIED` — `make install` sidesteps that entirely.)
3. On the Roku: **Settings → Theme → Screen saver** → select **Photostream**, and set
   **Wait time** to **5 minutes** (this idle-timeout is a system setting; the
   channel can't force it). The screensaver then starts after ~5 minutes of
   inactivity and shows a fresh photo every `REFRESH_SECONDS`.

### Notes

- Use plain `http://` on the LAN to avoid Roku TLS-certificate pain.
- The Roku polls on its own timer and the backend decides what's "current," so
  the two refresh intervals don't have to match exactly.
- **No-flash refresh:** before swapping the image, the Roku checks `/feed.id`.
  If the hash is unchanged (its timer fired before the backend advanced), it
  leaves the current image in place and retries every 1s until the hash changes
  — so you never see the same frame reload and "flash".
- **Outage grace period:** if the backend becomes unreachable, the Roku keeps
  showing the last frame for 60s (probing every 1s) before revealing the
  "can't reach the photo server" message, and switches straight back to the
  feed the moment the backend returns. (If no frame was ever shown — backend
  down at startup — the message appears right away.) The message is dimmed and
  **drifts around the screen every 30s** so it can't burn in during a long
  outage.

### Troubleshooting

The screensaver runs in a **separate** BrightScript context from a normally
launched channel, with its **own debugger on port 8087** (the channel uses
8085). If the screensaver misbehaves, `telnet <roku-ip> 8087` (while it's
active) shows its log — that's where fetch errors / script crashes appear.
