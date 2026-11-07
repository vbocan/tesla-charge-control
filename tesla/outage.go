package tesla

import (
	"context"
	"log"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"
)

// Grid-outage and inverter-data protection.
//
// When the grid fails the inverter runs the house from its battery, and a car
// charging at 5 kW would drain the battery reserve that is meant to keep the
// house running. So while the grid is down, charging is stopped and blocked in
// every mode (Idle mode is hands-off, so there only a car that is actually
// charging is stopped). The same protection applies when the Pi has lost the
// inverter altogether — it can then neither enforce the load cap nor see an
// outage — and stopping the car needs only Bluetooth, not the network. Idle
// mode is exempt from that one: the logger drops out briefly and often at
// night, and a stop would cut short a charge the car scheduled itself; the
// Idle-mode guard (guard.go) just holds the current until data returns.

// Why charging is paused, reported to the UI in Status.Paused.
const (
	PauseNone      = ""
	PauseOutage    = "outage"    // the grid is down (or an outage is being simulated)
	PauseRestoring = "restoring" // the grid is back; waiting for it to prove stable
	PauseBlind     = "blind"     // no inverter data
)

const (
	// gridOutageConfirm is how long the grid readings must look lost, over at
	// least gridMinSamples readings, before an outage is declared. The inverter
	// occasionally reports a transient bad value; a pause is cheap but must not
	// be triggered by one.
	gridOutageConfirm = 10 * time.Second
	gridMinSamples    = 2

	// nominalGridHz is the mains frequency; readings 10% either side are "normal".
	nominalGridHz = 50.0

	// deadGridHz: a frequency reading below this means there is no grid frequency.
	deadGridHz = 1.0

	// pauseStopRetry is how often a stop is re-sent while paused when the wall
	// connector can't say whether the car is still charging.
	pauseStopRetry = 60 * time.Second

	// watchdogTick is how often the inverter-data watchdog checks.
	watchdogTick = 5 * time.Second

	// simOutageMax bounds a simulated outage so one left on by mistake ends by itself.
	simOutageMax = 10 * time.Minute
)

// gridState reads the inverter's grid registers. lost is true when the grid
// voltage has collapsed or there is no grid frequency — an inverter that has
// islanded reports about 0 for both. known is false when the data can't be
// trusted, so the sample is ignored: a read that returned no battery voltage is
// a failed read, not an outage, and so is a frequency that is neither about 0
// nor about 50 Hz. The logger now and then returns a garbled first register
// block (3-127, which holds the frequency): 2.00 Hz with the grid voltage
// normal, or 0 V when the second block is garbled as well.
func gridState(d deye.InverterData, nominalV int) (lost, known bool) {
	if d.BattVoltage <= 0 {
		return false, false
	}
	noFreq := d.GridFreq < deadGridHz
	if !noFreq && (d.GridFreq < 0.9*nominalGridHz || d.GridFreq > 1.1*nominalGridHz) {
		return false, false
	}
	return noFreq || d.GridVoltage < 0.8*float64(nominalV), true
}

// trackGrid updates the outage state from one inverter reading. Caller holds mu.
func (c *Charger) trackGrid(data deye.InverterData, now time.Time) {
	if !c.cfg.GridOutagePause {
		c.gridOutage = false
		c.gridBadSince, c.gridBadSamples, c.gridGoodSince = time.Time{}, 0, time.Time{}
		return
	}
	lost, known := gridState(data, c.cfg.GridVoltage)
	if !known {
		return
	}
	if lost {
		c.gridGoodSince = time.Time{}
		if c.gridBadSince.IsZero() {
			c.gridBadSince, c.gridBadSamples = now, 0
		}
		c.gridBadSamples++
		if !c.gridOutage && c.gridBadSamples >= gridMinSamples && now.Sub(c.gridBadSince) >= gridOutageConfirm {
			c.gridOutage = true
			log.Printf("[TESLA] GRID OUTAGE: grid %.0fV %.1fHz — pausing car charging to protect the battery reserve",
				data.GridVoltage, data.GridFreq)
		}
		return
	}

	c.gridBadSince, c.gridBadSamples = time.Time{}, 0
	if !c.gridOutage {
		return
	}
	if c.gridGoodSince.IsZero() {
		c.gridGoodSince = now
		log.Printf("[TESLA] Grid readings normal again (%.0fV %.1fHz) — waiting %ds before resuming charging",
			data.GridVoltage, data.GridFreq, c.cfg.GridRestoreDelayS)
	}
	if now.Sub(c.gridGoodSince) >= time.Duration(c.cfg.GridRestoreDelayS)*time.Second {
		c.gridOutage = false
		c.gridGoodSince = time.Time{}
		log.Printf("[TESLA] Grid stable — charging may resume")
	}
}

// simOutage reports whether a simulated outage is active.
func (c *Charger) simOutage(now time.Time) bool {
	return !c.simOutageUntil.IsZero() && now.Before(c.simOutageUntil)
}

