// Command calrender fetches an iCalendar (.ics) feed for the week starting today
// and renders it into a single, TV-friendly schedule JPEG.
//
// It is a companion to the Roku photo backend: point this container's OUTPUT_PATH
// at the same file the backend serves as CALENDAR_PHOTO (a shared volume), and the
// screensaver will periodically show an up-to-date weekly schedule.
//
// The feed URL is built from CAL_URL_TEMPLATE, a Go text/template with these
// fields (dates are computed from "today" in TZ):
//
//	{{.Start}} {{.End}}                 YYYY-MM-DD (End is exclusive: Start + DAYS)
//	{{.StartYear}} {{.StartMonth}} {{.StartDay}}
//	{{.EndYear}}   {{.EndMonth}}   {{.EndDay}}
//	{{.TZ}}                             e.g. America/New_York
//	{{.TZEnc}}                          URL-encoded TZ (America%2FNew_York)
//
// Rendering: the schedule is laid out as SVG, rasterized to PNG (rsvg-convert,
// falling back to ImageMagick), and JPEG-encoded in-process. When a day has more
// events than fit, the font shrinks to a floor; past that, extra events collapse
// into a "+N more" line.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
)

// defaultURLTemplate is an example calendar export endpoint with the dynamic
// date range and timezone tokenized. It almost certainly won't resolve on your
// network — override the whole thing with CAL_URL_TEMPLATE pointing at your own
// .ics feed (see the README for the available tokens).
const defaultURLTemplate = "http://calendar.lan:8100/export/download?" +
	"feeds=US+Holidays+%28Google+Public%29" +
	"&start={{.Start}}&end={{.End}}&tz={{.TZEnc}}"

type config struct {
	urlTemplate string
	tz          string
	loc         *time.Location
	width       int
	height      int
	days        int
	title       string
	outputPath  string
	refresh     time.Duration
	listenAddr  string

	// Weather (top-right of the header) from wttr.in's j1 JSON. Disabled by default.
	weatherEnabled  bool
	weatherLocation string // wttr.in location, e.g. "New York"
	weatherUnits    string // "F" or "C"
	weatherURL      string // full override; if empty, built from weatherLocation
}

