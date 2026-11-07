package tesla

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"

	"github.com/teslamotors/vehicle-command/pkg/protocol"
)

// Charging modes.
const (
	ModeIdle      = "idle"
	ModeSolar     = "solar"
	ModeNoBattery = "nobattery"
	ModeMax       = "max"
)

// Charger status reported to the UI.
const (
	StatusIdle     = "idle"
	StatusWaiting  = "waiting"
	StatusCharging = "charging"
	StatusStopping = "stopping"
	StatusError    = "error"
)

// State machine states for solar mode.
const (
	StateStandby  = "standby"  // PV too low, polling slowly
	StateIdle     = "idle"     // Active, no surplus
	StateStarting = "starting" // Surplus sustained, waiting start_delay_s
	StateCharging = "charging" // Actively charging
	StateGrace    = "grace"    // Surplus dropped, grace period before stopping
	StateCooldown = "cooldown" // Just stopped, protecting contactor
)

const (
	// minChargeAmps is the lowest current the car will charge at; below it the
	// only option is to stop.
	minChargeAmps = 5

	// overloadStopAfter is how long the home load may stay above max_output_w
	// while the car is charging before we stop the charge outright. A current
	// reduction normally lands within a few seconds; if the load is still over
	// the limit after this, the reduction failed or isn't enough.
	overloadStopAfter = 15 * time.Second

	// maxResumeHoldoff is how long Max mode stays stopped after the load cap
	// stopped it, before it may start again. Stopping and restarting use the same
	// headroom threshold when load_margin_w is 0, so without this a house load
	// hovering at that threshold would cycle the contactor.
	maxResumeHoldoff = 60 * time.Second

	// maxRaiseDeadbandAmps is how much extra headroom (in amps) Max mode needs
	// before it raises the charging current; see evalMax.
	maxRaiseDeadbandAmps = 2

	// bleErrFreshFor is how long a failed BLE command keeps the status in error
	// after its last occurrence. Failing commands are retried every cycle, so a
	// persistent fault refreshes this; a one-off clears itself.
	bleErrFreshFor = 30 * time.Second

	// socControlHoldoff keeps the SoC poller off the radio for this long after
	// a charging-control command, which tends to be followed by more of them.
	socControlHoldoff = 10 * time.Second

	// socFailBackoffMax caps how far repeated SoC read failures (car asleep or
	// out of range) stretch the poll interval.
	socFailBackoffMax = 5 * time.Minute
)

// State is persisted to disk so the active mode survives restarts.
type State struct {
	Mode    string    `json:"mode"`
	Updated time.Time `json:"updated"`
}

// TWCStatus carries Tesla Wall Connector vitals to the UI.
type TWCStatus struct {
	Online           bool    `json:"online"`
	VehicleConnected bool    `json:"vehicle_connected"`
	ContactorClosed  bool    `json:"contactor_closed"`
	VehicleCurrentA  float64 `json:"vehicle_current_a"`
	ActualPowerW     int     `json:"actual_power_w"`
	GridV            float64 `json:"grid_v"`
	SessionEnergyWh  float64 `json:"session_energy_wh"`
	LifetimeWh       float64 `json:"lifetime_wh"` // everything the wall connector has delivered; 0 if unknown
	PCBATemp         float64 `json:"pcba_temp_c"`
	HandleTemp       float64 `json:"handle_temp_c"`
	EVSEState        int     `json:"evse_state"`
}

// Status is the live charger state sent to the UI via SSE.
type Status struct {
	Mode        string        `json:"mode"`
	Status      string        `json:"status"`
	Reason      string        `json:"reason"`
	CurrentAmps int           `json:"current_amps"`
	TargetAmps  int           `json:"target_amps"`
	MaxAmps     int           `json:"max_amps"`
	PowerW      int           `json:"power_w"`
	ExportW     int           `json:"export_w"`
	SurplusW    int           `json:"surplus_w"`
	ModeState   string        `json:"mode_state"`
	Enabled     bool          `json:"enabled"`
	Paused      string        `json:"paused"` // why charging is blocked: "", "outage", "restoring" or "blind"
	TWC         TWCStatus     `json:"twc"`
	Vehicle     VehicleStatus `json:"vehicle"`
}

// VehicleStatus carries the car's SoC / charge-limit data to the UI. Available
// is false until the first successful BLE read (and again after the car leaves).
type VehicleStatus struct {
	Available          bool    `json:"available"`
	BatteryLevel       int     `json:"battery_level"`
	UsableBatteryLevel int     `json:"usable_battery_level"`
	ChargeLimitSoc     int     `json:"charge_limit_soc"`
	ChargingState      string  `json:"charging_state"`
	MinutesToFull      int     `json:"minutes_to_full"`
	ChargeEnergyAdded  float64 `json:"charge_energy_added_kwh"`
	BatteryRangeKm     float64 `json:"battery_range_km"`
	AgeS               int     `json:"age_s"` // seconds since the reading was taken
}

