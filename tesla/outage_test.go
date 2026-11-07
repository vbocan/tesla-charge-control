package tesla

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"
)

// gridUp and gridDown are inverter readings with the grid present and lost.
// Everything else about the inverter looks normal in an outage: it is running
// the house from the battery.
var (
	gridUp   = deye.InverterData{GridVoltage: 221, GridFreq: 49.98, BattVoltage: 51.8, LoadPower: 1000}
	gridDown = deye.InverterData{GridVoltage: 0, GridFreq: 0, BattVoltage: 51.8, LoadPower: 1000}

	// gridGlitch is a reading with a garbled first register block, as the logger
	// returns a few times an hour (captured Oct 18 2026): the frequency reads
	// 2.00 Hz while the grid voltage, from the second block, is normal.
	gridGlitch = deye.InverterData{GridVoltage: 230, GridFreq: 2, BattVoltage: 52.82, LoadPower: 736}
)

// newOutageCharger is a Max-mode charger with the protections on, as the config
// defaults would have them, and a car charging at 20 A.
func newOutageCharger(t *testing.T) (*Charger, *opSink) {
	t.Helper()
	c, sink := newTestCharger(t, ModeMax)
	c.cfg.GridOutagePause = true
	c.cfg.GridRestoreDelayS = 300
	c.cfg.InverterStaleStopS = 120
	c.cfg.LoadMarginW = 0
	c.currentAmps = 20
	return c, sink
}

// chargingTWC is a wall connector that shows the car charging at the given amps.
func chargingTWC(amps float64) *TWCClient {
	return &TWCClient{online: true, vitals: &TWCVitals{
		VehicleConnected: true, ContactorClosed: true, VehicleCurrentA: amps, GridV: 230,
	}}
}

// declareOutage makes the next bad reading count as a confirmed outage.
func declareOutage(c *Charger) {
	c.gridBadSince = time.Now().Add(-2 * gridOutageConfirm)
	c.gridBadSamples = gridMinSamples
}

func TestGridState(t *testing.T) {
	tests := []struct {
		name        string
		d           deye.InverterData
		lost, known bool
	}{
		{"grid up", gridUp, false, true},
		{"grid down", gridDown, true, true},
		{"voltage collapsed, frequency still reported", deye.InverterData{GridVoltage: 12, GridFreq: 50, BattVoltage: 51}, true, true},
		{"frequency lost, voltage still reported", deye.InverterData{GridVoltage: 230, GridFreq: 0, BattVoltage: 51}, true, true},
		{"brown-out below the islanding level", deye.InverterData{GridVoltage: 150, GridFreq: 50, BattVoltage: 51}, true, true},
		{"slightly low but normal", deye.InverterData{GridVoltage: 200, GridFreq: 49.7, BattVoltage: 51}, false, true},
		{"a failed read (all zeros) says nothing", deye.InverterData{}, false, false},
		{"a garbled frequency says nothing", gridGlitch, false, false},
		{"nor does it with the voltage garbled too", deye.InverterData{GridVoltage: 0, GridFreq: 2, BattVoltage: 52}, false, false},
		{"a frequency far above nominal is no measurement either", deye.InverterData{GridVoltage: 230, GridFreq: 65.5, BattVoltage: 52}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lost, known := gridState(tc.d, 230)
			if lost != tc.lost || known != tc.known {
				t.Errorf("gridState = (lost %v, known %v), want (%v, %v)", lost, known, tc.lost, tc.known)
			}
		})
	}
}

// One bad reading (the inverter does produce transient ones) must not pause charging.
func TestSingleBadGridReadingDoesNotPause(t *testing.T) {
	c, sink := newOutageCharger(t)

	c.Evaluate(gridDown)

	if s := c.GetStatus(); s.Paused != PauseNone {
		t.Errorf("paused %q after a single bad reading, want none", s.Paused)
	}
	if op := sink.next(t); op.kind == opStop {
		t.Errorf("queued %v, charging must carry on", op)
	}
}

// Garbled readings come in runs of up to half a minute; however long the run,
// they must not pause charging, nor reset an outage that is being confirmed.
func TestGarbledReadingsAreIgnored(t *testing.T) {
	c, sink := newOutageCharger(t)

	for range 10 {
		c.Evaluate(gridGlitch)
	}
	if s := c.GetStatus(); s.Paused != PauseNone {
		t.Errorf("paused %q after garbled readings, want none", s.Paused)
	}
	if c.gridBadSamples != 0 {
		t.Errorf("garbled readings counted as %d bad samples, want 0", c.gridBadSamples)
	}
	if op := sink.next(t); op.kind == opStop {
		t.Errorf("queued %v, charging must carry on", op)
	}

	// A real outage being confirmed: a garbled reading in the middle neither
	// declares it nor resets the count, so the next real reading does.
	declareOutage(c)
	c.Evaluate(gridGlitch)
	if c.gridOutage {
		t.Fatal("a garbled reading must not declare the outage")
	}
	c.Evaluate(gridDown)
	if got := c.GetStatus().Paused; got != PauseOutage {
		t.Errorf("paused %q, want the outage declared on the next real reading", got)
	}
}

