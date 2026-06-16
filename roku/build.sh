#!/bin/sh
# Packages the Roku channel into a sideloadable .zip. Runs inside the builder
# container (see Dockerfile) so the host needs nothing but Docker.
#
# Placeholder icon/splash images are generated with ImageMagick here, so we
# don't have to keep binary assets in the repo.
set -e

# Paths default to the in-container layout (see Dockerfile) but can be overridden
# so the same script runs directly on a CI runner (e.g. SRC=./roku OUT=./dist).
SRC="${SRC:-/app}"
OUT="${OUT:-/out}"
STAGE="${STAGE:-/tmp/stage}"

rm -rf "$STAGE"
mkdir -p "$STAGE/images"

cp "$SRC/manifest" "$STAGE/"
cp -r "$SRC/source" "$STAGE/"
cp -r "$SRC/components" "$STAGE/"

# Channel poster / focus icons and splash screens referenced by the manifest.
# We draw a simple "framed landscape photo" glyph with ImageMagick (no external
# art assets needed), render a master at FHD size, then resize to the other
# resolutions Roku requires.
# Alpine ships DejaVu here; Debian/Ubuntu CI runners use a different path, so
# allow FONT to be overridden from the environment.
FONT="${FONT:-/usr/share/fonts/dejavu/DejaVuSans-Bold.ttf}"
ICON="$STAGE/images/icon_focus_fhd.png"

# Master icon (540x405): gradient sky, white photo frame, sun, mountains, label.
convert -size 540x405 gradient:'#2f74c0-#0c2238' \
    -fill '#FFFFFF' -draw 'roundrectangle 150,104 390,284 18,18' \
    -fill '#BFE3FF' -draw 'roundrectangle 165,119 375,269 10,10' \
    -fill '#FFD24A' -draw 'circle 212,166 212,144' \
    -fill '#4FB06A' -draw 'polygon 165,269 245,196 320,269' \
    -fill '#3C8F54' -draw 'polygon 285,269 348,208 375,269' \
    -font "$FONT" -fill '#FFFFFF' -pointsize 38 -gravity south -annotate +0+20 'Photostream' \
    "$ICON"

convert "$ICON" -resize 290x218! "$STAGE/images/icon_focus_hd.png"
convert "$ICON" -resize 214x144! "$STAGE/images/icon_focus_sd.png"

# Splash (master 1920x1080): dark gradient, icon centered, title underneath.
SPLASH="$STAGE/images/splash_fhd.png"
convert -size 1920x1080 gradient:'#0c2238-#06101c' \
    \( "$ICON" -resize 520x \) -gravity center -geometry +0-50 -composite \
    -font "$FONT" -fill '#FFFFFF' -pointsize 56 -gravity center -annotate +0+260 'Photostream' \
    "$SPLASH"

convert "$SPLASH" -resize 1280x720! "$STAGE/images/splash_hd.png"
convert "$SPLASH" -resize 720x480!  "$STAGE/images/splash_sd.png"

mkdir -p "$OUT"
rm -f "$OUT/screensaver.zip"
( cd "$STAGE" && zip -r -q "$OUT/screensaver.zip" manifest source components images )

echo "Built $OUT/screensaver.zip"
