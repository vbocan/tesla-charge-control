package main

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"
	"github.com/vbocan/tesla-charge-control/linkstat"
	"github.com/vbocan/tesla-charge-control/tesla"
)

//go:embed web/style.css web/app.js web/tesla-logo.png web/debug.css web/debug.js
var staticFS embed.FS

//go:embed web/index.html
var indexHTML string

//go:embed web/log.html
var logHTML string

//go:embed web/debug.html
var debugHTML string

// DashboardPayload is the JSON sent to clients via SSE.
type DashboardPayload struct {
	Timestamp      string            `json:"timestamp"`
	InverterOKUnix int64             `json:"inverter_ok_unix_ms"` // absolute time of last successful inverter read
	Data           deye.InverterData `json:"data"`
	Projection     ProjectionInfo    `json:"projection"`
	Sun            SunInfo           `json:"sun"`
	Tesla          tesla.Status      `json:"tesla"`
}

type ProjectionInfo struct {
	State        string  `json:"state"`
	RemainingKWh float64 `json:"remaining_kwh"`
	Hours        float64 `json:"hours"`
	AvgPower     float64 `json:"avg_power"`
	WindowFill   string  `json:"window_fill"`
}

type SunInfo struct {
	SunriseH  float64 `json:"sunrise_h"`
	SunsetH   float64 `json:"sunset_h"`
	IsDaytime bool    `json:"is_daytime"`
}

// PageData is passed to the Go HTML template.
type PageData struct {
	Title             string
	PollIntervalS     int
	BattCapacity      float64
	BattMinSOC        int
	BattMaxChargeW    int
	BattMaxDischargeW int
	PVPerStringW      int
	MaxOutputW        int
	TeslaEnabled      bool
	TeslaMaxAmps      int
	TeslaGridVoltage  int
	AssetVersion      string // changes with the embedded assets, see assetVersion
}

type sseClient struct {
	ch chan []byte
}

// inverterSource is where the poll loop gets its readings: the real inverter
// (*deye.Inverter) or, with -demo, a simulated one.
type inverterSource interface {
	ReadAll() (deye.InverterData, error)
	Reconnect()
	LinkStats() linkstat.Stats
}

type server struct {
	mu       sync.RWMutex
	clients  map[*sseClient]struct{}
	inv      inverterSource
	bp       *deye.BatteryProjection
	charger  *tesla.Charger
	store    *tesla.Store
	twc      *tesla.TWCClient
	cfg      deye.Config
	lastData []byte
	tmpl     *template.Template
	assetVer string

	// Who may reach the dashboard (see auth.go).
	accessRequired bool            // Cloudflare Access is configured (fully or partly)
	access         *accessVerifier // nil if not configured, or only half configured
	limiter        *failLimiter    // failed access-token attempts
}

func newServer(cfg deye.Config) *server {
	log.Printf("Connecting to Deye SG03LP1 at %s:%d (logger SN: %d)",
		cfg.Connection.Host, cfg.Connection.Port, cfg.Connection.LoggerSN)
	return buildServer(cfg, deye.Connect(cfg.Connection), "/state/tesla.db", nil)
}

