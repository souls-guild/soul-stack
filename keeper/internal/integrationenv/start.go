//go:build integration

package integrationenv

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// Why this exists (NIM-569, corrected by NIM-723). L1 lost packages during
// container bring-up, a different one each sweep, for months. Read the witness
// line to the end before changing anything here:
//
//	check target: retries: 503 address: localhost:32890: get state:
//	Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/<id>/json":
//	context deadline exceeded
//
// `address: localhost:32890` belongs to the wait strategy's description of what
// it is polling FOR; it is not the address that failed. The call that times out
// is `GET /containers/<id>/json` on the docker socket -- the daemon's own API.
// The container is up and the port is mapped; what cannot answer inside the 60s
// budget is the daemon, and ~500 retries mean the poll asked it for a minute
// and got nothing back.
//
// NIM-569's first reading quoted that line with the middle elided, which
// removes `get state: Get ".../containers/<id>/json"` -- exactly the segment
// that names the daemon -- and concluded the failure was a wedged host route to
// an already-published port, so concurrency could not matter. Four sweeps on one
// host say otherwise: at -p 4 they lost one and then three packages to bring-up,
// at -p 2 they lost none.
//
// So the primary control is INTEGRATION_PARALLEL, which keeps the daemon under
// its knee, and this package is the second line for the residual case. Retrying
// by replacing the container -- rather than waiting longer -- is kept because
// once the strategy's own deadline has passed the container's health is unknown,
// and [discard] bounds what a failed attempt leaves behind. But a retry is more
// work for the daemon, not less, which is precisely why it must never be the
// thing holding a saturated one upright.
//
// The retry is deliberately blind to WHY an attempt failed. Deciding "infra, so
// retry" from the error text would be the same string-matching that already has
// to be maintained in scripts/classify-l1-failure.py, and getting it wrong here
// silences a real failure instead of merely mislabelling it. A deterministic
// failure -- a missing image, a bad request -- still fails, just three times
// over; the honest red arrives a minute later and says so.
//
// What this does NOT do is make red mean less. A retry happens during setup,
// before the suite's first assertion, so no verdict is overwritten and nothing
// is rerun on a whim (NIM-393). Every retry is logged, and the final error
// carries every attempt's error so the classifier still sees the container-layer
// markers it keys on.

// AttemptsEnv caps how many containers a single bring-up may burn through.
// Set it to 1 to turn the retry off and get the pre-NIM-569 behaviour back --
// useful when you are debugging a bring-up and want the first failure verbatim.
const AttemptsEnv = "SOUL_STACK_INTEGRATION_START_ATTEMPTS"

// DefaultAttempts is deliberately small, and small for a reason that outlived
// the diagnosis it was chosen under. With INTEGRATION_PARALLEL holding the
// daemon below saturation a stalled bring-up should be rare, so three attempts
// cover the residual case; raising it would mostly buy time spent adding load to
// a daemon that is already failing to answer, which is the one situation where
// more attempts make things worse rather than better. It is also not a way to
// sit out a broken daemon: when docker is genuinely absent every attempt fails
// in milliseconds and the suite still reports it.
const DefaultAttempts = 3

// PerAttempt is the wall clock ONE attempt gets. It sits ABOVE
// testcontainers-go's own 60s default startup timeout rather than level with
// it: the strategy's clock starts after provider setup, image pull and
// container create, so an equal budget expires first or simultaneously and the
// suite reports OUR deadline instead of the strategy's message naming the phase
// that hung -- which is the diagnostic worth keeping. 90s is also what the ~47
// hand-written TestMain timeouts this replaced actually said, so no converted
// suite got less room than it had.
const PerAttempt = 90 * time.Second

// Attempts is [DefaultAttempts] unless [AttemptsEnv] overrides it. A value that
// is not a positive integer is ignored loudly rather than silently: a typo in a
// CI variable that quietly disabled the retry would reproduce NIM-569 while
// looking configured.
func Attempts() int {
	raw := os.Getenv(AttemptsEnv)
	if raw == "" {
		return DefaultAttempts
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		log.Printf("integrationenv: ignoring %s=%q (want a positive integer), using %d", AttemptsEnv, raw, DefaultAttempts)
		return DefaultAttempts
	}
	return n
}

// SetupContext is the context a suite's container bring-up runs under: one
// [PerAttempt] budget for every attempt [Start] is allowed to make.
//
// It replaces the hand-written `context.WithTimeout(context.Background(), 90s)`
// that each of ~47 TestMains used to carry. Those numbers were copies of each
// other, so "give bring-up more room" -- the obvious first reading of NIM-569 --
// was a 47-file edit nobody was going to make consistently, and a package that
// drifted lower than its own wait strategy would fail on our budget while
// reporting the strategy's phase. One knob, one place.
func SetupContext() (context.Context, context.CancelFunc) {
	return SetupContextFor(PerAttempt)
}

