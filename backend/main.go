// Command backend serves a single, full-screen JPEG at /feed.jpg (1920x1080 by
// default; set WIDTH/HEIGHT for other resolutions, e.g. 3840x2160 for 4K).
//
// Photos come from one of two sources:
//   - a local directory (PHOTO_DIR), or
//   - an Immich album (set IMMICH_ALBUM_ID + IMMICH_URL + IMMICH_API_KEY).
//
// It advances to the next photo every IMAGE_DURATION_SECONDS, and after every
// CALENDAR_INTERVAL photos it shows the calendar image at CALENDAR_PHOTO (a
// local file, used in either mode). Every image is normalized to the configured
// resolution by shelling out to ImageMagick's `convert` (installed in the
// Dockerfile); nothing is ever cropped — the bars around it are either black or
// a zoomed, blurred copy of the image (BLUR_FILL, the "Apple TV" effect).
//
// The Roku screensaver just polls /feed.jpg on its own timer; all of the
// playlist/cycling logic lives here.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type config struct {
	listenAddr       string        // e.g. ":9543"
	photoDir         string        // directory of source photos (dir mode)
	calendarPhoto    string        // path to the calendar image (optional)
	calendarInterval int           // show calendar after this many photos (0 = never)
	duration         time.Duration // how long each image is the "current" one
	prefetch         int           // how many converted frames to keep pre-built (>=1)

	width  int // output frame width in pixels (e.g. 1920, or 3840 for 4K)
	height int // output frame height in pixels (e.g. 1080, or 2160 for 4K)

	// Fill the letterbox bars with a zoomed, blurred copy of the same image
	// instead of plain black (the "Apple TV" effect).
	blurFill     bool // enabled by default; set BLUR_FILL=0 for plain black bars
	blurSigma    int  // Gaussian blur strength for the background (px)
	bgBrightness int  // background brightness %, <100 darkens so the photo pops

	// Batch mode: pre-render every source image to cacheDir and serve from there,
	// re-syncing (render new, delete gone) on the refresh schedule. cacheDir MUST
	// differ from photoDir — it's a derived cache the sync prunes, never originals.
	batch    bool   // enable batch pre-render + periodic sync
	cacheDir string // where pre-rendered frames live (default /cache)

	// Immich mode (active when immichAlbum != "").
	immichURL     string        // e.g. http://192.168.1.20:2283
	immichKey     string        // API key
	immichAlbum   string        // album UUID
	immichSize    string        // "original" (default), "preview", or "thumbnail"
	immichRefresh time.Duration // how often to re-fetch the album's asset IDs
}

// photoSource abstracts where the cycling photos come from. Identifiers are
// opaque strings (a file path for dirSource, an asset UUID for immichSource).
type photoSource interface {
	list() ([]string, error)        // current set of photo identifiers
	open(id string) ([]byte, error) // raw image bytes for an identifier
	label(id string) string         // short human label for logging
}

// server holds the currently-served JPEG and the playlist cursor.
type server struct {
	cfg    config
	source photoSource

	// refreshInterval controls how often the photo list is re-fetched. 0 means
	// "re-list every time the cursor wraps" (cheap, used for the local dir).
	refreshInterval time.Duration
	randomize       bool // shuffle the playlist (used for Immich)

	curMu     sync.RWMutex
	buildMu   sync.Mutex     // serializes frame production (one convert at a time)
	current   *cachedFrame   // the frame currently being served
	currentAt time.Time      // when current was promoted (for the duration cache)
	ready     []*cachedFrame // pre-built frames (FIFO), ready to promote instantly
	building  bool           // a background fill of the ready buffer is in flight

	plMu          sync.Mutex // guards the playlist cursor below
	photos        []string   // cached identifiers from source.list()
	photoIdx      int        // cursor into photos
	shownSinceCal int        // photos shown since the last calendar image
	lastFetch     time.Time  // when source.list() last succeeded
	recent        []string   // ring buffer of recently shown identifiers (randomize only)
}