// pauseState says why charging is paused right now, or PauseNone. Caller holds mu.
func (c *Charger) pauseState(now time.Time) string {
	switch {
	case c.blind && c.mode != ModeIdle:
		return PauseBlind
	case c.simOutage(now), c.gridOutage && c.gridGoodSince.IsZero():
		return PauseOutage
	case c.gridOutage:
		return PauseRestoring
	}
	return PauseNone
}

// pauseReason is the human-readable explanation for the current pause.
func (c *Charger) pauseReason(now time.Time) string {
	switch c.paused {
	case PauseOutage:
		return "Grid outage — charging paused to protect the battery reserve"
	case PauseRestoring:
		left := time.Duration(c.cfg.GridRestoreDelayS)*time.Second - now.Sub(c.gridGoodSince)
		return "Grid restored — charging resumes in " + itoa(int(left.Seconds())+1) + "s"
	case PauseBlind:
		return "No inverter data for " + itoa(int(now.Sub(c.lastInverterOK).Seconds())) +
			"s — charging paused until it returns"
	}
	return ""
}

// enforcePause carries out a pause: stop the car and keep it stopped, skipping
// the mode logic. It reports whether it handled this cycle; false means the
// mode logic should still run (Idle mode with nothing to stop). Caller holds mu.
func (c *Charger) enforcePause(now time.Time) bool {
	twcOnline := c.twc != nil && c.twc.Online()
	twcCharging := c.twc != nil && c.twc.IsCharging()

	// Idle mode is hands-off: only step in for a car that is actually charging.
	if c.mode == ModeIdle && !twcCharging {
		return false
	}

	// Stop on entering the pause (this also cancels a start still queued), then
	// for as long as the wall connector shows the car charging. Without a wall
	// connector we can't tell, so re-send now and then.
	if twcCharging || c.pauseStopAt.IsZero() || (!twcOnline && now.Sub(c.pauseStopAt) >= pauseStopRetry) {
		c.sendStop()
		c.pauseStopAt = now
	}
	// In Idle mode the car started this charge itself and won't resume it after
	// the pause; the guard restarts it then (see guard.go).
	if twcCharging {
		c.guardStoppedCharge()
	}

	// Forget what the mode logic was doing; it starts afresh when charging resumes.
	c.currentAmps = 0
	c.modeState = StateIdle
	c.samples, c.smoothedW = c.samples[:0], 0
	c.startingLowStreak = 0
	c.contactorOpenSince, c.overloadSince, c.overloadPausedAt = time.Time{}, time.Time{}, time.Time{}

	c.status = StatusWaiting
	c.reason = c.pauseReason(now)
	// A stop that can't reach the car matters more than anything else on screen.
	if err := c.recentBLEError(); err != nil && twcCharging {
		c.status = StatusError
		c.reason += " — can't stop the car: " + bleErrorReason(err)
	}
	return true
}

// resumeFromPause resets the state for a fresh start once a pause ends. The car
// may have gone to sleep meanwhile, so it is woken again first. Caller holds mu.
func (c *Charger) resumeFromPause() {
	log.Printf("[TESLA] Charging pause ended — resuming mode %s", c.mode)
	c.pauseStopAt = time.Time{}
	c.wakeAttempted = false
	c.modeState = StateIdle
}

// SimulateOutage turns a simulated grid outage on or off, for testing the
// protection end to end without switching off the grid. It lasts at most
// simOutageMax. Takes effect on the next Evaluate.
func (c *Charger) SimulateOutage(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if on {
		c.simOutageUntil = time.Now().Add(simOutageMax)
		log.Printf("[TESLA] Simulated grid outage ON (for at most %s)", simOutageMax)
	} else {
		c.simOutageUntil = time.Time{}
		log.Printf("[TESLA] Simulated grid outage OFF")
	}
}

// RunWatchdog stops the car when inverter data stops arriving. Without data
// the load cap and the outage detection are both blind, so the safe state is
// "not charging" — except in Idle mode, which leaves the car alone (see the top
// of this file). Run this in its own goroutine.
func (c *Charger) RunWatchdog(ctx context.Context) {
	t := time.NewTicker(watchdogTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.checkInverterStale()
		}
	}
}

// checkInverterStale is one watchdog check.
func (c *Charger) checkInverterStale() {
	stale := time.Duration(c.cfg.InverterStaleStopS) * time.Second
	if stale <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if now.Sub(c.lastInverterOK) < stale {
		return
	}

	c.cycleOp = nil
	defer c.flushOp() // runs before the unlock above

	if !c.blind {
		c.blind = true
		what := "pausing car charging until it returns"
		if c.mode == ModeIdle {
			what = "Idle mode leaves the car alone"
		}
		log.Printf("[TESLA] No inverter data for %ds — %s", int(now.Sub(c.lastInverterOK).Seconds()), what)
	}
	// The mode can change while we are blind, so the pause can end here too
	// (switching to Idle mode), not only when data returns in Evaluate.
	wasPaused := c.paused
	c.paused = c.pauseState(now)
	if wasPaused != PauseNone && c.paused == PauseNone {
		c.resumeFromPause()
	}
	if c.paused != PauseNone && c.enforcePause(now) {
		c.logDecision()
	}
}
