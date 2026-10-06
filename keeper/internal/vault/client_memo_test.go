package vault

import (
	"context"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
	"github.com/souls-guild/soul-stack/shared/config"
)

func newMemoTestClient(t *testing.T) (*fakeVaultMux, *Client) {
	t.Helper()
	mux, addr := startFakeVault(t, "secret")
	mux.secrets["app/creds"] = map[string]any{"password": "before-write"}
	mux.secrets["app/other"] = map[string]any{"token": "neighbour"}
	cl, err := NewClient(context.Background(), config.KeeperVault{Addr: addr, Token: "root", KVMount: "secret"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return mux, cl
}

// creds spellings that address one Vault entry and are three memo keys.
var credsSpellings = []string{"secret/app/creds", "/secret/app/creds", "app/creds"}

func primeMemo(t *testing.T, ctx context.Context, cl *Client) {
	t.Helper()
	for _, p := range append(append([]string(nil), credsSpellings...), "secret/app/other") {
		if _, err := cel.ReadKVMemoized(ctx, cl, p); err != nil {
			t.Fatalf("prime %s: %v", p, err)
		}
	}
}

// ★ The invalidation half of the run-wide vault() memo (NIM-934). A keeper-side step
// writes Vault mid-run through this client, on the run's context; every memoized read
// of that secret — in any spelling the client resolves to the same entry — must be
// dropped so the next pass reads the written value, and a neighbour must stay
// memoized. Without the forget in WriteKV the re-reads return before-write.
func TestWriteKV_ForgetsMemoizedReadsOfThatSecret(t *testing.T) {
	mux, cl := newMemoTestClient(t)
	ctx := cel.WithVaultMemo(context.Background())
	primeMemo(t, ctx, cl)
	before := mux.readCount("app/creds")

	if err := cl.WriteKV(ctx, "/secret/app/creds", map[string]any{"password": "after-write"}); err != nil {
		t.Fatalf("WriteKV: %v", err)
	}

	for _, p := range credsSpellings {
		got, err := cel.ReadKVMemoized(ctx, cl, p)
		if err != nil {
			t.Fatalf("re-read %s: %v", p, err)
		}
		if got["password"] != "after-write" {
			t.Errorf("%s after the write = %v, want after-write (memo served the value from before the write)", p, got["password"])
		}
	}
	if got := mux.readCount("app/creds") - before; got != len(credsSpellings) {
		t.Errorf("Vault reads of app/creds after the write = %d, want %d (each spelling read again)", got, len(credsSpellings))
	}
	if _, err := cel.ReadKVMemoized(ctx, cl, "secret/app/other"); err != nil {
		t.Fatalf("re-read other: %v", err)
	}
	if got := mux.readCount("app/other"); got != 1 {
		t.Errorf("Vault reads of app/other = %d, want 1 (an unwritten neighbour stays memoized)", got)
	}
}

// stubReader serves the memo without Vault, so a spelling the HTTP fake cannot serve
// can still be memoized.
type stubReader map[string]map[string]any

func (s stubReader) ReadKV(_ context.Context, p string) (map[string]any, error) {
	return s[p], nil
}

// Reads spelled with doubled slashes, inside the path or right after the mount, may
// reach the same entry, so a write to the clean path forgets them too.
func TestWriteKV_ForgetsDoubledSlashSpellings(t *testing.T) {
	_, cl := newMemoTestClient(t)
	ctx := cel.WithVaultMemo(context.Background())
	spellings := []string{"secret/app//creds", "secret///app/creds"}
	stale, fresh := stubReader{}, stubReader{}
	for _, p := range spellings {
		stale[p] = map[string]any{"password": "before-write"}
		fresh[p] = map[string]any{"password": "after-write"}
		if _, err := cel.ReadKVMemoized(ctx, stale, p); err != nil {
			t.Fatalf("prime %s: %v", p, err)
		}
	}

	if err := cl.WriteKV(ctx, "secret/app/creds", map[string]any{"password": "after-write"}); err != nil {
		t.Fatalf("WriteKV: %v", err)
	}

	for _, p := range spellings {
		got, err := cel.ReadKVMemoized(ctx, fresh, p)
		if err != nil {
			t.Fatalf("re-read %s: %v", p, err)
		}
		if got["password"] != "after-write" {
			t.Errorf("%s after a write to secret/app/creds = %v, want after-write", p, got["password"])
		}
	}
}

// A write Vault answered with an error may still have landed, so it forgets too.
func TestWriteKV_FailedWriteStillForgets(t *testing.T) {
	mux, cl := newMemoTestClient(t)
	ctx := cel.WithVaultMemo(context.Background())
	primeMemo(t, ctx, cl)
	before := mux.readCount("app/creds")

	mux.mu.Lock()
	mux.failWrites = true
	mux.mu.Unlock()
	if err := cl.WriteKV(ctx, "secret/app/creds", map[string]any{"password": "maybe"}); err == nil {
		t.Fatal("WriteKV succeeded against a refusing Vault")
	}

	if _, err := cel.ReadKVMemoized(ctx, cl, "secret/app/creds"); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got := mux.readCount("app/creds") - before; got != 1 {
		t.Errorf("Vault reads after a failed write = %d, want 1 (the failed write must forget too)", got)
	}
}

// A write outside a run (the API, the Reaper) has no memo on its context and must
// still work.
func TestWriteKV_WithoutMemoWrites(t *testing.T) {
	mux, cl := newMemoTestClient(t)
	if err := cl.WriteKV(context.Background(), "secret/app/creds", map[string]any{"password": "x"}); err != nil {
		t.Fatalf("WriteKV: %v", err)
	}
	mux.mu.Lock()
	got := mux.secrets["app/creds"]["password"]
	mux.mu.Unlock()
	if got != "x" {
		t.Fatalf("stored = %v, want x", got)
	}
}