func main() {
	cfg := loadConfig()

	s := &server{cfg: cfg, source: buildSource(cfg)}
	if cfg.immichAlbum != "" {
		// Immich: re-fetch IDs on the configured interval, shuffle the order.
		s.refreshInterval = cfg.immichRefresh
		s.randomize = true
	}
	// Otherwise (local dir) refreshInterval stays 0 = re-list on every wrap.

	log.Printf("config: listen=%s resolution=%dx%d blurFill=%t prefetch=%d batch=%t cache=%q calendar=%q calendarInterval=%d duration=%s",
		cfg.listenAddr, cfg.width, cfg.height, cfg.blurFill, cfg.prefetch, cfg.batch, cfg.cacheDir, cfg.calendarPhoto, cfg.calendarInterval, cfg.duration)

	if cfg.batch {
		// Refuse to run if the cache points at the originals — sync prunes the
		// cache, so this guard (plus the hashed-only file matching in pruneCache)
		// keeps it from ever deleting a user's photos.
		if cfg.cacheDir == "" || filepath.Clean(cfg.cacheDir) == filepath.Clean(cfg.photoDir) {
			log.Fatalf("BATCH mode: CACHE_DIR (%q) must be set and different from PHOTO_DIR (%q)",
				cfg.cacheDir, cfg.photoDir)
		}
		// Pre-render the whole source now, then re-sync on the refresh schedule.
		go s.syncLoop()
	}

	// Frames are produced lazily, on demand (see frame): nothing is downloaded
	// or converted until a request arrives, and a frame is reused for
	// cfg.duration. An idle screensaver does no work (in batch mode it serves
	// pre-rendered frames from the cache).

	mux := http.NewServeMux()
	mux.HandleFunc("/feed.jpg", s.handleFeed)
	mux.HandleFunc("/feed", s.handleFeed)
	mux.HandleFunc("/feed.id", s.handleID)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	log.Printf("listening on %s", cfg.listenAddr)
	if err := http.ListenAndServe(cfg.listenAddr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// buildSource selects the photo source from config, logging which one is used.
func buildSource(cfg config) photoSource {
	if cfg.immichAlbum != "" {
		if cfg.immichURL == "" || cfg.immichKey == "" {
			log.Fatal("IMMICH_ALBUM_ID is set but IMMICH_URL and/or IMMICH_API_KEY are missing")
		}
		log.Printf("photo source: Immich album %s at %s (size=%s)",
			cfg.immichAlbum, cfg.immichURL, cfg.immichSize)
		return &immichSource{
			baseURL: cfg.immichURL,
			apiKey:  cfg.immichKey,
			albumID: cfg.immichAlbum,
			size:    cfg.immichSize,
			client:  &http.Client{Timeout: 30 * time.Second},
		}
	}
	log.Printf("photo source: directory %s", cfg.photoDir)
	return &dirSource{dir: cfg.photoDir}
}

// firstEnv returns the value of the first non-empty environment variable in
// keys, or "" if none are set. Used for accepting alias names.
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func loadConfig() config {
	c := config{
		listenAddr:       ":9543",
		photoDir:         "/photos",
		calendarPhoto:    "",
		calendarInterval: 10,
		duration:         10 * time.Second,
		prefetch:         1,
		cacheDir:         "/cache",
		immichSize:       "original",
		width:            1920,
		height:           1080,
		blurFill:         true,
		blurSigma:        25,
		bgBrightness:     70,
	}

	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		c.listenAddr = v
	} else if p := os.Getenv("PORT"); p != "" {
		c.listenAddr = ":" + p
	}
	if v := os.Getenv("PHOTO_DIR"); v != "" {
		c.photoDir = v
	}
	if v := os.Getenv("CALENDAR_PHOTO"); v != "" {
		c.calendarPhoto = v
	}
	if v := os.Getenv("CALENDAR_INTERVAL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.calendarInterval = n
		}
	}
	if v := os.Getenv("IMAGE_DURATION_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.duration = time.Duration(n) * time.Second
		}
	}
	// PREFETCH is how many frames to keep converted and ready ahead of time, so
	// an advance is instant. Larger values trade memory (one JPEG each) for a
	// deeper buffer; clamped to at least 1.
	if v := os.Getenv("PREFETCH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			c.prefetch = n
		}
	}
	// Output resolution. WIDTH/HEIGHT (or SCREEN_WIDTH/SCREEN_HEIGHT) override
	// the 1920x1080 default; bump to 3840x2160 for a 4K display.
	if v := firstEnv("WIDTH", "SCREEN_WIDTH"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.width = n
		}
	}
	if v := firstEnv("HEIGHT", "SCREEN_HEIGHT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.height = n
		}
	}
	if v := os.Getenv("BLUR_FILL"); v != "" {
		// Disable on the usual "off" spellings; anything else leaves it on.
		switch strings.ToLower(v) {
		case "0", "false", "no", "off":
			c.blurFill = false
		}
	}
	if v := os.Getenv("BLUR_SIGMA"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.blurSigma = n
		}
	}
	if v := os.Getenv("BG_BRIGHTNESS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.bgBrightness = n
		}
	}

	// Batch pre-render + sync. CACHE_DIR (alias BATCH_DIR) is the derived-frame
	// cache; it must differ from PHOTO_DIR (enforced in main) so sync's pruning
	// can never delete originals.
	if v := os.Getenv("BATCH"); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			c.batch = true
		}
	}
	if v := firstEnv("CACHE_DIR", "BATCH_DIR"); v != "" {
		c.cacheDir = v
	}

	// Immich config. IMMICH_HOST is accepted as an alias for IMMICH_URL.
	c.immichURL = strings.TrimRight(os.Getenv("IMMICH_URL"), "/")
	if c.immichURL == "" {
		c.immichURL = strings.TrimRight(os.Getenv("IMMICH_HOST"), "/")
	}
	c.immichKey = os.Getenv("IMMICH_API_KEY")
	c.immichAlbum = os.Getenv("IMMICH_ALBUM_ID")
	if v := os.Getenv("IMMICH_IMAGE_SIZE"); v != "" {
		c.immichSize = v
	}
	c.immichRefresh = 24 * 60 * time.Minute
	if v := os.Getenv("IMMICH_REFRESH_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.immichRefresh = time.Duration(n) * time.Minute
		}
	}
	return c
}

