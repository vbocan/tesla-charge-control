package main

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func newIndexServer(t *testing.T) *server {
	t.Helper()
	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		t.Fatal(err)
	}
	return &server{tmpl: tmpl, assetVer: assetVersion()}
}

func lanGet(h http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "192.168.1.20:5555"
	h.ServeHTTP(w, r)
	return w
}

func TestAssetVersionIsShortHexAndStable(t *testing.T) {
	v := assetVersion()
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(v) {
		t.Errorf("assetVersion = %q, want 12 hex digits", v)
	}
	if again := assetVersion(); again != v {
		t.Errorf("assetVersion changed between calls: %q then %q", v, again)
	}
}

// A cache that keeps the old script or stylesheet next to a new page leaves
// the dashboard half updated (it happened behind Cloudflare), so the page
// must name the assets by version.
func TestDashboardNamesItsAssetsByVersion(t *testing.T) {
	s := newIndexServer(t)
	w := lanGet(s.routes(), "/")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /: status %d", w.Code)
	}
	body := w.Body.String()
	for _, ref := range []string{`href="/web/style.css?v=` + s.assetVer + `"`, `src="/web/app.js?v=` + s.assetVer + `"`} {
		if !strings.Contains(body, ref) {
			t.Errorf("dashboard does not contain %s", ref)
		}
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("dashboard Cache-Control = %q, want no-cache", cc)
	}
}

func TestStaticAssetsAreServedWithQueryAndMustRevalidate(t *testing.T) {
	s := newIndexServer(t)
	h := s.routes()
	for path, want := range map[string]string{
		"/web/style.css?v=abc": "text/css",
		"/web/app.js?v=abc":    "javascript",
		"/web/app.js":          "javascript",
	} {
		w := lanGet(h, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", path, w.Code)
			continue
		}
		if !strings.Contains(w.Header().Get("Content-Type"), want) {
			t.Errorf("GET %s: content type %q, want %q", path, w.Header().Get("Content-Type"), want)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s: Cache-Control = %q, want no-cache", path, cc)
		}
	}
}

// When the wall connector shows a car that the Bluetooth link cannot read, the
// card says why that is likely: the car is asleep, or out of range.
func TestVehicleCardSaysTheCarMayBeAsleep(t *testing.T) {
	js, err := staticFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "Car may be asleep or out of Bluetooth range") {
		t.Error("the vehicle card does not say the car may be asleep or out of Bluetooth range")
	}
	if strings.Contains(string(js), "Car not reachable over Bluetooth") {
		t.Error("the old wording is still in app.js")
	}
}

// The wall connector box shows the lifetime total the server sends (tesla.TWCStatus).
func TestWallConnectorBoxShowsTheLifetimeTotal(t *testing.T) {
	js, err := staticFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "lifetime_wh") || !strings.Contains(indexHTML, `id="twcLifetimeWh"`) {
		t.Error("the dashboard does not show the wall connector's lifetime total")
	}
	if strings.Contains(indexHTML, "carEnergyCard") || strings.Contains(string(js), "updateEnergy") {
		t.Error("the removed Car Energy card is still there")
	}
}

// The pause banner is for a connected car only; with none, the vehicle card says so.
func TestPauseBannerNeedsAConnectedCar(t *testing.T) {
	js, err := staticFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "t.paused && carConnected") {
		t.Error("the pause banner is not limited to a connected car")
	}
}

// The link-health page lives off the dashboard template, but a CDN that keeps
// an old debug.js next to it leaves the page empty, so it is versioned too.
func TestDebugPageVersionsItsAssets(t *testing.T) {
	s := newIndexServer(t)
	body := lanGet(s.routes(), "/debug").Body.String()
	for _, ref := range []string{`href="/web/debug.css?v=` + s.assetVer + `"`, `src="/web/debug.js?v=` + s.assetVer + `"`} {
		if !strings.Contains(body, ref) {
			t.Errorf("/debug does not reference %s", ref)
		}
	}
}