// Charger is the charging state machine. It evaluates inverter data each cycle
// and decides whether to charge, at what current, and sends BLE commands.
type Charger struct {
	mu            sync.RWMutex
	cfg           deye.TeslaConfig
	maxOutputW    int // inverter max output — caps total home load
	ble           *BLEClient
	twc           *TWCClient
	store         *Store
	pollIntervalS int
	mode          string

	// Runtime state
	currentAmps int    // amps currently commanded
	status      string // current UI status string
	reason      string // human-readable explanation
	surplusW    int    // grid export + current car draw
	exportW     int    // grid export watts (for UI display)
	prevStatus  string // previous status, for change-only logging
	prevAmps    int    // previous amps, for change-only logging

	// Smoothing: rolling average of surplus power samples
	samples   []pwrSample
	smoothedW float64

	// State machine
	modeState          string    // current state machine state
	startingAt         time.Time // when we entered STARTING state
	graceAt            time.Time // when we entered GRACE state
	cooldownAt         time.Time // when we entered COOLDOWN state
	contactorOpenSince time.Time // when TWC first reported contactor open while in StateCharging
	overloadSince      time.Time // when home load first exceeded max_output_w while charging; zero if not overloaded
	overloadPausedAt   time.Time // when Max mode was last stopped by the load cap; evalMax holds off resuming
	volts              int       // volts used to convert watts to amps this cycle (see carVolts); 0 until first Evaluate

	// Grid-outage and inverter-data protection (see outage.go).
	paused         string    // PauseNone / PauseOutage / PauseRestoring / PauseBlind; what the UI shows
	lastInverterOK time.Time // when Evaluate last received inverter data
	blind          bool      // no inverter data for InverterStaleStopS
	gridBadSince   time.Time // when the grid readings first looked lost; zero if they look fine
	gridBadSamples int       // consecutive lost-grid readings
	gridOutage     bool      // outage confirmed; stays set until the grid has been stable for the restore delay
	gridGoodSince  time.Time // during an outage: when the readings last turned normal
	simOutageUntil time.Time // a simulated outage (debug endpoint) is active until then
	pauseStopAt    time.Time // when a stop was last sent because charging is paused

	// BLE commands never run inside Evaluate: a command can take many seconds
	// or fail, and Evaluate holds mu. Decisions are collected in cycleOp, handed
	// to queue once per cycle, and carried out by RunBLEWorker.
	queue   *bleQueue
	cycleOp *bleOp // this cycle's requested car command; guarded by mu

	// startRetry: a Solar start command failed, so every cycle re-asks for the
	// start (rather than a bare current change) until one succeeds or the car is
	// seen charging. Guarded by mu.
	startRetry bool

	// Outcome of BLE commands, written by the worker. Its own leaf mutex (never
	// held across BLE I/O or while taking mu) so the worker never waits on mu.
	resMu       sync.Mutex
	bleErr      error     // error of the most recent failed command; nil once one succeeds
	bleErrAt    time.Time // when bleErr was recorded
	startFailed bool      // a start command failed and Evaluate hasn't acted on it yet
	startOK     bool      // a start command succeeded and Evaluate hasn't seen it yet

	// Idle-mode load guard (see guard.go).
	guarding      bool      // the car is charging on its own and Idle mode keeps it under the load limit
	guardCeiling  int       // the car's own current setting when the guard took over; restored at the end
	guardStopped  bool      // the guard stopped the car (overload or outage) and restarts it when allowed
	guardResumeAt time.Time // when the guard first tried to restart the car; zero if it hasn't

	// Wake tracking: send a proactive BLE wake when entering StateStarting so
	// the car is fully awake by the time the start delay elapses.
	wakeAttempted bool

	// Debounce for the StateStarting abort-on-drop check: a single-cycle dip
	// (heater inrush, register lie under the 7kW filter limit) shouldn't reset
	// the countdown. Require two consecutive low-surplus cycles.
	startingLowStreak int

	// Surplus jump diagnostic: previous cycle's surplus, used to detect and log
	// large single-cycle swings that point at a transient inverter register lie.
	prevSurplusForDiag    int
	hasPrevSurplusForDiag bool

	// Vehicle charge state (SoC), read over BLE on a slow cadence by RunSoCPoller.
	// Guarded by its own mutex so the (slow) BLE read never blocks Evaluate/GetStatus,
	// which hold c.mu. Lock ordering is always c.mu -> chargeMu, never the reverse.
	chargeMu       sync.RWMutex
	chargeState    *VehicleChargeState
	chargeStateAt  time.Time     // when chargeState was last refreshed
	lastSoCAttempt time.Time     // when a SoC read was last attempted (success or fail), for pacing
	socRefresh     chan struct{} // user asked for a read now; see RefreshSoC
}

type pwrSample struct {
	ts time.Time
	w  int
}

// NewCharger creates a charger, loads persisted mode from disk.
func NewCharger(cfg deye.TeslaConfig, maxOutputW, pollIntervalS int, store *Store, twc *TWCClient) (*Charger, error) {
	var privateKey protocol.ECDHPrivateKey
	if !cfg.DryRun {
		var err error
		privateKey, err = LoadOrCreatePrivateKey(cfg.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load BLE key: %w", err)
		}
	}

	c := &Charger{
		cfg:            cfg,
		maxOutputW:     maxOutputW,
		ble:            NewBLEClient(cfg.DryRun, cfg.VIN, cfg.KeyFile, privateKey, store),
		twc:            twc,
		store:          store,
		pollIntervalS:  pollIntervalS,
		mode:           ModeIdle,
		lastInverterOK: time.Now(), // a Pi that boots blind counts as blind from boot
		status:         StatusIdle,
		reason:         "Idle — plug in car, then select a charging mode",
		modeState:      StateIdle,
		socRefresh:     make(chan struct{}, 1),
	}
	c.queue = newBLEQueue(c.execBLEOp, c.onBLEResult)
	c.loadState()
	return c, nil
}

// SetDryRunVehicle makes dry-run car readings come from fn instead of a fixed
// snapshot. Call it before the SoC poller starts; it has no effect when
// commands really go to a car.
func (c *Charger) SetDryRunVehicle(fn func() *VehicleChargeState) {
	if c.cfg.DryRun {
		c.ble.dryRunState = fn
	}
}

// Mode returns the current charging mode (thread-safe).
func (c *Charger) Mode() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.mode
}

// Pair runs the BLE add-key flow. See BLEClient.Pair for semantics.
// During pairing, normal BLE commands from Evaluate() are suppressed so the
// pair flow has exclusive radio access.
func (c *Charger) Pair(ctx context.Context, rotate bool, windowS int, onProgress func(string)) error {
	if c.ble == nil {
		return fmt.Errorf("BLE client not initialized")
	}
	return c.ble.Pair(ctx, rotate, windowS, onProgress)
}

// SetMode changes the charging mode and persists it.
func (c *Charger) SetMode(mode string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch mode {
	case ModeIdle, ModeSolar, ModeNoBattery, ModeMax:
		// valid
	default:
		log.Printf("[TESLA] Invalid mode: %s", mode)
		return
	}

	c.mode = mode
	c.samples = c.samples[:0]
	c.smoothedW = 0
	c.modeState = StateIdle
	c.resetBLE()
	c.clearGuard()
	c.saveState()
	log.Printf("[TESLA] Mode set to: %s", mode)

	switch mode {
	case ModeIdle:
		c.currentAmps = 0
		c.surplusW = 0
		c.exportW = 0
		c.status = StatusIdle
		c.reason = "Idle — plug in car, then select a charging mode"
		// Purely passive — no BLE commands. The car manages its own charging.

	case ModeSolar:
		// Stay in StateIdle and let the first Evaluate cycle decide. If the car
		// is already drawing power AND surplus supports it, StateIdle will adopt
		// the in-progress session; if surplus is too low, StateIdle will issue a
		// stop. We can't make that call here because we don't have inverter data
		// yet — surplusW is whatever the previous mode left behind.
		c.status = StatusWaiting
		c.reason = "Evaluating..."

	case ModeNoBattery:
		// Same first-cycle logic as Solar — surplus formula differs (see Evaluate)
		// but the state machine is shared.
		c.status = StatusWaiting
		c.reason = "Evaluating..."

	case ModeMax:
		// Force a fresh wake attempt on the first Max eval. The car may have
		// been asleep (Infotainment off) since a previous idle period; without
		// a wake, SetChargingAmps + StartCharging get dropped silently.
		c.wakeAttempted = false
		c.status = StatusWaiting
		c.reason = "Evaluating..."

	default:
		c.status = StatusWaiting
		c.reason = "Evaluating..."
	}

	if c.store != nil {
		c.store.LogDecision("user", c.mode, c.status, c.currentAmps, c.surplusW,
			c.currentAmps*c.cfg.GridVoltage, c.reason)
		c.prevStatus = c.status
	}
}

