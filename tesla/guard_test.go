package tesla

import (
	"strings"
	"testing"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"
)

// newGuardCharger is an Idle-mode charger with the load guard on: a 6 kW
// inverter, 400 W margin (5600 W usable), 25 A max, 230 V.
func newGuardCharger(t *testing.T) (*Charger, *opSink) {
	t.Helper()
	c, sink := newTestCharger(t, ModeIdle)
	c.cfg.IdleLoadCap = true
	return c, sink
}

// house is an inverter reading with the given total load (house plus car).
func house(loadW int) deye.InverterData {
	return deye.InverterData{GridVoltage: 230, GridFreq: 50, BattVoltage: 52, LoadPower: loadW}
}

// pluggedTWC is a wall connector with the car plugged in but not charging.
func pluggedTWC() *TWCClient {
	return &TWCClient{online: true, vitals: &TWCVitals{VehicleConnected: true, GridV: 230}}
}

// The car decides when to charge in Idle mode; the guard must never start it.
func TestIdleGuardNeverStartsTheCar(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = pluggedTWC()

	c.Evaluate(house(300))
	c.Evaluate(house(300))

	sink.none(t)
	if c.guarding {
		t.Error("guarding a car that is not charging")
	}
}

func TestIdleGuardLowersAndRaisesTheCurrent(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = chargingTWC(25)

	// The car charges at 25 A on its own; with 300 W of house load only 23 A fit.
	c.Evaluate(house(300 + 25*230))
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 23, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
	if s := c.GetStatus(); s.Mode != ModeIdle || s.Status != StatusCharging {
		t.Errorf("mode %q status %q, want idle and charging", s.Mode, s.Status)
	}

	// The oven comes on: 2300 W of house load leaves room for 14 A.
	c.twc = chargingTWC(23)
	c.Evaluate(house(2300 + 23*230))
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 14, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
	if r := c.GetStatus().Reason; !strings.Contains(r, "Reduced to 14A") {
		t.Errorf("reason %q should say the current was reduced", r)
	}

	// And goes off again: back up, but only as far as the room allows.
	c.twc = chargingTWC(14)
	c.Evaluate(house(300 + 14*230))
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 23}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
}

// The car's own setting is the ceiling: the guard lowers it, never raises it.
func TestIdleGuardRespectsTheCarsSetting(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.chargeState = &VehicleChargeState{ChargeCurrentA: 16}
	c.twc = chargingTWC(16)

	c.Evaluate(house(300 + 16*230)) // room for 23 A, the car wants 16
	sink.none(t)

	c.Evaluate(house(3000 + 16*230)) // room for 11 A
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 11, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}

	c.twc = chargingTWC(11)
	c.Evaluate(house(300 + 11*230)) // room for 23 A again: back to 16, not 23
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 16}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
}

// The car keeps its current setting and plans its next scheduled charge with
// it, so the guard puts it back when the session ends.
func TestIdleGuardRestoresTheSettingWhenTheChargeEnds(t *testing.T) {
	for _, tc := range []struct {
		name string
		twc  *TWCClient
	}{
		{"the car finished", pluggedTWC()},
		{"the car was unplugged", &TWCClient{online: true, vitals: &TWCVitals{GridV: 230}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sink := newGuardCharger(t)
			c.twc = chargingTWC(25)
			c.Evaluate(house(3000 + 25*230))
			if op := sink.next(t); op.amps >= 25 {
				t.Fatalf("queued %v, want a reduction", op)
			}

			c.twc = tc.twc
			c.Evaluate(house(300))
			if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 25}); got != want {
				t.Fatalf("queued %v, want the car's setting back: %v", got, want)
			}
			if c.guarding {
				t.Error("still guarding after the charge ended")
			}
		})
	}
}

// Nothing to put back when the guard never had to touch the current.
func TestIdleGuardLeavesAnUntouchedSessionAlone(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = chargingTWC(10)
	c.chargeState = &VehicleChargeState{ChargeCurrentA: 10}

	c.Evaluate(house(300 + 10*230))
	c.twc = pluggedTWC()
	c.Evaluate(house(300))

	sink.none(t)
}