// buildServer wires the server around an inverter source. prepare, if not nil,
// sees the charger and the wall connector client before their goroutines start.
func buildServer(cfg deye.Config, inv inverterSource, dbPath string, prepare func(*tesla.Charger, *tesla.TWCClient)) *server {
	tmpl, err := template.New("index").Parse(indexHTML)
	if err != nil {
		log.Fatalf("Template error: %v", err)
	}

	var charger *tesla.Charger
	var store *tesla.Store
	var twc *tesla.TWCClient
	if cfg.Tesla.Enabled {
		var err error
		store, err = tesla.NewStore(dbPath, cfg.Tesla.LogRetainDays)
		if err != nil {
			log.Fatalf("Tesla store error: %v", err)
		}
		store.PrunePeriodic()
		if cfg.Tesla.TWCURL != "" {
			twc = tesla.NewTWCClient(cfg.Tesla.TWCURL)
		}
		charger, err = tesla.NewCharger(cfg.Tesla, cfg.MaxOutputW, cfg.PollIntervalS, store, twc)
		if err != nil {
			log.Fatalf("Tesla charger init error: %v", err)
		}
		if prepare != nil {
			prepare(charger, twc)
		}
		if twc != nil {
			go twc.Run(context.Background(), 3*time.Second)
			log.Printf("TWC polling enabled at %s", cfg.Tesla.TWCURL)
		}
		go charger.RunBLEWorker(context.Background())
		go charger.RunSoCPoller(context.Background())
		go charger.RunWatchdog(context.Background())
		log.Printf("Tesla charging enabled (max: %dA, dry_run: %v)",
			cfg.Tesla.MaxAmps, cfg.Tesla.DryRun)
	}

	s := &server{
		clients: make(map[*sseClient]struct{}),
		inv:     inv,
		bp:      deye.NewBatteryProjection(cfg.Projection.WindowS, cfg.Battery),
		charger: charger,
		store:   store,
		twc:     twc,
		cfg:     cfg,
		tmpl:    tmpl,

		assetVer: assetVersion(),
	}
	s.initAuth()
	return s
}

func (s *server) addClient(c *sseClient) {
	s.mu.Lock()
	s.clients[c] = struct{}{}
	s.mu.Unlock()
}

func (s *server) removeClient(c *sseClient) {
	s.mu.Lock()
	delete(s.clients, c)
	s.mu.Unlock()
}

func (s *server) broadcast(data []byte) {
	s.mu.Lock()
	s.lastData = data
	for c := range s.clients {
		select {
		case c.ch <- data:
		default:
		}
	}
	s.mu.Unlock()
}