// cachedFrame is a converted JPEG plus a short label, logged when it is promoted
// to the screen (built ahead of time, so the label is logged at display time).
type cachedFrame struct {
	jpeg  []byte
	label string
	id    string // content hash, used by clients to detect a real change
}

// newFrame wraps JPEG bytes with a short content hash so clients can tell when
// the served image has actually changed (vs. the same frame being re-fetched).
func newFrame(jpeg []byte, label string) *cachedFrame {
	sum := sha256.Sum256(jpeg)
	return &cachedFrame{jpeg: jpeg, label: label, id: hex.EncodeToString(sum[:8])}
}

// frame returns the JPEG to serve. A frame is reused for cfg.duration; once it
// ages out, the oldest pre-built frame is promoted instantly (no waiting on a
// download/convert) and the buffer is topped back up in the background. The very
// first request builds inline. An idle screensaver only ever holds up to
// cfg.prefetch spare frames and then does no further work.
func (s *server) frame() *cachedFrame {
	s.curMu.Lock()
	// Promote the oldest ready frame the moment the current one ages out.
	if (s.current == nil || time.Since(s.currentAt) >= s.cfg.duration) && len(s.ready) > 0 {
		s.current = s.ready[0]
		s.ready[0] = nil // release the popped frame for GC
		s.ready = s.ready[1:]
		s.currentAt = time.Now()
		log.Printf("now serving %s", s.current.label)
	}
	current := s.current
	// Top the buffer back up to cfg.prefetch frames if it has drained.
	startBuild := len(s.ready) < s.cfg.prefetch && !s.building
	if startBuild {
		s.building = true
	}
	s.curMu.Unlock()

	if current == nil {
		// Cold start: build the first frame inline so this request can serve it.
		s.buildCurrent()
		s.curMu.RLock()
		current = s.current
		s.curMu.RUnlock()
	}

	if startBuild {
		go s.fillReady()
	}

	return current
}