// Stop only when even the minimum current would overload the inverter, then
// restart the charge (the car won't) after the hold-off, once there is room.
func TestIdleGuardStopsForOverloadAndRestarts(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = chargingTWC(10)

	c.Evaluate(house(5000 + 10*230)) // 1000 W left under 6 kW: not even 5 A fits
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}

	// The car has stopped and the house is quiet again, but the hold-off runs.
	c.twc = pluggedTWC()
	c.Evaluate(house(300))
	sink.none(t)
	if r := c.GetStatus().Reason; !strings.Contains(r, "restarting in") {
		t.Errorf("reason %q should count down to the restart", r)
	}

	// Restarted at what the car was drawing (its setting was never read), not
	// at the 23 A the house would allow.
	c.overloadPausedAt = time.Now().Add(-maxResumeHoldoff - time.Second)
	c.Evaluate(house(300))
	if got, want := sink.next(t), (bleOp{kind: opStart, amps: 10}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}

	// The charge is running again: guarded as before, no more starts.
	c.twc = chargingTWC(10)
	c.Evaluate(house(300 + 10*230))
	sink.none(t)
	if c.guardStopped {
		t.Error("the restart landed, so the guard must no longer count the car as stopped")
	}
}

// A car that never picks the charge back up is left alone after a while.
func TestIdleGuardGivesUpARestartTheCarRefuses(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = chargingTWC(10)
	c.Evaluate(house(5000 + 10*230))
	sink.next(t) // the stop

	c.twc = pluggedTWC()
	c.overloadPausedAt = time.Now().Add(-maxResumeHoldoff - time.Second)
	c.Evaluate(house(300))
	sink.next(t) // the start

	c.guardResumeAt = time.Now().Add(-guardResumeTimeout - time.Second)
	c.Evaluate(house(300))
	if c.guarding || c.guardStopped {
		t.Error("still trying to restart a car that won't")
	}
}

// A car that filled up while the guard held it stopped refuses the restart; the
// guard must take that as final instead of asking until it times out.
func TestIdleGuardStopsRestartingAFullCar(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = chargingTWC(10)
	c.Evaluate(house(5000 + 10*230))
	sink.next(t) // the stop

	c.twc = pluggedTWC()
	c.overloadPausedAt = time.Now().Add(-maxResumeHoldoff - time.Second)
	sink.setErr(ErrChargeComplete)
	c.Evaluate(house(300))
	if op := sink.next(t); op.kind != opStart {
		t.Fatalf("queued %v, want the restart", op)
	}
	waitFor(t, "the refusal to come back", c.refusalSeen)

	sink.setErr(nil)
	c.Evaluate(house(300))
	if c.guarding || c.guardStopped {
		t.Error("still trying to restart a car at its charge limit")
	}
	if s := c.GetStatus(); s.Status != StatusIdle || !strings.Contains(s.Reason, "charge limit") {
		t.Errorf("status %q / reason %q, want idle at the charge limit", s.Status, s.Reason)
	}
	// The restart asked for the 10 A the car was drawing, so there is nothing to
	// put back, and nothing else is sent.
	c.Evaluate(house(300))
	sink.none(t)
}

// Right after plug-in the car's own setting is often not known yet. The guard
// must not take max_amps for it: a "reduction" from an assumed 25 A to the 23 A
// that fit would raise a car charging at 16 A. It asks the car instead.
func TestIdleGuardNeverRaisesAnUnreadSetting(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = chargingTWC(16)

	c.Evaluate(house(300 + 16*230)) // room for 23 A
	c.Evaluate(house(300 + 16*230))
	sink.none(t)
	if len(c.socRefresh) != 1 {
		t.Error("the guard should ask the car for its setting when it takes over")
	}
	if r := c.GetStatus().Reason; !strings.Contains(r, "up to 16A") {
		t.Errorf("reason %q should work from the 16 A the car draws", r)
	}

	// The oven: only 11 A fit. Reductions don't wait for the reading.
	c.Evaluate(house(3000 + 16*230))
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 11, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}

	// A reading now shows the Pi's own 11 A, not the car's setting: ignored.
	c.chargeMu.Lock()
	c.chargeState, c.chargeStateAt = &VehicleChargeState{ChargeCurrentA: 11}, time.Now()
	c.chargeMu.Unlock()

	// The oven goes off: back to the 16 A the car chose, not 11 and not 23.
	c.twc = chargingTWC(11)
	c.Evaluate(house(300 + 11*230))
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 16}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
}

