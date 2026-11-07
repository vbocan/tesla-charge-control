package deye

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/snowirbis/solarman"
	"github.com/vbocan/tesla-charge-control/linkstat"
)

// Inverter wraps the solarman logger connection.
type Inverter struct {
	inv *solarman.InverterLogger
	cfg ConnectionConfig

	link *linkstat.Tracker // 24h read health for the debug page

	// Outlier filter state. The Deye occasionally reports a transient peak in
	// regs 169 (grid), 178 (load), or 190 (battery) that the SolarMan library
	// passes through without complaint. We hold the last good value and
	// substitute it for short bursts; after the per-register reject streak hits
	// the limit we let the value through so a real sustained fault isn't masked.
	filterMu         sync.Mutex
	lastGoodLoadW    int
	hasLastGoodLoad  bool
	loadRejectStreak int
	lastGoodBattW    int
	hasLastGoodBatt  bool
	battRejectStreak int
	lastGoodGridW    int
	hasLastGoodGrid  bool
	gridRejectStreak int
}

const (
	// loadResidualThresholdW: how far |sources − sinks| can drift before we
	// treat the load reading as bogus. Inverter parasitic draw is ~50–150 W;
	// 500 W gives plenty of margin for sample skew across the two register
	// chunks while still catching multi-kW lies.
	loadResidualThresholdW = 500
	// maxLoadRejectStreak: after this many consecutive rejections, pass the
	// raw value through. Prevents a stuck/bad register from silently freezing
	// the gauge forever.
	maxLoadRejectStreak = 3

	// battPowerLimitW: physical ceiling for |BattPower| on the SUN-6K-SG03LP1.
	// The inverter caps battery throughput at ~6 kW; 7000 W leaves headroom for
	// brief peaks. Anything beyond is a register lie.
	battPowerLimitW = 7000
	// gridPowerLimitW: physical ceiling for |GridPower| on a single-phase
	// residential connection (32 A × 230 V ≈ 7.4 kW). 9000 W leaves headroom.
	gridPowerLimitW = 9000
	// battPowerDeltaLimitW: max plausible per-cycle change in BattPower. The BMS
	// can ramp fast when a heavy load engages, but a swing of ~6 kW in a single
	// 1–2 s poll cycle from a stable baseline is almost always a register lie.
	// Real load steps tend to span 2–3 cycles. If the new value is real it'll
	// be accepted after maxPowerRejectStreak consecutive readings anyway.
	battPowerDeltaLimitW = 4000
	// gridPowerDeltaLimitW: same idea for grid. The inverter's grid feed-in
	// ramps slower than the battery; 4 kW per cycle is a generous bound.
	gridPowerDeltaLimitW = 4000
	// maxPowerRejectStreak: matches maxLoadRejectStreak — after this many
	// consecutive rejections, accept the raw value so a real sustained change
	// isn't masked indefinitely.
	maxPowerRejectStreak = 3
)

// Connect creates a new inverter connection.
func Connect(cfg ConnectionConfig) *Inverter {
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	inv := solarman.Init(addr, cfg.LoggerSN, cfg.Timeout)
	return &Inverter{inv: inv, cfg: cfg, link: linkstat.New()}
}

// ReadAll reads all registers and returns parsed inverter data.
// Transparently retries once if the logger interleaves an async frame
// (observed as "expected 0x1510 ... got: 0x47XX") that desyncs the TCP stream.
func (i *Inverter) ReadAll() (InverterData, error) {
	start := time.Now()

	data, err := i.readAllOnce()
	if err != nil && isFrameDesync(err) {
		i.Reconnect()
		data, err = i.readAllOnce()
	}

	if err != nil {
		i.record(start, err)
		return InverterData{}, err
	}
	// Order matters: filter battery and grid first so the load filter's residual
	// check is computed against clean values. Otherwise a battery/grid lie can
	// mask a load lie (or vice versa).
	i.filterBattPower(&data)
	i.filterGridPower(&data)
	i.filterLoadPower(&data)
	i.record(start, nil)
	return data, nil
}

// filterBattPower rejects BattPower readings beyond the inverter's physical
// limit. The SolarMan logger occasionally emits a transient register lie that
// the library passes through; without filtering, surplus calc treats it as a
// massive battery discharge and trips a false grace/stop.
func (i *Inverter) filterBattPower(data *InverterData) {
	i.filterMu.Lock()
	defer i.filterMu.Unlock()

	if i.hasLastGoodBatt && i.battRejectStreak < maxPowerRejectStreak {
		overLimit := abs(data.BattPower) > battPowerLimitW
		overDelta := abs(data.BattPower-i.lastGoodBattW) > battPowerDeltaLimitW
		if overLimit || overDelta {
			reason := "limit"
			if overDelta && !overLimit {
				reason = "delta"
			} else if overDelta {
				reason = "limit+delta"
			}
			log.Printf("[INVERTER] Rejecting batt %dW (%s, last good %dW, Δ=%dW) — substituting last good",
				data.BattPower, reason, i.lastGoodBattW, data.BattPower-i.lastGoodBattW)
			data.BattPower = i.lastGoodBattW
			data.SelfConsumption = selfConsumption(data.TotalPV, data.BattPower, data.GridPower, data.LoadPower)
			i.battRejectStreak++
			return
		}
	}
	if i.battRejectStreak >= maxPowerRejectStreak {
		log.Printf("[INVERTER] Batt reject streak hit limit — accepting %dW", data.BattPower)
	}
	i.lastGoodBattW = data.BattPower
	i.hasLastGoodBatt = true
	i.battRejectStreak = 0
}

