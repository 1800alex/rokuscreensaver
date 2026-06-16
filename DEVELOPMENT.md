# Development

Everything you need to build the project from source, plus the internal details
of how each piece works. For just **running** it, see the [README](README.md).

## Repo layout

```
rokuscreensaver/
├── backend/        Go HTTP server (Docker) -> serves /feed.jpg at 1920x1080
├── calrender/      Go service (Docker) -> renders the weekly .ics into calendar.jpg
├── roku/           BrightScript screensaver channel + Docker packager
├── Makefile        builds all three with only Docker + make on the host
└── dist/           build output (screensaver.zip) — created by `make roku`
```

You only need **Docker** and **make** on your host. Nothing else (no Go, no
ImageMagick, no Roku tooling) is installed locally — everything builds inside
containers.

## Building

```sh
make            # build backend image, calrender image, and roku zip
make backend    # just the backend Docker image
make calrender  # just the calrender Docker image
make roku       # just dist/screensaver.zip
make clean      # remove dist/
```

The Roku zip is produced by a throwaway Alpine "builder" image
([roku/Dockerfile](roku/Dockerfile)) that runs [roku/build.sh](roku/build.sh):
it stages `manifest` + `source` + `components`, generates placeholder
icon/splash artwork with ImageMagick (so no binary art assets live in the repo),
and zips the result into `dist/screensaver.zip`. The script honors `SRC`, `OUT`,
and `FONT` env overrides so CI can run it directly on a runner instead of in the
container.

## Releases & CI

Every tag matching `v*` (e.g. `v1.0.0`) triggers the [GitHub Actions
workflow](.github/workflows/release.yml), which:

- builds and pushes both Docker images (multi-arch `linux/amd64` + `linux/arm64`
  via QEMU, so they run on a Raspberry Pi too) to the GitHub Container Registry
  as `ghcr.io/<owner>/rokuscreensaver-backend` and `-calrender`, and
- builds the Roku channel and attaches `screensaver.zip` to the matching GitHub
  Release.

Cut a release by pushing a tag:

```sh
git tag v1.0.0 && git push origin v1.0.0
```

The workflow authenticates with the built-in `GITHUB_TOKEN`, so there are no
secrets to configure. After the first tagged push the new packages default to
**private** — make them public under the repo's *Packages* settings if you want
anonymous `docker pull`.

## Backend internals

The backend serves a single "current frame" that advances on a timer:

