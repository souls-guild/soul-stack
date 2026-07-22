package watchman

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/health"
)

type stubPinger struct {
	err   error
	calls int
}

func (s *stubPinger) Ping(context.Context) error {
	s.calls++
	return s.err
}

var _ health.Pinger = (*stubPinger)(nil)

func TestNewDepsProbe_AllHealthy(t *testing.T) {
	pg := &stubPinger{}
	rd := &stubPinger{}
	p, err := NewDepsProbe(
		NamedPinger{Name: "postgres", Pinger: pg},
		NamedPinger{Name: "redis", Pinger: rd},
	)
	if err != nil {
		t.Fatalf("NewDepsProbe: %v", err)
	}
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe returned error on healthy deps: %v", err)
	}
	if pg.calls != 1 || rd.calls != 1 {
		t.Fatalf("calls pg=%d redis=%d, want 1 each", pg.calls, rd.calls)
	}
}

func TestNewDepsProbe_FirstFailShortCircuits(t *testing.T) {
	pg := &stubPinger{err: errors.New("pg down")}
	rd := &stubPinger{}
	p, _ := NewDepsProbe(
		NamedPinger{Name: "postgres", Pinger: pg},
		NamedPinger{Name: "redis", Pinger: rd},
	)
	err := p.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe did not surface PG failure")
	}
	// Short-circuit: second pinger not called after first failure.
	if rd.calls != 0 {
		t.Fatalf("redis pinged after pg failure: calls=%d", rd.calls)
	}
}

func TestNewDepsProbe_NilPingerSkipped(t *testing.T) {
	rd := &stubPinger{}
	p, err := NewDepsProbe(
		NamedPinger{Name: "postgres", Pinger: nil}, // Vault/Redis off-style skip
		NamedPinger{Name: "redis", Pinger: rd},
	)
	if err != nil {
		t.Fatalf("NewDepsProbe: %v", err)
	}
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe error: %v", err)
	}
	if rd.calls != 1 {
		t.Fatalf("redis calls=%d, want 1", rd.calls)
	}
}

func TestNewDepsProbe_NoDeps(t *testing.T) {
	if _, err := NewDepsProbe(); !errors.Is(err, ErrNoProbeDeps) {
		t.Fatalf("NewDepsProbe() error = %v, want ErrNoProbeDeps", err)
	}
	if _, err := NewDepsProbe(NamedPinger{Name: "redis", Pinger: nil}); !errors.Is(err, ErrNoProbeDeps) {
		t.Fatalf("NewDepsProbe(nil) error = %v, want ErrNoProbeDeps", err)
	}
}