// Once the car's setting is read, before the guard has changed anything, it is
// the ceiling, even above what the car happens to be drawing.
func TestIdleGuardAdoptsTheSettingOnceRead(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = chargingTWC(12) // still ramping up
	c.Evaluate(house(300 + 12*230))
	sink.none(t)

	c.chargeMu.Lock()
	c.chargeState, c.chargeStateAt = &VehicleChargeState{ChargeCurrentA: 20}, time.Now()
	c.chargeMu.Unlock()
	c.twc = chargingTWC(20)
	c.Evaluate(house(300 + 20*230))
	sink.none(t)

	c.Evaluate(house(2500 + 20*230)) // room for 13 A
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 13, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
	c.twc = pluggedTWC() // the car finishes: its 20 A goes back
	c.Evaluate(house(300))
	if got, want := sink.next(t), (bleOp{kind: opSetAmps, amps: 20}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
}

// A reading older than the last current the Pi set (in another mode, say) no
// longer shows the car's setting.
func TestIdleGuardIgnoresAStaleSetting(t *testing.T) {
	c, sink := newGuardCharger(t)
	read := time.Now().Add(-time.Minute)
	c.chargeState, c.chargeStateAt = &VehicleChargeState{ChargeCurrentA: 25}, read
	c.ampsSetAt = read.Add(time.Second) // Solar mode then set 12 A
	c.twc = chargingTWC(12)

	c.Evaluate(house(300 + 12*230)) // room for 23 A
	sink.none(t)
	if c.guardCeiling != 0 {
		t.Errorf("ceiling %d A from a stale reading", c.guardCeiling)
	}
}

// A stop doesn't change the car's setting, so there is nothing to put back.
func TestIdleGuardStopAloneRestoresNothing(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.chargeState = &VehicleChargeState{ChargeCurrentA: 10}
	c.twc = chargingTWC(10)
	c.Evaluate(house(5000 + 10*230)) // not even 5 A fits
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}

	c.twc = &TWCClient{online: true, vitals: &TWCVitals{GridV: 230}} // unplugged
	c.Evaluate(house(300))
	sink.none(t)
}

// A charge stopped for a grid outage is restarted once the grid is stable.
func TestIdleGuardRestartsAfterAGridOutage(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.cfg.GridOutagePause = true
	c.cfg.GridRestoreDelayS = 300
	c.chargeState = &VehicleChargeState{ChargeCurrentA: 16}
	c.twc = chargingTWC(16)
	c.Evaluate(house(300 + 16*230))
	sink.none(t)

	declareOutage(c)
	c.Evaluate(gridDown)
	if got, want := sink.next(t), (bleOp{kind: opStop, urgent: true}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}

	// The grid is back but must prove stable first: nothing happens yet.
	c.twc = pluggedTWC()
	c.Evaluate(house(300))
	sink.none(t)
	if s := c.GetStatus(); s.Paused != PauseRestoring || !strings.Contains(s.Reason, "Grid restored") {
		t.Errorf("paused %q reason %q, want restoring", s.Paused, s.Reason)
	}

	c.gridGoodSince = time.Now().Add(-6 * time.Minute)
	c.Evaluate(house(300))
	if got, want := sink.next(t), (bleOp{kind: opStart, amps: 16}); got != want {
		t.Fatalf("queued %v, want %v", got, want)
	}
}

// With idle_load_cap off, Idle mode is fully passive again.
func TestIdleGuardCanBeDisabled(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.cfg.IdleLoadCap = false
	c.twc = chargingTWC(25)

	c.Evaluate(house(3000 + 25*230))
	c.Evaluate(house(5500 + 25*230))

	sink.none(t)
}

// Switching mode hands the car's current to the new mode without restoring.
func TestModeChangeEndsTheGuardQuietly(t *testing.T) {
	c, sink := newGuardCharger(t)
	c.twc = chargingTWC(25)
	c.Evaluate(house(3000 + 25*230))
	sink.next(t) // the reduction

	c.SetMode(ModeSolar)
	if c.guarding || c.guardStopped || c.guardCeiling != 0 {
		t.Error("the guard's state must be cleared on a mode change")
	}
}
