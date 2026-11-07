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
//     back when the room returns, never above the car's own setting;
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

	if c.guardStopped {
		c.status = StatusWaiting
		if charging {
			// The car is charging again: our restart landed, or it resumed by itself.
			c.guardStopped = false
			c.guardResumeAt = time.Time{}
			c.overloadPausedAt = time.Time{}
			if c.currentAmps == 0 { // it resumed by itself, at its own setting
				c.currentAmps = c.guardCeiling
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
		c.guarding = true
		c.guardCeiling = c.carCurrentSetting()
		c.currentAmps = c.guardCeiling
		log.Printf("[TESLA] Idle: the car is charging on its own — keeping it under the %dW load limit (car set to %dA)",
			c.loadLimitW(), c.guardCeiling)
	}

	// Raise a reduced current back once there is room, by at least
	// maxRaiseDeadbandAmps (see evalMax), never above the car's own setting.
	// Reductions are enforceLoadCap's, which runs after this.
	if target := min(headroom, c.guardCeiling); target > c.currentAmps && target-c.currentAmps >= maxRaiseDeadbandAmps {
		c.sendSetAmps(target)
	}

	c.status = StatusCharging
	if c.currentAmps < c.guardCeiling {
		c.reason = "Idle — car charging on its own, held at " + itoa(c.currentAmps) + "A for the " +
			itoa(c.loadLimitW()) + "W load limit (house " + itoa(houseW) + "W)"
	} else {
		c.reason = "Idle — car charging on its own at up to " + itoa(c.guardCeiling) + "A (load limit " +
			itoa(c.loadLimitW()) + "W)"
	}
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
	amps := min(headroom, c.guardCeiling)
	if c.guardResumeAt.IsZero() {
		c.guardResumeAt = time.Now()
		log.Printf("[TESLA] Idle: restarting the charge the guard stopped, at %dA", amps)
	}
	// Re-requested every cycle until the contactor closes; the queue suppresses
	// a repeat of a start that just succeeded.
	c.sendStartCharge(amps)
	c.reason = "Idle — restarting the car's charge at " + itoa(amps) + "A"
}

// carCurrentSetting is the car's own charging-current setting as last read over
// Bluetooth, capped at max_amps, or max_amps if it has not been read yet.
// Caller holds mu.
func (c *Charger) carCurrentSetting() int {
	c.chargeMu.RLock()
	defer c.chargeMu.RUnlock()
	if c.chargeState != nil && c.chargeState.ChargeCurrentA >= minChargeAmps {
		return min(c.chargeState.ChargeCurrentA, c.cfg.MaxAmps)
	}
	return c.cfg.MaxAmps
}

// guardStoppedCharge records that the guard stopped a charge the car started
// itself, so it is restarted when allowed. Caller holds mu.
func (c *Charger) guardStoppedCharge() {
	if c.mode != ModeIdle || !c.guardOn() {
		return
	}
	if !c.guarding {
		c.guarding = true
		c.guardCeiling = c.carCurrentSetting()
	}
	c.guardStopped = true
	c.guardResumeAt = time.Time{}
}

// endGuard finishes guarding a session: the car's own current setting is put
// back, since the car keeps whatever it was last told. Caller holds mu.
func (c *Charger) endGuard() {
	if c.guarding && c.guardCeiling > 0 && c.currentAmps != c.guardCeiling {
		log.Printf("[TESLA] Idle: setting the car's charging current back to %dA", c.guardCeiling)
		c.want(bleOp{kind: opSetAmps, amps: c.guardCeiling})
	}
	c.clearGuard()
	c.currentAmps = 0
	c.overloadSince, c.overloadPausedAt = time.Time{}, time.Time{}
}

// clearGuard forgets the guard's state without telling the car anything, as
// when the mode changes and the new mode takes over the car's current.
// Caller holds mu.
func (c *Charger) clearGuard() {
	c.guarding, c.guardStopped = false, false
	c.guardCeiling = 0
	c.guardResumeAt = time.Time{}
}
