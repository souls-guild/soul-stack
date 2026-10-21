package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// treeVault serves the two endpoints DeleteKV and ListKVEntries reach, over a flat
// set of mount-relative entry names, on either KV version, and records every KV
// request so a test can assert WHICH path was deleted rather than only that the
// entry disappeared from the fake.
type treeVault struct {
	mount   string
	version string

	mu       sync.Mutex
	entries  map[string]bool
	requests []string
}

func (v *treeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	v.mu.Lock()
	defer v.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v1/sys/health":
		_, _ = w.Write([]byte(`{"initialized":true,"sealed":false,"standby":false}`))
		return
	case "/v1/sys/internal/ui/mounts/" + v.mount:
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"type": "kv", "options": map[string]any{"version": v.version},
		}})
		return
	}

	base := "/v1/" + v.mount + "/"
	if v.version == "2" {
		base += "metadata/"
	}
	if !strings.HasPrefix(r.URL.Path, base) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rel := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, base), "/")
	list := r.Method == http.MethodGet && r.URL.Query().Get("list") == "true"
	if list {
		v.requests = append(v.requests, "LIST "+r.URL.Path)
	} else {
		v.requests = append(v.requests, r.Method+" "+r.URL.Path)
	}

	switch {
	case list:
		seen := map[string]bool{}
		for e := range v.entries {
			rest, ok := strings.CutPrefix(e, rel+"/")
			if !ok {
				continue
			}
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				seen[rest[:i+1]] = true
			} else {
				seen[rest] = true
			}
		}
		if len(seen) == 0 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		keys := make([]any, 0, len(seen))
		for k := range seen {
			keys = append(keys, k)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
	case r.Method == http.MethodGet:
		if !v.entries[rel] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"current_version": 3}})
	case r.Method == http.MethodDelete:
		delete(v.entries, rel)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (v *treeVault) deletes() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	var out []string
	for _, r := range v.requests {
		if strings.HasPrefix(r, http.MethodDelete+" ") {
			out = append(out, r)
		}
	}
	return out
}

func newTreeVault(t *testing.T, mount, version string, entries ...string) (*treeVault, *Client) {
	t.Helper()
	v := &treeVault{mount: mount, version: version, entries: map[string]bool{}}
	for _, e := range entries {
		v.entries[e] = true
	}
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	cl, err := NewClient(context.Background(), config.KeeperVault{Addr: srv.URL, Token: "root", KVMount: mount})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return v, cl
}

// TestDeleteKV_V2DeletesTheMetadata — on KV v2 the delete goes to the METADATA path,
// which is what removes every version. A delete aimed at `data/` would soft-delete the
// latest version only, leave the rest recoverable, and still look done to ReadKV.
func TestDeleteKV_V2DeletesTheMetadata(t *testing.T) {
	v, cl := newTreeVault(t, "secret", "2", "svc/inc/users/alice")

	existed, err := cl.DeleteKV(context.Background(), "secret/svc/inc/users/alice")
	if err != nil {
		t.Fatalf("DeleteKV: %v", err)
	}
	if !existed {
		t.Error("DeleteKV of an existing entry reported existed=false")
	}
	want := []string{"DELETE /v1/secret/metadata/svc/inc/users/alice"}
	if got := v.deletes(); !reflect.DeepEqual(got, want) {
		t.Errorf("delete requests = %v, want %v", got, want)
	}
}

// TestDeleteKV_AbsentEntryIsNotDeleted — a missing entry is reported as such and no
// DELETE goes out, which is what lets the step report changed=false on a re-run.
func TestDeleteKV_AbsentEntryIsNotDeleted(t *testing.T) {
	v, cl := newTreeVault(t, "secret", "2")

	existed, err := cl.DeleteKV(context.Background(), "svc/inc/users/alice")
	if err != nil {
		t.Fatalf("DeleteKV: %v", err)
	}
	if existed {
		t.Error("DeleteKV of a missing entry reported existed=true")
	}
	if got := v.deletes(); len(got) != 0 {
		t.Errorf("a missing entry was still deleted: %v", got)
	}
}

// TestDeleteKV_V1DeletesThePath — KV v1 has no metadata; the path itself goes.
func TestDeleteKV_V1DeletesThePath(t *testing.T) {
	v, cl := newTreeVault(t, "kv", "1", "svc/inc/cfg")

	existed, err := cl.DeleteKV(context.Background(), "svc/inc/cfg")
	if err != nil || !existed {
		t.Fatalf("DeleteKV = (%v, %v), want (true, nil)", existed, err)
	}
	want := []string{"DELETE /v1/kv/svc/inc/cfg"}
	if got := v.deletes(); !reflect.DeepEqual(got, want) {
		t.Errorf("delete requests = %v, want %v", got, want)
	}
}

// TestDeleteKV_RefusesDotDot — the shared path guard covers the delete too.
func TestDeleteKV_RefusesDotDot(t *testing.T) {
	v, cl := newTreeVault(t, "secret", "2", "keeper/jwt-signing-key")

	if _, err := cl.DeleteKV(context.Background(), "svc/inc/../../keeper/jwt-signing-key"); err == nil {
		t.Fatal("DeleteKV accepted a path with a `..` segment")
	}
	if got := v.deletes(); len(got) != 0 {
		t.Errorf("a `..` path reached Vault: %v", got)
	}
}

// TestListKVEntries_KeepsFolders — a recursive walk needs the sub-folders that
// ListKV drops; they come back marked with a trailing `/`, sorted.
func TestListKVEntries_KeepsFolders(t *testing.T) {
	_, cl := newTreeVault(t, "secret", "2", "svc/inc/b", "svc/inc/a", "svc/inc/users/x", "svc/other/z")

	got, err := cl.ListKVEntries(context.Background(), "secret/svc/inc")
	if err != nil {
		t.Fatalf("ListKVEntries: %v", err)
	}
	want := []string{"a", "b", "users/"}
	if !sort.StringsAreSorted(got) || !reflect.DeepEqual(got, want) {
		t.Errorf("ListKVEntries = %v, want %v", got, want)
	}
}

// TestListKVEntries_MissingPrefix — nothing under the prefix is an empty answer, not
// an error: a re-run of a recursive delete lists an already-empty subtree.
func TestListKVEntries_MissingPrefix(t *testing.T) {
	_, cl := newTreeVault(t, "kv", "1")

	got, err := cl.ListKVEntries(context.Background(), "svc/inc")
	if err != nil || got != nil {
		t.Errorf("ListKVEntries on a missing prefix = (%v, %v), want (nil, nil)", got, err)
	}
}
