//go:build integration

package integrationenv

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// The retry loop in Start is the whole of NIM-569, and the failure it exists for
// happens roughly once in fifty container starts -- so a green L1 sweep is not
// evidence that it works, only that the wedge did not occur. These tests drive
// the loop directly with a synthetic bring-up, which is the only way to observe
// the recovery path on demand. They need no docker despite the tag: Start is
// declared in an `integration`-tagged file, so a test of it cannot be untagged.

type fakeContainer struct {
	id         int
	terminated *int
}

func (f *fakeContainer) Terminate(context.Context, ...testcontainers.TerminateOption) error {
	*f.terminated++
	return nil
}

func TestStartRetriesUntilAContainerComesUp(t *testing.T) {
	terminated := 0
	calls := 0

	ctx, cancel := SetupContext()
	defer cancel()

	got, err := Start(ctx, "fake", func(context.Context) (*fakeContainer, error) {
		calls++
		c := &fakeContainer{id: calls, terminated: &terminated}
		if calls < 3 {
			return c, errors.New("wait until ready: check target: retries: 447")
		}
		return c, nil
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if calls != 3 {
		t.Errorf("start calls = %d, want 3", calls)
	}
	if got.id != 3 {
		t.Errorf("returned container = #%d, want the third", got.id)
	}
	// Both losers must be dropped: on the host this fix is for, a failed attempt
	// leaves a running container holding the published port we are trying to get
	// away from.
	if terminated != 2 {
		t.Errorf("terminated %d failed container(s), want 2", terminated)
	}
}

func TestStartReportsEveryAttemptAndKeepsTheInfraMarkers(t *testing.T) {
	terminated := 0
	ctx, cancel := SetupContextFor(50 * time.Millisecond)
	defer cancel()

	_, err := Start(ctx, "fake", func(context.Context) (*fakeContainer, error) {
		return &fakeContainer{terminated: &terminated}, errors.New("generic container: start container: started hook: wait until ready: check target: retries: 447")
	})
	if err == nil {
		t.Fatal("Start returned nil error after every attempt failed")
	}
	msg := err.Error()
	for _, want := range []string{
		"fake: no container became reachable in 3 attempt(s)",
		"attempt 1/3",
		"attempt 3/3",
		// scripts/classify-l1-failure.py keys on these. Losing them turns an
		// INFRA verdict into UNCLEAR and quietly breaks a different guard.
		"generic container:",
		"started hook:",
		"check target: retries:",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error text is missing %q:\n%s", want, msg)
		}
	}
}

func TestStartAttemptsOnceWhenTheRetryIsTurnedOff(t *testing.T) {
	t.Setenv(AttemptsEnv, "1")
	terminated := 0
	calls := 0

	ctx, cancel := SetupContext()
	defer cancel()

	_, err := Start(ctx, "fake", func(context.Context) (*fakeContainer, error) {
		calls++
		return &fakeContainer{terminated: &terminated}, errors.New("boom")
	})
	if err == nil {
		t.Fatal("Start returned nil error")
	}
	if calls != 1 {
		t.Errorf("start calls = %d, want 1 with %s=1", calls, AttemptsEnv)
	}
}

func TestAttemptsIgnoresAValueThatWouldDisableTheRetryByAccident(t *testing.T) {
	for _, raw := range []string{"0", "-1", "three", "3 "} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(AttemptsEnv, raw)
			if got := Attempts(); got != DefaultAttempts {
				t.Errorf("Attempts() = %d for %s=%q, want %d", got, AttemptsEnv, raw, DefaultAttempts)
			}
		})
	}
}

func TestStartGivesTheLastAttemptWhatTheEarlierOnesDidNotSpend(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	terminated := 0
	var budgets []time.Duration

	_, _ = Start(ctx, "fake", func(attemptCtx context.Context) (*fakeContainer, error) {
		deadline, ok := attemptCtx.Deadline()
		if !ok {
			t.Fatal("attempt context carries no deadline")
		}
		budgets = append(budgets, time.Until(deadline))
		return &fakeContainer{terminated: &terminated}, errors.New("boom")
	})

	if len(budgets) != 3 {
		t.Fatalf("got %d attempts, want 3", len(budgets))
	}
	// Dividing the WHOLE budget by the TOTAL attempt count would hand every
	// attempt the same slice and let the first two donate their unused time to
	// nobody; the last attempt must inherit it.
	if budgets[2] <= budgets[0] {
		t.Errorf("last attempt got %v, first got %v: the remainder was not carried forward", budgets[2], budgets[0])
	}
	for i, b := range budgets {
		if b <= 0 {
			t.Errorf("attempt %d got a non-positive budget %v -- it would fail instantly while looking retried", i+1, b)
		}
	}
}

func TestStartDoesNotPanicOnATypedNilFromAFailedStart(t *testing.T) {
	ctx, cancel := SetupContextFor(50 * time.Millisecond)
	defer cancel()

	// A failed module Run returns a typed-nil *PostgresContainer, which satisfies
	// the terminator interface and panics when called. A panic here would fire
	// during the setup of a random package and look exactly like NIM-569 itself.
	_, err := Start(ctx, "fake", func(context.Context) (*fakeContainer, error) {
		var nilContainer *fakeContainer
		return nilContainer, errors.New("boom")
	})
	if err == nil {
		t.Fatal("Start returned nil error")
	}
}

func TestStartDoesNotStartAnAttemptUnderADeadContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	calls := 0
	_, err := Start(ctx, "fake", func(context.Context) (*fakeContainer, error) {
		calls++
		return nil, nil
	})
	if err == nil {
		t.Fatal("Start returned nil error under a cancelled context")
	}
	if calls != 0 {
		t.Errorf("start calls = %d under a cancelled context, want 0", calls)
	}
	if !strings.Contains(err.Error(), "not started") {
		t.Errorf("error does not say the attempt never ran:\n%s", err)
	}
}

// WithAttempts exists for the two fixtures the retry cannot help, and what it
// prevents is silent: the redis cluster pins host ports 7000-7005, so attempt 2
// asks for the same pinned ports, and the L2 stand publishes no port at all. Neither
// would give a wrong answer without the option -- only a slower, stranger red,
// and in the L2 case an image build squeezed into a third of the budget its
// author sized for the whole case.

func TestWithAttemptsBeatsTheEnvironment(t *testing.T) {
	t.Setenv(AttemptsEnv, "5")

	calls := 0
	terminated := 0
	_, err := Start(context.Background(), "fake", func(context.Context) (*fakeContainer, error) {
		calls++
		return &fakeContainer{terminated: &terminated}, errors.New("boom")
	}, WithAttempts(1))

	if calls != 1 {
		t.Fatalf("got %d attempts, want 1 -- a pinned-port fixture must not retry into the same pinned ports", calls)
	}
	if err == nil {
		t.Fatal("Start returned nil error after its only attempt failed")
	}
	if !strings.Contains(err.Error(), "in 1 attempt(s)") {
		t.Errorf("error does not report the attempt count it actually made: %v", err)
	}
}
