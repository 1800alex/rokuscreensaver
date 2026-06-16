package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const sampleICS = "BEGIN:VCALENDAR\r\n" +
	"BEGIN:VEVENT\r\n" +
	"SUMMARY:All-day holiday\r\n" +
	"DTSTART;VALUE=DATE:20260609\r\n" +
	"DTEND;VALUE=DATE:20260610\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"SUMMARY:Little League\\, game vs Tigers\r\n" +
	"DTSTART:20260611T230000Z\r\n" + // 23:00 UTC == 18:00 (EST, -05) on the 11th
	"DTEND:20260612T000000Z\r\n" +
	"LOCATION:Field 3\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

func TestParseAndGroup(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	start := time.Date(2026, 6, 9, 0, 0, 0, 0, loc)

	events := parseICS(sampleICS, loc)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}

	if !events[0].allDay {
		t.Errorf("event 0 should be all-day")
	}
	if events[0].summary != "All-day holiday" {
		t.Errorf("event 0 summary = %q", events[0].summary)
	}
	if events[1].summary != "Little League, game vs Tigers" {
		t.Errorf("event 1 summary unescaped wrong: %q", events[1].summary)
	}

	days := groupByDay(events, start, 7, loc)
	if len(days[0]) != 1 {
		t.Errorf("day 0 (Jun 9) should have 1 event, got %d", len(days[0]))
	}
	// 23:00Z on the 11th is 18:00 EST on the 11th -> index 2.
	if len(days[2]) != 1 {
		t.Errorf("day 2 (Jun 11) should have 1 event, got %d", len(days[2]))
	}
	if got := days[2][0].start.Hour(); got != 18 {
		t.Errorf("timed event local hour = %d, want 18", got)
	}
}

func TestBuildURL(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	s := &server{cfg: config{
		urlTemplate: defaultURLTemplate,
		tz:          "America/Indianapolis",
		days:        7,
		loc:         loc,
	}}
	start := time.Date(2026, 6, 9, 0, 0, 0, 0, loc)
	u, err := s.buildURL(start)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"start=2026-06-09", "end=2026-06-16", "tz=America%2FIndianapolis"} {
		if !strings.Contains(u, want) {
			t.Errorf("URL missing %q:\n%s", want, u)
		}
	}
	if strings.Contains(u, "{{") {
		t.Errorf("URL still has unrendered template tokens:\n%s", u)
	}
}

func TestBuildSVGWellFormed(t *testing.T) {
	loc := time.FixedZone("EST", -5*3600)
	s := &server{cfg: config{width: 1920, height: 1080, days: 7, title: "This Week", loc: loc}}
	start := time.Date(2026, 6, 9, 0, 0, 0, 0, loc)
	days := groupByDay(parseICS(sampleICS, loc), start, 7, loc)

	svg := s.buildSVG(start, days, nil)
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatalf("svg not wrapped properly: starts %q", svg[:min(40, len(svg))])
	}
	if !strings.Contains(svg, `width="1920"`) || !strings.Contains(svg, `height="1080"`) {
		t.Errorf("svg missing resolution")
	}
	if !strings.Contains(svg, "This Week") {
		t.Errorf("svg missing title")
	}
	if !strings.Contains(svg, "Little League") {
		t.Errorf("svg missing event text")
	}
	// XML-escaping: an ampersand in a summary must not appear raw.
	evil := []event{{summary: "Soccer & Pizza", start: start.Add(13 * time.Hour)}}
	d := make([][]event, 7)
	d[0] = evil
	svg2 := s.buildSVG(start, d, nil)
	if strings.Contains(svg2, "Soccer & Pizza") {
		t.Errorf("ampersand not escaped")
	}
	if !strings.Contains(svg2, "Soccer &amp; Pizza") {
		t.Errorf("escaped ampersand missing")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("hello world", 5); got != "hell…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("hi", 10); got != "hi" {
		t.Errorf("truncate short = %q", got)
	}
}

const sampleJ1 = `{
  "current_condition": [{
    "temp_C": "21", "temp_F": "70",
    "FeelsLikeC": "20", "FeelsLikeF": "68",
    "windspeedMiles": "7", "windspeedKmph": "11",
    "winddir16Point": "SW", "uvIndex": "5", "weatherCode": "116",
    "weatherDesc": [{"value": "Partly cloudy"}]
  }],
  "weather": [{"hourly": [
    {"time": "0", "chanceofrain": "10", "chanceofsnow": "0"},
    {"time": "1200", "chanceofrain": "40", "chanceofsnow": "0"},
    {"time": "1500", "chanceofrain": "20", "chanceofsnow": "0"}
  ]}]
}`

func TestFetchWeather(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(sampleJ1))
	}))
	defer srv.Close()
	loc := time.UTC

	wF, err := fetchWeather(srv.URL, "F", loc)
	if err != nil {
		t.Fatal(err)
	}
	if wF.temp != 70 || wF.feels != 68 || wF.wind != 7 || wF.windUnit != "mph" || wF.unit != "F" {
		t.Errorf("F: got %+v", wF)
	}
	if wF.windDir != "SW" || wF.uv != 5 || wF.icon != "partly" {
		t.Errorf("F: dir/uv/icon wrong: %+v", wF)
	}
	if wF.precip != 10 && wF.precip != 20 && wF.precip != 40 {
		t.Errorf("precip should be one of the hourly chances, got %d", wF.precip)
	}

	wC, err := fetchWeather(srv.URL, "C", loc)
	if err != nil {
		t.Fatal(err)
	}
	if wC.temp != 21 || wC.feels != 20 || wC.wind != 11 || wC.windUnit != "km/h" || wC.unit != "C" {
		t.Errorf("C: got %+v", wC)
	}
}

func TestWeatherIcon(t *testing.T) {
	cases := map[int]string{
		113: "sun", 116: "partly", 119: "cloud", 122: "cloud",
		143: "fog", 200: "thunder", 230: "snow", 266: "rain", 999: "cloud",
	}
	for code, want := range cases {
		if got := weatherIcon(code); got != want {
			t.Errorf("weatherIcon(%d) = %q, want %q", code, got, want)
		}
	}
}

func TestBuildSVGWithWeather(t *testing.T) {
	loc := time.UTC
	s := &server{cfg: config{width: 1920, height: 1080, days: 7, title: "This Week", loc: loc}}
	start := time.Date(2026, 6, 11, 0, 0, 0, 0, loc)
	wx := &weather{temp: 70, feels: 68, wind: 7, windUnit: "mph", windDir: "SW",
		precip: 20, uv: 5, unit: "F", icon: "partly"}

	svg := s.buildSVG(start, make([][]event, 7), wx)
	for _, want := range []string{"70°F", "Feels like 68°", "UV 5", "20% precip", "SW 7 mph"} {
		if !strings.Contains(svg, want) {
			t.Errorf("weather SVG missing %q", want)
		}
	}
	// Disabled weather should render nothing weather-related.
	if strings.Contains(s.buildSVG(start, make([][]event, 7), nil), "Feels like") {
		t.Errorf("nil weather should not render a weather block")
	}
}