func TestConfirmedOutageStopsAndBlocksCharging(t *testing.T) {
	c, sink := newOutageCharger(t)
	declareOutage(c)

	c.Evaluate(gridDown)

	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
	s := c.GetStatus()
	if s.Paused != PauseOutage || s.Status != StatusWaiting || s.CurrentAmps != 0 {
		t.Errorf("paused=%q status=%q amps=%d, want an outage pause, waiting, 0 A", s.Paused, s.Status, s.CurrentAmps)
	}
	if !strings.Contains(s.Reason, "Grid outage") {
		t.Errorf("reason %q should mention the outage", s.Reason)
	}

	// Next cycle: still in the outage, and Max mode must not restart the car.
	c.Evaluate(gridDown)
	sink.none(t)
}

// Max mode would start charging at once on a quiet house; during an outage it must not.
func TestOutageBlocksAStartEvenWithPlentyOfHeadroom(t *testing.T) {
	c, sink := newOutageCharger(t)
	c.currentAmps = 0
	declareOutage(c)

	c.Evaluate(gridDown)

	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
	sink.none(t)
}

// The stop must keep being retried while the wall connector shows the car charging.
func TestOutageRetriesStopWhileCarStillCharges(t *testing.T) {
	c, sink := newOutageCharger(t)
	c.twc = chargingTWC(20)
	declareOutage(c)

	c.Evaluate(gridDown)
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}

	// The stop did not take (the queue entry is cleared so a repeat isn't
	// suppressed): the contactor is still closed, so it is sent again.
	c.queue.Clear()
	c.Evaluate(gridDown)
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Errorf("queued %v, want a repeated %v", got, want)
	}
}

// If the stop cannot reach the car the status must say so, not hide it.
func TestOutageShowsAStopThatCannotReachTheCar(t *testing.T) {
	c, _ := newOutageCharger(t)
	c.twc = chargingTWC(20)
	declareOutage(c)
	c.onBLEResult(bleOp{kind: opStop}, errors.New("BLE connect for ChargeStop: context deadline exceeded"))

	c.Evaluate(gridDown)

	s := c.GetStatus()
	if s.Status != StatusError || !strings.Contains(s.Reason, "Grid outage") || !strings.Contains(s.Reason, "can't stop the car") {
		t.Errorf("status %q reason %q, want an error naming the outage and the failed stop", s.Status, s.Reason)
	}
}

func TestGridRestoreWaitsForTheDelayThenResumes(t *testing.T) {
	c, sink := newOutageCharger(t)
	c.gridOutage = true // a confirmed outage is in progress
	c.paused = PauseOutage
	c.currentAmps = 0

	// The readings are normal again: the pause continues while the grid proves stable.
	c.Evaluate(gridUp)
	s := c.GetStatus()
	if s.Paused != PauseRestoring || !strings.Contains(s.Reason, "Grid restored") {
		t.Fatalf("paused %q reason %q, want restoring", s.Paused, s.Reason)
	}
	if op := sink.next(t); op.kind != opStop { // the pause's initial stop
		t.Fatalf("queued %v, want the pause's stop", op)
	}
	c.Evaluate(gridUp)
	sink.none(t)

	// A relapse restarts the countdown.
	c.Evaluate(gridDown)
	if !c.gridGoodSince.IsZero() {
		t.Error("a bad reading must restart the restore countdown")
	}

	// Stable for the whole delay: charging resumes.
	c.gridBadSince, c.gridBadSamples = time.Time{}, 0
	c.gridGoodSince = time.Now().Add(-6 * time.Minute)
	c.Evaluate(gridUp)
	if got := c.GetStatus().Paused; got != PauseNone {
		t.Fatalf("paused %q after the restore delay, want none", got)
	}
	if op := sink.next(t); op.kind != opStart {
		t.Errorf("queued %v after the pause ended, want a start", op)
	}
}

func TestIdleModeIsLeftAloneUnlessTheCarIsCharging(t *testing.T) {
	c, sink := newOutageCharger(t)
	c.mode = ModeIdle
	declareOutage(c)

	c.Evaluate(gridDown) // no wall connector: nothing known to stop
	sink.none(t)
	if got := c.GetStatus().Paused; got != PauseOutage {
		t.Errorf("paused %q, the banner should still show the outage", got)
	}

	// With a car actually charging, Idle mode steps in.
	c.twc = chargingTWC(16)
	c.Evaluate(gridDown)
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
}