func loadConfig() config {
	c := config{
		urlTemplate: defaultURLTemplate,
		tz:          "America/New_York",
		width:       1920,
		height:      1080,
		days:        7,
		title:       "This Week",
		outputPath:  "/out/calendar.jpg",
		refresh:     60 * time.Minute,
		listenAddr:  ":9544",

		weatherLocation: "New York",
		weatherUnits:    "F",
	}

	if v := os.Getenv("CAL_URL_TEMPLATE"); v != "" {
		c.urlTemplate = v
	}
	if v := os.Getenv("TZ"); v != "" {
		c.tz = v
	}
	loc, err := time.LoadLocation(c.tz)
	if err != nil {
		log.Printf("unknown TZ %q (%v); falling back to UTC", c.tz, err)
		loc = time.UTC
		c.tz = "UTC"
	}
	c.loc = loc

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
	if v := os.Getenv("DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 14 {
			c.days = n
		}
	}
	if v := os.Getenv("TITLE"); v != "" {
		c.title = v
	}
	if v := os.Getenv("OUTPUT_PATH"); v != "" {
		c.outputPath = v
	}
	if v := os.Getenv("REFRESH_MINUTES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.refresh = time.Duration(n) * time.Minute
		}
	}
	if v := os.Getenv("LISTEN_ADDR"); v != "" {
		c.listenAddr = v
	} else if p := os.Getenv("PORT"); p != "" {
		c.listenAddr = ":" + p
	}

	if v := os.Getenv("WEATHER"); v != "" {
		switch strings.ToLower(v) {
		case "1", "true", "yes", "on":
			c.weatherEnabled = true
		}
	}
	if v := os.Getenv("WEATHER_LOCATION"); v != "" {
		c.weatherLocation = v
	}
	if v := os.Getenv("WEATHER_UNITS"); v != "" {
		switch strings.ToUpper(v[:1]) {
		case "C":
			c.weatherUnits = "C"
		case "F":
			c.weatherUnits = "F"
		}
	}
	if v := os.Getenv("WEATHER_URL"); v != "" {
		c.weatherURL = v
	}
	return c
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func main() {
	cfg := loadConfig()
	log.Printf("config: tz=%s resolution=%dx%d days=%d refresh=%s output=%q listen=%s weather=%t",
		cfg.tz, cfg.width, cfg.height, cfg.days, cfg.refresh, cfg.outputPath, cfg.listenAddr, cfg.weatherEnabled)

	s := &server{cfg: cfg}

	// Render immediately, then on the refresh schedule (which also rolls the
	// window forward each day).
	s.renderAndStore()
	go s.loop()

	mux := http.NewServeMux()
	mux.HandleFunc("/calendar.jpg", s.handleImage)
	mux.HandleFunc("/calendar", s.handleImage)
	mux.HandleFunc("/preview.jpg", s.handlePreview)
	mux.HandleFunc("/preview", s.handlePreview)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	log.Printf("listening on %s", cfg.listenAddr)
	if err := http.ListenAndServe(cfg.listenAddr, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

type server struct {
	cfg config

	mu  sync.RWMutex
	jpg []byte
}

func (s *server) loop() {
	t := time.NewTicker(s.cfg.refresh)
	defer t.Stop()
	for range t.C {
		s.renderAndStore()
	}
}

// renderAndStore renders the current week and, on success, updates the in-memory
// frame and writes it to OUTPUT_PATH. On failure it keeps the previous frame.
func (s *server) renderAndStore() {
	jpg, err := s.render()
	if err != nil {
		log.Printf("render: %v", err)
		return
	}
	s.mu.Lock()
	s.jpg = jpg
	s.mu.Unlock()

	if err := writeFileAtomic(s.cfg.outputPath, jpg); err != nil {
		log.Printf("write %s: %v", s.cfg.outputPath, err)
		return
	}
	log.Printf("rendered %d bytes -> %s", len(jpg), s.cfg.outputPath)
}

func (s *server) handleImage(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	jpg := s.jpg
	s.mu.RUnlock()
	if jpg == nil {
		http.Error(w, "no calendar rendered yet", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	_, _ = w.Write(jpg)
}

// handlePreview renders a one-off image for an arbitrary window and serves it
// inline, WITHOUT writing OUTPUT_PATH or disturbing the scheduled frame. Useful
// for testing layout against busier weeks. Query params (all optional) override
// the configured values for this render only:
//
//	start=YYYY-MM-DD   window start (default: today)
//	days=N             days in the window, 1–14
//	width=, height=    output resolution in px
//	tz=                timezone (e.g. America/New_York)
//	title=             heading text
//
// Example: /preview.jpg?start=2026-12-21&days=7
func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg // copy; mutate only this request's view
	q := r.URL.Query()

	if v := q.Get("tz"); v != "" {
		loc, err := time.LoadLocation(v)
		if err != nil {
			http.Error(w, "bad tz: "+v, http.StatusBadRequest)
			return
		}
		cfg.tz, cfg.loc = v, loc
	}
	if v := q.Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 14 {
			cfg.days = n
		} else {
			http.Error(w, "days must be 1–14", http.StatusBadRequest)
			return
		}
	}
	if v := q.Get("width"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.width = n
		}
	}
	if v := q.Get("height"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.height = n
		}
	}
	if v := q.Get("title"); v != "" {
		cfg.title = v
	}

	now := time.Now().In(cfg.loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, cfg.loc)
	if v := q.Get("start"); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, cfg.loc)
		if err != nil {
			http.Error(w, "bad start (want YYYY-MM-DD): "+v, http.StatusBadRequest)
			return
		}
		start = t
	}

	jpg, err := (&server{cfg: cfg}).renderAt(start)
	if err != nil {
		http.Error(w, "render: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	_, _ = w.Write(jpg)
}

// render renders the week starting today (in the configured timezone). This is
// what the scheduled refresh stores to OUTPUT_PATH.
func (s *server) render() ([]byte, error) {
	now := time.Now().In(s.cfg.loc)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.cfg.loc)
	return s.renderAt(start)
}