// assetVersion is a short hash of every embedded static file. The dashboard
// puts it in the URLs of its script and stylesheet, so a deploy that changes
// them is fetched afresh even by a browser or a CDN (Cloudflare caches .css and
// .js files for hours by default) that would otherwise keep serving the old
// ones next to the new page.
func assetVersion() string {
	h := sha256.New()
	names, _ := fs.Glob(staticFS, "web/*")
	for _, name := range names {
		b, err := staticFS.ReadFile(name)
		if err != nil {
			continue
		}
		h.Write([]byte(name))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// staticHandler serves the embedded assets. They carry no modification time,
// so without a Cache-Control header a cache is free to choose its own lifetime.
func staticHandler() http.Handler {
	files := http.FileServer(http.FS(staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	s.tmpl.Execute(w, PageData{
		AssetVersion:      s.assetVer,
		Title:             "Tesla Charge Control",
		PollIntervalS:     s.cfg.PollIntervalS,
		BattCapacity:      s.cfg.Battery.CapacityKWh,
		BattMinSOC:        s.cfg.Battery.MinSOC,
		BattMaxChargeW:    s.cfg.Battery.MaxChargeW(),
		BattMaxDischargeW: s.cfg.Battery.MaxDischargeW(),
		PVPerStringW:      s.cfg.Solar.PerStringW(),
		MaxOutputW:        s.cfg.MaxOutputW,
		TeslaEnabled:      s.cfg.Tesla.Enabled,
		TeslaMaxAmps:      s.cfg.Tesla.MaxAmps,
		TeslaGridVoltage:  s.cfg.Tesla.GridVoltage,
	})
}

func (s *server) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	c := &sseClient{ch: make(chan []byte, 4)}
	s.addClient(c)
	defer s.removeClient(c)

	// Send last known data immediately so new clients don't see a blank page.
	s.mu.RLock()
	last := s.lastData
	s.mu.RUnlock()
	if last != nil {
		fmt.Fprintf(w, "data: %s\n\n", last)
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case data := <-c.ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleTeslaPair runs the BLE add-key (pairing) flow and streams progress to
// the client via SSE. Query params:
//
//	rotate=true|false  (default false) — back up existing key pair and generate a fresh one
//	window=SECONDS     (default 120) — how long to wait for the keycard tap on the car
//
// Method must be POST. Response is text/event-stream with "progress", "done",
// or "error" events.
func (s *server) handleTeslaPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.charger == nil {
		http.Error(w, "Tesla control not enabled", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	rotate := r.URL.Query().Get("rotate") == "true"
	windowS := 120
	if s := r.URL.Query().Get("window"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			windowS = n
		}
	}

	log.Printf("[AUDIT] %s started car pairing (rotate key: %v, window: %ds)", who(r), rotate, windowS)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Run pairing in a goroutine; stream progress messages back.
	type progressEvent struct {
		typ string // "progress", "done", "error"
		msg string
	}
	events := make(chan progressEvent, 16)

	go func() {
		defer close(events)
		// Allow the full window + some buffer for the initial connect.
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(windowS+60)*time.Second)
		defer cancel()
		err := s.charger.Pair(ctx, rotate, windowS, func(msg string) {
			select {
			case events <- progressEvent{typ: "progress", msg: msg}:
			default:
			}
		})
		if err != nil {
			events <- progressEvent{typ: "error", msg: err.Error()}
		} else {
			events <- progressEvent{typ: "done", msg: "Pairing complete"}
		}
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			// Escape newlines in msg to keep SSE framing sane.
			msg := strings.ReplaceAll(ev.msg, "\n", " ")
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.typ, msg)
			flusher.Flush()
			if ev.typ == "done" || ev.typ == "error" {
				return
			}
		}
	}
}

// handleTeslaRefresh handles POST /api/tesla/refresh: read the car's battery state now.
func (s *server) handleTeslaRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.charger == nil {
		http.Error(w, "Tesla control not enabled", http.StatusBadRequest)
		return
	}
	log.Printf("[AUDIT] %s asked for a car battery read", who(r))
	s.charger.RefreshSoC()
	w.WriteHeader(http.StatusAccepted)
}

// handleTeslaMode handles POST /api/tesla/mode with body {"mode": "..."}
func (s *server) handleTeslaMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.charger == nil {
		http.Error(w, "Tesla control not enabled", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	var req struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	log.Printf("[AUDIT] %s set the charging mode to %q", who(r), req.Mode)
	s.charger.SetMode(req.Mode)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.charger.GetStatus())
}

// handleDebugOutage handles POST /api/debug/outage?on=true|false. It simulates a
// grid outage so the pause-and-resume behaviour can be tested end to end
// without switching off the grid. A simulated outage ends by itself after at
// most 10 minutes; it blocks charging, so it can only err on the safe side.
func (s *server) handleDebugOutage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.charger == nil {
		http.Error(w, "Tesla control not enabled", http.StatusBadRequest)
		return
	}
	on := r.URL.Query().Get("on")
	if on != "true" && on != "false" {
		http.Error(w, "use ?on=true or ?on=false", http.StatusBadRequest)
		return
	}
	log.Printf("[AUDIT] %s turned the simulated grid outage %s", who(r), on)
	s.charger.SimulateOutage(on == "true")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"simulated_outage": on == "true"})
}

