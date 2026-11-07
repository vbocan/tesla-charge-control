package deye

import (
	"time"
)

type sample struct {
	ts    float64 // monotonic seconds
	power int     // watts
}

// BatteryProjection tracks a rolling window of battery power samples
// for charge/discharge time projection.
type BatteryProjection struct {
	windowS       int
	samples       []sample
	lastDirection string // "charging" or "discharging"
	Cfg           BatteryConfig
	epoch         time.Time
}

func NewBatteryProjection(windowS int, cfg BatteryConfig) *BatteryProjection {
	return &BatteryProjection{
		windowS: windowS,
		Cfg:     cfg,
		epoch:   time.Now(),
	}
}

func (bp *BatteryProjection) mono() float64 {
	return time.Since(bp.epoch).Seconds()
}

func (bp *BatteryProjection) Add(powerW int) {
	var direction string
	if powerW > 10 {
		direction = "discharging"
	} else if powerW < -10 {
		direction = "charging"
	}

	if direction != "" && bp.lastDirection != "" && direction != bp.lastDirection {
		bp.samples = bp.samples[:0]
	}
	if direction != "" {
		bp.lastDirection = direction
	}

	now := bp.mono()
	bp.samples = append(bp.samples, sample{ts: now, power: powerW})

	cutoff := now - float64(bp.windowS)
	i := 0
	for i < len(bp.samples) && bp.samples[i].ts < cutoff {
		i++
	}
	if i > 0 {
		bp.samples = bp.samples[i:]
	}
}

// AvgPower returns the average battery power (W) over the window, or ok=false if insufficient data.
func (bp *BatteryProjection) AvgPower() (float64, bool) {
	if len(bp.samples) < 2 {
		return 0, false
	}
	span := bp.samples[len(bp.samples)-1].ts - bp.samples[0].ts
	if span < 30 {
		return 0, false
	}
	sum := 0.0
	for _, s := range bp.samples {
		sum += float64(s.power)
	}
	return sum / float64(len(bp.samples)), true
}

// Projection returns the battery state and projection.
//
//	state: "charging", "discharging", "idle", or "collecting"
//	remainingKWh: usable kWh remaining (above floor) or kWh to full
//	hours: estimated hours to full/empty, or -1 if not applicable
func (bp *BatteryProjection) Projection(soc int) (state string, remainingKWh float64, hours float64) {
	avg, ok := bp.AvgPower()
	if !ok {
		return "collecting", 0, -1
	}

	usableKWh := float64(soc-bp.Cfg.MinSOC) / 100 * bp.Cfg.CapacityKWh
	toFullKWh := float64(100-soc) / 100 * bp.Cfg.CapacityKWh
	oneWay := bp.Cfg.OneWayEff()

	if avg > 10 { // discharging
		if usableKWh <= 0 {
			return "discharging", 0, 0
		}
		h := usableKWh * oneWay / (avg / 1000)
		return "discharging", usableKWh, h
	} else if avg < -10 { // charging
		if toFullKWh <= 0 {
			return "charging", 0, 0
		}
		h := toFullKWh / ((-avg) / 1000 * oneWay)
		return "charging", toFullKWh, h
	}

	if usableKWh < 0 {
		usableKWh = 0
	}
	return "idle", usableKWh, -1
}

// WindowFill returns a string showing how full the averaging window is.
func (bp *BatteryProjection) WindowFill() string {
	if len(bp.samples) == 0 {
		return "0s"
	}
	span := bp.samples[len(bp.samples)-1].ts - bp.samples[0].ts
	if span >= float64(bp.windowS-10) {
		return itoa(bp.windowS/60) + "m"
	}
	return itoa(int(span)) + "s/" + itoa(bp.windowS/60) + "m"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	if neg {
		s = "-" + s
	}
	return s
}
