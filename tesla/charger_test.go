package tesla

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"

	"github.com/teslamotors/vehicle-command/pkg/protocol"
)

// opSink stands in for the BLE worker's executor: it records every op the
// charger queues, and can be told to hang or fail like a bad BLE link.
type opSink struct {
	ops     chan bleOp
	release chan struct{} // when non-nil, exec blocks until it is closed
	mu      sync.Mutex
	err     error // returned from exec; set it before the first command, or use setErr
}

func (s *opSink) exec(op bleOp) error {
	if s.release != nil {
		<-s.release
	}
	s.ops <- op
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// setErr changes what exec returns while the worker is running.
func (s *opSink) setErr(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// next returns the next queued op, failing the test if none arrives.
func (s *opSink) next(t *testing.T) bleOp {
	t.Helper()
	select {
	case op := <-s.ops:
		return op
	case <-time.After(2 * time.Second):
		t.Fatal("no BLE op was queued")
		return bleOp{}
	}
}

// none fails the test if any op is queued shortly after the call.
func (s *opSink) none(t *testing.T) {
	t.Helper()
	select {
	case op := <-s.ops:
		t.Fatalf("unexpected BLE op %v", op)
	case <-time.After(100 * time.Millisecond):
	}
}

// newTestCharger returns a charger in the given mode with a 6 kW inverter limit
// and 400 W margin (5600 W usable), whose BLE commands go to the returned sink.
func newTestCharger(t *testing.T, mode string) (*Charger, *opSink) {
	t.Helper()
	cfg := deye.TeslaConfig{
		Enabled:             true,
		DryRun:              true, // only to skip loading a BLE key
		MaxAmps:             25,
		GridVoltage:         230,
		SmoothingWindowS:    90,
		StartDelayS:         120,
		GracePeriodS:        240,
		CooldownS:           180,
		StandbyPVThresholdW: 200,
		LoadMarginW:         400,
	}
	c, err := NewCharger(cfg, 6000, 5, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Dry run would hide the car's commanded draw from the load-cap maths; a real
	// run (no TWC) sees it, so make the test see it too.
	c.cfg.DryRun = false
	c.mode = mode

	sink := &opSink{ops: make(chan bleOp, 16)}
	c.queue = newBLEQueue(sink.exec, c.onBLEResult)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.queue.Run(ctx)
	return c, sink
}

// The original bug: a stuck BLE command froze Evaluate, GetStatus and SetMode.
func TestEvaluateDoesNotWaitForBLE(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	sink.release = make(chan struct{}) // every BLE command hangs
	defer close(sink.release)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3; i++ {
			c.Evaluate(deye.InverterData{LoadPower: 1000})
			_ = c.GetStatus()
		}
		c.SetMode(ModeSolar)
		_ = c.GetStatus()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Evaluate/GetStatus/SetMode blocked behind a stuck BLE command")
	}
}

// Appliance switches on while the car is at 20 A: the house alone leaves room
// for less than the 5 A minimum inside the margin, but not beyond the inverter's
// real limit. Max mode must throttle to the minimum rather than ignore it.
func TestMaxModeDropsToMinimumCurrentWithinMargin(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.currentAmps = 20 // 4600 W

	c.Evaluate(deye.InverterData{LoadPower: 4600 + 4600}) // house 4600 W + car 4600 W

	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 5, urgent: true}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
	if s := c.GetStatus(); s.Status != StatusWaiting || s.CurrentAmps != 5 {
		t.Errorf("status %q at %dA, want waiting at 5A", s.Status, s.CurrentAmps)
	}
}

// If even 5 A would push the house past the inverter's limit, Max mode stops.
func TestMaxModeStopsWhenMinimumCurrentWouldOverload(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.currentAmps = 20

	c.Evaluate(deye.InverterData{LoadPower: 5000 + 4600}) // house 5000 W + car 4600 W

	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
	if s := c.GetStatus(); s.CurrentAmps != 0 {
		t.Errorf("current amps %d after stop, want 0", s.CurrentAmps)
	}
}

// A reduction that never lands (BLE down) leaves the load over the limit; after
// overloadStopAfter the charger must escalate to a stop.
func TestOverloadEscalatesToStop(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.currentAmps = 10 // 2300 W
	c.overloadSince = time.Now().Add(-2 * overloadStopAfter)

	// House 3800 W + car 2300 W = 6100 W: over the 6000 W limit, yet a reduction
	// to 7 A would nominally fit, so only the backstop can catch this.
	c.Evaluate(deye.InverterData{LoadPower: 6100})

	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
	if s := c.GetStatus(); !strings.Contains(s.Reason, "charging stopped") {
		t.Errorf("reason %q should say charging stopped", s.Reason)
	}
}

// Solar logic wants 25 A, the load cap allows 11 A. Only the capped value may
// reach the worker: queueing the 25 A first could let it escape to the car.
func TestLoadCapIsTheOnlyCommandQueuedInACycle(t *testing.T) {
	c, sink := newTestCharger(t, ModeSolar)
	c.modeState = StateCharging
	c.currentAmps = 16 // 3680 W

	// 3 kW export plus the car's own draw gives plenty of surplus; house 3000 W.
	c.Evaluate(deye.InverterData{GridPower: -3000, LoadPower: 3000 + 3680})

	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 11, urgent: true}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
	sink.none(t)
	if s := c.GetStatus(); s.CurrentAmps != 11 {
		t.Errorf("current amps %d, want 11", s.CurrentAmps)
	}
}

