package main

// Demo mode (-demo): the whole app runs against a simulated house, so the
// dashboard can be tried, screenshotted and developed on without a Deye
// inverter, a Wall Connector or a Tesla.
//
// Only the edges are simulated. The charger, its state machine, the load cap,
// the outage protection, the store and the wall-connector client are the real
// code: the car is "commanded" in dry-run mode, a fake Wall Connector on
// 127.0.0.1 reports what the simulated car draws in response, and the simulated
// inverter balances PV, house, car and battery around it. The loop is closed,
// so the charger reacts to the car it controls just as it does at home.
//
// The scene is a sunny early afternoon (the simulated clock starts at 12:40 and
// runs in real time) with passing clouds and household appliances switching on
// and off. The home battery is nearly full, so Solar mode soon finds a surplus
// and starts the car.

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vbocan/tesla-charge-control/deye"
	"github.com/vbocan/tesla-charge-control/linkstat"
	"github.com/vbocan/tesla-charge-control/tesla"
)

// Scene parameters.
const (
	demoStartHour   = 12 + 40.0/60    // simulated time of day when the demo starts
	demoPVPeakW     = 7400.0          // clear-sky peak of the 8 kWp array
	demoSunrise     = 6.4             // hours
	demoSunset      = 19.8            // hours
	demoBattKWh     = 10.0            // home battery
	demoBattMaxW    = 5000.0          // battery charge/discharge limit
	demoBattStart   = 93.5            // home battery SoC at start, %
	demoCarKWh      = 75.0            // car battery
	demoCarStart    = 58.0            // car SoC at start, %
	demoCarLimit    = 80              // car charge limit, %
	demoHouseBaseW  = 420.0           // always-on house load
	demoSelfUseW    = 48.0            // inverter's own draw
	demoLifetimeKWh = 2016.4          // wall connector lifetime total at start
	demoOpening     = 7 * time.Minute // scripted start (see step), then random weather and appliances
)

type appliance struct {
	name string
	w    float64
	dur  time.Duration
}

var demoAppliances = []appliance{
	{"kettle", 2100, 2 * time.Minute},
	{"oven", 2400, 6 * time.Minute},
	{"dishwasher", 1900, 4 * time.Minute},
	{"washing machine", 2000, 3 * time.Minute},
	{"microwave", 1150, 2 * time.Minute},
	{"hair dryer", 1600, 90 * time.Second},
}

// demoSim is the simulated house. step advances it once a second; the inverter
// source and the fake Wall Connector read snapshots of it.
type demoSim struct {
	mu      sync.Mutex
	rng     *rand.Rand
	charger *tesla.Charger
	link    *linkstat.Tracker
	started time.Time

	cloud, cloudTarget float64 // PV attenuation, 1 = clear sky
	nextCloud          time.Time
	appliance          *appliance
	applianceUntil     time.Time
	nextAppliance      time.Time

	battSoC, carSoC float64
	carA            float64 // what the car actually draws, A
	carSetA         int     // the car's own current setting
	sessionStart    time.Time
	sessionWh       float64
	lifetimeWh      float64

	// Last computed flows, W (battery: + discharging; grid: + importing).
	pv1, pv2, house, battW, gridW, gridV, gridHz float64

	// Energy counters, kWh.
	dayProd, dayBought, daySold, dayBattChg, dayBattDis float64
	totProd, totSold, totBattChg, totBattDis            float64
	lastDay                                             int
}

