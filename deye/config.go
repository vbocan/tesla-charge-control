package deye

import (
	"fmt"
	"math"
	"os"

	"gopkg.in/yaml.v3"
)

type ConnectionConfig struct {
	Host     string `yaml:"host"     json:"host"`
	Port     int    `yaml:"port"     json:"port"`
	LoggerSN uint32 `yaml:"logger_sn" json:"logger_sn"`
	Timeout  int    `yaml:"timeout"  json:"timeout"`
}

type BatteryConfig struct {
	CapacityKWh           float64 `yaml:"capacity_kwh"             json:"capacity_kwh"`
	MinSOC                int     `yaml:"min_soc"                  json:"min_soc"`
	RoundTripEff          float64 `yaml:"round_trip_eff"           json:"round_trip_eff"`
	NominalVoltage        float64 `yaml:"nominal_voltage"          json:"nominal_voltage"`
	ChargeCurrentLimit    float64 `yaml:"charge_current_limit"     json:"charge_current_limit"`
	DischargeCurrentLimit float64 `yaml:"discharge_current_limit"  json:"discharge_current_limit"`
}

// MaxChargeW returns the maximum charge power in watts.
func (b BatteryConfig) MaxChargeW() int {
	return int(b.NominalVoltage * b.ChargeCurrentLimit)
}

// MaxDischargeW returns the maximum discharge power in watts.
func (b BatteryConfig) MaxDischargeW() int {
	return int(b.NominalVoltage * b.DischargeCurrentLimit)
}

// OneWayEff returns the one-way (single direction) efficiency.
func (b BatteryConfig) OneWayEff() float64 {
	return math.Sqrt(b.RoundTripEff)
}

type SolarConfig struct {
	TotalCapacityW int `yaml:"total_capacity_w" json:"total_capacity_w"` // Total installed PV capacity in watts
	StringCount    int `yaml:"string_count"     json:"string_count"`     // Number of PV strings (typically 2)
}

// PerStringW returns max watts per string (evenly divided).
func (s SolarConfig) PerStringW() int {
	if s.StringCount < 1 {
		return s.TotalCapacityW
	}
	return s.TotalCapacityW / s.StringCount
}

type ProjectionConfig struct {
	WindowS int `yaml:"window_s" json:"window_s"`
}

type LocationConfig struct {
	Latitude  float64 `yaml:"latitude"  json:"latitude"`
	Longitude float64 `yaml:"longitude" json:"longitude"`
}

type TeslaConfig struct {
	Enabled             bool   `yaml:"enabled"                  json:"enabled"`
	VIN                 string `yaml:"vin"                      json:"vin"`
	KeyFile             string `yaml:"key_file"                 json:"key_file"`
	MaxAmps             int    `yaml:"max_amps"                 json:"max_amps"`
	GridVoltage         int    `yaml:"grid_voltage"             json:"grid_voltage"`
	GracePeriodS        int    `yaml:"grace_period_s"           json:"grace_period_s"`
	SmoothingWindowS    int    `yaml:"smoothing_window_s"       json:"smoothing_window_s"`
	StartDelayS         int    `yaml:"start_delay_s"            json:"start_delay_s"`
	CooldownS           int    `yaml:"cooldown_s"               json:"cooldown_s"`
	StandbyPVThresholdW int    `yaml:"standby_pv_threshold_w"   json:"standby_pv_threshold_w"`
	DryRun              bool   `yaml:"dry_run"                  json:"dry_run"`
	StateFile           string `yaml:"state_file"               json:"state_file"`
	LogRetainDays       int    `yaml:"log_retain_days"          json:"log_retain_days"`
	TWCURL              string `yaml:"twc_url"                  json:"twc_url"`
	SoCPollChargingS    int    `yaml:"soc_poll_charging_s"      json:"soc_poll_charging_s"`  // SoC BLE read cadence while charging
	SoCPollConnectedS   int    `yaml:"soc_poll_connected_s"     json:"soc_poll_connected_s"` // SoC BLE read cadence while plugged in but idle
	LoadMarginW         int    `yaml:"load_margin_w"            json:"load_margin_w"`        // headroom kept below max_output_w when sizing the car's draw

	GridOutagePause    bool `yaml:"grid_outage_pause"     json:"grid_outage_pause"`     // stop and block car charging while the grid is down
	GridRestoreDelayS  int  `yaml:"grid_restore_delay_s"  json:"grid_restore_delay_s"`  // grid must be stable this long before charging resumes
	InverterStaleStopS int  `yaml:"inverter_stale_stop_s" json:"inverter_stale_stop_s"` // stop the car if no inverter data for this long (0 = never)
	IdleLoadCap        bool `yaml:"idle_load_cap"         json:"idle_load_cap"`         // in Idle mode, keep a charge the car started itself under the load limit
}

