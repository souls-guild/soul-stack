package scenario

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
)

// warnCapture is a slog.Handler collecting WARN+ records to check the
// multi-keeper guard (footgun acolytes=0). Under mutex — the guard is called
// from the dispatch loop, tests are sequential, but we keep the handler concurrency-safe.
type warnCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *warnCapture) Enabled(_ context.Context, lvl slog.Level) bool { return lvl >= slog.LevelWarn }

func (h *warnCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r)
	h.mu.Unlock()
	return nil
}

func (h *warnCapture) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *warnCapture) WithGroup(_ string) slog.Handler      { return h }

func (h *warnCapture) warnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// lastAttr returns the value of an attribute on the last WARN record, by key.
func (h *warnCapture) lastAttr(key string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.records) == 0 {
		return "", false
	}
	var out string
	var found bool
	h.records[len(h.records)-1].Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			found = true
			return false
		}
		return true
	})
	return out, found
}

// stubLeaseOwner is a controllable [LeaseOwnerChecker] implementation for testing the guard.
type stubLeaseOwner struct {
	kid string
	ok  bool
	err error
}

func (s stubLeaseOwner) SoulLeaseOwner(_ context.Context, _ string) (string, bool, error) {
	return s.kid, s.ok, s.err
}

func TestWarnCrossKeeperDispatch(t *testing.T) {
	const selfKID = "keeper-a"

	tests := []struct {
		name       string
		leaseOwner LeaseOwnerChecker
		wantWarn   bool
	}{
		{
			name:       "lease held by another KID -> WARN",
			leaseOwner: stubLeaseOwner{kid: "keeper-b", ok: true},
			wantWarn:   true,
		},
		{
			name:       "lease held by ourselves -> no WARN",
			leaseOwner: stubLeaseOwner{kid: selfKID, ok: true},
			wantWarn:   false,
		},
		{
			name:       "no lease key (Soul not on anyone's stream) -> no WARN",
			leaseOwner: stubLeaseOwner{ok: false},
			wantWarn:   false,
		},
		{
			name:       "empty-string owner -> no WARN",
			leaseOwner: stubLeaseOwner{kid: "", ok: true},
			wantWarn:   false,
		},
		{
			name:       "lease read error -> no WARN (best-effort, stay quiet)",
			leaseOwner: stubLeaseOwner{kid: "keeper-b", ok: true, err: errors.New("redis down")},
			wantWarn:   false,
		},
		{
			name:       "guard disabled (nil checker) -> no WARN",
			leaseOwner: nil,
			wantWarn:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cap := &warnCapture{}
			r := &Runner{kid: selfKID, leaseOwner: tt.leaseOwner}
			log := slog.New(cap)

			r.warnCrossKeeperDispatch(context.Background(), "host.example.com", log)

			gotWarn := cap.warnCount() > 0
			if gotWarn != tt.wantWarn {
				t.Fatalf("warn emitted = %v, want %v", gotWarn, tt.wantWarn)
			}
			if tt.wantWarn {
				if owner, ok := cap.lastAttr("stream_owner_kid"); !ok || owner != "keeper-b" {
					t.Errorf("stream_owner_kid attr = %q (found=%v), want keeper-b", owner, ok)
				}
				if self, ok := cap.lastAttr("self_kid"); !ok || self != selfKID {
					t.Errorf("self_kid attr = %q (found=%v), want %q", self, ok, selfKID)
				}
				if sid, ok := cap.lastAttr("sid"); !ok || sid != "host.example.com" {
					t.Errorf("sid attr = %q (found=%v), want host.example.com", sid, ok)
				}
			}
		})
	}
}