// buildCurrent produces the first frame and installs it as current. Used only on
// cold start; serialized with fillReady so two converts never run at once, and a
// no-op if another request already filled current.
func (s *server) buildCurrent() {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()

	s.curMu.RLock()
	have := s.current != nil
	s.curMu.RUnlock()
	if have {
		return
	}

	f := s.produce()
	if f == nil {
		return // transient failure; the next request retries
	}
	s.curMu.Lock()
	s.current = f
	s.currentAt = time.Now()
	s.curMu.Unlock()
	log.Printf("now serving %s", f.label)
}

// fillReady builds frames until the buffer holds cfg.prefetch of them, one at a
// time (serialized with buildCurrent) so only one convert runs at once. It clears
// the in-flight flag when it stops — at capacity, or on a transient failure to be
// retried on the next request.
func (s *server) fillReady() {
	for {
		s.curMu.RLock()
		need := len(s.ready) < s.cfg.prefetch
		s.curMu.RUnlock()
		if !need {
			break
		}

		s.buildMu.Lock()
		f := s.produce()
		s.buildMu.Unlock()
		if f == nil {
			break // transient failure; retry on a later request
		}

		s.curMu.Lock()
		s.ready = append(s.ready, f)
		s.curMu.Unlock()
	}

	s.curMu.Lock()
	s.building = false
	s.curMu.Unlock()
}

// produce picks the next image and converts it, returning the ready-to-serve
// frame. It returns a black frame when the source is empty, or nil on a
// transient fetch/convert error (the caller keeps whatever it already had).
func (s *server) produce() *cachedFrame {
	id, isCal := s.next()
	if id == "" {
		// Nothing to show yet (e.g. empty album/dir): a black frame.
		jpeg, err := blackFrame(s.cfg)
		if err != nil {
			log.Printf("blackFrame: %v", err)
			return nil
		}
		return newFrame(jpeg, "black frame")
	}

	jpeg, err := s.jpegFor(id, isCal)
	if err != nil {
		log.Printf("render %s: %v", s.source.label(id), err)
		return nil
	}
	kind := "photo"
	name := s.source.label(id)
	if isCal {
		kind = "calendar"
		name = filepath.Base(s.cfg.calendarPhoto)
	}
	return newFrame(jpeg, kind+": "+name)
}

// jpegFor returns the converted JPEG for id. In batch mode it serves the
// pre-rendered file from cacheDir, rendering and caching on a miss; otherwise it
// converts on the fly. The calendar is never cached (it's a local file that
// changes in place), so it always converts fresh.
func (s *server) jpegFor(id string, isCal bool) ([]byte, error) {
	if s.cfg.batch && !isCal {
		path := s.cachePath(id)
		if b, err := os.ReadFile(path); err == nil {
			return b, nil // cache hit
		}
		return s.renderAndCache(id, path)
	}
	raw, err := s.read(id, isCal)
	if err != nil {
		return nil, err
	}
	return convertRaw(raw, s.cfg)
}

// renderAndCache downloads + converts the source image for id and writes the
// result to path (atomically). A cache-write failure is logged but not fatal —
// the rendered bytes are still returned. NOT internally locked: callers that may
// run concurrently must hold buildMu (the serve path already does via produce).
func (s *server) renderAndCache(id, path string) ([]byte, error) {
	raw, err := s.source.open(id)
	if err != nil {
		return nil, err
	}
	jpeg, err := convertRaw(raw, s.cfg)
	if err != nil {
		return nil, err
	}
	if err := s.writeCacheFile(path, jpeg); err != nil {
		log.Printf("batch: cache write %s: %v", filepath.Base(path), err)
	}
	return jpeg, nil
}

// ---- batch pre-render + sync -------------------------------------------------

// cacheFileRE matches the names this server gives its cache files: a hex SHA-256
// plus ".jpg". pruneCache only ever deletes files matching this, so foreign
// files that happen to sit in cacheDir (or the user's photos, if misconfigured)
// are never removed.
var cacheFileRE = regexp.MustCompile(`^[0-9a-f]{64}\.jpg$`)