// renderAt performs the full pipeline for a given window start: build the URL,
// fetch the ICS, parse it, group events by day, lay out the SVG, and rasterize to
// JPEG. It has no side effects (no file writes), so it's safe for ad-hoc previews.
func (s *server) renderAt(start time.Time) ([]byte, error) {
	feedURL, err := s.buildURL(start)
	if err != nil {
		return nil, fmt.Errorf("build url: %w", err)
	}

	ics, err := fetchICS(feedURL)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}

	events := parseICS(ics, s.cfg.loc)
	days := groupByDay(events, start, s.cfg.days, s.cfg.loc)

	// Weather is best-effort: a failure logs and renders the calendar without it.
	var wx *weather
	if s.cfg.weatherEnabled {
		if w, err := fetchWeather(s.weatherURL(), s.cfg.weatherUnits, s.cfg.loc); err != nil {
			log.Printf("weather: %v", err)
		} else {
			wx = w
		}
	}

	svg := s.buildSVG(start, days, wx)
	return s.svgToJPEG(svg)
}

// ---- URL templating ----------------------------------------------------------

type tmplVars struct {
	Start, End                      string
	StartYear, StartMonth, StartDay string
	EndYear, EndMonth, EndDay       string
	TZ, TZEnc                       string
}

func (s *server) buildURL(start time.Time) (string, error) {
	end := start.AddDate(0, 0, s.cfg.days)
	v := tmplVars{
		Start:      start.Format("2006-01-02"),
		End:        end.Format("2006-01-02"),
		StartYear:  start.Format("2006"),
		StartMonth: start.Format("01"),
		StartDay:   start.Format("02"),
		EndYear:    end.Format("2006"),
		EndMonth:   end.Format("01"),
		EndDay:     end.Format("02"),
		TZ:         s.cfg.tz,
		TZEnc:      url.QueryEscape(s.cfg.tz),
	}
	t, err := template.New("url").Parse(s.cfg.urlTemplate)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := t.Execute(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func fetchICS(u string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20)) // 16 MiB cap
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ---- ICS parsing -------------------------------------------------------------

type event struct {
	start    time.Time
	end      time.Time
	allDay   bool
	summary  string
	location string
}

// parseICS extracts VEVENTs. It assumes the feed has already expanded recurring
// events into concrete instances for the requested window (the export endpoint
// does this), so RRULE is ignored.
func parseICS(data string, loc *time.Location) []event {
	lines := unfold(data)
	var out []event
	var cur *event
	inEvent := false

	for _, ln := range lines {
		switch {
		case ln == "BEGIN:VEVENT":
			inEvent = true
			cur = &event{}
			continue
		case ln == "END:VEVENT":
			if cur != nil && (!cur.start.IsZero() || cur.summary != "") {
				out = append(out, *cur)
			}
			inEvent = false
			cur = nil
			continue
		}
		if !inEvent || cur == nil {
			continue
		}

		name, params, value := splitProp(ln)
		switch name {
		case "SUMMARY":
			cur.summary = unescapeText(value)
		case "LOCATION":
			cur.location = unescapeText(value)
		case "DTSTART":
			if t, allDay, err := parseICSTime(value, params, loc); err == nil {
				cur.start, cur.allDay = t, allDay
			}
		case "DTEND":
			if t, _, err := parseICSTime(value, params, loc); err == nil {
				cur.end = t
			}
		}
	}
	return out
}

// unfold joins RFC 5545 folded continuation lines (those beginning with a space
// or tab) onto the preceding line, and splits on CRLF/LF.
func unfold(data string) []string {
	raw := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	var out []string
	for _, ln := range raw {
		if len(ln) > 0 && (ln[0] == ' ' || ln[0] == '\t') && len(out) > 0 {
			out[len(out)-1] += ln[1:]
			continue
		}
		out = append(out, ln)
	}
	return out
}

// splitProp parses "NAME;PARAM=val;PARAM2=val:VALUE" into name, params, value.
func splitProp(ln string) (name string, params map[string]string, value string) {
	colon := strings.IndexByte(ln, ':')
	if colon < 0 {
		return "", nil, ""
	}
	left, value := ln[:colon], ln[colon+1:]
	parts := strings.Split(left, ";")
	name = strings.ToUpper(parts[0])
	params = map[string]string{}
	for _, p := range parts[1:] {
		if eq := strings.IndexByte(p, '='); eq >= 0 {
			params[strings.ToUpper(p[:eq])] = strings.Trim(p[eq+1:], `"`)
		}
	}
	return name, params, value
}

func parseICSTime(val string, params map[string]string, loc *time.Location) (time.Time, bool, error) {
	// All-day: VALUE=DATE or a bare 8-digit YYYYMMDD.
	if params["VALUE"] == "DATE" || len(val) == 8 {
		t, err := time.ParseInLocation("20060102", val, loc)
		return t, true, err
	}
	// UTC instant.
	if strings.HasSuffix(val, "Z") {
		t, err := time.ParseInLocation("20060102T150405Z", val, time.UTC)
		return t.In(loc), false, err
	}
	// Zoned or floating local time.
	l := loc
	if tz := params["TZID"]; tz != "" {
		if z, err := time.LoadLocation(tz); err == nil {
			l = z
		}
	}
	t, err := time.ParseInLocation("20060102T150405", val, l)
	return t.In(loc), false, err
}

func unescapeText(s string) string {
	r := strings.NewReplacer(`\n`, " ", `\N`, " ", `\,`, ",", `\;`, ";", `\\`, `\`)
	return strings.TrimSpace(r.Replace(s))
}

// groupByDay buckets events into the `days` columns starting at `start`, by their
// start date in loc. Each day is sorted all-day-first, then by start time.
func groupByDay(events []event, start time.Time, days int, loc *time.Location) [][]event {
	out := make([][]event, days)
	end := start.AddDate(0, 0, days)
	for _, e := range events {
		if e.start.IsZero() || e.start.Before(start) || !e.start.Before(end) {
			continue
		}
		idx := int(e.start.In(loc).Sub(start).Hours()) / 24
		if idx < 0 || idx >= days {
			continue
		}
		out[idx] = append(out[idx], e)
	}
	for i := range out {
		sort.SliceStable(out[i], func(a, b int) bool {
			ea, eb := out[i][a], out[i][b]
			if ea.allDay != eb.allDay {
				return ea.allDay // all-day events first
			}
			return ea.start.Before(eb.start)
		})
	}
	return out
}

// ---- weather (wttr.in j1) ----------------------------------------------------

// weather holds the current conditions we display, already converted to the
// configured units.
type weather struct {
	temp     int
	feels    int
	wind     int
	windUnit string // "mph" or "km/h"
	windDir  string // e.g. "SW" (may be empty)
	precip   int    // % chance, from the nearest hourly forecast
	uv       int
	unit     string // "F" or "C" (for the temperature label)
	icon     string // category: sun|partly|cloud|rain|snow|thunder|fog
	desc     string
}

// wttrJ1 is the subset of wttr.in's ?format=j1 JSON that we read.
type wttrJ1 struct {
	CurrentCondition []struct {
		TempC          string `json:"temp_C"`
		TempF          string `json:"temp_F"`
		FeelsLikeC     string `json:"FeelsLikeC"`
		FeelsLikeF     string `json:"FeelsLikeF"`
		WindspeedMiles string `json:"windspeedMiles"`
		WindspeedKmph  string `json:"windspeedKmph"`
		Winddir16Point string `json:"winddir16Point"`
		UvIndex        string `json:"uvIndex"`
		WeatherCode    string `json:"weatherCode"`
		WeatherDesc    []struct {
			Value string `json:"value"`
		} `json:"weatherDesc"`
	} `json:"current_condition"`
	Weather []struct {
		Hourly []struct {
			Time         string `json:"time"`
			ChanceOfRain string `json:"chanceofrain"`
			ChanceOfSnow string `json:"chanceofsnow"`
		} `json:"hourly"`
	} `json:"weather"`
}

func (s *server) weatherURL() string {
	if s.cfg.weatherURL != "" {
		return s.cfg.weatherURL
	}
	return "https://wttr.in/" + url.PathEscape(s.cfg.weatherLocation) + "?format=j1"
}

func atoiSafe(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// fetchWeather pulls current conditions and converts to the requested units. The
// chance of precipitation comes from the hourly forecast slot nearest to now.
func fetchWeather(u, units string, loc *time.Location) (*weather, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "curl/8") // ensure JSON, not the ANSI/HTML page
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}

	var j wttrJ1
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&j); err != nil {
		return nil, err
	}
	if len(j.CurrentCondition) == 0 {
		return nil, fmt.Errorf("no current_condition in response")
	}
	cc := j.CurrentCondition[0]

	w := &weather{
		windDir: cc.Winddir16Point,
		uv:      atoiSafe(cc.UvIndex),
		unit:    "F",
		icon:    weatherIcon(atoiSafe(cc.WeatherCode)),
	}
	if len(cc.WeatherDesc) > 0 {
		w.desc = cc.WeatherDesc[0].Value
	}
	if strings.EqualFold(units, "C") {
		w.unit, w.windUnit = "C", "km/h"
		w.temp, w.feels, w.wind = atoiSafe(cc.TempC), atoiSafe(cc.FeelsLikeC), atoiSafe(cc.WindspeedKmph)
	} else {
		w.unit, w.windUnit = "F", "mph"
		w.temp, w.feels, w.wind = atoiSafe(cc.TempF), atoiSafe(cc.FeelsLikeF), atoiSafe(cc.WindspeedMiles)
	}

	// Precipitation chance: nearest hourly slot (times are "0","300",…,"2100").
	if len(j.Weather) > 0 {
		nowH := time.Now().In(loc).Hour()
		bestDiff := 1 << 30
		for _, hr := range j.Weather[0].Hourly {
			h := atoiSafe(hr.Time) / 100
			diff := nowH - h
			if diff < 0 {
				diff = -diff
			}
			if diff < bestDiff {
				bestDiff = diff
				p := atoiSafe(hr.ChanceOfRain)
				if sn := atoiSafe(hr.ChanceOfSnow); sn > p {
					p = sn
				}
				w.precip = p
			}
		}
	}
	return w, nil
}

// weatherIcon maps a WWO weather code to one of our drawable categories.
func weatherIcon(code int) string {
	switch code {
	case 113:
		return "sun"
	case 116:
		return "partly"
	case 119, 122:
		return "cloud"
	case 143, 248, 260:
		return "fog"
	case 200, 386, 389, 392, 395:
		return "thunder"
	case 179, 227, 230, 317, 320, 323, 326, 329, 332, 335, 338, 350, 362, 365, 368, 371, 374, 377:
		return "snow"
	case 176, 182, 185, 263, 266, 281, 284, 293, 296, 299, 302, 305, 308, 311, 314, 353, 356, 359:
		return "rain"
	default:
		return "cloud"
	}
}

// ---- SVG layout --------------------------------------------------------------

// Dark, high-contrast palette that reads well from across a room.
const (
	colBG       = "#0f172a"
	colRow      = "#1e293b"
	colTitle    = "#f8fafc"
	colRange    = "#94a3b8"
	colDayName  = "#e2e8f0"
	colDayDate  = "#94a3b8"
	colEvent    = "#e2e8f0"
	colTime     = "#38bdf8"
	colMuted    = "#64748b"
	colToday    = "#f59e0b"
	colTodayRow = "#27344b"

	// Weather icon palette.
	colSun     = "#fbbf24"
	colCloud   = "#cbd5e1"
	colCloudDk = "#94a3b8"
	colRain    = "#38bdf8"
	colSnow    = "#e0f2fe"
	colBolt    = "#fde047"
)

func (s *server) buildSVG(start time.Time, days [][]event, wx *weather) string {
	W, H := float64(s.cfg.width), float64(s.cfg.height)
	today := time.Date(time.Now().In(s.cfg.loc).Year(), time.Now().In(s.cfg.loc).Month(),
		time.Now().In(s.cfg.loc).Day(), 0, 0, 0, 0, s.cfg.loc)

	margin := H * 0.035
	headerH := H * 0.11
	gutterW := W * 0.16
	gap := H * 0.008

	contentY := margin + headerH
	contentH := H - contentY - margin
	n := len(days)
	availH := contentH - gap*float64(n-1)

	rowH := rowHeights(days, availH)

	// Font sizes scaled to the canvas height.
	titleF := H * 0.046
	rangeF := H * 0.026
	dayNameF := H * 0.030
	dayDateF := H * 0.021
	eventF := H * 0.024
	eventMinF := H * 0.016

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="DejaVu Sans, sans-serif">`,
		s.cfg.width, s.cfg.height, s.cfg.width, s.cfg.height)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="%s"/>`, s.cfg.width, s.cfg.height, colBG)

	// Header: title with the date range beneath it (left); weather, if enabled,
	// occupies the top-right.
	end := start.AddDate(0, 0, n-1)
	rangeStr := start.Format("Mon Jan 2") + "  –  " + end.Format("Mon Jan 2")
	titleY := margin + titleF*0.85
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" fill="%s" font-size="%.0f" font-weight="bold">%s</text>`,
		margin, titleY, colTitle, titleF, xmlEscape(s.cfg.title))
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" fill="%s" font-size="%.0f">%s</text>`,
		margin, titleY+rangeF*1.25, colRange, rangeF, xmlEscape(rangeStr))
	if wx != nil {
		writeWeather(&b, wx, W, margin, headerH)
	}

	// Day rows.
	y := contentY
	for d := 0; d < n; d++ {
		date := start.AddDate(0, 0, d)
		isToday := date.Equal(today)
		h := rowH[d]

		rowFill := colRow
		if isToday {
			rowFill = colTodayRow
		}
		fmt.Fprintf(&b, `<rect x="%.0f" y="%.0f" width="%.0f" height="%.0f" rx="%.0f" fill="%s"/>`,
			margin, y, W-2*margin, h, H*0.012, rowFill)
		if isToday {
			fmt.Fprintf(&b, `<rect x="%.0f" y="%.0f" width="%.0f" height="%.0f" rx="%.0f" fill="%s"/>`,
				margin, y, H*0.008, h, H*0.004, colToday)
		}

		// Left gutter: weekday + date, vertically centered.
		nameCol := colDayName
		if isToday {
			nameCol = colToday
		}
		gx := margin + gutterW*0.10
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" fill="%s" font-size="%.0f" font-weight="bold">%s</text>`,
			gx, y+h*0.5-dayNameF*0.15, nameCol, dayNameF, xmlEscape(date.Format("Monday")))
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" fill="%s" font-size="%.0f">%s</text>`,
			gx, y+h*0.5+dayDateF*1.1, colDayDate, dayDateF, xmlEscape(date.Format("January 2")))

		// Right area: the day's events.
		evX := margin + gutterW
		evW := W - margin - evX - W*0.012
		writeEvents(&b, days[d], evX, y, evW, h, eventF, eventMinF)

		y += h + gap
	}

	// Footer: subtle "updated" stamp, bottom-right.
	upd := "updated " + time.Now().In(s.cfg.loc).Format("Mon Jan 2 3:04 PM")
	fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" fill="%s" font-size="%.0f" text-anchor="end">%s</text>`,
		W-margin, H-margin*0.25, colMuted, H*0.015, xmlEscape(upd))

	b.WriteString(`</svg>`)
	return b.String()
}

