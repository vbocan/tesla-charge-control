package tesla

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/vbocan/tesla-charge-control/linkstat"
)

// TWCVitals represents the response from GET /api/1/vitals on a Tesla Wall Connector Gen 3.
type TWCVitals struct {
	ContactorClosed  bool     `json:"contactor_closed"`
	VehicleConnected bool     `json:"vehicle_connected"`
	SessionS         int      `json:"session_s"`
	GridV            float64  `json:"grid_v"`
	GridHz           float64  `json:"grid_hz"`
	VehicleCurrentA  float64  `json:"vehicle_current_a"`
	CurrentAA        float64  `json:"currentA_a"`
	CurrentBA        float64  `json:"currentB_a"`
	CurrentCA        float64  `json:"currentC_a"`
	CurrentNA        float64  `json:"currentN_a"`
	VoltageAV        float64  `json:"voltageA_v"`
	VoltageBV        float64  `json:"voltageB_v"`
	VoltageCV        float64  `json:"voltageC_v"`
	PCBATemp         float64  `json:"pcba_temp_c"`
	HandleTemp       float64  `json:"handle_temp_c"`
	MCUTemp          float64  `json:"mcu_temp_c"`
	UptimeS          int      `json:"uptime_s"`
	SessionEnergyWh  float64  `json:"session_energy_wh"`
	ConfigStatus     int      `json:"config_status"`
	EVSEState        int      `json:"evse_state"`
	CurrentAlerts    []string `json:"current_alerts"`
	PilotHighV       float64  `json:"pilot_high_v"`
	PilotLowV        float64  `json:"pilot_low_v"`
}

// TWCLifetime is the part of GET /api/1/lifetime that this app uses: the wall
// connector's counters since it was installed.
type TWCLifetime struct {
	EnergyWh float64 `json:"energy_wh"` // everything it has delivered
}

// twcLifetimeEvery is how often the lifetime counters are read. They change
// only while a car charges, and a total that is a minute old is fine.
const twcLifetimeEvery = time.Minute

// TWCClient polls the Tesla Wall Connector Gen 3 local HTTP API.
type TWCClient struct {
	mu         sync.RWMutex
	baseURL    string
	vitals     *TWCVitals
	lifetimeWh float64 // from /api/1/lifetime; 0 until it has been read
	online     bool
	wasOnline  bool // for transition logging
	client     *http.Client

	link *linkstat.Tracker // 24h poll health for the debug page
}

// NewTWCClient creates a new TWC client for the given base URL.
func NewTWCClient(baseURL string) *TWCClient {
	return &TWCClient{
		baseURL: baseURL,
		link:    linkstat.New(),
		client: &http.Client{
			Timeout: 2 * time.Second,
		},
	}
}

// Run polls the TWC vitals endpoint at the given interval until ctx is cancelled.
func (t *TWCClient) Run(ctx context.Context, interval time.Duration) {
	// Initial poll immediately
	if err := t.Poll(); err != nil {
		log.Printf("[TWC] Initial poll failed: %v", err)
	}
	t.PollLifetime()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lifetimeEvery := max(1, int(twcLifetimeEvery/interval))
	for n := 1; ; n++ {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.Poll()
			if n%lifetimeEvery == 0 {
				t.PollLifetime()
			}
		}
	}
}

// PollLifetime fetches /api/1/lifetime and keeps the energy the wall connector
// has delivered in its lifetime. A failure leaves the last value in place: the
// total is not worth marking the wall connector offline.
func (t *TWCClient) PollLifetime() error {
	resp, err := t.client.Get(t.baseURL + "/api/1/lifetime")
	if err != nil {
		return fmt.Errorf("TWC lifetime: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("TWC lifetime: HTTP %d", resp.StatusCode)
	}
	var l TWCLifetime
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&l); err != nil {
		return fmt.Errorf("TWC lifetime decode: %w", err)
	}
	if l.EnergyWh > 0 {
		t.mu.Lock()
		t.lifetimeWh = l.EnergyWh
		t.mu.Unlock()
	}
	return nil
}

// LifetimeWh is the energy the wall connector has delivered since it was
// installed; ok is false until it has been read.
func (t *TWCClient) LifetimeWh() (wh float64, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lifetimeWh, t.lifetimeWh > 0
}

// Poll fetches /api/1/vitals and updates internal state.
func (t *TWCClient) Poll() error {
	start := time.Now()
	resp, err := t.client.Get(t.baseURL + "/api/1/vitals")
	if err != nil {
		t.mu.Lock()
		if t.wasOnline {
			log.Printf("[TWC] Wall connector offline: %v", err)
			t.wasOnline = false
		}
		t.online = false
		t.mu.Unlock()
		t.link.Record(start, false, time.Since(start), err.Error())
		return fmt.Errorf("TWC poll: %w", err)
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		t.mu.Lock()
		t.online = false
		t.mu.Unlock()
		t.link.Record(start, false, time.Since(start), readErr.Error())
		return fmt.Errorf("TWC read: %w", readErr)
	}

	var v TWCVitals
	if err := json.Unmarshal(body, &v); err != nil {
		t.mu.Lock()
		t.online = false
		t.mu.Unlock()
		t.link.Record(start, false, time.Since(start), "decode: "+err.Error())
		return fmt.Errorf("TWC decode: %w", err)
	}

	t.mu.Lock()
	t.vitals = &v
	t.online = true
	if !t.wasOnline {
		log.Printf("[TWC] Wall connector online (vehicle_connected=%v, grid=%.1fV)", v.VehicleConnected, v.GridV)
		t.wasOnline = true
	}
	t.mu.Unlock()
	t.link.Record(start, true, time.Since(start), "")
	return nil
}

// LinkStats summarises the last 24 hours of vitals polls.
func (t *TWCClient) LinkStats() linkstat.Stats { return t.link.Snapshot(time.Now()) }

// LinkTracker exposes the poll-health record so the demo can seed a history.
func (t *TWCClient) LinkTracker() *linkstat.Tracker { return t.link }

// Vitals returns a copy of the latest vitals, or nil if unavailable.
func (t *TWCClient) Vitals() *TWCVitals {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.vitals == nil {
		return nil
	}
	v := *t.vitals
	return &v
}

// Online returns true if the last poll succeeded.
func (t *TWCClient) Online() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.online
}

// ActualCarW returns the actual car power draw in watts from the TWC measurement.
// Returns (watts, true) if TWC is online; (0, false) otherwise.
func (t *TWCClient) ActualCarW() (int, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if !t.online || t.vitals == nil {
		return 0, false
	}
	return int(t.vitals.VehicleCurrentA * t.vitals.GridV), true
}

// IsCharging returns true if the TWC contactor is closed (actively charging).
func (t *TWCClient) IsCharging() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.online && t.vitals != nil && t.vitals.ContactorClosed
}

// IsConnected returns true if a vehicle is connected to the TWC.
func (t *TWCClient) IsConnected() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.online && t.vitals != nil && t.vitals.VehicleConnected
}