// renderSig is a signature of the settings that affect a rendered frame. It's
// mixed into the cache key so changing resolution or the blur fill produces new
// cache files (and the old ones get pruned automatically on the next sync).
func (s *server) renderSig() string {
	return fmt.Sprintf("%dx%d|fill=%t|sigma=%d|bright=%d",
		s.cfg.width, s.cfg.height, s.cfg.blurFill, s.cfg.blurSigma, s.cfg.bgBrightness)
}

// cachePath maps a source identifier to its file in cacheDir, keyed by a hash of
// (id + render settings) so it's stable across runs and safe for any id (the
// Immich UUID or a local file path with slashes).
func (s *server) cachePath(id string) string {
	sum := sha256.Sum256([]byte(id + "\x00" + s.renderSig()))
	return filepath.Join(s.cfg.cacheDir, hex.EncodeToString(sum[:])+".jpg")
}

// writeCacheFile writes b to path atomically (temp file + rename) so a reader
// never sees a half-written frame.
func (s *server) writeCacheFile(path string, b []byte) error {
	if err := os.MkdirAll(s.cfg.cacheDir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// syncLoop runs an initial batch sync, then repeats it on the refresh schedule
// (the Immich refresh interval, or daily in directory mode).
func (s *server) syncLoop() {
	s.syncCache()

	interval := s.refreshInterval
	if interval <= 0 {
		interval = 24 * time.Hour // dir mode has no refresh interval of its own
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		s.syncCache()
	}
}

// syncCache pre-renders every current source image into cacheDir, then deletes
// cache files whose source is gone — and, because the render settings are part
// of the key, any frames left over from older settings. It only ever creates or
// deletes files matching cacheFileRE inside cacheDir, so originals are untouched.
// Each render holds buildMu so it never runs a convert alongside a serve build.
func (s *server) syncCache() {
	ids, err := s.source.list()
	if err != nil {
		log.Printf("batch sync: list source: %v", err)
		return
	}

	keep := make(map[string]bool, len(ids))
	rendered := 0
	for _, id := range ids {
		path := s.cachePath(id)
		keep[filepath.Base(path)] = true
		if _, err := os.Stat(path); err == nil {
			continue // already rendered for the current settings
		}
		s.buildMu.Lock()
		_, err := s.renderAndCache(id, path)
		s.buildMu.Unlock()
		if err != nil {
			log.Printf("batch sync: render %s: %v", s.source.label(id), err)
			continue
		}
		rendered++
	}

	removed := s.pruneCache(keep)
	log.Printf("batch sync: %d source images, %d newly rendered, %d removed (cache=%s)",
		len(ids), rendered, removed, s.cfg.cacheDir)
}

// pruneCache deletes every cache file not in keep. It skips directories and any
// name that isn't one of our hashed <sha256>.jpg files, so nothing the server
// didn't create is ever removed.
func (s *server) pruneCache(keep map[string]bool) int {
	entries, err := os.ReadDir(s.cfg.cacheDir)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("batch sync: read cache dir: %v", err)
		}
		return 0
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !cacheFileRE.MatchString(name) {
			continue // never touch foreign files
		}
		if keep[name] {
			continue
		}
		if err := os.Remove(filepath.Join(s.cfg.cacheDir, name)); err != nil {
			log.Printf("batch sync: remove %s: %v", name, err)
			continue
		}
		removed++
	}
	return removed
}

// read returns the raw bytes for an identifier. The calendar is always a local
// file; everything else comes from the configured source.
func (s *server) read(id string, isCal bool) ([]byte, error) {
	if isCal {
		return os.ReadFile(s.cfg.calendarPhoto)
	}
	return s.source.open(id)
}