// rowHeights distributes availH across the days, giving busier days more space
// (weight = clamped event count) but never less than a readable minimum. If the
// minimums don't fit, it falls back to equal rows.
func rowHeights(days [][]event, availH float64) []float64 {
	n := len(days)
	minH := availH / float64(n) * 0.6
	weights := make([]float64, n)
	var sum float64
	for i, ev := range days {
		w := float64(len(ev))
		if w < 1 {
			w = 1
		}
		if w > 8 {
			w = 8
		}
		weights[i] = w
		sum += w
	}
	out := make([]float64, n)
	var total float64
	for i := range out {
		h := availH * weights[i] / sum
		if h < minH {
			h = minH
		}
		out[i] = h
		total += h
	}
	if total > availH+0.5 { // minimums overflowed: equalize
		for i := range out {
			out[i] = availH / float64(n)
		}
	}
	return out
}

// writeEvents lays out one day's events within its row, shrinking the font to fit
// down to minF, then collapsing the overflow into a "+N more" line.
func writeEvents(b *strings.Builder, evs []event, x, y, w, h, baseF, minF float64) {
	if len(evs) == 0 {
		return
	}
	pad := h * 0.12
	availH := h - 2*pad

	f := baseF
	lineH := f * 1.32
	for f > minF && float64(len(evs))*lineH > availH {
		f -= 1
		lineH = f * 1.32
	}
	maxLines := int(availH / lineH)
	if maxLines < 1 {
		maxLines = 1
	}

	show := evs
	overflow := 0
	if len(evs) > maxLines {
		keep := maxLines - 1
		if keep < 1 {
			keep = 1
		}
		overflow = len(evs) - keep
		show = evs[:keep]
	}

	// Approximate character budget for truncation (no real font metrics in SVG).
	maxChars := int(w / (f * 0.55))
	ty := y + pad + f*0.9
	for _, e := range show {
		timeStr := ""
		if !e.allDay {
			timeStr = strings.ToLower(strings.TrimSpace(e.start.Format("3:04 PM")))
		}
		summary := truncate(e.summary, maxChars-len(timeStr)-2)
		if timeStr != "" {
			// dx adds real horizontal space between the time and the title; SVG
			// collapses literal spaces at a tspan boundary, so a gap char won't do.
			fmt.Fprintf(b, `<text x="%.0f" y="%.0f" font-size="%.0f"><tspan fill="%s">%s</tspan><tspan dx="%.1f" fill="%s">%s</tspan></text>`,
				x, ty, f, colTime, xmlEscape(timeStr), f*0.35, colEvent, xmlEscape(summary))
		} else {
			fmt.Fprintf(b, `<text x="%.0f" y="%.0f" font-size="%.0f" fill="%s">%s</text>`,
				x, ty, f, colEvent, xmlEscape(summary))
		}
		ty += lineH
	}
	if overflow > 0 {
		fmt.Fprintf(b, `<text x="%.0f" y="%.0f" font-size="%.0f" fill="%s">+%d more</text>`,
			x, ty, f*0.92, colMuted, overflow)
	}
}

