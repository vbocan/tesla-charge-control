package tesla

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The wall connector's lifetime total is read from /api/1/lifetime and joins the status.
func TestLifetimeTotalComesFromTheWallConnector(t *testing.T) {
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/1/lifetime" || fail.Load() {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"contactor_cycles":511,"charge_starts":511,"energy_wh":1758789,"uptime_s":17007136}`))
	}))
	defer srv.Close()

	twc := NewTWCClient(srv.URL)
	if _, ok := twc.LifetimeWh(); ok {
		t.Error("a lifetime total before it was read")
	}
	if err := twc.PollLifetime(); err != nil {
		t.Fatal(err)
	}
	if wh, ok := twc.LifetimeWh(); !ok || wh != 1758789 {
		t.Errorf("lifetime = %.0f (ok %v), want 1758789", wh, ok)
	}

	// A failing poll keeps the last value instead of blanking the total.
	fail.Store(true)
	if err := twc.PollLifetime(); err == nil {
		t.Error("a 500 was not reported")
	}
	if wh, ok := twc.LifetimeWh(); !ok || wh != 1758789 {
		t.Errorf("lifetime after a failed poll = %.0f (ok %v), want the old value", wh, ok)
	}

	c, _ := newTestCharger(t, ModeMax)
	twc.vitals, twc.online = &TWCVitals{VehicleConnected: true}, true
	c.twc = twc
	if got := c.GetStatus().TWC.LifetimeWh; got != 1758789 {
		t.Errorf("status lifetime = %.0f, want 1758789", got)
	}
}

func TestLifetimeIsZeroInTheStatusUntilKnown(t *testing.T) {
	c, _ := newTestCharger(t, ModeMax)
	c.twc = &TWCClient{online: true, vitals: &TWCVitals{}}
	if got := c.GetStatus().TWC.LifetimeWh; got != 0 {
		t.Errorf("status lifetime = %.0f, want 0 (unknown)", got)
	}
}
