package tesla

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// A BLE command to the car can take many seconds (scan, connect, handshake) or
// fail outright, so the control loop must never wait on one. Instead, Evaluate
// decides what the car should be doing and hands that to a bleQueue; a single
// worker goroutine performs it and reports the outcome back afterwards.

type bleOpKind int

const (
	opSetAmps bleOpKind = iota // change the charging current only
	opStart                    // set the current, then start charging
	opStop                     // stop charging
)

// bleRepeatInterval is how long a start or stop that just succeeded suppresses
// an identical one. Max mode (and the stop-desync retry) re-request the same
// state every cycle; without this each request would open a fresh BLE connection.
const bleRepeatInterval = 20 * time.Second

// Retry backoff. When commands keep failing, retrying every cycle just keeps
// opening connections the car (or a sleeping/out-of-range link) can't use, and
// can tie up the car's few BLE connection slots. After repeated failures,
// non-urgent commands are held back for an increasing time. Urgent ones
// (overload reductions, stop) are never held back.
const (
	bleBackoffBase = 5 * time.Second
	bleBackoffMax  = 60 * time.Second
)

// errBackingOff is what the queue reports for a non-urgent op it held back
// instead of running, so the charger knows the link is still considered down.
var errBackingOff = errors.New("BLE retries backing off after repeated failures")

// retryDelay is how long to hold back non-urgent ops after streak consecutive
// failures: none after the first (it may be a blip), then 10s, 20s, 40s, 60s.
func retryDelay(streak int) time.Duration {
	if streak <= 1 {
		return 0
	}
	return min(bleBackoffBase<<min(streak-1, 5), bleBackoffMax)
}

// bleOp is one desired car command.
type bleOp struct {
	kind bleOpKind
	amps int
	// urgent marks a safety command (overload reduction or stop). It preempts
	// background BLE work and is attempted even during the slot-full cooldown.
	urgent bool
}

func (o bleOp) String() string {
	switch o.kind {
	case opSetAmps:
		return fmt.Sprintf("SetAmps(%dA)", o.amps)
	case opStart:
		return fmt.Sprintf("Start(%dA)", o.amps)
	case opStop:
		return "Stop"
	}
	return "?"
}

// sameAs reports whether two ops ask the car for the same thing.
func (o bleOp) sameAs(p bleOp) bool {
	return o.kind == p.kind && (o.kind == opStop || o.amps == p.amps)
}

// mergeOps combines an op that hasn't run yet with a newer request. The newest
// request normally wins, except where it would silently drop part of the
// earlier one:
//   - a new current for a pending start must still start the charge;
//   - a pending stop is not undone by a current change (that assumes the car is
//     charging) — only by an explicit new start.
func mergeOps(pending, next bleOp) bleOp {
	if next.kind == opSetAmps {
		switch pending.kind {
		case opStart:
			return bleOp{kind: opStart, amps: next.amps, urgent: next.urgent}
		case opStop:
			return pending
		}
	}
	return next
}

// bleQueue is a single-slot, newest-wins mailbox in front of one worker
// goroutine. Submit never blocks, however long the BLE layer takes.
type bleQueue struct {
	exec func(bleOp) error  // performs an op; may block for a long time
	done func(bleOp, error) // reports the outcome; must not block on the radio
	kick chan struct{}      // wakes the worker
	now  func() time.Time   // the clock; replaced in tests

	mu         sync.Mutex
	pending    *bleOp
	inFlight   bool
	lastDone   time.Time
	applied    *bleOp // last op that succeeded, for repeat suppression
	appliedAt  time.Time
	failStreak int       // consecutive failed ops; reset by a success
	retryAfter time.Time // non-urgent ops are held back until then
}

func newBLEQueue(exec func(bleOp) error, done func(bleOp, error)) *bleQueue {
	return &bleQueue{exec: exec, done: done, kick: make(chan struct{}, 1), now: time.Now}
}

// Submit queues op, replacing (via mergeOps) any op still waiting to run.
func (q *bleQueue) Submit(op bleOp) {
	q.mu.Lock()
	if q.pending != nil {
		op = mergeOps(*q.pending, op)
	}
	q.pending = &op
	q.mu.Unlock()

	select {
	case q.kick <- struct{}{}:
	default: // worker already has a wake-up pending
	}
}

// Clear drops any waiting op and forgets what was last applied and any retry
// backoff, so the next request is always sent right away. An op already in
// flight can't be recalled.
func (q *bleQueue) Clear() {
	q.mu.Lock()
	q.pending = nil
	q.applied = nil
	q.failStreak = 0
	q.retryAfter = time.Time{}
	q.mu.Unlock()
}

// Quiet reports whether nothing is queued or running and the last op finished
// at least holdoff ago. Background work uses it to stay out of the way.
func (q *bleQueue) Quiet(holdoff time.Duration) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending == nil && !q.inFlight && q.now().Sub(q.lastDone) >= holdoff
}

// Run is the worker loop. It returns when ctx is cancelled.
func (q *bleQueue) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-q.kick:
		}
		for q.runNext() {
		}
	}
}

// runNext performs the waiting op, if any. It reports whether there was one.
func (q *bleQueue) runNext() bool {
	q.mu.Lock()
	if q.pending == nil {
		q.mu.Unlock()
		return false
	}
	op := *q.pending
	q.pending = nil
	// A repeat of what just succeeded is dropped. A current that's already set
	// stays dropped (the BLE layer skips it too); start/stop are re-sent once
	// bleRepeatInterval has passed, in case the car's state drifted.
	if q.applied != nil && q.applied.sameAs(op) &&
		(op.kind == opSetAmps || q.now().Sub(q.appliedAt) < bleRepeatInterval) {
		q.mu.Unlock()
		return true
	}
	// After repeated failures, hold back non-urgent ops until the backoff
	// expires. The charger re-requests every cycle, so the op is simply retried
	// later; the report keeps its error status alive meanwhile.
	if !op.urgent && q.now().Before(q.retryAfter) {
		q.mu.Unlock()
		q.done(op, errBackingOff)
		return true
	}
	q.inFlight = true
	q.mu.Unlock()

	err := q.exec(op)

	q.mu.Lock()
	q.inFlight = false
	q.lastDone = q.now()
	if err == nil {
		q.applied = &op
		q.appliedAt = q.lastDone
		q.failStreak = 0
		q.retryAfter = time.Time{}
	} else {
		q.applied = nil
		q.failStreak++
		q.retryAfter = q.lastDone.Add(retryDelay(q.failStreak))
	}
	q.mu.Unlock()

	q.done(op, err)
	return true
}
