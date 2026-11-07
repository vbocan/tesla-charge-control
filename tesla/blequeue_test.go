package tesla

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMergeOps(t *testing.T) {
	start := bleOp{kind: opStart, amps: 20}
	stop := bleOp{kind: opStop, urgent: true}

	tests := []struct {
		name          string
		pending, next bleOp
		want          bleOp
	}{
		{
			name:    "newest current replaces an older one",
			pending: bleOp{kind: opSetAmps, amps: 10},
			next:    bleOp{kind: opSetAmps, amps: 12},
			want:    bleOp{kind: opSetAmps, amps: 12},
		},
		{
			name:    "a new current keeps a pending start",
			pending: start,
			next:    bleOp{kind: opSetAmps, amps: 12, urgent: true},
			want:    bleOp{kind: opStart, amps: 12, urgent: true},
		},
		{
			name:    "a new current does not undo a pending stop",
			pending: stop,
			next:    bleOp{kind: opSetAmps, amps: 12},
			want:    stop,
		},
		{
			name:    "stop replaces a pending start",
			pending: start,
			next:    stop,
			want:    stop,
		},
		{
			name:    "an explicit start replaces a pending stop",
			pending: stop,
			next:    start,
			want:    start,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeOps(tc.pending, tc.next); got != tc.want {
				t.Errorf("mergeOps(%v, %v) = %v, want %v", tc.pending, tc.next, got, tc.want)
			}
		})
	}
}

// A stuck BLE command must not make Submit block, and the requests that pile up
// behind it must collapse to the newest one.
func TestQueueSubmitNeverBlocksAndKeepsNewest(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var mu sync.Mutex
	var ran []bleOp

	q := newBLEQueue(func(op bleOp) error {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		mu.Lock()
		ran = append(ran, op)
		mu.Unlock()
		return nil
	}, func(bleOp, error) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Submit(bleOp{kind: opSetAmps, amps: 10})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never picked up the first op")
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for amps := 11; amps <= 20; amps++ {
			q.Submit(bleOp{kind: opSetAmps, amps: amps})
		}
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Submit blocked while a command was in flight")
	}
	if q.Quiet(0) {
		t.Error("queue reports quiet while a command is in flight")
	}

	close(release)
	waitFor(t, "both ops to run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ran) == 2
	})

	mu.Lock()
	defer mu.Unlock()
	if ran[0].amps != 10 || ran[1].amps != 20 {
		t.Errorf("ran %v, want the in-flight 10A then only the newest, 20A", ran)
	}
}

