package push

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/config"
)

// fakeTargetReader is a stub PGTargetReader for unit tests without a live PG.
type fakeTargetReader struct {
	target *soul.SSHTarget
	err    error
	calls  int
}

func (f *fakeTargetReader) SelectSshTarget(_ context.Context, _ string) (*soul.SSHTarget, error) {
	f.calls++
	return f.target, f.err
}

func TestPGFallbackTargetResolver_PGRowPopulated(t *testing.T) {
	reader := &fakeTargetReader{target: &soul.SSHTarget{
		SSHPort: 2222, SSHUser: "deploy", SoulPath: "/opt/soul/bin/soul",
	}}
	r := &PGFallbackTargetResolver{
		Reader: reader,
		// Fallback / AllowLegacy aren't used (the PG row isn't NULL).
	}

	got, err := r.Resolve(context.Background(), "soul-a.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := SSHTarget{Host: "soul-a.example.com", Port: 2222, User: "deploy", SoulPath: "/opt/soul/bin/soul"}
	if got != want {
		t.Errorf("got = %+v, want = %+v", got, want)
	}
}

func TestPGFallbackTargetResolver_PGRowPopulated_FillsDefaults(t *testing.T) {
	// Empty fields in storage → defaults during resolve (storage holds ONLY
	// what the operator set; defaults live in a single place).
	reader := &fakeTargetReader{target: &soul.SSHTarget{}}
	r := &PGFallbackTargetResolver{Reader: reader}

	got, err := r.Resolve(context.Background(), "soul-a.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := SSHTarget{Host: "soul-a.example.com", Port: defaultSSHPort, User: defaultSSHUser, SoulPath: defaultSoulPath}
	if got != want {
		t.Errorf("got = %+v, want = %+v", got, want)
	}
}

func TestPGFallbackTargetResolver_NullColumn_NoLegacy(t *testing.T) {
	// PG row.ssh_target IS NULL + AllowLegacy=false (default) →
	// ErrTargetNotConfigured.
	reader := &fakeTargetReader{target: nil}
	r := &PGFallbackTargetResolver{
		Reader:      reader,
		Fallback:    NewConfigTargetResolver([]config.KeeperPushTarget{{SID: "soul-a.example.com"}}),
		AllowLegacy: false,
	}
	_, err := r.Resolve(context.Background(), "soul-a.example.com")
	if !errors.Is(err, ErrTargetNotConfigured) {
		t.Errorf("err = %v, want ErrTargetNotConfigured", err)
	}
}

func TestPGFallbackTargetResolver_NullColumn_LegacyFallback(t *testing.T) {
	// PG row IS NULL + AllowLegacy=true → fallback to ConfigTargetResolver +
	// a one-time WARN.
	reader := &fakeTargetReader{target: nil}
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	r := &PGFallbackTargetResolver{
		Reader: reader,
		Fallback: NewConfigTargetResolver([]config.KeeperPushTarget{
			{SID: "soul-a.example.com", SSHPort: 22022, SSHUser: "deploy", SoulPath: "/usr/bin/soul"},
		}),
		AllowLegacy: true,
		Logger:      logger,
	}

	got, err := r.Resolve(context.Background(), "soul-a.example.com")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := SSHTarget{Host: "soul-a.example.com", Port: 22022, User: "deploy", SoulPath: "/usr/bin/soul"}
	if got != want {
		t.Errorf("got = %+v, want = %+v", got, want)
	}

	// Second call: WARN must not repeat (sync.Once gating).
	got2, err := r.Resolve(context.Background(), "soul-a.example.com")
	if err != nil {
		t.Fatalf("Resolve(2): %v", err)
	}
	if got2 != want {
		t.Errorf("got2 = %+v, want = %+v", got2, want)
	}
	logs := logBuf.String()
	count := bytesCount(logs, "S7-1 deprecation")
	if count != 1 {
		t.Errorf("WARN count = %d; want 1 (sync.Once gating)", count)
	}
}

func TestPGFallbackTargetResolver_NullColumn_LegacyFallback_UnknownSID(t *testing.T) {
	// AllowLegacy=true, but even Fallback doesn't know the SID →
	// ErrTargetNotConfigured (from ConfigTargetResolver).
	reader := &fakeTargetReader{target: nil}
	r := &PGFallbackTargetResolver{
		Reader:      reader,
		Fallback:    NewConfigTargetResolver(nil),
		AllowLegacy: true,
		Logger:      slog.Default(),
	}
	_, err := r.Resolve(context.Background(), "soul-a.example.com")
	if !errors.Is(err, ErrTargetNotConfigured) {
		t.Errorf("err = %v, want ErrTargetNotConfigured (chained from ConfigTargetResolver)", err)
	}
}

func TestPGFallbackTargetResolver_ReaderError(t *testing.T) {
	reader := &fakeTargetReader{err: errors.New("pg unavailable")}
	r := &PGFallbackTargetResolver{Reader: reader}
	_, err := r.Resolve(context.Background(), "soul-a.example.com")
	if err == nil {
		t.Fatal("expected error from PG reader, got nil")
	}
	if errors.Is(err, ErrTargetNotConfigured) {
		t.Errorf("reader-error should propagate as-is, not be mapped into ErrTargetNotConfigured: %v", err)
	}
}

// bytesCount counts occurrences of needle in haystack (we avoid
// strings.Count in tests to skip adding a package import for a single
// function).
func bytesCount(haystack, needle string) int {
	n := 0
	for i := 0; i+len(needle) <= len(haystack); {
		if haystack[i:i+len(needle)] == needle {
			n++
			i += len(needle)
			continue
		}
		i++
	}
	return n
}