// newDemoServer builds a server around the simulated house.
func newDemoServer() *server {
	dir, err := os.MkdirTemp("", "tesla-charge-control-demo-")
	if err != nil {
		log.Fatalf("Demo: %v", err)
	}
	// Start in Solar mode: the charger loads its mode from the state file.
	os.WriteFile(filepath.Join(dir, "tesla.json"), []byte(`{"mode":"solar"}`), 0o644)

	sim := &demoSim{
		rng:           rand.New(rand.NewPCG(2026, 11)),
		link:          linkstat.New(),
		started:       time.Now(),
		cloud:         1,
		cloudTarget:   1,
		nextCloud:     time.Now().Add(demoOpening + time.Minute),
		nextAppliance: time.Now().Add(demoOpening + 2*time.Minute),
		battSoC:       demoBattStart,
		carSoC:        demoCarStart,
		carSetA:       16,
		lifetimeWh:    demoLifetimeKWh * 1000,
		dayProd:       24.6, dayBought: 1.9, daySold: 6.8, dayBattChg: 8.1, dayBattDis: 1.4,
		totProd: 18432.6, totSold: 6120.3, totBattChg: 4210.7, totBattDis: 3905.2,
		gridV: 231, gridHz: 50,
	}
	sim.lastDay = sim.dayIndex(time.Now())
	sim.step(0)

	twcURL := sim.serveWallConnector()
	cfg := demoConfig(dir, twcURL)

	log.Printf("DEMO MODE: simulated inverter, wall connector and car; nothing real is contacted")
	s := buildServer(cfg, sim, filepath.Join(dir, "tesla.db"), func(c *tesla.Charger, twc *tesla.TWCClient) {
		sim.charger = c
		c.SetDryRunVehicle(sim.vehicle)
		if twc != nil {
			seedLinkHistory(twc.LinkTracker(), 3*time.Second, 35, 70, 0.002,
				"Get \"http://192.168.1.60/api/1/vitals\": context deadline exceeded (Client.Timeout exceeded while awaiting headers)")
		}
	})
	seedLinkHistory(sim.link, 5*time.Second, 620, 940, 0.004,
		"read regs 3-127: ERROR: Read.do [1234567890] request failed: read tcp 192.168.1.50:8899: i/o timeout")
	seedChargeLog(s.store)

	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for range t.C {
			sim.step(1)
		}
	}()
	return s
}

func demoConfig(dir, twcURL string) deye.Config {
	return deye.Config{
		Connection:    deye.ConnectionConfig{Host: "demo", Port: 8899, LoggerSN: 1234567890, Timeout: 10},
		Battery:       deye.BatteryConfig{CapacityKWh: demoBattKWh, MinSOC: 25, RoundTripEff: 0.94, NominalVoltage: 52, ChargeCurrentLimit: 100, DischargeCurrentLimit: 100},
		Solar:         deye.SolarConfig{TotalCapacityW: 8000, StringCount: 2},
		Projection:    deye.ProjectionConfig{WindowS: 300},
		Location:      deye.LocationConfig{Latitude: 48.14, Longitude: 11.58},
		PollIntervalS: 3,
		MaxOutputW:    6000,
		Tesla: deye.TeslaConfig{
			Enabled:             true,
			VIN:                 "DEMO0000000000000",
			KeyFile:             filepath.Join(dir, "tesla-key.pem"),
			MaxAmps:             25,
			GridVoltage:         230,
			GracePeriodS:        60,
			SmoothingWindowS:    30,
			StartDelayS:         20,
			CooldownS:           60,
			StandbyPVThresholdW: 200,
			DryRun:              true,
			StateFile:           filepath.Join(dir, "tesla.json"),
			LogRetainDays:       90,
			TWCURL:              twcURL,
			SoCPollChargingS:    15,
			SoCPollConnectedS:   60,
			LoadMarginW:         0,
			GridOutagePause:     true,
			GridRestoreDelayS:   60,
			InverterStaleStopS:  120,
			IdleLoadCap:         true,
		},
	}
}

// hour is the simulated time of day.
func (d *demoSim) hour(now time.Time) float64 {
	return math.Mod(demoStartHour+now.Sub(d.started).Hours(), 24)
}

func (d *demoSim) dayIndex(now time.Time) int {
	return int((demoStartHour + now.Sub(d.started).Hours()) / 24)
}