func TestOutageProtectionCanBeDisabled(t *testing.T) {
	c, sink := newOutageCharger(t)
	c.cfg.GridOutagePause = false
	declareOutage(c)

	c.Evaluate(gridDown)

	if got := c.GetStatus().Paused; got != PauseNone {
		t.Errorf("paused %q with the protection disabled", got)
	}
	if op := sink.next(t); op.kind == opStop {
		t.Errorf("queued %v, but the protection is disabled", op)
	}
}

func TestSimulatedOutagePausesAtOnceAndEndsOnRequest(t *testing.T) {
	c, sink := newOutageCharger(t)

	c.SimulateOutage(true)
	c.Evaluate(gridUp) // the grid is fine; only the simulation says otherwise
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
	if got := c.GetStatus().Paused; got != PauseOutage {
		t.Errorf("paused %q, want outage", got)
	}

	c.SimulateOutage(false)
	c.Evaluate(gridUp)
	if got := c.GetStatus().Paused; got != PauseNone {
		t.Errorf("paused %q after ending the simulation, want none", got)
	}
	if op := sink.next(t); op.kind != opStart {
		t.Errorf("queued %v after the simulation, want a start", op)
	}
}

func TestSimulatedOutageEndsByItself(t *testing.T) {
	c, _ := newOutageCharger(t)
	c.simOutageUntil = time.Now().Add(-time.Second)
	if c.simOutage(time.Now()) {
		t.Error("an expired simulated outage must not count")
	}
}

// Without inverter data the load cap and the outage detection are blind, so the
// watchdog stops the car; fresh data brings it back.
func TestWatchdogStopsTheCarWhenInverterDataStops(t *testing.T) {
	c, sink := newOutageCharger(t)

	c.lastInverterOK = time.Now().Add(-30 * time.Second)
	c.checkInverterStale()
	sink.none(t) // not stale yet

	c.lastInverterOK = time.Now().Add(-3 * time.Minute)
	c.checkInverterStale()
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
	s := c.GetStatus()
	if s.Paused != PauseBlind || !strings.Contains(s.Reason, "No inverter data") {
		t.Errorf("paused %q reason %q, want a blind pause", s.Paused, s.Reason)
	}

	// Still blind on the next check: no repeat stop (no wall connector says it is needed).
	c.checkInverterStale()
	sink.none(t)

	// Data returns: charging resumes.
	c.Evaluate(gridUp)
	if got := c.GetStatus().Paused; got != PauseNone {
		t.Errorf("paused %q after data returned, want none", got)
	}
	if op := sink.next(t); op.kind != opStart {
		t.Errorf("queued %v after data returned, want a start", op)
	}
}

// Idle mode never enforces the load cap, so losing the inverter is no reason to
// stop a charge the car scheduled itself; nothing in Idle mode would restart it.
func TestWatchdogLeavesIdleModeAlone(t *testing.T) {
	c, sink := newOutageCharger(t)
	c.mode = ModeIdle
	c.currentAmps = 0
	c.twc = chargingTWC(16) // the car's own schedule started it
	c.lastInverterOK = time.Now().Add(-3 * time.Minute)

	c.checkInverterStale()

	sink.none(t)
	if got := c.GetStatus().Paused; got != PauseNone {
		t.Errorf("paused %q in Idle mode, want none", got)
	}

	// Switching to a mode the Pi drives while still blind pauses at once.
	c.mode = ModeSolar
	c.checkInverterStale()
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
	if got := c.GetStatus().Paused; got != PauseBlind {
		t.Errorf("paused %q in Solar mode, want blind", got)
	}

	// And back to Idle: the pause ends and its state is cleared, so the next
	// pause starts with a fresh stop.
	c.mode = ModeIdle
	c.checkInverterStale()
	sink.none(t)
	if got := c.GetStatus().Paused; got != PauseNone {
		t.Errorf("paused %q back in Idle mode, want none", got)
	}
	if !c.pauseStopAt.IsZero() {
		t.Error("leaving the pause must clear pauseStopAt")
	}
}

func TestWatchdogCanBeDisabled(t *testing.T) {
	c, sink := newOutageCharger(t)
	c.cfg.InverterStaleStopS = 0
	c.lastInverterOK = time.Now().Add(-time.Hour)

	c.checkInverterStale()

	sink.none(t)
	if got := c.GetStatus().Paused; got != PauseNone {
		t.Errorf("paused %q with the watchdog disabled", got)
	}
}