func truncate(s string, max int) string {
	if max < 1 {
		max = 1
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return "…"
	}
	return strings.TrimSpace(string(r[:max-1])) + "…"
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// writeWeather draws the current conditions in the top-right of the header: an
// icon at the corner, with temperature / feels-like / details right-aligned to
// its left.
func writeWeather(b *strings.Builder, wx *weather, W, margin, headerH float64) {
	iconS := headerH * 0.92
	iconX := W - margin - iconS
	iconY := margin + (headerH-iconS)/2
	drawWeatherIcon(b, wx.icon, iconX, iconY, iconS)

	tx := iconX - W*0.012 // right edge for the text, just left of the icon
	tempF := headerH * 0.42
	subF := headerH * 0.20
	detF := headerH * 0.17

	y := margin + tempF*0.85
	fmt.Fprintf(b, `<text x="%.0f" y="%.0f" fill="%s" font-size="%.0f" font-weight="bold" text-anchor="end">%d°%s</text>`,
		tx, y, colTitle, tempF, wx.temp, wx.unit)

	y += subF * 1.3
	fmt.Fprintf(b, `<text x="%.0f" y="%.0f" fill="%s" font-size="%.0f" text-anchor="end">Feels like %d°</text>`,
		tx, y, colRange, subF, wx.feels)

	y += detF * 1.4
	detail := strings.TrimSpace(fmt.Sprintf("%s %d %s", wx.windDir, wx.wind, wx.windUnit)) +
		fmt.Sprintf(" · %d%% precip · UV %d", wx.precip, wx.uv)
	fmt.Fprintf(b, `<text x="%.0f" y="%.0f" fill="%s" font-size="%.0f" text-anchor="end">%s</text>`,
		tx, y, colMuted, detF, xmlEscape(detail))
}

// drawWeatherIcon renders a simple flat icon for the category inside the box
// [x, y, s, s].
func drawWeatherIcon(b *strings.Builder, cat string, x, y, s float64) {
	cx, cy := x+s/2, y+s/2
	switch cat {
	case "sun":
		drawSun(b, cx, cy, s)
	case "partly":
		drawSun(b, cx-s*0.16, cy-s*0.16, s*0.60)
		drawCloud(b, cx+s*0.04, cy+s*0.12, s*0.82, colCloud)
	case "fog":
		drawFog(b, x, y, s)
	case "rain":
		drawCloud(b, cx, cy-s*0.10, s, colCloudDk)
		drawRain(b, cx, cy+s*0.24, s)
	case "snow":
		drawCloud(b, cx, cy-s*0.10, s, colCloudDk)
		drawSnow(b, cx, cy+s*0.24, s)
	case "thunder":
		drawCloud(b, cx, cy-s*0.10, s, colCloudDk)
		drawBolt(b, cx, cy+s*0.06, s)
	default: // cloud
		drawCloud(b, cx, cy, s, colCloud)
	}
}

func drawSun(b *strings.Builder, cx, cy, s float64) {
	r := s * 0.20
	fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s"/>`, cx, cy, r, colSun)
	for i := 0; i < 8; i++ {
		ang := float64(i) * math.Pi / 4
		x1, y1 := cx+math.Cos(ang)*r*1.5, cy+math.Sin(ang)*r*1.5
		x2, y2 := cx+math.Cos(ang)*r*2.1, cy+math.Sin(ang)*r*2.1
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="%.1f" stroke-linecap="round"/>`,
			x1, y1, x2, y2, colSun, s*0.05)
	}
}

// drawCloud draws a cloud roughly centered on (cx, cy) with overall width ~s.
func drawCloud(b *strings.Builder, cx, cy, s float64, fill string) {
	fmt.Fprintf(b, `<g fill="%s">`, fill)
	fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="%.1f"/>`, cx-s*0.20, cy+s*0.04, s*0.16)
	fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="%.1f"/>`, cx+s*0.02, cy-s*0.08, s*0.22)
	fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="%.1f"/>`, cx+s*0.22, cy+s*0.02, s*0.17)
	fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="%.1f"/>`,
		cx-s*0.36, cy+s*0.02, s*0.72, s*0.18, s*0.09)
	b.WriteString(`</g>`)
}

func drawRain(b *strings.Builder, cx, cy, s float64) {
	for i := -1; i <= 1; i++ {
		x := cx + float64(i)*s*0.18
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="%.1f" stroke-linecap="round"/>`,
			x, cy, x-s*0.04, cy+s*0.16, colRain, s*0.045)
	}
}