func (s *server) pollLoop() {
	// Exponential backoff on consecutive failures to avoid hammering a wedged
	// SolarMan logger. Resets to base after any successful read.
	const baseBackoff = 2 * time.Second
	const maxBackoff = 30 * time.Second
	backoff := baseBackoff
	for {
		data, err := s.inv.ReadAll()
		if err != nil {
			log.Printf("Read error: %v — reconnecting in %s", err, backoff)
			s.inv.Reconnect()
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		backoff = baseBackoff

		s.bp.Add(data.BattPower)
		state, remainingKWh, hours := s.bp.Projection(data.BattSOC)
		avgPower, _ := s.bp.AvgPower()

		// Run Tesla charging state machine
		var teslaStatus tesla.Status
		if s.charger != nil {
			s.charger.Evaluate(data)
			teslaStatus = s.charger.GetStatus()
		}

		sunriseH, sunsetH := deye.SunTimes(s.cfg.Location.Latitude, s.cfg.Location.Longitude)
		now := time.Now()
		nowH := float64(now.Hour()) + float64(now.Minute())/60 + float64(now.Second())/3600

		payload := DashboardPayload{
			Timestamp:      now.Format("15:04:05"),
			InverterOKUnix: now.UnixMilli(),
			Data:           data,
			Projection: ProjectionInfo{
				State:        state,
				RemainingKWh: math.Round(remainingKWh*10) / 10,
				Hours:        math.Round(hours*10) / 10,
				AvgPower:     math.Round(avgPower),
				WindowFill:   s.bp.WindowFill(),
			},
			Sun: SunInfo{
				SunriseH:  math.Round(sunriseH*100) / 100,
				SunsetH:   math.Round(sunsetH*100) / 100,
				IsDaytime: sunriseH <= nowH && nowH <= sunsetH,
			},
			Tesla: teslaStatus,
		}

		jsonData, err := json.Marshal(payload)
		if err != nil {
			log.Printf("JSON error: %v", err)
			continue
		}
		s.broadcast(jsonData)

		time.Sleep(time.Duration(s.cfg.PollIntervalS) * time.Second)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.Handle("/web/", staticHandler())
	mux.HandleFunc("/api/events", s.handleSSE)
	mux.HandleFunc("/api/tesla/mode", s.handleTeslaMode)
	mux.HandleFunc("/api/debug/outage", s.handleDebugOutage)
	mux.HandleFunc("/api/tesla/refresh", s.handleTeslaRefresh)
	mux.HandleFunc("/api/tesla/pair", s.handleTeslaPair)
	mux.HandleFunc("/api/tesla/log", s.handleTeslaLog)
	mux.HandleFunc("/api/tesla/ble-log", s.handleTeslaBLELog)
	mux.HandleFunc("/api/debug/link", s.handleDebugLink)
	mux.HandleFunc("/log", s.handleLogPage)
	mux.HandleFunc("/debug", s.handleDebugPage)
	// The two separate logs became one page; keep the old addresses working.
	redirectToDebug := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/debug", http.StatusFound)
	}
	mux.HandleFunc("/debug/inverter", redirectToDebug)
	mux.HandleFunc("/debug/twc", redirectToDebug)
	if s.authEnabled() {
		return s.authMiddleware(mux)
	}
	return mux
}

// isPrivateAddr returns true if the address is a private/loopback IP.
func isPrivateAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func (s *server) handleLogPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(logHTML))
}

func (s *server) handleDebugPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	// Version the script and stylesheet as the dashboard does, or a CDN keeps
	// serving the old debug.js next to this page and the page stays empty.
	v := "?v=" + s.assetVer
	page := strings.NewReplacer("/web/debug.js", "/web/debug.js"+v, "/web/debug.css", "/web/debug.css"+v).Replace(debugHTML)
	w.Write([]byte(page))
}

// handleDebugLink returns the 24-hour health of the inverter and wall connector
// links; a link that is not configured is null.
func (s *server) handleDebugLink(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"inverter": nil, "twc": nil}
	if s.inv != nil {
		out["inverter"] = s.inv.LinkStats()
	}
	if s.twc != nil {
		out["twc"] = s.twc.LinkStats()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

// handleTeslaConnector handles POST /api/tesla/connector with body {"connector": "mobile"|"wall"}
// handleTeslaLog returns recent charging decisions as JSON.
// Optional query param: ?limit=100 (default 200)
func (s *server) handleTeslaLog(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "Tesla store not enabled", http.StatusBadRequest)
		return
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	entries := s.store.GetLog(limit)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

// handleTeslaBLELog returns recent BLE command results as JSON.
// Optional query param: ?limit=100 (default 200)
func (s *server) handleTeslaBLELog(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		http.Error(w, "Tesla store not enabled", http.StatusBadRequest)
		return
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	entries := s.store.GetBLELog(limit)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}