// GetStatus returns the current charger status for the UI (thread-safe).
func (c *Charger) GetStatus() Status {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s := Status{
		Mode:        c.mode,
		Status:      c.status,
		Reason:      c.reason,
		CurrentAmps: c.currentAmps,
		TargetAmps:  c.computeTargetAmps(),
		MaxAmps:     c.cfg.MaxAmps,
		PowerW:      c.currentAmps * c.cfg.GridVoltage,
		ExportW:     c.exportW,
		SurplusW:    c.surplusW,
		ModeState:   c.modeState,
		Enabled:     c.cfg.Enabled,
		Paused:      c.paused,
	}
	if c.twc != nil {
		if v := c.twc.Vitals(); v != nil {
			actualW := int(v.VehicleCurrentA * v.GridV)
			lifetimeWh, _ := c.twc.LifetimeWh()
			s.TWC = TWCStatus{
				Online:           c.twc.Online(),
				VehicleConnected: v.VehicleConnected,
				ContactorClosed:  v.ContactorClosed,
				VehicleCurrentA:  v.VehicleCurrentA,
				ActualPowerW:     actualW,
				GridV:            v.GridV,
				SessionEnergyWh:  v.SessionEnergyWh,
				LifetimeWh:       lifetimeWh,
				PCBATemp:         v.PCBATemp,
				HandleTemp:       v.HandleTemp,
				EVSEState:        v.EVSEState,
			}
			// s.PowerW is left as commanded amps × voltage; actual power is in s.TWC.ActualPowerW
		} else {
			s.TWC.Online = false
		}
	}
	c.chargeMu.RLock()
	if c.chargeState != nil {
		s.Vehicle = VehicleStatus{
			Available:          true,
			BatteryLevel:       c.chargeState.BatteryLevel,
			UsableBatteryLevel: c.chargeState.UsableBatteryLevel,
			ChargeLimitSoc:     c.chargeState.ChargeLimitSoc,
			ChargingState:      c.chargeState.ChargingState,
			MinutesToFull:      c.chargeState.MinutesToFull,
			ChargeEnergyAdded:  c.chargeState.ChargeEnergyAdded,
			BatteryRangeKm:     c.chargeState.BatteryRangeKm,
			AgeS:               int(time.Since(c.chargeStateAt).Seconds()),
		}
	}
	c.chargeMu.RUnlock()
	return s
}

// Evaluate runs one cycle of the charging state machine. It never waits on the
// car: any BLE command it decides on is queued for RunBLEWorker.
func (c *Charger) Evaluate(data deye.InverterData) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Deferred after the unlock so it runs before it: once every decision this
	// cycle (including the load cap) has had its say, queue the single result.
	c.cycleOp = nil
	defer c.flushOp()

	// --- Grid outage / inverter data (see outage.go) ---
	// Track these before anything else: they matter whatever the mode is.
	now := time.Now()
	c.lastInverterOK = now
	if c.blind {
		c.blind = false
		log.Printf("[TESLA] Inverter data is back")
	}
	c.trackGrid(data, now)
	wasPaused := c.paused
	c.paused = c.pauseState(now)
	if wasPaused != PauseNone && c.paused == PauseNone {
		c.resumeFromPause()
	}

	// --- Auto-idle when car disconnected ---
	if c.twc != nil && c.twc.Online() && !c.twc.IsConnected() {
		if c.guarding || c.guardStopped {
			c.endGuard()
		}
		if c.mode != ModeIdle {
			log.Printf("[TESLA] Car disconnected — switching to Idle")
			c.mode = ModeIdle
			c.currentAmps = 0
			c.surplusW = 0
			c.exportW = 0
			c.status = StatusIdle
			c.modeState = StateIdle
			c.resetBLE()
			c.saveState()
		}
		// Drop the cached SoC — it belongs to a car that's no longer here.
		c.chargeMu.Lock()
		c.chargeState = nil
		c.chargeMu.Unlock()
		c.reason = "Idle — car not connected"
		return
	}

	// While charging is paused (grid outage, no inverter data) stop the car and
	// skip the mode logic. Idle mode is left alone unless the car is charging.
	if c.paused != PauseNone && c.enforcePause(now) {
		c.logDecision()
		return
	}

	// Idle mode: the car charges when it decides to; the Pi only keeps it under
	// the load limit (see guard.go).
	if c.mode == ModeIdle {
		c.volts = c.carVolts(data)
		carW := 0
		if c.twc != nil {
			if w, ok := c.twc.ActualCarW(); ok {
				carW = w
			}
		}
		c.evalIdle(data, carW)
		if c.guarding {
			c.enforceLoadCap(data, carW)
			if err := c.recentBLEError(); err != nil {
				c.status = StatusError
				c.reason = bleErrorReason(err)
			}
		}
		c.logDecision()
		return
	}

	// --- Compute surplus from grid export ---
	// surplus = grid export + current car draw − battery discharge
	// Export-based so the Deye's priority logic naturally feeds the battery
	// before the car. We add back the car's own draw so the formula is
	// self-consistent when the car is already charging (its draw reduced the
	// export). We subtract battery discharge because the inverter will also
	// drain the battery to cover house load (including the car); without this
	// term the car's own consumption masquerades as surplus and the battery
	// silently empties in Solar mode.
	// When TWC is available, use actual measured power instead of estimate.
	c.volts = c.carVolts(data)
	currentCarW := 0
	if c.twc != nil {
		if w, ok := c.twc.ActualCarW(); ok {
			currentCarW = w
		}
	} else if !c.cfg.DryRun {
		currentCarW = c.currentAmps * c.wattsPerAmp()
	}
	c.exportW = max(0, -data.GridPower)
	switch c.mode {
	case ModeSolar:
		battDischargeW := max(0, data.BattPower)
		c.surplusW = c.exportW + currentCarW - battDischargeW
	case ModeNoBattery:
		// surplus = grid export + car draw − battery net flow.
		// −BattPower expands to +battChargeW − battDischargeW:
		//   battery charging (BattPower<0) → add it back so the car absorbs
		//     what would otherwise top up the battery;
		//   battery discharging (BattPower>0) → subtract so the car backs off
		//     and grid — not battery — covers the shortfall.
		// exportW is included so the formula still has a signal when the
		// battery is full: excess then flows to grid instead of the battery,
		// and a closed loop around BattPower alone would collapse to ~0.
		c.surplusW = c.exportW + currentCarW - data.BattPower
	}

	// Diagnostic: a >4 kW single-cycle swing in surplus is almost always a
	// transient inverter register lie that slipped past the upstream filters.
	// Log raw inputs so we can confirm which channel is misbehaving.
	if delta := c.surplusW - c.prevSurplusForDiag; c.hasPrevSurplusForDiag && (delta > 4000 || delta < -4000) {
		log.Printf("[TESLA] Surplus jump %dW->%dW — grid=%dW batt=%dW load=%dW pv=%dW carW=%dW exportW=%d",
			c.prevSurplusForDiag, c.surplusW,
			data.GridPower, data.BattPower, data.LoadPower, data.TotalPV, currentCarW, c.exportW)
	}
	c.prevSurplusForDiag = c.surplusW
	c.hasPrevSurplusForDiag = true

	switch c.mode {
	case ModeSolar, ModeNoBattery:
		c.evalSolar(data)
	case ModeMax:
		c.evalMax(data)
	}

	// --- Home load safety cap (applies to all modes) ---
	c.enforceLoadCap(data, currentCarW)

	// BLE commands finish after Evaluate has moved on, so a failure is shown
	// here for as long as commands keep failing.
	if err := c.recentBLEError(); err != nil {
		c.status = StatusError
		c.reason = bleErrorReason(err)
	}

	c.logDecision()
}