func drawSnow(b *strings.Builder, cx, cy, s float64) {
	for i := -1; i <= 1; i++ {
		x := cx + float64(i)*s*0.18
		fmt.Fprintf(b, `<circle cx="%.1f" cy="%.1f" r="%.1f" fill="%s"/>`, x, cy+s*0.08, s*0.045, colSnow)
	}
}

func drawBolt(b *strings.Builder, cx, cy, s float64) {
	fmt.Fprintf(b, `<polygon points="%.1f,%.1f %.1f,%.1f %.1f,%.1f %.1f,%.1f %.1f,%.1f %.1f,%.1f" fill="%s"/>`,
		cx+s*0.04, cy-s*0.02,
		cx-s*0.10, cy+s*0.18,
		cx-s*0.01, cy+s*0.18,
		cx-s*0.06, cy+s*0.32,
		cx+s*0.12, cy+s*0.08,
		cx+s*0.02, cy+s*0.08,
		colBolt)
}

func drawFog(b *strings.Builder, x, y, s float64) {
	for i := 0; i < 4; i++ {
		yy := y + s*0.30 + float64(i)*s*0.16
		fmt.Fprintf(b, `<line x1="%.1f" y1="%.1f" x2="%.1f" y2="%.1f" stroke="%s" stroke-width="%.1f" stroke-linecap="round"/>`,
			x+s*0.15, yy, x+s*0.85, yy, colCloud, s*0.06)
	}
}