// A start command that fails after Evaluate has already moved to StateCharging
// must surface the BLE error and keep asking for the start. Dropping back to idle
// would make the retry wait out the whole start delay again.
func TestFailedStartKeepsRetryingTheStart(t *testing.T) {
	c, sink := newTestCharger(t, ModeSolar)
	sink.err = errors.New("BLE connect for SetChargingAmps(13): context deadline exceeded")
	c.modeState = StateStarting
	c.startingAt = time.Now().Add(-10 * time.Minute) // start delay long elapsed
	data := deye.InverterData{GridPower: -3000, LoadPower: 500, TotalPV: 3500}

	c.Evaluate(data)
	if got := c.GetStatus().ModeState; got != StateCharging {
		t.Fatalf("mode state %q after the start request, want %q", got, StateCharging)
	}
	if op := sink.next(t); op.kind != opStart {
		t.Fatalf("queued %v, want a start", op)
	}
	waitFor(t, "the failure to be recorded", func() bool { return c.recentBLEError() != nil })

	c.Evaluate(data)
	s := c.GetStatus()
	if s.ModeState != StateCharging {
		t.Errorf("mode state %q after a failed start, want %q", s.ModeState, StateCharging)
	}
	if s.Status != StatusError || !strings.Contains(s.Reason, "Car not reachable") {
		t.Errorf("status %q / reason %q, want an unreachable-car error", s.Status, s.Reason)
	}
	if op := sink.next(t); op.kind != opStart {
		t.Errorf("queued %v after a failed start, want the start again", op)
	}
}

// Once the retried start gets through, Solar mode goes back to plain current
// changes instead of re-sending ChargeStart.
func TestRetriedStartStopsOnceItSucceeds(t *testing.T) {
	c, sink := newTestCharger(t, ModeSolar)
	c.modeState = StateCharging
	c.currentAmps = 13
	start := bleOp{kind: opStart, amps: 13}
	data := deye.InverterData{GridPower: -3000, LoadPower: 500 + 13*230}

	c.onBLEResult(start, errors.New("BLE connect for ChargeStart: closed pipe"))
	c.Evaluate(data)
	if op := sink.next(t); op.kind != opStart {
		t.Fatalf("queued %v while the start is outstanding, want a start", op)
	}

	c.onBLEResult(start, nil)
	c.Evaluate(data)
	if op := sink.next(t); op.kind != opSetAmps {
		t.Errorf("queued %v after the start succeeded, want a current change only", op)
	}
}