// logDecision records the decision in the charging log when the status or the
// commanded current changed. Caller holds mu.
func (c *Charger) logDecision() {
	if c.store != nil && (c.status != c.prevStatus || c.currentAmps != c.prevAmps) {
		c.store.LogDecision("auto", c.mode, c.status, c.currentAmps, c.surplusW,
			c.currentAmps*c.cfg.GridVoltage, c.reason)
		c.prevStatus = c.status
		c.prevAmps = c.currentAmps
	}
}

// carVolts is the voltage to convert watts to amps with when budgeting the
// load. Real mains sags under load — a wall connector can see ~215 V against the
// nominal 230 V — so the nominal figure overestimates the watts one amp costs and
// leaves usable headroom idle. Prefer the wall connector's reading (what the car
// is fed, and what its measured draw is computed from), then the inverter's,
// then the configured nominal. Readings far from nominal are ignored.
func (c *Charger) carVolts(data deye.InverterData) int {
	nominal := float64(c.cfg.GridVoltage)
	plausible := func(v float64) bool { return v >= 0.8*nominal && v <= 1.2*nominal }
	if c.twc != nil && c.twc.Online() {
		if v := c.twc.Vitals(); v != nil && plausible(v.GridV) {
			return int(v.GridV)
		}
	}
	if plausible(data.GridVoltage) {
		return int(data.GridVoltage)
	}
	return c.cfg.GridVoltage
}

// wattsPerAmp is the voltage chosen for this cycle by carVolts, or the nominal
// one if no cycle has run yet.
func (c *Charger) wattsPerAmp() int {
	if c.volts > 0 {
		return c.volts
	}
	return c.cfg.GridVoltage
}

// loadLimitW is the total home load the car is allowed to bring us up to:
// max_output_w less a margin, so a surge between two polls (an oven element
// switching on) lands in the margin instead of tripping the inverter before the
// next reduction can reach the car.
func (c *Charger) loadLimitW() int {
	return c.maxOutputW - c.cfg.LoadMarginW
}

// headroomAmps is the most current the car may draw (up to max_amps) with the
// house load as it is now, and that house load without the car's carW.
func (c *Charger) headroomAmps(data deye.InverterData, carW int) (amps, houseW int) {
	houseW = data.LoadPower - carW
	return clamp((c.loadLimitW()-houseW)/c.wattsPerAmp(), 0, c.cfg.MaxAmps), houseW
}

// enforceLoadCap keeps total home load under the inverter's AC limit. It runs
// last in a cycle so whatever the mode logic asked for, the car is never
// commanded to draw more than the limit leaves room for.
func (c *Charger) enforceLoadCap(data deye.InverterData, currentCarW int) {
	if c.maxOutputW <= 0 || c.currentAmps <= 0 {
		c.overloadSince = time.Time{}
		return
	}
	volts := c.wattsPerAmp()
	solar := c.mode == ModeSolar || c.mode == ModeNoBattery

	carW := currentCarW
	if carW == 0 {
		carW = c.currentAmps * volts
	}
	hwc := data.LoadPower - carW // home load without the car
	maxAmps := clamp((c.loadLimitW()-hwc)/volts, 0, c.cfg.MaxAmps)

	// Backstop: a reduction that failed (or hasn't landed) leaves the load over
	// the hard limit. If that persists, stop rather than wait for the inverter
	// to cut the whole house.
	if data.LoadPower > c.maxOutputW {
		if c.overloadSince.IsZero() {
			c.overloadSince = time.Now()
		}
	} else {
		c.overloadSince = time.Time{}
	}
	if !c.overloadSince.IsZero() && time.Since(c.overloadSince) >= overloadStopAfter {
		log.Printf("[TESLA] Overload: home load %dW > %dW for %ds with car at %dA — stopping charge",
			data.LoadPower, c.maxOutputW, int(time.Since(c.overloadSince).Seconds()), c.currentAmps)
		c.pauseForOverload(solar)
		c.reason = "Home load " + itoa(data.LoadPower) + "W over " + itoa(c.maxOutputW) + "W limit for " +
			itoa(int(overloadStopAfter.Seconds())) + "s — charging stopped"
		return
	}

	if maxAmps >= minChargeAmps {
		if maxAmps < c.currentAmps {
			c.sendSetAmpsUrgent(maxAmps)
			c.reason = "Reduced to " + itoa(maxAmps) + "A — home load " + itoa(hwc) + "W (limit " + itoa(c.loadLimitW()) + "W)"
		}
		return
	}

	// No room for even the minimum current within the limit.
	switch {
	case solar:
		c.pauseForOverload(true)
		c.reason = "Home load " + itoa(data.LoadPower) + "W — paused to stay under " + itoa(c.loadLimitW()) + "W limit"
	case c.maxOutputW-hwc < minChargeAmps*volts:
		// Max mode doesn't stop for a soft limit, but if even the minimum
		// current would exceed the inverter's real output there's nothing left to try.
		c.pauseForOverload(false)
		c.reason = "Home load " + itoa(hwc) + "W — no room for even " + itoa(minChargeAmps) + "A under " + itoa(c.maxOutputW) + "W, paused"
	default:
		// Max mode within the margin: drop to the minimum current and hold there.
		if c.currentAmps > minChargeAmps {
			c.sendSetAmpsUrgent(minChargeAmps)
		}
		c.status = StatusWaiting
		c.reason = "Home load " + itoa(hwc) + "W — holding at " + itoa(minChargeAmps) + "A (limit " + itoa(c.loadLimitW()) + "W)"
	}
}

// pauseForOverload stops the charge because the load cap demands it. Solar
// modes go to cooldown, like any other stop; Max mode goes back to idle and
// resumes by itself once evalMax sees enough headroom.
func (c *Charger) pauseForOverload(solar bool) {
	c.sendStop()
	c.overloadSince = time.Time{}
	c.guardStoppedCharge()
	if solar {
		if c.modeState == StateCharging || c.modeState == StateGrace {
			c.modeState = StateCooldown
			c.cooldownAt = time.Now()
		}
	} else {
		c.modeState = StateIdle
		c.overloadPausedAt = time.Now()
	}
	c.status = StatusWaiting
}

