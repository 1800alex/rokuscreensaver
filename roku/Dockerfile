# Builder image for the Roku channel. Packaging a sideloadable Roku app is just
# zipping the manifest + source + components, so all we need is zip (+ ImageMagick
# to generate placeholder icons). The host only needs Docker.
FROM alpine:3.20

RUN apk add --no-cache zip imagemagick ttf-dejavu

COPY build.sh /usr/local/bin/build.sh
RUN chmod +x /usr/local/bin/build.sh

# /app  = the channel source (mounted read-only)
# /out  = where screensaver.zip is written (mounted from the host dist/ dir)
ENTRYPOINT ["/usr/local/bin/build.sh"]
