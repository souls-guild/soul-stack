package reaper

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Console recording is mandatory (ADR-0074(g)), so `console_recordings` grows
// with every shell anybody opens and no operator action stops it. These pin the
// retention rule that keeps that from being a disk-growth bug.

func TestConsoleRecordingsPurger_DeletesByBakedTTL(t *testing.T) {
	fe := &fakeErrandsExecer{rowsAff: 12}
	p := newConsoleRecordingsPurgerFromExecer(fe, silentLogger())

	got, err := p.Run(context.Background(), 90*24*time.Hour, 1000)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != 12 {
		t.Errorf("affected = %d, want 12", got)
	}
	if fe.lastSQL != purgeOldConsoleRecordingsSQL {
		t.Errorf("SQL mismatch:\n got: %q\nwant: %q", fe.lastSQL, purgeOldConsoleRecordingsSQL)
	}
	// The rule's max_age must not reach the predicate: the TTL a recording was
	// taken under belongs to that recording. Lowering retention must not
	// silently delete recordings kept under the old policy.
	if len(fe.args) != 0 {
		t.Errorf("args len = %d, want 0 (max_age is not part of the predicate)", len(fe.args))
	}
	if !strings.Contains(purgeOldConsoleRecordingsSQL, "ttl_at < NOW()") {
		t.Errorf("the predicate does not read the baked TTL: %q", purgeOldConsoleRecordingsSQL)
	}
}

func TestConsoleRecordingsPurger_PropagatesError(t *testing.T) {
	want := errors.New("pg down")
	p := newConsoleRecordingsPurgerFromExecer(&fakeErrandsExecer{err: want}, silentLogger())

	if _, err := p.Run(context.Background(), 0, 0); !errors.Is(err, want) {
		t.Fatalf("err = %v, want it to wrap %v", err, want)
	}
}