// next returns the identifier of the next image to display and whether it is
// the calendar image. It refreshes the photo list each time the cursor wraps,
// so newly-added photos are picked up automatically.
func (s *server) next() (string, bool) {
	s.plMu.Lock()
	defer s.plMu.Unlock()

	// Time for the calendar? (Only when there are photos to count between —
	// shownSinceCal never advances while the list is empty, so an empty source
	// never triggers the calendar.)
	if s.cfg.calendarPhoto != "" && s.cfg.calendarInterval > 0 &&
		s.shownSinceCal >= s.cfg.calendarInterval {
		s.shownSinceCal = 0
		return s.cfg.calendarPhoto, true
	}

	now := time.Now()
	wrapped := s.photoIdx >= len(s.photos)

	// Decide whether to (re)fetch the list: always when empty/never-fetched;
	// on wrap when refreshInterval is 0 (local dir) or has elapsed (Immich).
	refetch := len(s.photos) == 0 || s.lastFetch.IsZero()
	if wrapped && !refetch {
		if s.refreshInterval <= 0 || now.Sub(s.lastFetch) >= s.refreshInterval {
			refetch = true
		}
	}
	if refetch {
		// On error, keep the old list so a transient hiccup doesn't blank out.
		if list, err := s.source.list(); err != nil {
			log.Printf("refresh photo list: %v", err)
		} else {
			if s.randomize {
				shuffle(list)
			}
			s.photos = list
			s.photoIdx = 0
			s.lastFetch = now
		}
	}

	// No photos: skip the calendar entirely and serve nothing (black frame).
	if len(s.photos) == 0 {
		return "", false
	}

	// Recent-window size: avoid repeating a photo until we've shown this many
	// others. Capped at half the pool so a non-recent photo always exists.
	limit := len(s.photos) / 2
	if limit > 25 {
		limit = 25
	}

	// Walk the shuffled list, reshuffling each time the cursor wraps so every
	// loop is a fresh random order (Immich only). When randomizing, skip any
	// photo still inside the recent window — this kills back-to-back repeats at
	// the wrap boundary, the main source of duplicates. The scan terminates:
	// at most `limit` (<= len/2) photos are recent, so within one wrapped pass
	// we always reach a non-recent one.
	p := s.photos[s.photoIdx] // fallback (e.g. local dir, or limit == 0)
	for i := 0; i < len(s.photos)+1; i++ {
		if s.photoIdx >= len(s.photos) {
			s.photoIdx = 0
			if s.randomize {
				shuffle(s.photos)
			}
		}
		p = s.photos[s.photoIdx]
		s.photoIdx++
		if !s.randomize || limit == 0 || !contains(s.recent, p) {
			break
		}
	}

	if s.randomize && limit > 0 {
		s.recent = append(s.recent, p)
		if len(s.recent) > limit {
			s.recent = s.recent[len(s.recent)-limit:]
		}
	}

	s.shownSinceCal++
	return p, false
}

// contains reports whether v is in ss.
func contains(ss []string, v string) bool {
	for _, x := range ss {
		if x == v {
			return true
		}
	}
	return false
}

// shuffle randomizes s in place.
func shuffle(s []string) {
	rand.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
}

