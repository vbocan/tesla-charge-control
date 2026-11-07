package linkstat

import (
	"testing"
	"time"
)

func TestSnapshot(t *testing.T) {
	tr := New()
	now := time.Unix(1_800_000_000, 0)

	tr.Record(now.Add(-30*time.Hour), true, 100*time.Millisecond, "") // outside the window
	tr.Record(now.Add(-90*time.Minute), true, 200*time.Millisecond, "")
	tr.Record(now.Add(-10*time.Minute), true, 400*time.Millisecond, "")
	tr.Record(now.Add(-5*time.Minute), false, time.Second, "timeout")
	tr.Record(now.Add(-4*time.Minute), false, time.Second, "timeout")
	tr.Record(now.Add(-time.Minute), true, 300*time.Millisecond, "")

	s := tr.Snapshot(now)
	if s.OK != 3 || s.Fail != 2 || s.Total != 5 {
		t.Fatalf("counts = ok %d fail %d total %d, want 3/2/5", s.OK, s.Fail, s.Total)
	}
	if s.Percent != 60 {
		t.Errorf("percent = %v, want 60", s.Percent)
	}
	if s.AvgMs != 300 || s.MaxMs != 400 {
		t.Errorf("avg/max = %d/%d, want 300/400", s.AvgMs, s.MaxMs)
	}
	if !s.StreakOK || s.StreakN != 1 {
		t.Errorf("streak = ok %v n %d, want ok 1", s.StreakOK, s.StreakN)
	}
	if len(s.TopErrors) != 1 || s.TopErrors[0].Count != 2 {
		t.Errorf("top errors = %+v, want timeout x2", s.TopErrors)
	}
	if s.Hours[hourSlices-1].OK != 2 || s.Hours[hourSlices-2].OK != 1 {
		t.Errorf("hours = %+v", s.Hours[hourSlices-2:])
	}
}

func TestOldBucketIsReusedNotCounted(t *testing.T) {
	tr := New()
	now := time.Unix(1_800_000_000, 0)
	tr.Record(now.Add(-window), true, time.Millisecond, "") // same slot as now, a day older
	tr.Record(now, false, time.Millisecond, "x")            // overwrites that slot
	if s := tr.Snapshot(now); s.OK != 0 || s.Fail != 1 {
		t.Errorf("ok %d fail %d, want 0/1", s.OK, s.Fail)
	}
}

func TestEmpty(t *testing.T) {
	s := New().Snapshot(time.Now())
	if s.Total != 0 || s.Percent != 0 || s.LastOK != nil || s.LastFail != nil {
		t.Errorf("empty snapshot = %+v", s)
	}
}
