# ---- build stage ----
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /out/calrender .

# ---- runtime stage ----
FROM alpine:3.20
# SVG -> PNG rasterization: rsvg-convert (from librsvg) is preferred, with
# ImageMagick's `convert` as a fallback (it uses the same librsvg delegate). The
# JPEG re-encode is done in Go. font-dejavu + fontconfig give the text a real
# font; tzdata is required for Go's time.LoadLocation (Alpine ships no zoneinfo);
# ca-certificates lets the optional weather fetch reach wttr.in over HTTPS.
RUN apk add --no-cache \
    librsvg \
    imagemagick \
    font-dejavu \
    fontconfig \
    tzdata \
    ca-certificates

COPY --from=build /out/calrender /usr/local/bin/calrender

# Defaults; override at `docker run` time with -e.
# CAL_URL_TEMPLATE is a Go text/template — see the package doc for the fields
# ({{.Start}} {{.End}} {{.StartYear}} {{.TZEnc}} …). WIDTH/HEIGHT set the output
# resolution. OUTPUT_PATH should live on a volume shared with the photo backend's
# CALENDAR_PHOTO so the screensaver picks up the rendered schedule.
# WEATHER=1 adds current conditions (wttr.in) to the header's top-right;
# WEATHER_LOCATION / WEATHER_UNITS (F|C) tune it.
ENV TZ=America/New_York \
    WIDTH=1920 \
    HEIGHT=1080 \
    DAYS=7 \
    TITLE="This Week" \
    OUTPUT_PATH=/out/calendar.jpg \
    REFRESH_MINUTES=60 \
    WEATHER=0 \
    WEATHER_LOCATION="New York" \
    WEATHER_UNITS=F \
    PORT=9544

VOLUME ["/out"]

EXPOSE 9544
ENTRYPOINT ["/usr/local/bin/calrender"]