// Near the surplus the target flips between neighbouring amps. Every change is a
// BLE connection, so a raise needs room for maxRaiseDeadbandAmps more; a
// reduction is applied at once.
func TestSolarRaiseDeadband(t *testing.T) {
	tests := []struct {
		name string
		data deye.InverterData
		want int
	}{
		{"room for 1 A more is not enough", deye.InverterData{GridPower: -300, LoadPower: 500 + 4600}, 20},
		{"room for 2 A more raises", deye.InverterData{GridPower: -520, LoadPower: 500 + 4600}, 22},
		{"less surplus reduces at once", deye.InverterData{BattPower: 500, LoadPower: 500 + 4600}, 17},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, sink := newTestCharger(t, ModeSolar)
			c.modeState = StateCharging
			c.currentAmps = 20 // 20 A * 230 V = 4600 W

			c.Evaluate(tc.data)

			if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: tc.want}); got != want {
				t.Errorf("queued %v, want %v", got, want)
			}
		})
	}
}

func TestPreemptBackground(t *testing.T) {
	b := NewBLEClient(true, "", "", nil, nil)
	socCtx, socCancel := context.WithCancel(context.Background())
	wakeCtx, wakeCancel := context.WithCancel(context.Background())
	defer socCancel()
	defer wakeCancel()
	b.socCancel, b.wakeCancel = socCancel, wakeCancel

	b.PreemptBackground(false)
	if socCtx.Err() == nil {
		t.Error("a control command must abort an in-flight SoC read")
	}
	if wakeCtx.Err() != nil {
		t.Error("a non-urgent command must let the wake-up finish")
	}

	b.PreemptBackground(true)
	if wakeCtx.Err() == nil {
		t.Error("an urgent command must abort the wake-up")
	}
}

// While the queue holds commands back during a backoff, the BLE error status
// must stay up (Evaluate keeps asking), but a held-back command is not a new
// failure and must not flag another start failure.
func TestBackingOffKeepsBLEErrorAlive(t *testing.T) {
	c, _ := newTestCharger(t, ModeMax)
	start := bleOp{kind: opStart, amps: 20}

	c.onBLEResult(start, errors.New("BLE connect for ChargeStart: context deadline exceeded"))
	if !c.takeStartFailed() {
		t.Fatal("a failed start should be flagged")
	}

	// Long enough ago that the error would have expired.
	c.resMu.Lock()
	c.bleErrAt = time.Now().Add(-2 * bleErrFreshFor)
	c.resMu.Unlock()
	if c.recentBLEError() != nil {
		t.Fatal("test setup: error should be stale")
	}

	c.onBLEResult(start, errBackingOff)
	if c.recentBLEError() == nil {
		t.Error("a held-back command should keep the BLE error status alive")
	}
	if c.takeStartFailed() {
		t.Error("a held-back command must not flag another start failure")
	}
}

func TestBackingOffWithoutAnErrorStaysQuiet(t *testing.T) {
	c, _ := newTestCharger(t, ModeMax)
	c.onBLEResult(bleOp{kind: opStart, amps: 20}, errBackingOff)
	if c.recentBLEError() != nil {
		t.Error("a held-back command with no earlier failure must not invent an error")
	}
}