type Config struct {
	Connection    ConnectionConfig `yaml:"connection"      json:"connection"`
	Battery       BatteryConfig    `yaml:"battery"         json:"battery"`
	Solar         SolarConfig      `yaml:"solar"           json:"solar"`
	Projection    ProjectionConfig `yaml:"projection"      json:"projection"`
	Location      LocationConfig   `yaml:"location"        json:"location"`
	PollIntervalS int              `yaml:"poll_interval_s" json:"poll_interval_s"`
	MaxOutputW    int              `yaml:"max_output_w"    json:"max_output_w"`
	AuthToken     string           `yaml:"auth_token"      json:"-"`
	Domain        string           `yaml:"domain"          json:"-"`
	Tesla         TeslaConfig      `yaml:"tesla"           json:"tesla"`

	// Remote access through a Cloudflare Tunnel gated by Cloudflare Access.
	// Requests that arrive through a proxy must then carry a valid Access token,
	// which the app verifies itself. See docs/deployment.md.
	CloudflareTunnel bool   `yaml:"cloudflare_tunnel"  json:"-"` // deploy a cloudflared service beside the app
	AccessTeamDomain string `yaml:"access_team_domain" json:"-"` // e.g. "yourteam.cloudflareaccess.com"
	AccessAUD        string `yaml:"access_aud"         json:"-"` // the Access application's Audience (AUD) tag
}

// DefaultLoadMarginW is the headroom kept below max_output_w when load_margin_w
// is not set in the config file. Setting it to 0 means no margin.
const DefaultLoadMarginW = 400

// Defaults for the grid-outage and inverter-data protections. Like the margin
// they are applied before parsing, so an explicit false or 0 in the file works.
const (
	DefaultGridRestoreDelayS  = 300 // grid stable for 5 minutes before charging resumes
	DefaultInverterStaleStopS = 120 // stop the car after 2 minutes without inverter data
)

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	// Defaults that must tell "key absent" from "explicitly 0" are set before
	// parsing: the YAML decoder leaves a field alone when its key is missing.
	cfg.Tesla.LoadMarginW = DefaultLoadMarginW
	cfg.Tesla.GridOutagePause = true
	cfg.Tesla.GridRestoreDelayS = DefaultGridRestoreDelayS
	cfg.Tesla.InverterStaleStopS = DefaultInverterStaleStopS
	cfg.Tesla.IdleLoadCap = true
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	// Apply Tesla config defaults
	if cfg.Tesla.GracePeriodS == 0 {
		cfg.Tesla.GracePeriodS = 240
	}
	if cfg.Tesla.SmoothingWindowS == 0 {
		cfg.Tesla.SmoothingWindowS = 90
	}
	if cfg.Tesla.StartDelayS == 0 {
		cfg.Tesla.StartDelayS = 120
	}
	if cfg.Tesla.CooldownS == 0 {
		cfg.Tesla.CooldownS = 180
	}
	if cfg.Tesla.StandbyPVThresholdW == 0 {
		cfg.Tesla.StandbyPVThresholdW = 200
	}
	if cfg.Tesla.LogRetainDays == 0 {
		cfg.Tesla.LogRetainDays = 90
	}
	if cfg.Tesla.SoCPollChargingS == 0 {
		cfg.Tesla.SoCPollChargingS = 60
	}
	if cfg.Tesla.SoCPollConnectedS == 0 {
		cfg.Tesla.SoCPollConnectedS = 1200
	}
	return cfg, nil
}