// SetupContextFor is [SetupContext] for a suite whose wait strategy legitimately
// needs longer than [PerAttempt] -- a Redis cluster forming quorum, an image
// built on the spot. Pass what ONE attempt needs; the retries are accounted for
// here.
func SetupContextFor(perAttempt time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), perAttempt*time.Duration(Attempts()))
}

// Option adjusts one bring-up.
type Option func(*settings)

type settings struct{ attempts int }

// WithAttempts fixes how many containers this bring-up may burn through,
// ignoring [AttemptsEnv].
//
// Pass 1 where the retry cannot help, and say why at the call site. The retry
// works by obtaining a DIFFERENT published port, so it is worth nothing to a
// fixture that publishes none, and worse than nothing to one that PINS its host
// ports: attempt 2 asks for the same pinned ports, and terminate-then-rebind
// can add "port is already allocated" on top of the failure already there.
// Switching it off there is not an exemption from the convention -- the call
// still goes through [Start], so the scope guard still sees it, and the reason
// stays next to the fixture that has it.
func WithAttempts(n int) Option {
	return func(s *settings) { s.attempts = n }
}

// Start brings a container up, replacing it wholesale when it does not become
// reachable, and returns the first attempt that succeeds.
//
// `what` names the container in the retry log ("postgres", "vault", "sshd") and
// is not otherwise interpreted. `start` must create a NEW container per call --
// handing back a previously started one defeats the entire mechanism, since the
// point is to obtain a different published port.
//
// The budget in ctx is divided across the remaining attempts, so a caller
// keeps control of the total: three attempts under a 180s context get 60s each,
// and a caller who passes a 90s context gets three 30s attempts rather than one
// 90s attempt that silently eats the retry. Use [SetupContext] and this arranges
// itself.
func Start[T any](ctx context.Context, what string, start func(context.Context) (T, error), opts ...Option) (T, error) {
	var zero T
	cfg := settings{attempts: Attempts()}
	for _, opt := range opts {
		opt(&cfg)
	}
	attempts := cfg.attempts
	failures := make([]error, 0, attempts)

	for i := 1; i <= attempts; i++ {
		if err := ctx.Err(); err != nil {
			failures = append(failures, fmt.Errorf("attempt %d/%d not started: %w", i, attempts, err))
			break
		}

		attemptCtx, cancel := context.WithTimeout(ctx, cfg.budget(ctx, attempts-i+1))
		container, err := start(attemptCtx)
		cancel()
		if err == nil {
			if i > 1 {
				log.Printf("integrationenv: %s came up on attempt %d/%d", what, i, attempts)
			}
			return container, nil
		}

		failures = append(failures, fmt.Errorf("attempt %d/%d: %w", i, attempts, err))
		// The failed attempt may have left a running container behind:
		// testcontainers returns the container alongside the error when the wait
		// strategy is what failed. Nothing else will drop it before the session
		// ends, and on this host it is holding the published port we are trying
		// to get away from.
		discard(container)

		if i < attempts {
			log.Printf("integrationenv: %s did not come up on attempt %d/%d, starting a NEW container: %v", what, i, attempts, err)
		}
	}

	return zero, fmt.Errorf("%s: no container became reachable in %d attempt(s): %w", what, attempts, errors.Join(failures...))
}

// budget is what ONE attempt gets: an equal share of what is left of ctx,
// recomputed each round so a fast failure hands its unspent time to the
// attempts after it.
//
// The share is of what is LEFT, not of the original: tearing down a failed
// attempt (see discard) is charged to the same parent deadline and is not part
// of the split, so a round that ends in a slow terminate leaves the attempts
// after it with less than the first one had. That is the right direction --
// the parent deadline is the promise being kept -- but it is not "every attempt
// gets the same", and a reader sizing a context should not assume it is.
func (s settings) budget(ctx context.Context, attemptsLeft int) time.Duration {
	deadline, ok := ctx.Deadline()
	if !ok {
		return PerAttempt
	}
	return time.Until(deadline) / time.Duration(attemptsLeft)
}

// terminator is every testcontainers container type, reached without naming any
// of them: the generic path returns the testcontainers.Container interface, and
// each module wrapper EMBEDS that same interface (modules/postgres/postgres.go,
// modules/vault/vault.go) rather than a concrete type. That distinction is the
// one discard depends on -- an embedded interface in a nil *PostgresContainer
// still satisfies terminator, which is exactly why the typed-nil check below
// has to exist.
type terminator interface {
	Terminate(context.Context, ...testcontainers.TerminateOption) error
}

// discard drops a container left over from a failed attempt. Best effort by
// construction -- the attempt already failed, and a bring-up that cannot be
// undone is not a reason to fail differently.
func discard(container any) {
	if container == nil {
		return
	}
	// A failed generic start yields a nil interface, but a failed module start
	// yields a TYPED nil pointer, which satisfies terminator and panics on call.
	if v := reflect.ValueOf(container); v.Kind() == reflect.Ptr && v.IsNil() {
		return
	}
	c, ok := container.(terminator)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = c.Terminate(ctx)
}