// With load_margin_w 0 the car may use everything up to max_output_w: house
// 4600 W leaves room for 6 A. With the 400 W margin the same house load leaves
// only 4 A, so Max mode would have to hold at the 5 A minimum instead.
func TestZeroMarginUsesTheFullInverterLimit(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.cfg.LoadMarginW = 0
	c.currentAmps = 20 // 4600 W

	c.Evaluate(deye.InverterData{LoadPower: 4600 + 4600}) // house 4600 W + car 4600 W

	if got, want := sink.next(t), (bleOp{kind: opStart, amps: 6}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
	sink.none(t)
	if s := c.GetStatus(); s.CurrentAmps != 6 {
		t.Errorf("current amps %d, want 6", s.CurrentAmps)
	}
}

// After the load cap stops Max mode it must not restart at once: with no margin
// the stop and restart thresholds coincide, so a load hovering there would
// cycle the contactor.
func TestMaxModeHoldsOffResumingAfterOverloadStop(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.cfg.LoadMarginW = 0
	c.currentAmps = 20

	// House 5000 W + car 4600 W: even 5 A (1150 W) would exceed 6000 W.
	c.Evaluate(deye.InverterData{LoadPower: 5000 + 4600})
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}

	// The house load falls right away, so there is plenty of headroom again.
	c.Evaluate(deye.InverterData{LoadPower: 1000})
	sink.none(t)
	if s := c.GetStatus(); s.Status != StatusWaiting || !strings.Contains(s.Reason, "resuming in") {
		t.Errorf("status %q / reason %q, want waiting with a resume countdown", s.Status, s.Reason)
	}

	// Once the hold-off has passed it starts again.
	c.overloadPausedAt = time.Now().Add(-2 * maxResumeHoldoff)
	c.Evaluate(deye.InverterData{LoadPower: 1000})
	if op := sink.next(t); op.kind != opStart {
		t.Errorf("queued %v after the hold-off, want a start", op)
	}
}

// During the hold-off the load cap no longer watches the car (it believes the
// car stopped), so a stop that never arrived must be retried here.
func TestMaxModeRetriesStopWhileHeldOffIfCarStillCharges(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.cfg.LoadMarginW = 0
	c.twc = &TWCClient{online: true, vitals: &TWCVitals{
		VehicleConnected: true, ContactorClosed: true, VehicleCurrentA: 20, GridV: 230,
	}}
	c.overloadPausedAt = time.Now() // just stopped for overload

	c.Evaluate(deye.InverterData{LoadPower: 5000 + 4600})

	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
}

// The wall connector sees ~215 V, not the nominal 230 V, so one amp costs ~215 W.
// With 777 W of other load that leaves room for 24 A under 6000 W; at 230 V the
// same maths gave 22 A and left ~490 W unused.
func TestMaxModeSizesTheCurrentWithMeasuredVoltage(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.cfg.LoadMarginW = 0
	c.twc = &TWCClient{online: true, vitals: &TWCVitals{
		VehicleConnected: true, ContactorClosed: true, VehicleCurrentA: 22, GridV: 215,
	}}
	c.currentAmps = 22 // the car draws 22 A * 215 V = 4730 W

	c.Evaluate(deye.InverterData{LoadPower: 777 + 4730})

	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 24}); got != want {
		t.Errorf("queued %v, want %v", got, want)
	}
}

// With the contactor closed the car is already charging, so Max mode adjusts the
// current but never re-sends ChargeStart; with it open (or no TWC) it starts.
func TestMaxModeStartsOnlyWhenNotCharging(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.twc = &TWCClient{online: true, vitals: &TWCVitals{
		VehicleConnected: true, ContactorClosed: true, VehicleCurrentA: 22, GridV: 230,
	}}
	c.currentAmps = 22

	c.Evaluate(deye.InverterData{LoadPower: 1000})
	if op := sink.next(t); op.kind != opSetAmps {
		t.Errorf("queued %v while charging, want a current change only", op)
	}

	c.twc.vitals.ContactorClosed = false
	c.Evaluate(deye.InverterData{LoadPower: 1000})
	if op := sink.next(t); op.kind != opStart {
		t.Errorf("queued %v while not charging, want a start", op)
	}
}