// evalSolar implements the full state machine for solar charging.
func (c *Charger) evalSolar(data deye.InverterData) {
	startFailed := c.takeStartFailed()
	startOK := c.takeStartOK()

	c.addSample(c.surplusW)
	smoothed := c.smoothedAvg()

	// effectiveW = min(smoothed, instantaneous surplus).
	// Fast to drop: if the real surplus collapses, amp targets react immediately
	// without waiting for the rolling window to drain.
	// Slow to rise: if surplus spikes, we still wait for the average to confirm,
	// preventing overshoot on transient bursts.
	effectiveW := smoothed
	if float64(c.surplusW) < effectiveW {
		effectiveW = float64(c.surplusW)
	}

	minAmps := 5
	maxAmps := c.cfg.MaxAmps
	startThresholdW := minAmps*c.cfg.GridVoltage + 50 // 5A * 230V + 50W margin = 1200W

	switch c.modeState {
	case StateStandby:
		c.status = StatusWaiting
		c.reason = "PV " + itoa(data.TotalPV) + "W — standby (need " + itoa(c.cfg.StandbyPVThresholdW) + "W)"
		if data.TotalPV >= c.cfg.StandbyPVThresholdW {
			c.modeState = StateIdle
			log.Printf("[TESLA] State: standby -> idle (PV %dW >= %dW)", data.TotalPV, c.cfg.StandbyPVThresholdW)
		}

	case StateIdle:
		c.status = StatusWaiting
		// Desync / adoption: if TWC reports the car is physically charging while
		// we believe we're idle, either adopt the session (when surplus supports
		// it) or force a stop (when it doesn't). This recovers from a previous
		// ChargeStop that never reached the car.
		if c.twc != nil && c.twc.IsCharging() {
			if c.surplusW >= startThresholdW {
				c.modeState = StateCharging
				// Seed currentAmps from the actual TWC measurement so the next
				// sendSetAmps is meaningful.
				if w, ok := c.twc.ActualCarW(); ok {
					c.currentAmps = clamp(w/c.cfg.GridVoltage, minAmps, maxAmps)
				}
				c.reason = "Adopting in-progress charge (surplus " + itoa(c.surplusW) + "W)"
				log.Printf("[TESLA] State: idle -> charging (adopted in-progress session at %dA, surplus %dW)", c.currentAmps, c.surplusW)
				return
			}
			log.Printf("[TESLA] Desync: car charging in StateIdle (surplus %dW < %dW) — sending stop", c.surplusW, startThresholdW)
			c.sendStop()
			c.reason = "Stopping car — surplus " + itoa(c.surplusW) + "W below " + itoa(startThresholdW) + "W"
			return
		}
		if data.TotalPV < c.cfg.StandbyPVThresholdW {
			c.modeState = StateStandby
			c.reason = "PV " + itoa(data.TotalPV) + "W — entering standby"
			log.Printf("[TESLA] State: idle -> standby (PV %dW < %dW)", data.TotalPV, c.cfg.StandbyPVThresholdW)
			return
		}
		// Fast-start: if instantaneous surplus is ≥ 2× the start threshold the
		// surplus is unambiguously strong — skip waiting for the average to build.
		if c.surplusW >= 2*startThresholdW {
			c.modeState = StateStarting
			c.startingAt = time.Now()
			c.wakeAttempted = false
			c.startingLowStreak = 0
			c.reason = "Surplus " + itoa(c.surplusW) + "W — fast start (strong surplus)"
			log.Printf("[TESLA] State: idle -> starting (fast-start: %dW >= %dW)", c.surplusW, 2*startThresholdW)
			return
		}
		if smoothed >= float64(startThresholdW) {
			c.modeState = StateStarting
			c.startingAt = time.Now()
			c.wakeAttempted = false
			c.startingLowStreak = 0
			c.reason = "Surplus " + itoa(int(smoothed)) + "W — starting delay (" + itoa(c.cfg.StartDelayS) + "s)"
			log.Printf("[TESLA] State: idle -> starting (surplus %dW >= %dW)", int(smoothed), startThresholdW)
		} else {
			if smoothed > 0 {
				c.reason = "Surplus " + itoa(int(smoothed)) + "W — need " + itoa(startThresholdW) + "W to start"
			} else {
				c.reason = "No surplus — need " + itoa(startThresholdW) + "W to start"
			}
		}

	case StateStarting:
		c.status = StatusWaiting
		// Send a proactive BLE wake on the first cycle so the car is fully awake
		// by the time the start delay elapses. VCSEC stays reachable during deep
		// sleep, so this succeeds even after many idle hours.
		if !c.wakeAttempted {
			c.wakeAttempted = true
			go func() {
				if err := c.ble.WakeUp(); err != nil {
					log.Printf("[TESLA] BLE wake-up failed: %v", err)
				}
			}()
		}
		// Reset on instantaneous drop, debounced. The start countdown must abort
		// when real surplus falls below the threshold so the UI doesn't show
		// "starting in Ns" while the surplus tile reads <1200W. But a single-cycle
		// dip — heater inrush, washing-machine fill, or a register transient under
		// the 7kW filter limit — shouldn't reset the countdown either. Require two
		// consecutive low cycles.
		if c.surplusW < startThresholdW {
			c.startingLowStreak++
			if c.startingLowStreak >= 2 {
				c.modeState = StateIdle
				c.startingLowStreak = 0
				c.reason = "Surplus dropped to " + itoa(c.surplusW) + "W during start delay — reset"
				log.Printf("[TESLA] State: starting -> idle (instantaneous surplus %dW < %dW, 2 cycles)", c.surplusW, startThresholdW)
				return
			}
			c.reason = "Surplus dipped to " + itoa(c.surplusW) + "W — tolerating one cycle"
			log.Printf("[TESLA] StateStarting: surplus dip %dW < %dW (cycle 1, will abort next cycle if persists)", c.surplusW, startThresholdW)
			return
		}
		c.startingLowStreak = 0
		if smoothed < float64(startThresholdW) {
			// Smoothed average dropped — abort. The smoothing window already
			// integrates over many cycles, so no extra debounce needed here.
			c.modeState = StateIdle
			c.reason = "Surplus dropped to " + itoa(int(smoothed)) + "W during start delay — reset"
			log.Printf("[TESLA] State: starting -> idle (smoothed surplus %dW < %dW)", int(smoothed), startThresholdW)
			return
		}
		elapsed := time.Since(c.startingAt)
		remaining := time.Duration(c.cfg.StartDelayS)*time.Second - elapsed
		if remaining <= 0 {
			// Start delay complete — begin charging. Use effectiveW so we don't
			// open at a higher rate than the instantaneous surplus supports.
			targetAmps := clamp(int(effectiveW)/c.cfg.GridVoltage, minAmps, maxAmps)
			c.sendStartCharge(targetAmps)
			c.startRetry = false
			// The command runs asynchronously. If it fails, the StateCharging
			// case notices (startFailed) and keeps retrying the start.
			c.modeState = StateCharging
			c.status = StatusCharging
			c.reason = "Charging at " + itoa(c.currentAmps) + "A"
			log.Printf("[TESLA] State: starting -> charging at %dA", c.currentAmps)
		} else {
			c.reason = "Surplus " + itoa(c.surplusW) + "W — starting in " + itoa(int(remaining.Seconds())) + "s"
		}

	case StateCharging:
		// The start command runs asynchronously, so we learn that it failed a cycle
		// or more later. Stay in Charging and ask for the start again: dropping back to idle would
		// make the retry wait out the whole start delay, costing minutes of
		// charging per BLE failure. The queue's backoff paces the retries, and an
		// adopted session (contactor closed) or a collapse in surplus ends them.
		if startOK {
			c.startRetry = false
		}
		if startFailed {
			c.startRetry = true
			log.Printf("[TESLA] Start command failed — retrying while surplus holds")
		}
		if c.startRetry && c.twc != nil && c.twc.IsCharging() {
			c.startRetry = false
		}

		// Contactor recovery: if TWC is online and the contactor is still open,
		// the car accepted ChargeStart at the BLE level but the onboard charger
		// didn't engage. Wait 30s then retry ChargeStart.
		if c.twc != nil && c.twc.Online() && !c.twc.IsCharging() {
			if c.contactorOpenSince.IsZero() {
				c.contactorOpenSince = time.Now()
			}
			elapsed := time.Since(c.contactorOpenSince)
			if elapsed >= 30*time.Second || c.startRetry {
				log.Printf("[TESLA] Contactor open for %ds despite active session — resending ChargeStart", int(elapsed.Seconds()))
				c.contactorOpenSince = time.Time{}
				targetAmps := clamp(int(effectiveW)/c.cfg.GridVoltage, minAmps, maxAmps)
				c.sendStartCharge(targetAmps)
				c.status = StatusCharging
				c.reason = "Retrying charge — contactor had not engaged"
				return
			}
			remaining := 30 - int(elapsed.Seconds())
			c.status = StatusWaiting
			c.reason = "Waiting for contactor to engage (" + itoa(remaining) + "s)"
			return
		}
		c.contactorOpenSince = time.Time{} // contactor is closed — clear tracker

		// Amp targeting uses effectiveW = min(smoothed, instantaneous) so the
		// system reacts immediately when surplus collapses, but ramps up conservatively.
		newAmps := clamp(int(effectiveW)/c.cfg.GridVoltage, 0, maxAmps)
		if newAmps < minAmps {
			// Can't sustain minimum — enter grace
			c.modeState = StateGrace
			c.graceAt = time.Now()
			c.startRetry = false // not worth starting a charge that is about to be given up
			c.sendSetAmps(minAmps)
			c.status = StatusStopping
			c.reason = "Surplus " + itoa(c.surplusW) + "W — grace period " + itoa(c.cfg.GracePeriodS) + "s"
			log.Printf("[TESLA] State: charging -> grace (surplus %dW, target %dA < %dA)", c.surplusW, newAmps, minAmps)
		} else if c.startRetry {
			c.sendStartCharge(newAmps)
			c.status = StatusCharging
			c.reason = "Retrying charge start at " + itoa(c.currentAmps) + "A"
		} else {
			// Every current change is a BLE connection, and near the surplus the
			// target flips between neighbouring amps as the house load wobbles. Only
			// raise the current once there is room for maxRaiseDeadbandAmps more; a
			// reduction is applied at once.
			if c.currentAmps >= minAmps && newAmps > c.currentAmps &&
				newAmps-c.currentAmps < maxRaiseDeadbandAmps {
				newAmps = c.currentAmps
			}
			c.sendSetAmps(newAmps)
			c.status = StatusCharging
			c.reason = "Charging at " + itoa(c.currentAmps) + "A (surplus " + itoa(c.surplusW) + "W)"
		}

	case StateGrace:
		// Recovery check also uses effectiveW — both average and instantaneous must
		// confirm surplus before exiting grace, preventing re-entry on a brief spike.
		newAmps := clamp(int(effectiveW)/c.cfg.GridVoltage, 0, maxAmps)
		if newAmps >= minAmps {
			// Surplus recovered — back to charging
			c.modeState = StateCharging
			c.sendSetAmps(newAmps)
			c.status = StatusCharging
			c.reason = "Surplus recovered — charging at " + itoa(c.currentAmps) + "A"
			log.Printf("[TESLA] State: grace -> charging (recovered, %dA)", newAmps)
		} else {
			elapsed := time.Since(c.graceAt)
			remaining := time.Duration(c.cfg.GracePeriodS)*time.Second - elapsed
			if remaining <= 0 {
				// Grace expired — stop charging
				c.sendStop()
				c.modeState = StateCooldown
				c.cooldownAt = time.Now()
				c.status = StatusWaiting
				c.reason = "Surplus too low for " + itoa(c.cfg.GracePeriodS) + "s — stopped, cooldown " + itoa(c.cfg.CooldownS) + "s"
				log.Printf("[TESLA] State: grace -> cooldown")
			} else {
				c.sendSetAmps(minAmps)
				c.status = StatusStopping
				c.reason = "Surplus " + itoa(c.surplusW) + "W, stopping in " + itoa(int(remaining.Seconds())) + "s"
			}
		}

	case StateCooldown:
		c.status = StatusWaiting
		// Desync recovery: if a previous stop never reached the car, retry it
		// rather than waiting out cooldown with the car still drawing power.
		if c.twc != nil && c.twc.IsCharging() {
			log.Printf("[TESLA] Desync: car still charging in StateCooldown — resending stop")
			c.sendStop()
			c.reason = "Retrying stop — car still drawing power"
			return
		}
		elapsed := time.Since(c.cooldownAt)
		remaining := time.Duration(c.cfg.CooldownS)*time.Second - elapsed
		if remaining <= 0 {
			c.modeState = StateIdle
			c.reason = "Cooldown complete — evaluating"
			log.Printf("[TESLA] State: cooldown -> idle")
		} else {
			c.reason = "Cooldown — restart in " + itoa(int(remaining.Seconds())) + "s"
		}

	default:
		// Unknown state, reset to idle
		c.modeState = StateIdle
	}
}