func (s *server) handleFeed(w http.ResponseWriter, r *http.Request) {
	f := s.frame()
	if f == nil {
		http.Error(w, "no image available yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("ETag", `"`+f.id+`"`)
	_, _ = w.Write(f.jpeg)
}

// handleID returns just the current frame's content hash (cheap), so a client
// can poll to detect when the image actually changes without downloading it.
func (s *server) handleID(w http.ResponseWriter, r *http.Request) {
	f := s.frame()
	if f == nil {
		http.Error(w, "no image available yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	_, _ = w.Write([]byte(f.id))
}

// ---- dirSource: photos from a local directory --------------------------------

type dirSource struct{ dir string }

func (s *dirSource) list() ([]string, error)        { return listPhotos(s.dir), nil }
func (s *dirSource) open(id string) ([]byte, error) { return os.ReadFile(id) }
func (s *dirSource) label(id string) string         { return filepath.Base(id) }

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".gif": true, ".bmp": true, ".tif": true, ".tiff": true, ".heic": true,
}

// listPhotos returns the sorted set of image files directly inside dir.
func listPhotos(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("read photo dir %s: %v", dir, err)
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if imageExts[strings.ToLower(filepath.Ext(e.Name()))] {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// ---- immichSource: photos from an Immich album -------------------------------

type immichSource struct {
	baseURL string
	apiKey  string
	albumID string
	size    string // "original", "preview", or "thumbnail"
	client  *http.Client
}

// list fetches the album and returns the IDs of its image assets.
func (s *immichSource) list() ([]string, error) {
	url := s.baseURL + "/api/albums/" + s.albumID
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", s.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("album request: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var album struct {
		Assets []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&album); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(album.Assets))
	for _, a := range album.Assets {
		if strings.EqualFold(a.Type, "IMAGE") {
			ids = append(ids, a.ID)
		}
	}
	log.Printf("Immich album: %d image assets", len(ids))
	return ids, nil
}

// open downloads the bytes for an asset, either the original file or a
// server-rendered thumbnail/preview JPEG.
func (s *immichSource) open(id string) ([]byte, error) {
	var url string
	switch s.size {
	case "preview", "thumbnail":
		url = s.baseURL + "/api/assets/" + id + "/thumbnail?size=" + s.size
	default: // "original"
		url = s.baseURL + "/api/assets/" + id + "/original"
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-api-key", s.apiKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asset %s: %s", id, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func (s *immichSource) label(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ---- image conversion --------------------------------------------------------

// convertRaw normalizes any source image (given as raw bytes) to a
// cfg.width x cfg.height JPEG using ImageMagick. The whole image is always shown
// without cropping; the surrounding bars are either plain black (cfg.blurFill ==
// false) or a zoomed, blurred copy of the same image (the "Apple TV" fill).
func convertRaw(raw []byte, cfg config) ([]byte, error) {
	dims := strconv.Itoa(cfg.width) + "x" + strconv.Itoa(cfg.height)
	in, err := os.CreateTemp("", "in-*")
	if err != nil {
		return nil, err
	}
	inName := in.Name()
	defer os.Remove(inName)
	if _, err := in.Write(raw); err != nil {
		in.Close()
		return nil, err
	}
	in.Close()

	outName := inName + ".out.jpg"
	defer os.Remove(outName)

	var cmd *exec.Cmd
	if cfg.blurFill {
		// Build two layers from the same source and composite:
		//   clone 0 -> background: "cover" the frame (^ + crop) so it fills
		//              edge-to-edge, then blur and (optionally) darken it.
		//   clone 0 -> foreground: "contain" the frame so the whole photo fits.
		// Deleting the original leaves [bg, fg]; -composite lays fg over bg.
		cmd = exec.Command("convert",
			inName+"[0]", // [0] = first frame, in case of multi-frame inputs
			"-auto-orient",
			"(", "-clone", "0",
			"-resize", dims+"^",
			"-gravity", "center",
			"-extent", dims,
			"-blur", "0x"+strconv.Itoa(cfg.blurSigma),
			"-modulate", strconv.Itoa(cfg.bgBrightness),
			")",
			"(", "-clone", "0",
			"-resize", dims,
			")",
			"-delete", "0",
			"-gravity", "center",
			"-compose", "over", "-composite",
			"-quality", "85",
			outName,
		)
	} else {
		cmd = exec.Command("convert",
			inName+"[0]", // [0] = first frame, in case of multi-frame inputs
			"-auto-orient",
			"-resize", dims,
			"-background", "black",
			"-gravity", "center",
			"-extent", dims,
			"-quality", "85",
			outName,
		)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("convert output: %s", strings.TrimSpace(string(out)))
		return nil, err
	}
	return os.ReadFile(outName)
}

// blackFrame produces a plain black cfg.width x cfg.height JPEG, used when there
// is nothing to display.
func blackFrame(cfg config) ([]byte, error) {
	tmp, err := os.CreateTemp("", "black-*.jpg")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpName)

	dims := strconv.Itoa(cfg.width) + "x" + strconv.Itoa(cfg.height)
	cmd := exec.Command("convert", "-size", dims, "xc:black", tmpName)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("blackFrame output: %s", strings.TrimSpace(string(out)))
		return nil, err
	}
	return os.ReadFile(tmpName)
}