// Near the limit the target wobbles by an amp as the house load moves. Raises
// need room for at least maxRaiseDeadbandAmps more; reductions are immediate.
func TestMaxModeRaiseDeadband(t *testing.T) {
	tests := []struct {
		name  string
		house int // house load without the car, W
		want  int // current requested, A
	}{
		{"room for 1 A more is not enough", 600, 22}, // target 23
		{"room for exactly 2 A more raises", 370, 24},
		{"plenty of room raises", 140, 25},
		{"less room reduces at once", 1000, 21}, // target 21
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, sink := newTestCharger(t, ModeMax)
			c.cfg.LoadMarginW = 0
			c.currentAmps = 22 // 22 A * 230 V = 5060 W

			c.Evaluate(deye.InverterData{LoadPower: tc.house + 5060})

			if got, want := sink.next(t), (bleOp{kind: opStart, amps: tc.want}); got != want {
				t.Errorf("queued %v, want %v", got, want)
			}
		})
	}
}

func TestCarVoltsPrefersMeasurements(t *testing.T) {
	c, _ := newTestCharger(t, ModeMax) // nominal 230 V
	twc := func(online bool, v float64) *TWCClient {
		return &TWCClient{online: online, vitals: &TWCVitals{VehicleConnected: true, GridV: v}}
	}
	tests := []struct {
		name     string
		twc      *TWCClient
		inverter float64
		want     int
	}{
		{"wall connector wins", twc(true, 215), 223, 215},
		{"inverter when there is no wall connector", nil, 223, 223},
		{"inverter when the wall connector is offline", twc(false, 215), 223, 223},
		{"inverter when the wall connector reading is absurd", twc(true, 50), 223, 223},
		{"nominal when nothing is plausible", twc(true, 50), 0, 230},
		{"nominal with no data at all", nil, 0, 230},
		{"nominal when the inverter reading is absurd", nil, 150, 230},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c.twc = tc.twc
			if got := c.carVolts(deye.InverterData{GridVoltage: tc.inverter}); got != tc.want {
				t.Errorf("carVolts = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- Charge limit ---

// setCar stores a SoC reading as if the poller had taken it at the given time.
func setCar(c *Charger, cs *VehicleChargeState, at time.Time) {
	c.chargeMu.Lock()
	c.chargeState, c.chargeStateAt = cs, at
	c.chargeMu.Unlock()
}

// refusalSeen reports whether a "car is full" answer is waiting for Evaluate.
func (c *Charger) refusalSeen() bool {
	c.resMu.Lock()
	defer c.resMu.Unlock()
	return c.startFull
}

// fullCar is a car sitting at its 60 % charge limit.
func fullCar() *VehicleChargeState {
	return &VehicleChargeState{BatteryLevel: 60, ChargeLimitSoc: 60, ChargingState: "Complete"}
}

// refuseFirstStart runs one cycle whose start the car refuses as full, and
// waits until the refusal is back.
func refuseFirstStart(t *testing.T, c *Charger, sink *opSink, data deye.InverterData) {
	t.Helper()
	sink.setErr(ErrChargeComplete)
	c.Evaluate(data)
	if op := sink.next(t); op.kind != opStart {
		t.Fatalf("queued %v, want a start", op)
	}
	waitFor(t, "the refusal to come back", c.refusalSeen)
}

// assertHeldAtLimit checks the charger shows a calm idle at the charge limit and
// queues nothing over a few cycles.
func assertHeldAtLimit(t *testing.T, c *Charger, sink *opSink, data deye.InverterData) {
	t.Helper()
	for range 3 {
		c.Evaluate(data)
		s := c.GetStatus()
		if s.Status != StatusIdle || !strings.Contains(s.Reason, "charge limit") {
			t.Fatalf("status %q / reason %q, want idle at the charge limit", s.Status, s.Reason)
		}
		if s.CurrentAmps != 0 || s.TargetAmps != 0 {
			t.Errorf("current %dA / target %dA at the charge limit, want 0", s.CurrentAmps, s.TargetAmps)
		}
	}
	sink.none(t)
	if err := c.recentBLEError(); err != nil {
		t.Errorf("a full car is not a BLE error, got %v", err)
	}
}

// The live bug: at its 60 % limit the car answers ChargeStart with "complete".
// Max mode took that for a failure and asked again every few seconds, keeping
// the radio busy and the dashboard in error.
func TestMaxModeGoesIdleAtTheChargeLimit(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	data := deye.InverterData{LoadPower: 1000}

	refuseFirstStart(t, c, sink, data)
	assertHeldAtLimit(t, c, sink, data)
}

// With a SoC reading the reason names the limit, and only a reading taken after
// the refusal that shows room (a raised limit) lets Max mode start again.
func TestMaxModeResumesWhenTheLimitIsRaised(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	data := deye.InverterData{LoadPower: 1000}
	setCar(c, fullCar(), time.Now().Add(-time.Minute))

	refuseFirstStart(t, c, sink, data)
	assertHeldAtLimit(t, c, sink, data)
	if r := c.GetStatus().Reason; !strings.Contains(r, "(60%)") {
		t.Errorf("reason %q should name the 60%% limit", r)
	}

	// A fresh reading that still says full changes nothing.
	setCar(c, fullCar(), time.Now().Add(time.Millisecond))
	assertHeldAtLimit(t, c, sink, data)

	// The owner raises the limit to 80 %.
	sink.setErr(nil)
	setCar(c, &VehicleChargeState{BatteryLevel: 60, ChargeLimitSoc: 80, ChargingState: "Stopped"}, time.Now().Add(time.Millisecond))
	c.Evaluate(data)
	if op := sink.next(t); op.kind != opStart {
		t.Fatalf("queued %v after the limit was raised, want a start", op)
	}
	if s := c.GetStatus(); s.Status != StatusCharging {
		t.Errorf("status %q after resuming, want charging", s.Status)
	}
}

// A battery that has dropped below its limit resumes too, but only on a reading
// taken after the refusal: an older one may be what the car just contradicted.
func TestMaxModeResumesWhenTheBatteryHasRoom(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	data := deye.InverterData{LoadPower: 1000}
	room := &VehicleChargeState{BatteryLevel: 55, ChargeLimitSoc: 60, ChargingState: "Stopped"}
	setCar(c, room, time.Now().Add(-time.Minute))

	refuseFirstStart(t, c, sink, data)
	assertHeldAtLimit(t, c, sink, data)

	sink.setErr(nil)
	setCar(c, room, time.Now().Add(time.Millisecond))
	c.Evaluate(data)
	if op := sink.next(t); op.kind != opStart {
		t.Fatalf("queued %v with room in the battery, want a start", op)
	}
}

// If the car starts charging by itself (the limit was raised from the app), Max
// mode picks the session up and manages its current again.
func TestMaxModeResumesWhenTheCarChargesAgain(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	c.twc = pluggedTWC()
	data := deye.InverterData{LoadPower: 1000}

	refuseFirstStart(t, c, sink, data)
	assertHeldAtLimit(t, c, sink, data)

	sink.setErr(nil)
	c.twc = chargingTWC(16)
	c.Evaluate(deye.InverterData{LoadPower: 1000 + 16*230})
	if op := sink.next(t); op.kind != opSetAmps {
		t.Fatalf("queued %v once the car charges again, want a current change", op)
	}
}

// Choosing a mode again asks the car afresh.
func TestModeChangeEndsTheChargeLimitHold(t *testing.T) {
	c, sink := newTestCharger(t, ModeMax)
	data := deye.InverterData{LoadPower: 1000}

	refuseFirstStart(t, c, sink, data)
	assertHeldAtLimit(t, c, sink, data)

	sink.setErr(nil)
	c.SetMode(ModeMax)
	c.Evaluate(data)
	if op := sink.next(t); op.kind != opStart {
		t.Fatalf("queued %v after the mode was chosen again, want a start", op)
	}
}

// Solar mode had the same loop through its start retry and contactor recovery.
// A full car must leave it idle, even with plenty of surplus for a fast start.
func TestSolarModeGoesIdleAtTheChargeLimit(t *testing.T) {
	c, sink := newTestCharger(t, ModeSolar)
	c.twc = pluggedTWC()
	c.modeState = StateStarting
	c.startingAt = time.Now().Add(-10 * time.Minute) // start delay long elapsed
	data := deye.InverterData{GridPower: -4000, LoadPower: 500, TotalPV: 4500}

	refuseFirstStart(t, c, sink, data)
	assertHeldAtLimit(t, c, sink, data)
	if s := c.GetStatus().ModeState; s != StateIdle {
		t.Errorf("mode state %q at the charge limit, want %q", s, StateIdle)
	}
}

func TestRoomToCharge(t *testing.T) {
	tests := []struct {
		name  string
		cs    VehicleChargeState
		limit int // limit when the car refused; 0 if unknown
		want  bool
	}{
		{"still full", VehicleChargeState{BatteryLevel: 60, ChargeLimitSoc: 60, ChargingState: "Complete"}, 60, false},
		{"above the limit", VehicleChargeState{BatteryLevel: 62, ChargeLimitSoc: 60, ChargingState: "Stopped"}, 60, false},
		{"limit raised", VehicleChargeState{BatteryLevel: 60, ChargeLimitSoc: 80, ChargingState: "Complete"}, 60, true},
		{"battery dropped", VehicleChargeState{BatteryLevel: 55, ChargeLimitSoc: 60, ChargingState: "Stopped"}, 60, true},
		{"just under but the car calls it complete", VehicleChargeState{BatteryLevel: 59, ChargeLimitSoc: 60, ChargingState: "Complete"}, 60, false},
		{"room, limit unknown at refusal", VehicleChargeState{BatteryLevel: 50, ChargeLimitSoc: 80, ChargingState: "Stopped"}, 0, true},
		{"limit lowered", VehicleChargeState{BatteryLevel: 60, ChargeLimitSoc: 50, ChargingState: "Stopped"}, 60, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := roomToCharge(&tc.cs, tc.limit); got != tc.want {
				t.Errorf("roomToCharge = %v, want %v", got, tc.want)
			}
		})
	}
}

// The car's "complete" answer arrives from vehicle-command as a NominalError
// carrying its plain-text reason; it must be told apart from real failures.
func TestIsChargeCompleteRefusal(t *testing.T) {
	refusal := func(reason string) error {
		return &protocol.NominalError{Details: protocol.NewError("car could not execute command: "+reason, false, false)}
	}
	tests := []struct {
		err  error
		want bool
	}{
		{refusal("complete"), true},
		{fmt.Errorf("ChargeStart: %w", refusal("complete")), true},
		{refusal("is_charging"), false},
		{refusal("disconnected"), false},
		{errors.New("ble failed to enumerate device services"), false},
		{nil, false},
	}
	for _, tc := range tests {
		if got := isChargeCompleteRefusal(tc.err); got != tc.want {
			t.Errorf("isChargeCompleteRefusal(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// The demo's simulated car refuses a start once it is full, like a real one.
func TestDryRunStartRefusesAFullCar(t *testing.T) {
	b := NewBLEClient(true, "", "", nil, nil)
	cs := &VehicleChargeState{BatteryLevel: 80, ChargeLimitSoc: 80, ChargingState: "Complete"}
	b.dryRunState = func() *VehicleChargeState { return cs }
	if _, err := b.StartCharging(); !errors.Is(err, ErrChargeComplete) {
		t.Errorf("full car: StartCharging error %v, want ErrChargeComplete", err)
	}
	cs = &VehicleChargeState{BatteryLevel: 70, ChargeLimitSoc: 80, ChargingState: "Stopped"}
	if _, err := b.StartCharging(); err != nil {
		t.Errorf("car with room: StartCharging error %v, want nil", err)
	}
}
