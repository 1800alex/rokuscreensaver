# Build both halves of the project with nothing on the host but Docker + make.
#
#   make           # build backend image, calrender image, and roku zip
#   make backend   # build the Go backend Docker image
#   make calrender # build the Go calendar-render Docker image
#   make roku      # build the sideloadable Roku screensaver zip -> dist/
#   make install   # build + sideload the zip onto the Roku (see ROKU_* vars)
#   make run       # run the backend container (see vars below)
#   make clean     # remove dist/

BACKEND_DIR   := backend
CALRENDER_DIR := calrender
ROKU_DIR      := roku
DIST          := dist

BACKEND_IMAGE   := rokuscreensaver-backend
CALRENDER_IMAGE := rokuscreensaver-calrender
ROKU_BUILDER    := rokuscreensaver-roku-builder

# --- backend run-time config (override on the command line) -----------------
PHOTO_DIR              ?= $(CURDIR)/photos
CALENDAR_PHOTO         ?= $(CURDIR)/calendar/calendar.jpg
CALENDAR_INTERVAL      ?= 10
IMAGE_DURATION_SECONDS ?= 10
PORT                   ?= 9543

# --- Roku sideload config (override on the command line) --------------------
ROKU_IP   ?= 192.168.1.201
ROKU_USER ?= rokudev
ROKU_PASS ?= roku

.PHONY: all backend calrender roku install run clean

all: backend calrender roku

backend:
	docker build -t $(BACKEND_IMAGE) $(BACKEND_DIR)

calrender:
	docker build -t $(CALRENDER_IMAGE) $(CALRENDER_DIR)

roku: | $(DIST)
	docker build -t $(ROKU_BUILDER) $(ROKU_DIR)
	docker run --rm \
		-v $(CURDIR)/$(ROKU_DIR):/app:ro \
		-v $(CURDIR)/$(DIST):/out \
		$(ROKU_BUILDER)

# Sideload the built zip straight to the Roku's developer installer, bypassing
# the browser upload (which can fail with net::ERR_ACCESS_DENIED). Uses HTTP
# digest auth. Override creds/IP like:
#   make install ROKU_IP=192.168.1.201 ROKU_USER=rokudev ROKU_PASS=yourpass
install: roku
	@echo "Sideloading dist/screensaver.zip -> http://$(ROKU_IP)/ ..."
	@curl -s --max-time 60 --user '$(ROKU_USER):$(ROKU_PASS)' --digest \
		-F 'mysubmit=Install' \
		-F 'archive=@$(DIST)/screensaver.zip;type=application/zip' \
		http://$(ROKU_IP)/plugin_install \
		| grep -ioE 'install success|identical to the currently|compile[^<]*|install failure[^<]*|failed[^<]*' \
		| head -5 \
		|| echo "No recognizable status in response — check creds / dev mode."

# Convenience target to run the backend. Mounts your photo dir and calendar
# image into the container. Example:
#   make run PHOTO_DIR=/srv/family-photos CALENDAR_PHOTO=/srv/cal/week.png
run: backend
	docker run --rm -p $(PORT):$(PORT) \
		-e PHOTO_DIR=/photos \
		-e CALENDAR_PHOTO=/calendar/calendar.jpg \
		-e CALENDAR_INTERVAL=$(CALENDAR_INTERVAL) \
		-e IMAGE_DURATION_SECONDS=$(IMAGE_DURATION_SECONDS) \
		-e PORT=$(PORT) \
		-v $(PHOTO_DIR):/photos:ro \
		-v $(dir $(CALENDAR_PHOTO)):/calendar:ro \
		$(BACKEND_IMAGE)

$(DIST):
	mkdir -p $(DIST)

clean:
	rm -rf $(DIST)