// step advances the simulation by dt seconds.
func (d *demoSim) step(dt float64) {
	// Read the charger before taking our own lock: Evaluate holds the charger's
	// lock while it polls our wall connector, so never wait on it under d.mu.
	commanded := 0
	if d.charger != nil {
		commanded = d.charger.GetStatus().CurrentAmps
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	r := d.rng

	if day := d.dayIndex(now); day != d.lastDay {
		d.lastDay = day
		d.dayProd, d.dayBought, d.daySold, d.dayBattChg, d.dayBattDis = 0, 0, 0, 0, 0
	}

	if el := now.Sub(d.started); el < demoOpening {
		// A scripted opening, so the first minutes always tell the same story:
		// the car starts, a cloud passes, the kettle boils, then clear sky.
		d.cloudTarget = 1
		if el >= 70*time.Second && el < 110*time.Second {
			d.cloudTarget = 0.72
		}
		d.appliance = nil
		if el >= 150*time.Second && el < 210*time.Second {
			d.appliance = &demoAppliances[0]
		}
	} else {
		// Clouds: mostly clear, now and then one drifts across the sun.
		if now.After(d.nextCloud) {
			if d.cloudTarget < 1 || r.Float64() < 0.6 {
				d.cloudTarget = 1
				d.nextCloud = now.Add(time.Duration(60+r.IntN(120)) * time.Second)
			} else {
				d.cloudTarget = 0.3 + 0.4*r.Float64()
				d.nextCloud = now.Add(time.Duration(25+r.IntN(50)) * time.Second)
			}
		}
		// Appliances: one at a time, every few minutes.
		if d.appliance != nil && now.After(d.applianceUntil) {
			d.appliance = nil
			d.nextAppliance = now.Add(time.Duration(120+r.IntN(240)) * time.Second)
		}
		if d.appliance == nil && now.After(d.nextAppliance) {
			a := demoAppliances[r.IntN(len(demoAppliances))]
			d.appliance = &a
			d.applianceUntil = now.Add(a.dur)
		}
	}
	d.cloud += (d.cloudTarget - d.cloud) * math.Min(1, dt/6)

	// PV: a clear-sky bell between sunrise and sunset, times the cloud cover.
	h := d.hour(now)
	clear := 0.0
	if h > demoSunrise && h < demoSunset {
		clear = demoPVPeakW * math.Pow(math.Sin(math.Pi*(h-demoSunrise)/(demoSunset-demoSunrise)), 1.25)
	}
	pv := clear * d.cloud * (1 + 0.01*r.NormFloat64())
	pv = math.Max(0, pv)
	d.pv1, d.pv2 = pv*0.53, pv*0.47

	d.house = demoHouseBaseW + 25*r.NormFloat64()
	if d.appliance != nil {
		d.house += d.appliance.w
	}

	// The car follows what the charger commanded (in dry run nothing is sent,
	// so this is the car's reply), ramping a couple of amps a second.
	target := 0.0
	if commanded > 0 && d.carSoC < demoCarLimit {
		target = float64(commanded)
		d.carSetA = commanded
	}
	if diff := target - d.carA; math.Abs(diff) <= 2*dt || dt == 0 {
		d.carA = target
	} else {
		d.carA += math.Copysign(2*dt, diff)
	}
	if d.carA > 0 && d.sessionStart.IsZero() {
		d.sessionStart = now
	}

	// Mains: rises a little while exporting, sags while importing.
	d.gridHz = 50 + 0.012*r.NormFloat64()
	carV := d.gridV - 1.5
	carW := d.carA * carV
	load := d.house + carW
	net := pv - load - demoSelfUseW

	// The battery charges first (what the Deye does), tapering above 90 %.
	battW, gridW := 0.0, 0.0
	if net >= 0 {
		accept := demoBattMaxW
		if d.battSoC >= 90 {
			accept = math.Max(150, demoBattMaxW*(100-d.battSoC)/10)
		}
		if d.battSoC >= 100 {
			accept = 0
		}
		chg := math.Min(net, accept)
		battW = -chg
		gridW = -(net - chg)
	} else {
		dis := 0.0
		if d.battSoC > 25 {
			dis = math.Min(-net, demoBattMaxW)
		}
		battW = dis
		gridW = -net - dis
	}
	d.battW, d.gridW = battW, gridW
	d.gridV = 230.6 + 0.0005*math.Max(0, -gridW) - 0.0009*math.Max(0, gridW) + 0.3*r.NormFloat64()

	// Integrate.
	hrs := dt / 3600
	eff := math.Sqrt(0.94)
	if battW < 0 {
		d.battSoC += -battW * eff * hrs / (demoBattKWh * 1000) * 100
		d.dayBattChg += -battW * hrs / 1000
		d.totBattChg += -battW * hrs / 1000
	} else {
		d.battSoC -= battW / eff * hrs / (demoBattKWh * 1000) * 100
		d.dayBattDis += battW * hrs / 1000
		d.totBattDis += battW * hrs / 1000
	}
	d.battSoC = math.Max(0, math.Min(100, d.battSoC))
	if gridW > 0 {
		d.dayBought += gridW * hrs / 1000
	} else {
		d.daySold += -gridW * hrs / 1000
		d.totSold += -gridW * hrs / 1000
	}
	d.dayProd += pv * hrs / 1000
	d.totProd += pv * hrs / 1000
	d.carSoC += carW * 0.92 * hrs / (demoCarKWh * 1000) * 100
	d.sessionWh += carW * hrs
	d.lifetimeWh += carW * hrs
}

// ReadAll implements inverterSource: the simulated inverter's registers.
func (d *demoSim) ReadAll() (deye.InverterData, error) {
	start := time.Now()
	d.mu.Lock()
	r := d.rng
	pv1, pv2 := int(d.pv1), int(d.pv2)
	v1, v2 := 0.0, 0.0
	if pv1 > 5 {
		v1 = 372 + 18*math.Min(1, float64(pv1)/3000) + r.NormFloat64()
	}
	if pv2 > 5 {
		v2 = 361 + 17*math.Min(1, float64(pv2)/3000) + r.NormFloat64()
	}
	amps := func(w int, v float64) float64 {
		if v == 0 {
			return 0
		}
		return math.Round(float64(w)/v*10) / 10
	}
	battV := 50.4 + 0.037*d.battSoC - 0.0004*d.battW
	carW := d.carA * (d.gridV - 1.5)
	load := int(d.house + carW)
	inv := int(d.pv1 + d.pv2 + d.battW - demoSelfUseW)
	data := deye.InverterData{
		PV1V: round1(v1), PV1A: amps(pv1, v1), PV1W: pv1,
		PV2V: round1(v2), PV2A: amps(pv2, v2), PV2W: pv2,
		TotalPV: pv1 + pv2,

		GridVoltage: round1(d.gridV),
		GridFreq:    math.Round(d.gridHz*100) / 100,
		GridPower:   int(d.gridW),
		CTL1:        int(d.gridW) - 12,
		ExtCTL1:     int(d.gridW),

		InvVoltage: round1(d.gridV + 0.2),
		InvFreq:    math.Round(d.gridHz*100) / 100,
		InvL1Power: inv,
		InvTotal:   inv,
		InvCurrent: math.Round(float64(inv)/d.gridV*100) / 100,

		BattSOC:            int(d.battSoC),
		BattVoltage:        math.Round(battV*100) / 100,
		BattCurrent:        math.Round(d.battW/battV*100) / 100,
		BattPower:          int(d.battW),
		BattTemp:           24.5 + 0.0004*math.Abs(d.battW),
		DailyBattCharge:    round1(d.dayBattChg),
		DailyBattDischarge: round1(d.dayBattDis),
		TotalBattCharge:    round1(d.totBattChg),
		TotalBattDischarge: round1(d.totBattDis),

		LoadPower:       load,
		SelfConsumption: int(demoSelfUseW),

		DailyProduction:  round1(d.dayProd),
		TotalProduction:  round1(d.totProd),
		DailyBought:      round1(d.dayBought),
		DailySold:        round1(d.daySold),
		DailyConsumption: round1(d.dayProd + d.dayBought - d.daySold - d.dayBattChg + d.dayBattDis),
		TotalSold:        round1(d.totSold),

		DCTemp: round1(36 + (d.pv1+d.pv2)/900),
		ACTemp: round1(33 + float64(load)/1200),
	}
	latency := time.Duration(640+r.IntN(260)) * time.Millisecond
	d.mu.Unlock()

	d.link.Record(start, true, latency, "")
	return data, nil
}

// Reconnect implements inverterSource; there is nothing to reconnect.
func (d *demoSim) Reconnect() {}

// LinkStats implements inverterSource.
func (d *demoSim) LinkStats() linkstat.Stats { return d.link.Snapshot(time.Now()) }

// vehicle is the simulated car's charge state, read "over BLE" in dry run.
func (d *demoSim) vehicle() *tesla.VehicleChargeState {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := "Stopped"
	mins := 0
	switch {
	case d.carSoC >= demoCarLimit:
		state = "Complete"
	case d.carA > 0.5:
		state = "Charging"
		kw := d.carA * d.gridV / 1000 * 0.92
		mins = int((demoCarLimit - d.carSoC) / 100 * demoCarKWh / kw * 60)
	}
	return &tesla.VehicleChargeState{
		BatteryLevel:       int(d.carSoC),
		UsableBatteryLevel: int(d.carSoC) - 1,
		ChargeLimitSoc:     demoCarLimit,
		ChargingState:      state,
		MinutesToFull:      mins,
		ChargeEnergyAdded:  math.Round(d.sessionWh*0.92/100) / 10,
		BatteryRangeKm:     math.Round(d.carSoC * 5.1),
		ChargeCurrentA:     d.carSetA,
	}
}

// serveWallConnector starts a fake Tesla Wall Connector Gen 3 on 127.0.0.1
// (the two endpoints the app reads) and returns its base URL.
func (d *demoSim) serveWallConnector() string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("Demo wall connector: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/1/vitals", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		charging := d.carA > 0.5
		v := tesla.TWCVitals{
			ContactorClosed:  charging,
			VehicleConnected: true,
			GridV:            round1(d.gridV - 1.5),
			GridHz:           math.Round(d.gridHz*1000) / 1000,
			VehicleCurrentA:  round1(d.carA),
			CurrentAA:        round1(d.carA),
			VoltageAV:        round1(d.gridV - 1.5),
			PCBATemp:         round1(29 + 0.3*d.carA),
			HandleTemp:       round1(19 + 0.25*d.carA),
			MCUTemp:          round1(33 + 0.2*d.carA),
			UptimeS:          int(time.Since(d.started).Seconds()) + 864000,
			SessionEnergyWh:  math.Round(d.sessionWh),
			EVSEState:        4, // connected, not charging
		}
		if charging {
			v.EVSEState = 11
		}
		if !d.sessionStart.IsZero() {
			v.SessionS = int(time.Since(d.sessionStart).Seconds())
		}
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("/api/1/lifetime", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		wh := math.Round(d.lifetimeWh)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"energy_wh":%.0f}`, wh)
	})
	go http.Serve(ln, mux)
	return "http://" + ln.Addr().String()
}

// seedLinkHistory fills a link tracker with a plausible last 24 hours: polls
// every interval, latencies in [minMs, maxMs), and a sprinkle of failures.
func seedLinkHistory(t *linkstat.Tracker, interval time.Duration, minMs, maxMs int, failRate float64, failMsg string) {
	r := rand.New(rand.NewPCG(uint64(interval), 7))
	now := time.Now()
	for at := now.Add(-24 * time.Hour); at.Before(now); at = at.Add(interval) {
		ms := minMs + r.IntN(maxMs-minMs)
		if r.Float64() < failRate {
			t.Record(at, false, 10*time.Second, failMsg)
			continue
		}
		t.Record(at, true, time.Duration(ms)*time.Millisecond, "")
	}
}

// seedChargeLog writes a morning's worth of decisions, so the log page has a
// story to tell before the live ones start arriving.
func seedChargeLog(st *tesla.Store) {
	if st == nil {
		return
	}
	now := time.Now()
	at := func(minAgo int) time.Time { return now.Add(-time.Duration(minAgo) * time.Minute) }
	entries := []struct {
		ago                  int
		source, mode, status string
		amps, availW, powerW int
		reason               string
	}{
		{312, "user", "solar", "waiting", 0, 0, 0, "Evaluating..."},
		{311, "auto", "solar", "waiting", 0, 0, 0, "PV 142W — standby (need 200W)"},
		{268, "auto", "solar", "waiting", 0, 0, 0, "No surplus — need 1200W to start"},
		{196, "auto", "solar", "waiting", 0, 940, 0, "Surplus 940W — need 1200W to start"},
		{171, "auto", "solar", "charging", 6, 1460, 1380, "Charging at 6A"},
		{158, "auto", "solar", "charging", 9, 2150, 2070, "Charging at 9A (surplus 2150W)"},
		{141, "auto", "solar", "charging", 7, 1720, 1610, "Reduced to 7A — home load 4410W (limit 6000W)"},
		{133, "auto", "solar", "charging", 11, 2610, 2530, "Charging at 11A (surplus 2610W)"},
		{118, "auto", "solar", "charging", 5, 870, 1150, "Surplus 870W — grace period 240s"},
		{116, "auto", "solar", "charging", 10, 2380, 2300, "Surplus recovered — charging at 10A"},
		{97, "auto", "solar", "charging", 14, 3270, 3220, "Charging at 14A (surplus 3270W)"},
		{64, "auto", "solar", "waiting", 0, 610, 0, "Surplus too low for 240s — stopped, cooldown 180s"},
		{61, "auto", "solar", "waiting", 0, 1890, 0, "Cooldown — restart in 2s"},
		{58, "auto", "solar", "charging", 8, 1950, 1840, "Charging at 8A"},
		{41, "user", "max", "waiting", 0, 0, 0, "Evaluating..."},
		{40, "auto", "max", "charging", 22, 0, 5060, "Charging at 22A (load limit 6000W)"},
		{23, "user", "solar", "waiting", 0, 0, 0, "Evaluating..."},
		{22, "auto", "solar", "charging", 12, 2840, 2760, "Charging at 12A (surplus 2840W)"},
	}
	for _, e := range entries {
		st.LogDecisionAt(at(e.ago), e.source, e.mode, e.status, e.amps, e.availW, e.powerW, e.reason)
	}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
