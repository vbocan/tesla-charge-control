package tesla

import (
	"log"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"
)

// Idle-mode load guard.
//
// In Idle mode the car decides when to charge: on its own schedule, or as soon
// as it is plugged in. The Pi never starts a charge there, but the inverter must
// still stay under max_output_w with the car drawing up to max_amps on top of
// the house; at night, with no solar, an oven and a kettle can push it over. So
// while the car charges on its own, Idle mode applies the same load cap as the
// other modes:
//   - it lowers the car's current when the house needs the room, and raises it
//     back when the room returns, never above the car's own setting. Until that
//     setting has been read over Bluetooth (it is asked for the moment the guard
//     takes over), the most the car has been measured drawing stands in for it:
//     the car cannot draw more than its setting, so the guard never pushes the
//     car's current up past what the car chose;
//   - it stops the car only when even the minimum current would overload the
//     inverter, or when the grid is down (see outage.go);
//   - a charge it stopped, it restarts once that is safe again, because the car
//     would not resume by itself; a charge the car ended itself is left alone;
//   - when the session ends it puts the car's own current setting back: the car
//     keeps the setting and plans its next scheduled charge with it.
// Without inverter data it does nothing (see outage.go). It needs the wall
// connector to see that the car is charging; without one Idle mode stays passive.

// guardResumeTimeout is how long the guard keeps trying to restart a charge it
// stopped. The car may have finished or ended the session meanwhile.
const guardResumeTimeout = 2 * time.Minute

// guardOn reports whether the Idle-mode guard can work this cycle. Caller holds mu.
func (c *Charger) guardOn() bool {
	return c.cfg.IdleLoadCap && c.maxOutputW > 0 && c.twc != nil && c.twc.Online()
}

// evalIdle runs Idle mode for one cycle. carW is the car's measured draw.
// Caller holds mu.
func (c *Charger) evalIdle(data deye.InverterData, carW int) {
	c.status = StatusIdle
	if c.twc != nil && c.twc.Online() {
		c.reason = "Idle — car connected, select a charging mode"
	}
	if !c.guardOn() {
		return
	}
	if c.paused != PauseNone {
		// A car that is still charging was handled by enforcePause; one that is
		// stopped waits for the pause to end.
		c.reason = c.pauseReason(time.Now())
		return
	}

	charging := c.twc.IsCharging()
	headroom, houseW := c.headroomAmps(data, carW)
	if c.guarding {
		c.learnCarSetting(carW)
	}

	if c.guardStopped {
		c.status = StatusWaiting
		if charging {
			// The car is charging again: our restart landed, or it resumed by itself.
			c.guardStopped = false
			c.guardResumeAt = time.Time{}
			c.overloadPausedAt = time.Time{}
			if c.currentAmps == 0 { // it resumed by itself, at its own setting
				c.currentAmps = max(c.guardLimit(), minChargeAmps)
			}
		} else {
			c.restartStoppedCharge(headroom, houseW)
			return
		}
	}

	if !charging {
		if c.guarding {
			log.Printf("[TESLA] Idle: the car ended its charge")
			c.endGuard()
		}
		c.status = StatusIdle
		return
	}

	if !c.guarding {
		c.startGuard(carW)
		if c.guardCeiling > 0 {
			log.Printf("[TESLA] Idle: the car is charging on its own — keeping it under the %dW load limit (car set to %dA)",
				c.loadLimitW(), c.guardCeiling)
		} else {
			log.Printf("[TESLA] Idle: the car is charging on its own — keeping it under the %dW load limit (drawing %dA; reading its own setting)",
				c.loadLimitW(), c.currentAmps)
		}
	}

	// Raise a reduced current back once there is room, by at least
	// maxRaiseDeadbandAmps (see evalMax), never above the car's own setting.
	// Reductions are enforceLoadCap's, which runs after this.
	limit := c.guardLimit()
	if target := min(headroom, limit); target > c.currentAmps && target-c.currentAmps >= maxRaiseDeadbandAmps {
		c.sendSetAmps(target)
	}

	c.status = StatusCharging
	if c.currentAmps < limit {
		c.reason = "Idle — car charging on its own, held at " + itoa(c.currentAmps) + "A for the " +
			itoa(c.loadLimitW()) + "W load limit (house " + itoa(houseW) + "W)"
	} else {
		c.reason = "Idle — car charging on its own at up to " + itoa(max(limit, c.currentAmps)) + "A (load limit " +
			itoa(c.loadLimitW()) + "W)"
	}
}

// startGuard begins guarding a charge the car started itself. carW is its
// measured draw (0 if unknown). The ceiling is the car's own current setting if
// a reading of it is current; otherwise the car is asked for it now, and until
// the answer comes the guard works from what the car is measured drawing.
// Caller holds mu.
func (c *Charger) startGuard(carW int) {
	c.guarding = true
	c.guardTouched = false
	c.guardPeakA = 0
	if amps, ok := c.carSetting(); ok {
		c.guardCeiling = amps
		c.currentAmps = amps
	} else {
		c.guardCeiling = 0
		c.currentAmps = 0
		c.RefreshSoC()
	}
	c.learnCarSetting(carW)
}

