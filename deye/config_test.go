package deye

import (
	"os"
	"path/filepath"
	"testing"
)

func loadTestConfig(t *testing.T, yaml string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestLoadMarginDefaultsWhenOmitted(t *testing.T) {
	cfg := loadTestConfig(t, "max_output_w: 6000\ntesla:\n  enabled: true\n")
	if cfg.Tesla.LoadMarginW != DefaultLoadMarginW {
		t.Errorf("load_margin_w = %d, want the default %d", cfg.Tesla.LoadMarginW, DefaultLoadMarginW)
	}
}

func TestLoadMarginDefaultsWithoutATeslaSection(t *testing.T) {
	cfg := loadTestConfig(t, "max_output_w: 6000\n")
	if cfg.Tesla.LoadMarginW != DefaultLoadMarginW {
		t.Errorf("load_margin_w = %d, want the default %d", cfg.Tesla.LoadMarginW, DefaultLoadMarginW)
	}
}

// 0 is a real setting (no margin), not "unset".
func TestLoadMarginExplicitZeroMeansNoMargin(t *testing.T) {
	cfg := loadTestConfig(t, "max_output_w: 6000\ntesla:\n  enabled: true\n  load_margin_w: 0\n")
	if cfg.Tesla.LoadMarginW != 0 {
		t.Errorf("load_margin_w = %d, want 0", cfg.Tesla.LoadMarginW)
	}
}

func TestLoadMarginExplicitValue(t *testing.T) {
	cfg := loadTestConfig(t, "max_output_w: 6000\ntesla:\n  enabled: true\n  load_margin_w: 250\n")
	if cfg.Tesla.LoadMarginW != 250 {
		t.Errorf("load_margin_w = %d, want 250", cfg.Tesla.LoadMarginW)
	}
}

// The shipped config.yaml must parse, and keep the other defaults working.
func TestShippedConfigParses(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join("..", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxOutputW != 6000 || cfg.Tesla.LoadMarginW != 0 {
		t.Errorf("max_output_w=%d load_margin_w=%d, want 6000 and 0", cfg.MaxOutputW, cfg.Tesla.LoadMarginW)
	}
	if cfg.Tesla.GracePeriodS == 0 || cfg.Tesla.SoCPollChargingS == 0 {
		t.Error("other Tesla defaults were lost")
	}
	if !cfg.Tesla.GridOutagePause || cfg.Tesla.GridRestoreDelayS != 300 || cfg.Tesla.InverterStaleStopS != 120 {
		t.Errorf("outage protection = %v / %d s / %d s, want on / 300 / 120",
			cfg.Tesla.GridOutagePause, cfg.Tesla.GridRestoreDelayS, cfg.Tesla.InverterStaleStopS)
	}
}

func TestCloudflareKeysAreOffByDefault(t *testing.T) {
	cfg := loadTestConfig(t, "max_output_w: 6000\n")
	if cfg.CloudflareTunnel || cfg.AccessTeamDomain != "" || cfg.AccessAUD != "" {
		t.Errorf("tunnel=%v team=%q aud=%q, want all unset by default", cfg.CloudflareTunnel, cfg.AccessTeamDomain, cfg.AccessAUD)
	}
}

func TestCloudflareKeysAreRead(t *testing.T) {
	cfg := loadTestConfig(t, "cloudflare_tunnel: true\naccess_team_domain: \"team.cloudflareaccess.com\"\naccess_aud: \"abc123\"\n")
	if !cfg.CloudflareTunnel || cfg.AccessTeamDomain != "team.cloudflareaccess.com" || cfg.AccessAUD != "abc123" {
		t.Errorf("tunnel=%v team=%q aud=%q", cfg.CloudflareTunnel, cfg.AccessTeamDomain, cfg.AccessAUD)
	}
}

func TestOutageProtectionDefaults(t *testing.T) {
	cfg := loadTestConfig(t, "max_output_w: 6000\ntesla:\n  enabled: true\n")
	if !cfg.Tesla.GridOutagePause {
		t.Error("grid_outage_pause should default to on")
	}
	if cfg.Tesla.GridRestoreDelayS != DefaultGridRestoreDelayS || cfg.Tesla.InverterStaleStopS != DefaultInverterStaleStopS {
		t.Errorf("restore delay %d / stale stop %d, want %d / %d", cfg.Tesla.GridRestoreDelayS, cfg.Tesla.InverterStaleStopS,
			DefaultGridRestoreDelayS, DefaultInverterStaleStopS)
	}
	if !cfg.Tesla.IdleLoadCap {
		t.Error("idle_load_cap should default to on")
	}
}

func TestOutageProtectionExplicitValues(t *testing.T) {
	cfg := loadTestConfig(t, "tesla:\n  grid_outage_pause: false\n  grid_restore_delay_s: 0\n  inverter_stale_stop_s: 0\n  idle_load_cap: false\n")
	if cfg.Tesla.GridOutagePause {
		t.Error("an explicit false must switch the outage pause off")
	}
	if cfg.Tesla.IdleLoadCap {
		t.Error("an explicit false must switch the Idle-mode load cap off")
	}
	if cfg.Tesla.GridRestoreDelayS != 0 || cfg.Tesla.InverterStaleStopS != 0 {
		t.Errorf("restore delay %d / stale stop %d, want the explicit zeros kept", cfg.Tesla.GridRestoreDelayS, cfg.Tesla.InverterStaleStopS)
	}
}