// filterGridPower rejects GridPower readings beyond a residential connection's
// physical limit. Same rationale as filterBattPower.
func (i *Inverter) filterGridPower(data *InverterData) {
	i.filterMu.Lock()
	defer i.filterMu.Unlock()

	if i.hasLastGoodGrid && i.gridRejectStreak < maxPowerRejectStreak {
		overLimit := abs(data.GridPower) > gridPowerLimitW
		overDelta := abs(data.GridPower-i.lastGoodGridW) > gridPowerDeltaLimitW
		if overLimit || overDelta {
			reason := "limit"
			if overDelta && !overLimit {
				reason = "delta"
			} else if overDelta {
				reason = "limit+delta"
			}
			log.Printf("[INVERTER] Rejecting grid %dW (%s, last good %dW, Δ=%dW) — substituting last good",
				data.GridPower, reason, i.lastGoodGridW, data.GridPower-i.lastGoodGridW)
			data.GridPower = i.lastGoodGridW
			data.SelfConsumption = selfConsumption(data.TotalPV, data.BattPower, data.GridPower, data.LoadPower)
			i.gridRejectStreak++
			return
		}
	}
	if i.gridRejectStreak >= maxPowerRejectStreak {
		log.Printf("[INVERTER] Grid reject streak hit limit — accepting %dW", data.GridPower)
	}
	i.lastGoodGridW = data.GridPower
	i.hasLastGoodGrid = true
	i.gridRejectStreak = 0
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// filterLoadPower rejects LoadPower samples that claim more power than the
// system can physically supply. residual = PV + battDischarge + gridImport −
// load. When residual < −threshold, the load register is reporting more than
// all sources combined — physically impossible, so it's a transient lie.
//
// We deliberately do NOT reject positive residuals (sources > load): a real
// high-draw event (e.g. dishwasher heater) can saturate reg 178 at the
// inverter's AC peak rating (~6.5 kW) while the battery honestly reports the
// full draw. That looks like residual > 0, but the load reading is the best
// approximation the inverter can give and shouldn't be substituted.
func (i *Inverter) filterLoadPower(data *InverterData) {
	i.filterMu.Lock()
	defer i.filterMu.Unlock()

	residual := data.TotalPV + data.BattPower + data.GridPower - data.LoadPower

	if i.hasLastGoodLoad && residual < -loadResidualThresholdW && i.loadRejectStreak < maxLoadRejectStreak {
		log.Printf("[INVERTER] Rejecting load %dW (residual %dW, PV=%d batt=%d grid=%d) — using last good %dW",
			data.LoadPower, residual, data.TotalPV, data.BattPower, data.GridPower, i.lastGoodLoadW)
		data.LoadPower = i.lastGoodLoadW
		data.SelfConsumption = selfConsumption(data.TotalPV, data.BattPower, data.GridPower, data.LoadPower)
		i.loadRejectStreak++
		return
	}
	if i.loadRejectStreak >= maxLoadRejectStreak {
		log.Printf("[INVERTER] Load reject streak hit limit — accepting %dW (residual %dW)",
			data.LoadPower, residual)
	}
	i.lastGoodLoadW = data.LoadPower
	i.hasLastGoodLoad = true
	i.loadRejectStreak = 0
}

func (i *Inverter) readAllOnce() (InverterData, error) {
	chunk1, err := i.inv.Read(3, 125)
	if err != nil {
		return InverterData{}, fmt.Errorf("read regs 3-127: %w", err)
	}
	chunk2, err := i.inv.Read(128, 125)
	if err != nil {
		return InverterData{}, fmt.Errorf("read regs 128-252: %w", err)
	}
	for k, v := range chunk2 {
		chunk1[k] = v
	}
	return ParseRegisters(chunk1), nil
}

// isFrameDesync detects the known pattern where the logger pushes an async
// data-upload frame ahead of the modbus reply. The library's control-code
// validator catches these as "frame unmarshal failed: expected 0x1510, got: 0xNNNN".
func isFrameDesync(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "expected 0x1510")
}

// Close closes the inverter connection.
func (i *Inverter) Close() {
	i.inv.Close()
}

// Reconnect closes and re-establishes the connection.
func (i *Inverter) Reconnect() {
	i.inv.Close()
	addr := fmt.Sprintf("%s:%d", i.cfg.Host, i.cfg.Port)
	i.inv = solarman.Init(addr, i.cfg.LoggerSN, i.cfg.Timeout)
}

// LinkStats summarises the last 24 hours of reads.
func (i *Inverter) LinkStats() linkstat.Stats { return i.link.Snapshot(time.Now()) }

func (i *Inverter) record(start time.Time, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	i.link.Record(start, err == nil, time.Since(start), msg)
}
