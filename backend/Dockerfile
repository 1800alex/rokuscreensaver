# ---- build stage ----
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /out/backend .

# ---- runtime stage ----
FROM alpine:3.20
# ImageMagick provides the external `convert` program used to normalize images
# to the configured resolution. On Alpine the format delegates are split into separate packages,
# so JPEG/WEBP/TIFF/HEIC support must be added explicitly (the base package only
# handles PNG).
RUN apk add --no-cache \
    imagemagick \
    imagemagick-jpeg \
    imagemagick-webp \
    imagemagick-tiff \
    imagemagick-heic

COPY --from=build /out/backend /usr/local/bin/backend

# Defaults; override at `docker run` time with -e.
# WIDTH/HEIGHT set the output resolution (default 1080p); use 3840x2160 for 4K.
# BLUR_FILL=0 disables the zoomed/blurred letterbox fill (plain black bars).
# BLUR_SIGMA / BG_BRIGHTNESS tune the fill blur strength and background darkness.
# BATCH=1 pre-renders the whole source into CACHE_DIR and re-syncs on schedule
# (deleting cache frames whose source is gone). CACHE_DIR MUST be a separate
# volume from PHOTO_DIR — the server refuses to start if they are the same.
ENV PHOTO_DIR=/photos \
    CALENDAR_PHOTO=/calendar/calendar.jpg \
    CALENDAR_INTERVAL=10 \
    IMAGE_DURATION_SECONDS=10 \
    PREFETCH=1 \
    WIDTH=1920 \
    HEIGHT=1080 \
    BLUR_FILL=1 \
    BLUR_SIGMA=25 \
    BG_BRIGHTNESS=70 \
    BATCH=0 \
    CACHE_DIR=/cache \
    PORT=9543

# Persist the pre-rendered cache across restarts (only used when BATCH=1).
VOLUME ["/cache"]

EXPOSE 9543
ENTRYPOINT ["/usr/local/bin/backend"]