- It pulls photos from one of two sources (directory or Immich — see the
  [README](README.md#photo-source)) and advances to the next every
  `IMAGE_DURATION_SECONDS`.
- After every `CALENDAR_INTERVAL` photos it serves `CALENDAR_PHOTO` (always a
  local file, in either mode).
- Every image is converted to the configured resolution with ImageMagick's
  `convert`, letterboxed (optionally blur-filled) so nothing is cropped.

### Endpoints

- **`/feed.jpg`** (and `/feed`) — the current frame, with no-cache headers and an
  `ETag` content hash.
- **`/feed.id`** — just that hash (cheap), so the client can tell when the image
  actually changed without re-downloading it.
- **`/healthz`** — health check.

### Lazy conversion vs. batch pre-render

By default the backend converts images **lazily** — only when a frame is
requested — keeping `PREFETCH` of them ready in memory. With a large album at
high resolution that conversion is slow, and the work repeats every time the
in-memory cache turns over.

Set **`BATCH=1`** to instead **pre-render the entire source up front** into
`CACHE_DIR` as finished JPEGs (already at the configured resolution, with the
blur fill baked in). Serving then becomes a plain file read — instant, with no
repeated conversion. On the **same refresh schedule** as the photo list
(`IMMICH_REFRESH_MINUTES` in Immich mode; daily in directory mode) it then
**re-syncs**:

- **renders** any source images not yet cached (e.g. photos newly added to the
  Immich album), and
- **deletes** cached frames whose source is **gone** (e.g. assets removed from
  the album) — keeping the cache in lock-step with the source.

Changing a render setting (`WIDTH`/`HEIGHT`/`BLUR_*`/`BG_BRIGHTNESS`) is part of
the cache key, so changed frames are re-rendered and stale ones pruned
automatically on the next sync — no manual cache clear needed.

> ⚠️ **Your originals are never touched.** `CACHE_DIR` **must be a different
> directory from `PHOTO_DIR`** — the server refuses to start if they're equal.
> Sync only ever creates and deletes files matching its own hash scheme
> (`<sha256>.jpg`) inside `CACHE_DIR`; any other file there — and everything
> outside it — is left alone. Mount your photos read-only (`:ro`) for belt-and-
> braces safety and point `CACHE_DIR` at a small **writable** volume. The cache
> is fully derived: deleting it just triggers a re-render on the next sync.

### Running the backend directly

```sh
make run PHOTO_DIR=/srv/family-photos CALENDAR_PHOTO=/srv/cal/week.png
```

or:

```sh
docker run --rm -p 9543:9543 \
  -e CALENDAR_INTERVAL=10 -e IMAGE_DURATION_SECONDS=10 \
  -v /srv/family-photos:/photos:ro \
  -v /srv/cal:/calendar:ro \
  ghcr.io/1800alex/rokuscreensaver-backend
```

Test it: `curl -o frame.jpg http://localhost:9543/feed.jpg`. Batch + Immich:

```sh
docker run --rm -p 9543:9543 \
  -e BATCH=1 -e CACHE_DIR=/cache \
  -e IMMICH_HOST=http://host:2283 -e IMMICH_API_KEY=... -e IMMICH_ALBUM_ID=... \
  -v /srv/roku-cache:/cache \
  ghcr.io/1800alex/rokuscreensaver-backend
```

## calrender internals

`calrender` fetches an iCalendar (`.ics`) feed for the week starting **today**
and renders it into a single TV-friendly schedule JPEG, written to the shared
`calendar` volume the backend reads as `CALENDAR_PHOTO`. It also serves the image
at `http://host:9544/calendar.jpg` (with `/healthz`), and re-renders every
`REFRESH_MINUTES`, which rolls the 7-day window forward as the date changes.

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
| `tz`             | `America/New_York` | timezone                         |
| `title`          | `Holiday Week` | heading text                         |

```sh
# Test layout against a busier week:
curl -o preview.jpg 'http://localhost:9544/preview.jpg?start=2026-12-21&days=7'
```

### Feed URL template tokens

`CAL_URL_TEMPLATE` is a [Go `text/template`](https://pkg.go.dev/text/template)
evaluated each render with the date window computed from *today* in `TZ`:

| Token                                            | Example            | Meaning                              |
| ------------------------------------------------ | ------------------ | ------------------------------------ |
| `{{.Start}}` / `{{.End}}`                        | `2026-06-09` / `2026-06-16` | window start / end (`End` exclusive = `Start + DAYS`) |
| `{{.StartYear}}` `{{.StartMonth}}` `{{.StartDay}}` | `2026` `06` `09` | start components (zero-padded)        |
| `{{.EndYear}}` `{{.EndMonth}}` `{{.EndDay}}`     | `2026` `06` `16`   | end components (zero-padded)          |
| `{{.TZ}}`                                         | `America/New_York` | timezone, raw                        |
| `{{.TZEnc}}`                                      | `America%2FNew_York` | timezone, URL-encoded              |

The feed is expected to have **already expanded recurring events** into concrete
instances for the requested window (export endpoints typically do); `RRULE` is
not interpreted.

### Layout: too many events

The layout is one row per day, sized **proportionally to how busy each day is**.
Within a day, if the events don't fit, the font **shrinks** to a readable floor;
past that, the remainder collapses into a **"+N more"** line. So a packed day
degrades gracefully instead of overflowing.

### Weather internals

`WEATHER=1` shows current conditions in the header's top-right, from
[wttr.in](https://wttr.in)'s `?format=j1` JSON: an icon
(sunny/partly/cloudy/rain/snow/thunder/fog), the **temperature** and **feels
like**, plus a details line with **wind**, **chance of precipitation**, and **UV
index**. The fetch is best-effort — if wttr.in is unreachable, the calendar still
renders, just without the weather block. Weather also appears in `/preview.jpg`,
so you can test it live.

### Running calrender standalone

```sh
docker run --rm -p 9544:9544 \
  -e TZ=America/New_York \
  -e CAL_URL_TEMPLATE='http://calendar.lan:8100/export/download?feeds=...&start={{.Start}}&end={{.End}}&tz={{.TZEnc}}' \
  -v /srv/cal:/out \
  ghcr.io/1800alex/rokuscreensaver-calrender
```

Then point the backend at it: `CALENDAR_PHOTO=/calendar/calendar.jpg` with
`/srv/cal` mounted at `/calendar`. Preview directly with
`curl -o cal.jpg http://localhost:9544/calendar.jpg`.

## Roku channel internals

### On-device settings panel

Configuration lives **on the device**, no rebuild needed. Opening the
"Photostream" channel tile launches straight into a **live preview** of the feed
with a *"Press \* for Settings"* hint; pressing the remote's **`*` (options)**
button opens the settings overlay (Protocol / Address / Port / Interval / Save).
Settings persist in the device registry and are read each time the screensaver
activates. The hint/overlay only appear on a manual channel launch — the real
screensaver (on idle) is just the photos.

> **Why the channel tile and not "Change screensaver settings"?** The OS menu
> *Settings → Screen saver → Photostream → Change screensaver settings* calls the
> channel's `RunScreenSaverSettings` entry point, but that path renders a blank
> screen on some firmware. The same UI is reached reliably by opening the channel
> tile (which runs `Main`), so that's the supported way in. The actual
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

### Why it downloads frames instead of pointing a Poster at the URL

In the Roku *screensaver* context a `Poster` aimed straight at a remote `http://`
URL comes up blank (it works as a normal channel, but not as a screensaver). So
[PhotoView](roku/components/PhotoView.brs) downloads each frame to a local
`tmp:/` file and the Poster shows that. `PhotoView` is shared by the screensaver
and the settings **Preview**.

Downloads run on a **single long-lived worker Task**
([ImageFetcher](roku/components/ImageFetcher.brs)) that reuses its `roUrlTransfer`
connections and aborts a stalled request after a timeout. (Spawning a fresh
task/connection per fetch leaked threads/sockets and wedged all networking after
a few hours — the "can't reach the backend even though it's up" failure.)

### Sideload via `make install`

For iterating on the channel, `make install` POSTs the freshly built zip straight
to the Roku's dev installer (HTTP digest auth), bypassing the browser upload
(which Chrome sometimes fails with `net::ERR_ACCESS_DENIED`):

```sh
make install ROKU_IP=192.168.1.201 ROKU_USER=rokudev ROKU_PASS=roku
```

A successful run prints `Install Success`. (End users who just want to install a
release can use the browser upload — see the [README](README.md#install-the-roku-channel).)

### Runtime behavior notes

- Use plain `http://` on the LAN to avoid Roku TLS-certificate pain.
- The Roku polls on its own timer and the backend decides what's "current," so
  the two refresh intervals don't have to match exactly.
- **No-flash refresh:** before swapping the image, the Roku checks `/feed.id`. If
  the hash is unchanged (its timer fired before the backend advanced), it leaves
  the current image in place and retries every 1s until the hash changes — so you
  never see the same frame reload and "flash".
- **Outage grace period:** if the backend becomes unreachable, the Roku keeps
  showing the last frame for 60s (probing every 1s) before revealing the "can't
  reach the photo server" message, and switches straight back to the feed the
  moment the backend returns. (If no frame was ever shown — backend down at
  startup — the message appears right away.) The message is dimmed and **drifts
  around the screen every 30s** so it can't burn in during a long outage.

### Troubleshooting

The screensaver runs in a **separate** BrightScript context from a normally
launched channel, with its **own debugger on port 8087** (the channel uses 8085).
If the screensaver misbehaves, `telnet <roku-ip> 8087` (while it's active) shows
its log — that's where fetch errors / script crashes appear.