// evalMax charges at maximum rate, respecting home load limit. Never stops charging.
func (c *Charger) evalMax(data deye.InverterData) {
	// Max mode retries a failed command every cycle anyway, so there's no
	// start-failure to act on — just discard the flag.
	c.takeStartFailed()

	// After the load cap stopped the charge, stay stopped for a while.
	if !c.overloadPausedAt.IsZero() {
		if remaining := maxResumeHoldoff - time.Since(c.overloadPausedAt); remaining > 0 {
			// If the stop never reached the car it is still drawing power; the load
			// cap no longer watches it (currentAmps is 0), so retry the stop here.
			// The queue suppresses a repeat of a stop that already succeeded.
			if c.twc != nil && c.twc.IsCharging() {
				c.sendStop()
			}
			c.modeState = StateIdle
			c.status = StatusWaiting
			c.reason = "Paused for overload — resuming in " + itoa(int(remaining.Seconds())+1) + "s"
			return
		}
		c.overloadPausedAt = time.Time{}
	}

	maxAmps := c.cfg.MaxAmps

	if c.maxOutputW > 0 {
		// Prefer the TWC's actual measurement so we correctly subtract the car's
		// real draw — including any in-progress session we haven't commanded yet.
		// Falling back to commanded amps would treat that draw as house load and
		// keep us stuck in "waiting for headroom" forever.
		currentCarW := 0
		if c.twc != nil {
			if w, ok := c.twc.ActualCarW(); ok {
				currentCarW = w
			}
		}
		if currentCarW == 0 && !c.cfg.DryRun {
			currentCarW = c.currentAmps * c.wattsPerAmp()
		}
		targetAmps, houseWithoutCar := c.headroomAmps(data, currentCarW)

		if targetAmps < minChargeAmps {
			// Home load too high to start or raise the current. This sends nothing
			// itself; enforceLoadCap (run after this) drops a running session to
			// the minimum current, or stops it if even that would overload.
			c.modeState = StateIdle
			c.status = StatusWaiting
			c.reason = "Home load " + itoa(houseWithoutCar) + "W — waiting for " + itoa(c.loadLimitW()) + "W headroom"
			return
		}
		// Close to the limit the target flips between neighbouring amps as the
		// house load wobbles, and every change is a BLE command. Only raise the
		// current once there is room for maxRaiseDeadbandAmps more; a reduction
		// is always applied at once.
		if c.currentAmps >= minChargeAmps && targetAmps > c.currentAmps &&
			targetAmps-c.currentAmps < maxRaiseDeadbandAmps {
			targetAmps = c.currentAmps
		}
		maxAmps = targetAmps
	}

	// Proactive BLE wake on the first Max eval. Wakeup uses VCSEC which stays
	// reachable during deep sleep; Infotainment (needed for SetChargingAmps +
	// StartCharging) then comes online within ~10–30s. Without this, the first
	// few start attempts just fail silently while the car is asleep.
	if !c.wakeAttempted {
		c.wakeAttempted = true
		go func() {
			if err := c.ble.WakeUp(); err != nil {
				log.Printf("[TESLA] Max: BLE wake-up failed: %v", err)
			}
		}()
	}

	// Queue set-amps + start. A failure is surfaced via the BLE error status and
	// retried automatically on the next cycle; an identical request that just
	// succeeded is suppressed by the queue so we don't reconnect every 5s.
	//
	// When the wall connector shows the contactor closed the car is already
	// charging, so only the current is (re)sent: a ChargeStart would open a fresh
	// BLE connection every bleRepeatInterval for nothing, and each one is another
	// chance for the link to fail. Without a TWC reading, start as before.
	if c.twc != nil && c.twc.IsCharging() {
		c.sendSetAmps(maxAmps)
	} else {
		c.sendStartCharge(maxAmps)
	}
	c.modeState = StateCharging
	c.status = StatusCharging
	if c.maxOutputW > 0 {
		c.reason = "Charging at " + itoa(c.currentAmps) + "A (load limit " + itoa(c.loadLimitW()) + "W)"
	} else {
		c.reason = "Max charge at " + itoa(c.currentAmps) + "A (max " + itoa(c.cfg.MaxAmps) + "A)"
	}
}