// learnCarSetting keeps the guard's picture of the car's setting up to date:
// the most the car has been measured drawing, and the setting itself once a
// reading comes in, as long as the guard hasn't changed it (after that a reading
// would show the Pi's value). Caller holds mu.
func (c *Charger) learnCarSetting(carW int) {
	volts := c.wattsPerAmp()
	if volts <= 0 || carW < 0 {
		return
	}
	drawing := min((carW+volts/2)/volts, c.cfg.MaxAmps) // nearest amp: 15.8 A measured is a 16 A setting
	c.guardPeakA = max(c.guardPeakA, drawing)
	if c.guardCeiling > 0 || c.guardTouched {
		return
	}
	if amps, ok := c.carSetting(); ok {
		c.guardCeiling = amps
		c.currentAmps = amps
		log.Printf("[TESLA] Idle: the car's own charging current is %dA", amps)
		return
	}
	// Until the setting is known, what the car draws (rounded up, so the load
	// cap errs on the safe side) is what the cap has to work with.
	c.currentAmps = max(c.currentAmps, min((carW+volts-1)/volts, c.cfg.MaxAmps), minChargeAmps)
}

// guardLimit is the most the guard may set the car's current to: the car's own
// setting, or while that is unknown the most it has been measured drawing.
// Caller holds mu.
func (c *Charger) guardLimit() int {
	if c.guardCeiling > 0 {
		return c.guardCeiling
	}
	return c.guardPeakA
}

// restartStoppedCharge restarts a charge the guard stopped, once that is safe.
// Caller holds mu.
func (c *Charger) restartStoppedCharge(headroom, houseW int) {
	if c.takeStartFull() {
		// The car filled up while it was stopped (or was nearly full): it refuses
		// the restart, and asking again would get the same answer.
		log.Printf("[TESLA] Idle: the car is at its charge limit — leaving it alone")
		c.endGuard()
		c.status = StatusIdle
		c.reason = "Idle — car at its charge limit"
		return
	}
	if !c.overloadPausedAt.IsZero() {
		if remaining := maxResumeHoldoff - time.Since(c.overloadPausedAt); remaining > 0 {
			c.reason = "Idle — car stopped for overload, restarting in " + itoa(int(remaining.Seconds())+1) + "s"
			return
		}
	}
	if !c.guardResumeAt.IsZero() && time.Since(c.guardResumeAt) > guardResumeTimeout {
		log.Printf("[TESLA] Idle: the car did not restart its charge within %s — leaving it alone", guardResumeTimeout)
		c.endGuard()
		c.status = StatusIdle
		return
	}
	if headroom < minChargeAmps {
		c.reason = "Idle — car stopped, home load " + itoa(houseW) + "W, waiting for " + itoa(c.loadLimitW()) + "W headroom"
		return
	}
	amps := max(min(headroom, c.guardLimit()), minChargeAmps)
	if c.guardResumeAt.IsZero() {
		c.guardResumeAt = time.Now()
		log.Printf("[TESLA] Idle: restarting the charge the guard stopped, at %dA", amps)
	}
	// Re-requested every cycle until the contactor closes; the queue suppresses
	// a repeat of a start that just succeeded.
	c.sendStartCharge(amps)
	c.reason = "Idle — restarting the car's charge at " + itoa(amps) + "A"
}

// carSetting is the car's own charging-current setting, capped at max_amps, if
// the last reading of it is current: taken no earlier than the last time the Pi
// asked the car for a current. ok is false when there is no such reading.
// Caller may hold mu.
func (c *Charger) carSetting() (amps int, ok bool) {
	c.chargeMu.RLock()
	defer c.chargeMu.RUnlock()
	cs := c.chargeState
	if cs == nil || cs.ChargeCurrentA < minChargeAmps || c.ampsSetAt.After(c.chargeStateAt) {
		return 0, false
	}
	return min(cs.ChargeCurrentA, c.cfg.MaxAmps), true
}

// guardStoppedCharge records that the guard stopped a charge the car started
// itself, so it is restarted when allowed. Caller holds mu.
func (c *Charger) guardStoppedCharge() {
	if c.mode != ModeIdle || !c.guardOn() {
		return
	}
	if !c.guarding {
		carW := 0
		if c.twc != nil {
			carW, _ = c.twc.ActualCarW()
		}
		c.startGuard(carW)
	}
	c.guardStopped = true
	c.guardResumeAt = time.Time{}
}

// endGuard finishes guarding a session. If the guard changed the car's current
// setting it puts it back, since the car keeps whatever it was last told and
// plans its next scheduled charge with it: to the setting it read, or, if that
// was never read, to the most the car was measured drawing, which is as close
// as the guard can get without going above the car's own choice.
// Caller holds mu.
func (c *Charger) endGuard() {
	if back := c.guardLimit(); c.guarding && c.guardTouched && back >= minChargeAmps && c.currentAmps != back {
		if c.guardCeiling > 0 {
			log.Printf("[TESLA] Idle: setting the car's charging current back to %dA", back)
		} else {
			log.Printf("[TESLA] Idle: the car's own setting was never read; setting its charging current back to %dA, the most it drew", back)
		}
		c.want(bleOp{kind: opSetAmps, amps: back})
	}
	c.clearGuard()
	c.currentAmps = 0
	c.overloadSince, c.overloadPausedAt = time.Time{}, time.Time{}
}

// clearGuard forgets the guard's state without telling the car anything, as
// when the mode changes and the new mode takes over the car's current.
// Caller holds mu.
func (c *Charger) clearGuard() {
	c.guarding, c.guardStopped, c.guardTouched = false, false, false
	c.guardCeiling, c.guardPeakA = 0, 0
	c.guardResumeAt = time.Time{}
}
