package beacon

import (
	"context"
	"errors"
	"testing"
)

// fakeUsage returns a fixed usage percentage — deterministic,
// independent of the host's actual free space.
func fakeUsage(percent float64) func(string) (diskUsage, error) {
	return func(string) (diskUsage, error) { return diskUsage{usedPercent: percent}, nil }
}

func TestDiskFullOK(t *testing.T) {
	b := &DiskFull{Usage: fakeUsage(50)}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": "/"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateDiskOK {
		t.Fatalf("state = %q, want ok", state)
	}
	if data.GetFields()["used_percent"].GetNumberValue() != 50 {
		t.Error("data.used_percent must carry the actual percentage")
	}
	if data.GetFields()["threshold"].GetNumberValue() != diskFullDefaultThreshold {
		t.Error("data.threshold must carry the default threshold 90")
	}
}

func TestDiskFullOverThreshold(t *testing.T) {
	b := &DiskFull{Usage: fakeUsage(92)}
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": "/var"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateDiskFull {
		t.Fatalf("state = %q, want full (92%% ≥ 90%%)", state)
	}
}

func TestDiskFullAtThresholdIsFull(t *testing.T) {
	// Exactly at threshold → "full" (inclusive boundary: usage ≥ threshold).
	b := &DiskFull{Usage: fakeUsage(90)}
	state, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": "/"}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateDiskFull {
		t.Fatalf("state = %q, want full (90%% == threshold)", state)
	}
}

func TestDiskFullCustomThreshold(t *testing.T) {
	// 50% at threshold 40 → full; the same percentage at the default threshold would be ok.
	b := &DiskFull{Usage: fakeUsage(50)}
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path":              "/",
		"threshold_percent": 40,
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if state != stateDiskFull {
		t.Fatalf("state = %q, want full (50%% ≥ 40%%)", state)
	}
	if data.GetFields()["threshold"].GetNumberValue() != 40 {
		t.Error("data.threshold must carry the custom threshold 40")
	}
}

func TestDiskFullRealStatfs(t *testing.T) {
	// End-to-end run of the production sampler on t.TempDir (real filesystem):
	// percentage is in [0,100]; the check doesn't flake on the actual value.
	b := NewDiskFull()
	state, data, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path":              t.TempDir(),
		"threshold_percent": 100, // 100 → full only when the filesystem is completely full
	}))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	up := data.GetFields()["used_percent"].GetNumberValue()
	if up < 0 || up > 100 {
		t.Fatalf("used_percent outside [0,100]: %v", up)
	}
	// At threshold 100 the filesystem is usually "ok" (not packed to the brim); we
	// only check that state is one of the valid values, without pinning to the test host's actual usage.
	if state != stateDiskOK && state != stateDiskFull {
		t.Fatalf("unexpected state %q", state)
	}
}

func TestDiskFullStatfsError(t *testing.T) {
	b := &DiskFull{Usage: func(string) (diskUsage, error) { return diskUsage{}, errors.New("ENOENT") }}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{"path": "/nope"})); err == nil {
		t.Fatal("expected an error on statfs failure")
	}
}

func TestDiskFullMissingPath(t *testing.T) {
	b := &DiskFull{Usage: fakeUsage(10)}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{})); err == nil {
		t.Fatal("expected an error when param path is missing")
	}
}

func TestDiskFullInvalidThreshold(t *testing.T) {
	b := &DiskFull{Usage: fakeUsage(10)}
	if _, _, err := b.Check(context.Background(), paramStruct(t, map[string]any{
		"path":              "/",
		"threshold_percent": 0,
	})); err == nil {
		t.Fatal("expected an error when threshold_percent is outside 1..100")
	}
}