// --- SoC polling ---

// RunSoCPoller periodically reads the car's SoC over BLE while a car is
// connected, caching the result for the UI. Cadence is deliberately
// conservative because every BLE read wakes the Infotainment domain: faster
// while actively charging, slower when merely plugged in, and never when the
// car is disconnected (so it can sleep and not drain its 12V battery).
//
// Pacing is by attempt, not by success, so a car that's asleep-but-plugged-in
// doesn't get probed every tick; repeated failures stretch the interval further
// (up to socFailBackoffMax). The poller also stays off the radio shortly after
// any charging-control command, and a read in progress is aborted the moment a
// control command needs the radio (see BLEClient.PreemptBackground). Run this
// in its own goroutine.
func (c *Charger) RunSoCPoller(ctx context.Context) {
	// tick is the resolution at which we re-check state and pacing. It must be
	// finer than the charging interval so a configured cadence is actually
	// honored (a 60s interval with a 30s tick would round up to 90s). 15s also
	// makes the poller notice a charge start/stop within ~15s.
	const tick = 15 * time.Second

	chargingInterval := time.Duration(c.cfg.SoCPollChargingS) * time.Second
	connectedInterval := time.Duration(c.cfg.SoCPollConnectedS) * time.Second
	if chargingInterval <= 0 {
		chargingInterval = 60 * time.Second
	}
	if connectedInterval <= 0 {
		connectedInterval = 20 * time.Minute
	}

	failStreak := 0 // consecutive failed reads; reset on success or when the car leaves

	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		// forced: the user asked for a read now (RefreshSoC). It skips the pacing
		// and the holdoff, but still only runs while a car is connected.
		forced := false
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.socRefresh:
			forced = true
		}
		// Decide whether a car is present and whether it's charging. With a
		// TWC we know for sure; without one, fall back to "any active mode".
		connected, charging := true, false
		if c.twc != nil {
			connected = c.twc.Online() && c.twc.IsConnected()
			charging = c.twc.IsCharging()
		} else {
			connected = c.Mode() != ModeIdle
		}
		if !connected {
			failStreak = 0
			continue
		}
		if forced {
			failStreak = 0
		}

		interval := connectedInterval
		if charging {
			interval = chargingInterval
		}
		if failStreak > 0 {
			interval = max(interval, min(interval<<min(failStreak, 6), socFailBackoffMax))
		}

		c.chargeMu.RLock()
		sinceAttempt := time.Since(c.lastSoCAttempt)
		c.chargeMu.RUnlock()
		if !forced && sinceAttempt < interval {
			continue
		}

		// Stay out of the way while the control path is busy. Checked after the
		// pacing test so a skipped tick doesn't consume the interval.
		if !forced && !c.queue.Quiet(socControlHoldoff) {
			continue
		}

		c.chargeMu.Lock()
		c.lastSoCAttempt = time.Now()
		c.chargeMu.Unlock()

		switch err := c.pollChargeState(); {
		case err == nil:
			failStreak = 0
		case errors.Is(err, ErrPreempted):
			// A control command took the radio. That isn't a failure: retry at
			// the next tick (subject to the holdoff) instead of a full interval.
			c.chargeMu.Lock()
			c.lastSoCAttempt = time.Time{}
			c.chargeMu.Unlock()
			log.Printf("[TESLA] SoC read yielded to a control command")
		default:
			failStreak++
			log.Printf("[TESLA] SoC read failed: %v", err)
		}
	}
}

// RefreshSoC asks the SoC poller to read the car now instead of at its next
// paced attempt, e.g. after the user woke the car from the Tesla app. It does
// not block and does nothing while a request is already pending.
func (c *Charger) RefreshSoC() {
	select {
	case c.socRefresh <- struct{}{}:
	default:
	}
}

// pollChargeState performs one BLE SoC read and caches the result. The BLE
// round-trip happens outside any lock; only the cache write is guarded.
func (c *Charger) pollChargeState() error {
	cs, err := c.ble.ReadChargeState()
	if err != nil {
		return err
	}
	c.chargeMu.Lock()
	c.chargeState = cs
	c.chargeStateAt = time.Now()
	c.chargeMu.Unlock()
	log.Printf("[TESLA] SoC %d%% (limit %d%%, %s, %d min to full)",
		cs.BatteryLevel, cs.ChargeLimitSoc, cs.ChargingState, cs.MinutesToFull)
	return nil
}

// --- BLE command helpers ---
//
// The send* helpers only record what the car should be doing — none of them
// touches the radio, so they're safe to call from Evaluate under mu. The
// requests are merged into cycleOp and queued once at the end of the cycle
// (flushOp), after the load cap has had the last word; queueing each one
// separately could let a worker command slip out between a mode decision and
// the cap that overrides it. c.currentAmps records intent; the actual car
// current is tracked separately via TWC.

// sendStartCharge sets amps and starts charging (also used by Max mode, which
// re-requests this every cycle to make sure charging is active).
func (c *Charger) sendStartCharge(amps int) {
	c.currentAmps = amps
	c.want(bleOp{kind: opStart, amps: amps})
}

// sendSetAmps adjusts amps without sending start/stop.
func (c *Charger) sendSetAmps(amps int) {
	c.currentAmps = amps
	c.want(bleOp{kind: opSetAmps, amps: amps})
}

