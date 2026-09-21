//go:build linux

package beacon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// L0 unit tests for core.beacon.inotify (Linux-only). Real inotify fd on a
// tmpdir — always available under Linux (even without root), no testcontainers.

// TestInotify_QuietThenEvents — happy-path:
//   - first Check: lazy-init watch + return state="quiet" (buffer empty).
//   - touch a file in the watched dir → kernel sends IN_CREATE / IN_MODIFY.
//   - second Check: state="events", data.count > 0, data.events contains
//     an entry with type="created".
func TestInotify_QuietThenEvents(t *testing.T) {
	dir := t.TempDir()

	b := NewInotify()
	params := paramStruct(t, map[string]any{"path": dir})

	state, _, err := b.Check(context.Background(), params)
	if err != nil {
		t.Fatalf("first Check: %v", err)
	}
	if state != stateInotifyQuiet {
		t.Fatalf("first Check with no events should be quiet, got %q", state)
	}

	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Let readLoop pick up the event. inotifyReadIdle = 100 ms.
	waitForInotifyEvents(t, b, params, 2*time.Second)

	state, data, err := b.Check(context.Background(), params)
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	if state != stateInotifyEvents {
		t.Fatalf("after touch expected state=events, got %q", state)
	}
	if data.GetFields()["count"].GetNumberValue() < 1 {
		t.Errorf("data.count = %v, want >= 1", data.GetFields()["count"].GetNumberValue())
	}
	if got := data.GetFields()["path"].GetStringValue(); got != dir {
		t.Errorf("data.path = %q, want %q", got, dir)
	}
	events := data.GetFields()["events"].GetListValue().GetValues()
	if len(events) == 0 {
		t.Fatal("data.events empty after touch")
	}
	firstType := events[0].GetStructValue().GetFields()["type"].GetStringValue()
	if firstType != "created" && firstType != "modified" {
		t.Errorf("first event type=%q, want created/modified", firstType)
	}
}

// TestInotify_FilterCreatedOnly — the `events: ["created"]` filter drops
// IN_MODIFY/IN_ATTRIB at the kernel level (mask), the beacon only sees IN_CREATE.
func TestInotify_FilterCreatedOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing")
	if err := os.WriteFile(target, []byte("v0"), 0o644); err != nil {
		t.Fatal(err)
	}

	b := NewInotify()
	params := paramStruct(t, map[string]any{
		"path":   dir,
		"events": []any{"created"},
	})

	// Baseline Check registers the watch (filter is created-only).
	if _, _, err := b.Check(context.Background(), params); err != nil {
		t.Fatalf("baseline Check: %v", err)
	}

	// IN_MODIFY (not in the filter) — should NOT land in the window.
	if err := os.WriteFile(target, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	state, _, err := b.Check(context.Background(), params)
	if err != nil {
		t.Fatalf("Check after modify: %v", err)
	}
	if state != stateInotifyQuiet {
		t.Errorf("modify is not in the filter, but state=%q", state)
	}

	// IN_CREATE — should land.
	if err := os.WriteFile(filepath.Join(dir, "new"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForInotifyEvents(t, b, params, 2*time.Second)

	state, data, err := b.Check(context.Background(), params)
	if err != nil {
		t.Fatalf("Check after create: %v", err)
	}
	if state != stateInotifyEvents {
		t.Errorf("create is in the filter, but state=%q", state)
	}
	if got := data.GetFields()["count"].GetNumberValue(); got < 1 {
		t.Errorf("count=%v, want >= 1", got)
	}
}

// TestInotify_MissingPath — the path doesn't exist: inotify_add_watch
// returns ENOENT, the beacon returns an error (scheduler skips the tick,
// baseline never gets established).
func TestInotify_MissingPath(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "does-not-exist")

	b := NewInotify()
	_, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": absent}))
	if err == nil {
		t.Fatal("expected an error for a missing path")
	}
}

// TestInotify_RecursiveRejected — recursive: true isn't supported in the
// MVP, the beacon rejects it at params validation (before any syscall).
func TestInotify_RecursiveRejected(t *testing.T) {
	b := NewInotify()
	_, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path":      "/tmp",
		"recursive": true,
	}))
	if err == nil {
		t.Fatal("recursive=true must be rejected in MVP")
	}
}

// TestInotify_MissingParam — missing required path → error.
func TestInotify_MissingParam(t *testing.T) {
	b := NewInotify()
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("expected an error when param path is missing")
	}
}