// ---- rasterize ---------------------------------------------------------------

// svgToJPEG rasterizes the SVG to PNG (via an external tool) and re-encodes it as
// JPEG in-process with image/jpeg.
func (s *server) svgToJPEG(svg string) ([]byte, error) {
	pngBytes, err := s.svgToPNG(svg)
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode png: %w", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		return nil, fmt.Errorf("encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

// svgToPNG renders the SVG to PNG bytes. It prefers rsvg-convert (the best SVG
// text rendering) and falls back to ImageMagick's `convert` (which uses the same
// librsvg delegate), so it works whichever of the two the image actually ships.
func (s *server) svgToPNG(svg string) ([]byte, error) {
	dims := strconv.Itoa(s.cfg.width) + "x" + strconv.Itoa(s.cfg.height)

	rsvg := exec.Command("rsvg-convert",
		"-w", strconv.Itoa(s.cfg.width),
		"-h", strconv.Itoa(s.cfg.height),
		"-f", "png",
		"--background-color", colBG,
	)
	rsvg.Stdin = strings.NewReader(svg)
	var rsvgErr bytes.Buffer
	rsvg.Stderr = &rsvgErr
	if out, err := rsvg.Output(); err == nil {
		return out, nil
	} else {
		log.Printf("rsvg-convert unavailable/failed (%v: %s); falling back to ImageMagick",
			err, strings.TrimSpace(rsvgErr.String()))
	}

	im := exec.Command("convert",
		"-background", colBG,
		"-density", "96",
		"svg:-",
		"-resize", dims,
		"png:-",
	)
	im.Stdin = strings.NewReader(svg)
	var imErr bytes.Buffer
	im.Stderr = &imErr
	out, err := im.Output()
	if err != nil {
		return nil, fmt.Errorf("convert: %v: %s", err, strings.TrimSpace(imErr.String()))
	}
	return out, nil
}

// ---- util --------------------------------------------------------------------

func writeFileAtomic(path string, b []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