func TestQueueSuppressesRepeatAfterSuccess(t *testing.T) {
	var calls atomic.Int32
	q := newBLEQueue(func(bleOp) error { calls.Add(1); return nil }, func(bleOp, error) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Submit(bleOp{kind: opStart, amps: 20})
	waitFor(t, "first start", func() bool { return calls.Load() == 1 && q.Quiet(0) })

	// Max mode re-requests the same thing every cycle; it must not reconnect.
	q.Submit(bleOp{kind: opStart, amps: 20})
	waitFor(t, "repeat to be consumed", func() bool { return q.Quiet(0) })
	if n := calls.Load(); n != 1 {
		t.Errorf("identical op ran %d times, want 1", n)
	}

	q.Submit(bleOp{kind: opStart, amps: 21})
	waitFor(t, "changed op to run", func() bool { return calls.Load() == 2 })
}

func TestQueueRetriesAfterFailure(t *testing.T) {
	var calls atomic.Int32
	var failures atomic.Int32
	q := newBLEQueue(func(bleOp) error {
		if calls.Add(1) == 1 {
			return errors.New("car not reachable")
		}
		return nil
	}, func(_ bleOp, err error) {
		if err != nil {
			failures.Add(1)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Submit(bleOp{kind: opStart, amps: 20})
	waitFor(t, "failure report", func() bool { return failures.Load() == 1 && q.Quiet(0) })

	// The failure must not count as applied, so the same request goes out again.
	q.Submit(bleOp{kind: opStart, amps: 20})
	waitFor(t, "retry", func() bool { return calls.Load() == 2 })
}

func TestQueueClearDropsPending(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	q := newBLEQueue(func(bleOp) error {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return nil
	}, func(bleOp, error) {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	q.Submit(bleOp{kind: opSetAmps, amps: 10})
	<-started
	q.Submit(bleOp{kind: opSetAmps, amps: 12}) // waiting behind the stuck one
	q.Clear()
	close(release)

	waitFor(t, "queue to drain", func() bool { return q.Quiet(0) })
	if n := calls.Load(); n != 1 {
		t.Errorf("%d ops ran, want only the one already in flight", n)
	}
}

// fakeClock is a manually advanced clock that the queue's worker can read
// safely from its own goroutine.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestRetryDelay(t *testing.T) {
	want := []time.Duration{
		0,                // no failures
		0,                // first failure: retry at once, it may be a blip
		10 * time.Second, // then back off...
		20 * time.Second,
		40 * time.Second,
		60 * time.Second, // ...up to the cap
		60 * time.Second,
		60 * time.Second,
	}
	for streak, w := range want {
		if got := retryDelay(streak); got != w {
			t.Errorf("retryDelay(%d) = %v, want %v", streak, got, w)
		}
	}
}

// backoffRig is a queue on a fake clock whose exec can be made to fail, plus
// counters for what actually ran and what was held back.
type backoffRig struct {
	q         *bleQueue
	clk       *fakeClock
	execs     atomic.Int32 // ops that actually ran
	held      atomic.Int32 // ops held back by the backoff
	processed atomic.Int32 // ops the worker has finished with, either way
	fail      atomic.Bool
}

func newBackoffRig(t *testing.T) *backoffRig {
	t.Helper()
	r := &backoffRig{clk: &fakeClock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}}
	r.fail.Store(true)
	r.q = newBLEQueue(func(bleOp) error {
		r.execs.Add(1)
		if r.fail.Load() {
			return errors.New("car not reachable")
		}
		return nil
	}, func(_ bleOp, err error) {
		if errors.Is(err, errBackingOff) {
			r.held.Add(1)
		}
		r.processed.Add(1)
	})
	r.q.now = r.clk.Now
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.q.Run(ctx)
	return r
}

// submit queues op and waits until the worker has finished with it.
func (r *backoffRig) submit(t *testing.T, op bleOp) {
	t.Helper()
	before := r.processed.Load()
	r.q.Submit(op)
	waitFor(t, "the worker to process "+op.String(), func() bool { return r.processed.Load() == before+1 })
}

func (r *backoffRig) expect(t *testing.T, execs, held int32) {
	t.Helper()
	if e, h := r.execs.Load(), r.held.Load(); e != execs || h != held {
		t.Fatalf("ran %d and held back %d ops, want %d and %d", e, h, execs, held)
	}
}

func TestQueueBacksOffNonUrgentOpsAfterRepeatedFailures(t *testing.T) {
	r := newBackoffRig(t)
	start := bleOp{kind: opStart, amps: 20}
	stop := bleOp{kind: opStop, urgent: true}

	r.submit(t, start) // 1st failure: no backoff yet
	r.submit(t, start) // runs, 2nd failure: now backing off for 10s
	r.expect(t, 2, 0)

	r.submit(t, start) // held back
	r.expect(t, 2, 1)
	r.clk.Advance(9 * time.Second)
	r.submit(t, start) // 9s < 10s: still held back
	r.expect(t, 2, 2)

	// An urgent op is never held back, and its failure lengthens the backoff to 20s.
	r.submit(t, stop)
	r.expect(t, 3, 2)

	r.clk.Advance(11 * time.Second)
	r.submit(t, start) // 11s < 20s
	r.expect(t, 3, 3)

	r.clk.Advance(10 * time.Second)
	r.submit(t, start) // 21s: backoff over, runs (and fails, extending to 40s)
	r.expect(t, 4, 3)
	r.submit(t, start)
	r.expect(t, 4, 4)
}

func TestQueueSuccessEndsBackoff(t *testing.T) {
	r := newBackoffRig(t)
	start := bleOp{kind: opStart, amps: 20}

	r.submit(t, start)
	r.submit(t, start)
	r.submit(t, start) // held back
	r.expect(t, 2, 1)

	// An urgent op gets through once the link recovers, which clears the backoff.
	r.fail.Store(false)
	r.submit(t, bleOp{kind: opStop, urgent: true})
	r.expect(t, 3, 1)

	r.fail.Store(true)
	r.submit(t, start) // not held back any more
	r.submit(t, start) // 1st failure after recovery retries at once too
	r.expect(t, 5, 1)
}

func TestQueueClearEndsBackoff(t *testing.T) {
	r := newBackoffRig(t)
	start := bleOp{kind: opStart, amps: 20}

	r.submit(t, start)
	r.submit(t, start)
	r.submit(t, start) // held back
	r.expect(t, 2, 1)

	r.q.Clear() // e.g. the user changed mode
	r.submit(t, start)
	r.expect(t, 3, 1)
}