// TestInotify_MultipleVigilsSeparateWatches — two Checks with different
// paths get independent watches (one InotifyBeacon per process, per-path
// independent kernel fds). A change in dirA must NOT show up in dirB's window.
func TestInotify_MultipleVigilsSeparateWatches(t *testing.T) {
	dirA := t.TempDir()
	dirB := t.TempDir()

	b := NewInotify()
	paramsA := paramStruct(t, map[string]any{"path": dirA})
	paramsB := paramStruct(t, map[string]any{"path": dirB})

	// Register both watches.
	if _, _, err := b.Check(context.Background(), paramsA); err != nil {
		t.Fatalf("baseline A: %v", err)
	}
	if _, _, err := b.Check(context.Background(), paramsB); err != nil {
		t.Fatalf("baseline B: %v", err)
	}

	// Event only in A.
	if err := os.WriteFile(filepath.Join(dirA, "x"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitForInotifyEvents(t, b, paramsA, 2*time.Second)

	// B should stay quiet.
	stateB, _, err := b.Check(context.Background(), paramsB)
	if err != nil {
		t.Fatalf("Check B: %v", err)
	}
	if stateB != stateInotifyQuiet {
		t.Errorf("watch B must not see the event from A, state=%q", stateB)
	}

	// A should pick up the event.
	stateA, _, err := b.Check(context.Background(), paramsA)
	if err != nil {
		t.Fatalf("Check A: %v", err)
	}
	if stateA != stateInotifyEvents {
		t.Errorf("watch A must see its own event, state=%q", stateA)
	}
}

// TestInotify_RegistryAndDefault — the beacon is registered in the Default
// registry under the canonical address from beaconaddr.Inotify. Guards the
// "keeper-enum == soul-registry == beaconaddr" invariant (see
// shared/beaconaddr/beaconaddr_test.go).
func TestInotify_RegistryAndDefault(t *testing.T) {
	reg := Default()
	b, ok := reg.Lookup(InotifyName)
	if !ok {
		t.Fatalf("InotifyName=%q is missing from Default()", InotifyName)
	}
	if _, isInotify := b.(*InotifyBeacon); !isInotify {
		t.Errorf("Lookup(%q) returned %T, want *InotifyBeacon", InotifyName, b)
	}
}

// TestInotify_SchedulerEdgeTriggered — end-to-end path through the
// scheduler: quiet → events transition emits exactly one Portent with a
// typed payload (V5-1 mapper) + the data branch (deprecation period).
// Mirrors TestSchedulerWithRealFileBeacon for file_changed.
func TestInotify_SchedulerEdgeTriggered(t *testing.T) {
	dir := t.TempDir()

	s := NewScheduler(SchedulerConfig{Registry: Default(), SID: "host.example"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.Stop()

	def := &keeperv1.VigilDef{
		Name:     "inotify-watch",
		Check:    InotifyName,
		Interval: "50ms",
		Params:   paramStruct(t, map[string]any{"path": dir}),
	}
	s.Apply(ctx, []*keeperv1.VigilDef{def})

	// Baseline (first tick) — no Portent.
	select {
	case ev := <-s.Portents():
		t.Fatalf("baseline must not emit Portent, got %q", ev.GetBeaconName())
	case <-time.After(200 * time.Millisecond):
	}

	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("v"), 0o644); err != nil {
		t.Fatal(err)
	}

	select {
	case ev := <-s.Portents():
		if ev.GetBeaconName() != "inotify-watch" {
			t.Fatalf("beacon_name=%q, want inotify-watch", ev.GetBeaconName())
		}
		// V5-1 dual-write: data + typed payload.
		if ev.GetData() == nil {
			t.Error("data branch is empty (deprecation hand-off violation)")
		}
		ino := ev.GetInotify()
		if ino == nil {
			t.Fatal("typed payload InotifyPortent is empty")
		}
		if ino.GetPath() != dir {
			t.Errorf("InotifyPortent.path=%q, want %q", ino.GetPath(), dir)
		}
		if ino.GetCount() < 1 {
			t.Errorf("InotifyPortent.count=%d, want >= 1", ino.GetCount())
		}
		if len(ino.GetEvents()) == 0 {
			t.Error("InotifyPortent.events is empty")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("touching the file did not raise a Portent through the scheduler")
	}
}

// waitForInotifyEvents polls Check until events show up OR timeout. Without
// a sleep before Check, the kernel might not deliver events in time
// (readLoop spins with inotifyReadIdle=100ms between read syscalls). Keeps
// the sleep out of the main test logic.
func waitForInotifyEvents(t *testing.T, b *InotifyBeacon, params interface{}, timeout time.Duration) {
	t.Helper()
	// peek-Check: read the buffer WITHOUT flushing? The current Check always
	// flushes, so a peek loop would corrupt the test window. Instead of a
	// peek, we use a passive wait, trusting that 200-500 ms is enough for
	// kernel+readLoop. On a loaded CI machine the timeout can be longer, so
	// we wait in several steps.
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		if hasBufferedEvents(b) {
			return
		}
	}
}

// hasBufferedEvents — a non-flushing peek into w.events. Used only by the
// test for synchronization; production Check always flushes. Access is under
// the beacon's shared lock to avoid a race with readLoop.
func hasBufferedEvents(b *InotifyBeacon) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, w := range b.watches {
		w.mu.Lock()
		n := len(w.events)
		w.mu.Unlock()
		if n > 0 {
			return true
		}
	}
	return false
}
