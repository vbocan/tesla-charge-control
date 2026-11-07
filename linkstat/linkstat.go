// Package linkstat keeps a rolling 24-hour health record of a polled link
// (the inverter logger, the wall connector). It stores per-minute counters
// rather than every attempt, so memory stays flat however fast the link is polled.
package linkstat

import (
	"sort"
	"sync"
	"time"
)

const (
	window     = 24 * time.Hour
	minutes    = int(window / time.Minute)
	maxRecent  = 200 // failures kept for the list and the error breakdown
	errMaxLen  = 160
	topErrors  = 5
	hourSlices = 24
)

type bucket struct {
	minute int64 // unix minute this slot currently holds
	ok     int
	fail   int
	okMs   int64 // summed duration of successful attempts
	maxMs  int64 // slowest successful attempt
}

// Failure is one failed attempt.
type Failure struct {
	Time       time.Time `json:"time"`
	DurationMs int64     `json:"duration_ms"`
	Error      string    `json:"error"`
}

// Tracker accumulates attempts. The zero value is not usable; call New.
type Tracker struct {
	mu      sync.Mutex
	buckets [minutes]bucket
	recent  []Failure // oldest first
	lastOK  time.Time
	lastBad time.Time
	streakN int
	streakK bool // true: streak of successes
}

func New() *Tracker { return &Tracker{} }

// Record notes one attempt. errMsg is ignored on success.
func (t *Tracker) Record(at time.Time, ok bool, d time.Duration, errMsg string) {
	ms := d.Milliseconds()
	min := at.Unix() / 60

	t.mu.Lock()
	defer t.mu.Unlock()

	b := &t.buckets[int(min%int64(minutes))]
	if b.minute != min {
		*b = bucket{minute: min}
	}
	if ok {
		b.ok++
		b.okMs += ms
		if ms > b.maxMs {
			b.maxMs = ms
		}
		t.lastOK = at
	} else {
		b.fail++
		t.lastBad = at
		if len(errMsg) > errMaxLen {
			errMsg = errMsg[:errMaxLen]
		}
		t.recent = append(t.recent, Failure{Time: at, DurationMs: ms, Error: errMsg})
		if len(t.recent) > maxRecent {
			t.recent = t.recent[len(t.recent)-maxRecent:]
		}
	}
	if t.streakN > 0 && t.streakK == ok {
		t.streakN++
	} else {
		t.streakK, t.streakN = ok, 1
	}
}

// Hour is one hour of the trailing day, oldest first in Stats.Hours.
type Hour struct {
	OK    int   `json:"ok"`
	Fail  int   `json:"fail"`
	AvgMs int64 `json:"avg_ms"`
}

// ErrorCount is a failure reason and how often it happened in the window.
type ErrorCount struct {
	Error string `json:"error"`
	Count int    `json:"count"`
}

// Stats is the 24-hour picture served to the gauges page.
type Stats struct {
	Total      int          `json:"total"`
	OK         int          `json:"ok"`
	Fail       int          `json:"fail"`
	Percent    float64      `json:"percent"` // success rate, 0 when there are no attempts
	AvgMs      int64        `json:"avg_ms"`  // mean duration of successful attempts
	MaxMs      int64        `json:"max_ms"`
	LastOK     *time.Time   `json:"last_ok"`
	LastFail   *time.Time   `json:"last_fail"`
	StreakOK   bool         `json:"streak_ok"` // what the current streak is made of
	StreakN    int          `json:"streak_n"`
	Hours      []Hour       `json:"hours"`
	TopErrors  []ErrorCount `json:"top_errors"`
	Failures   []Failure    `json:"failures"` // newest first
	ServerTime time.Time    `json:"server_time"`
}

// Snapshot summarises the trailing 24 hours as of now.
func (t *Tracker) Snapshot(now time.Time) Stats {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := Stats{Hours: make([]Hour, hourSlices), ServerTime: now}
	nowMin := now.Unix() / 60
	var okMs int64
	hourMs := make([]int64, hourSlices)

	for i := range t.buckets {
		b := &t.buckets[i]
		age := nowMin - b.minute
		if b.minute == 0 || age < 0 || age >= int64(minutes) {
			continue
		}
		s.OK += b.ok
		s.Fail += b.fail
		okMs += b.okMs
		if b.maxMs > s.MaxMs {
			s.MaxMs = b.maxMs
		}
		h := hourSlices - 1 - int(age/60)
		s.Hours[h].OK += b.ok
		s.Hours[h].Fail += b.fail
		hourMs[h] += b.okMs
	}
	for h := range s.Hours {
		if s.Hours[h].OK > 0 {
			s.Hours[h].AvgMs = hourMs[h] / int64(s.Hours[h].OK)
		}
	}
	s.Total = s.OK + s.Fail
	if s.Total > 0 {
		s.Percent = float64(s.OK) / float64(s.Total) * 100
	}
	if s.OK > 0 {
		s.AvgMs = okMs / int64(s.OK)
	}
	if !t.lastOK.IsZero() {
		v := t.lastOK
		s.LastOK = &v
	}
	if !t.lastBad.IsZero() {
		v := t.lastBad
		s.LastFail = &v
	}
	s.StreakOK, s.StreakN = t.streakK, t.streakN

	counts := map[string]int{}
	s.Failures = []Failure{}
	cutoff := now.Add(-window)
	for i := len(t.recent) - 1; i >= 0; i-- {
		f := t.recent[i]
		if f.Time.Before(cutoff) {
			break
		}
		s.Failures = append(s.Failures, f)
		counts[f.Error]++
	}
	s.TopErrors = []ErrorCount{}
	for e, n := range counts {
		s.TopErrors = append(s.TopErrors, ErrorCount{Error: e, Count: n})
	}
	sort.Slice(s.TopErrors, func(a, b int) bool {
		if s.TopErrors[a].Count != s.TopErrors[b].Count {
			return s.TopErrors[a].Count > s.TopErrors[b].Count
		}
		return s.TopErrors[a].Error < s.TopErrors[b].Error
	})
	if len(s.TopErrors) > topErrors {
		s.TopErrors = s.TopErrors[:topErrors]
	}
	return s
}