// sendSetAmpsUrgent is sendSetAmps for a load-cap reduction: it preempts
// background BLE work and is attempted even during the slot-full cooldown.
func (c *Charger) sendSetAmpsUrgent(amps int) {
	c.currentAmps = amps
	c.want(bleOp{kind: opSetAmps, amps: amps, urgent: true})
}

// sendStop tells the car to stop charging. If the stop never reaches the car,
// the next Evaluate cycle sees TWC.IsCharging() and retries via the desync
// recovery in StateIdle/StateCooldown.
func (c *Charger) sendStop() {
	c.currentAmps = 0
	c.contactorOpenSince = time.Time{}
	c.want(bleOp{kind: opStop, urgent: true})
}

// want records a requested car command for this cycle, merged with any earlier
// request made in the same cycle. Caller holds mu.
func (c *Charger) want(op bleOp) {
	if c.cycleOp != nil {
		op = mergeOps(*c.cycleOp, op)
	}
	c.cycleOp = &op
}

// flushOp hands the cycle's command, if any, to the BLE worker. It never
// blocks. Caller holds mu.
func (c *Charger) flushOp() {
	if c.cycleOp == nil {
		return
	}
	c.queue.Submit(*c.cycleOp)
	c.cycleOp = nil
}

// resetBLE forgets queued commands and recorded BLE errors — used when the mode
// changes or the car leaves, when pending commands no longer mean anything.
// Caller holds mu.
func (c *Charger) resetBLE() {
	c.queue.Clear()
	c.cycleOp = nil
	c.overloadSince = time.Time{}
	c.overloadPausedAt = time.Time{}
	c.resMu.Lock()
	c.bleErr = nil
	c.startFailed = false
	c.startOK = false
	c.resMu.Unlock()
	c.startRetry = false
}

// RunBLEWorker performs queued car commands, one at a time, until ctx is
// cancelled. Run this in its own goroutine.
func (c *Charger) RunBLEWorker(ctx context.Context) {
	c.queue.Run(ctx)
}

// execBLEOp carries out one command over BLE. It runs on the worker goroutine
// and may block for the length of the BLE time budget.
func (c *Charger) execBLEOp(op bleOp) error {
	// Take the radio from any SoC read or wake-up in progress rather than
	// queueing behind it.
	c.ble.PreemptBackground(op.urgent)

	switch op.kind {
	case opSetAmps:
		return c.ble.SetChargingAmps(op.amps, op.urgent)
	case opStart:
		if err := c.ble.SetChargingAmps(op.amps, op.urgent); err != nil {
			return err
		}
		_, err := c.ble.StartCharging()
		return err
	case opStop:
		return c.ble.StopCharging()
	}
	return nil
}

// onBLEResult records a finished command for Evaluate to pick up. It runs on
// the worker goroutine, so it takes only resMu, never mu.
func (c *Charger) onBLEResult(op bleOp, err error) {
	c.resMu.Lock()
	defer c.resMu.Unlock()
	if errors.Is(err, errBackingOff) {
		// The queue held this op back because the link keeps failing. Nothing
		// new went wrong, so don't log it or flag a start failure again; just keep
		// the existing error status alive while we're still asking.
		if c.bleErr != nil {
			c.bleErrAt = time.Now()
		}
		return
	}
	if err == nil {
		c.bleErr = nil
		if op.kind == opStart {
			// Latest outcome wins if Evaluate hasn't looked at an earlier one yet.
			c.startOK, c.startFailed = true, false
		}
		return
	}
	c.bleErr, c.bleErrAt = err, time.Now()
	if op.kind == opStart {
		c.startFailed, c.startOK = true, false
	}
	log.Printf("[TESLA] BLE %s failed: %v", op, err)
}

// recentBLEError returns the last BLE command failure if it is still fresh.
func (c *Charger) recentBLEError() error {
	c.resMu.Lock()
	defer c.resMu.Unlock()
	if c.bleErr != nil && time.Since(c.bleErrAt) < bleErrFreshFor {
		return c.bleErr
	}
	return nil
}

// takeStartFailed reports, and clears, whether a start command failed since the
// last call.
func (c *Charger) takeStartFailed() bool {
	c.resMu.Lock()
	defer c.resMu.Unlock()
	failed := c.startFailed
	c.startFailed = false
	return failed
}

// takeStartOK reports, and clears, whether a start command succeeded since the
// last call.
func (c *Charger) takeStartOK() bool {
	c.resMu.Lock()
	defer c.resMu.Unlock()
	ok := c.startOK
	c.startOK = false
	return ok
}

// bleErrorReason returns a user-friendly error message for BLE failures.
func bleErrorReason(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "listner timed out") || strings.Contains(msg, "context deadline") {
		return "Car not reachable — may be asleep or out of BLE range"
	}
	if strings.Contains(msg, "not_charging") || strings.Contains(msg, "is_charging") {
		return "Car rejected command: " + msg
	}
	return "BLE error: " + msg
}

// --- Smoothing ---

func (c *Charger) addSample(w int) {
	now := time.Now()
	c.samples = append(c.samples, pwrSample{ts: now, w: w})
	cutoff := now.Add(-time.Duration(c.cfg.SmoothingWindowS) * time.Second)
	i := 0
	for i < len(c.samples) && c.samples[i].ts.Before(cutoff) {
		i++
	}
	if i > 0 {
		c.samples = c.samples[i:]
	}
}

func (c *Charger) smoothedAvg() float64 {
	if len(c.samples) == 0 {
		return 0
	}
	sum := 0.0
	for _, s := range c.samples {
		sum += float64(s.w)
	}
	c.smoothedW = sum / float64(len(c.samples))
	return c.smoothedW
}

// computeTargetAmps returns what amps we'd set (for UI display).
func (c *Charger) computeTargetAmps() int {
	if c.mode == ModeIdle {
		return 0
	}
	if c.mode == ModeMax {
		return c.cfg.MaxAmps
	}
	return clamp(int(c.smoothedW)/c.cfg.GridVoltage, 0, c.cfg.MaxAmps)
}

// --- State persistence ---

func (c *Charger) loadState() {
	if c.cfg.StateFile == "" {
		return
	}
	data, err := os.ReadFile(c.cfg.StateFile)
	if err != nil {
		return
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		log.Printf("[TESLA] Failed to parse state file: %v", err)
		return
	}
	// Migrate old mode names
	switch s.Mode {
	case "solar_surplus", "solar_priority":
		c.mode = ModeSolar
	case "max_charge", "forced":
		c.mode = ModeMax
	case "off":
		c.mode = ModeIdle
	default:
		c.mode = s.Mode
	}
	log.Printf("[TESLA] Loaded saved state: mode=%s", c.mode)
}

func (c *Charger) saveState() {
	if c.cfg.StateFile == "" {
		return
	}
	s := State{Mode: c.mode, Updated: time.Now()}
	data, err := json.Marshal(s)
	if err != nil {
		log.Printf("[TESLA] Failed to marshal state: %v", err)
		return
	}
	dir := filepath.Dir(c.cfg.StateFile)
	os.MkdirAll(dir, 0755)
	if err := os.WriteFile(c.cfg.StateFile, data, 0644); err != nil {
		log.Printf("[TESLA] Failed to save state: %v", err)
	}
}

// --- Helpers ---

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
