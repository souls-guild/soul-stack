package module_test

import (
	"os"
	"path/filepath"
	"testing"

	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"
)

// withRescan пересобирает модуль fixture с Rescan-счётчиком (S4, ADR-065(d)).
func (f *fixture) withRescan(calls *int) {
	deps := f.deps
	deps.Rescan = func() { *calls++ }
	f.mod = installmod.New(deps)
}

func TestApplyInstalledChangedCallsRescan(t *testing.T) {
	f := newFixture(t)
	var calls int
	f.withRescan(&calls)

	ev := f.apply(t, map[string]any{"name": "community.redis"})
	if ev.GetFailed() || !ev.GetChanged() {
		t.Fatalf("ожидался changed=true, получено failed=%v changed=%v message=%q",
			ev.GetFailed(), ev.GetChanged(), ev.GetMessage())
	}
	if calls != 1 {
		t.Errorf("Rescan вызван %d раз(а) после установки, want 1", calls)
	}
}

func TestApplyInstalledIdempotentSkipsRescan(t *testing.T) {
	f := newFixture(t)
	var calls int
	f.withRescan(&calls)
	if err := os.MkdirAll(filepath.Dir(f.binPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.binPath(), f.binData, 0o755); err != nil {
		t.Fatal(err)
	}

	ev := f.apply(t, map[string]any{"name": "community.redis"})
	if ev.GetFailed() || ev.GetChanged() {
		t.Fatalf("ожидался идемпотентный changed=false, получено failed=%v changed=%v message=%q",
			ev.GetFailed(), ev.GetChanged(), ev.GetMessage())
	}
	if calls != 0 {
		t.Errorf("Rescan вызван %d раз(а) на идемпотентном skip-е, want 0", calls)
	}
}

func TestApplyInstalledFailedSkipsRescan(t *testing.T) {
	f := newFixture(t)
	var calls int
	f.withRescan(&calls)
	f.fetcher.stream = &fakeChunkStream{chunks: [][]byte{[]byte("malicious payload")}}

	ev := f.apply(t, map[string]any{"name": "community.redis"})
	if !ev.GetFailed() {
		t.Fatalf("ожидался failed на verify, получено changed=%v", ev.GetChanged())
	}
	if calls != 0 {
		t.Errorf("Rescan вызван %d раз(а) на проваленной установке, want 0", calls)
	}
}
