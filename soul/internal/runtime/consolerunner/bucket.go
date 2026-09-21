package consolerunner

import "time"

// tokenBucket paces console output to a sustained byte rate with a burst
// allowance. Hand-rolled rather than pulled from x/time/rate: it is twenty lines,
// it needs no dependency in the Soul agent, and `take` has the exact semantics we
// want — block this goroutine, never drop (dropping is the queue's job).
//
// Not concurrent-safe: exactly one sendLoop owns one bucket.
type tokenBucket struct {
	ratePerSec float64
	burst      float64
	tokens     float64
	last       time.Time

	// sleep is the blocking primitive, swapped out in tests so pacing can be
	// asserted without spending real seconds. It must return early when abort
	// fires — see [tokenBucket.take].
	sleep func(d time.Duration, abort <-chan struct{})
	now   func() time.Time
}

func newTokenBucket(ratePerSec, burst int) *tokenBucket {
	b := &tokenBucket{
		ratePerSec: float64(ratePerSec),
		burst:      float64(burst),
		tokens:     float64(burst),
		sleep:      interruptibleSleep,
		now:        time.Now,
	}
	b.last = b.now()
	return b
}

// take waits until n bytes of budget are available, then spends them. A single
// payload larger than the burst is allowed through after draining the bucket —
// the caller's chunk size is already capped, and refusing it would stall the
// session forever.
//
// abort cuts the wait short and is what keeps teardown honest: `rate_limit_kbps`
// is operator-configurable, so a deliberately throttled session can owe whole
// seconds of pacing. Without this, CloseAll would sit out its budget and report a
// leak for a pty that is already dead. Once the session is ending there is
// nothing left to protect the stream from — the queue is bounded and drains at
// full speed.
func (b *tokenBucket) take(n int, abort <-chan struct{}) {
	if n <= 0 {
		return
	}
	b.refill()
	need := float64(n)
	if deficit := need - b.tokens; deficit > 0 {
		b.sleep(time.Duration(deficit/b.ratePerSec*float64(time.Second)), abort)
		b.refill()
	}
	b.tokens -= need
	if b.tokens < 0 {
		b.tokens = 0
	}
}

// interruptibleSleep waits out d, or returns as soon as abort fires.
func interruptibleSleep(d time.Duration, abort <-chan struct{}) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-abort:
	}
}

func (b *tokenBucket) refill() {
	now := b.now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	if elapsed <= 0 {
		return
	}
	b.tokens += elapsed * b.ratePerSec
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
}
