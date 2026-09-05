package sigil

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
)

// soulModuleSchemaJSON is a canonical soul_module document. The modules are named;
// the ARTIFACT is not — address level 1 comes from the grant's alias.
const soulModuleSchemaJSON = `{"kind":"soul_module","modules":[{"name":"acl","states":{"present":{"description":"the ACL user exists"}}}],"protocol_version":1}`

const moduleSourceURL = "https://example.com/soul-mod-redis.git"

// mapSlotReader is a SlotReader keyed by ALIAS (for lookup tests with several slots;
// fakeSlotReader returns a single fixed slot).
type mapSlotReader struct {
	slots map[string]*pluginhost.SlotContents
}

func (m mapSlotReader) ReadSlot(alias string) (*pluginhost.SlotContents, error) {
	if s, ok := m.slots[alias]; ok {
		return s, nil
	}
	return nil, pluginhost.ErrSlotNotFound
}

// ArtifactByDigest answers from the same fixture slots, so a test that changes what a
// slot holds changes both reads at once.
func (m mapSlotReader) ArtifactByDigest(alias, sha string) (string, error) {
	s, ok := m.slots[alias]
	if !ok {
		return "", pluginhost.ErrSlotNotFound
	}
	for _, a := range s.Artifacts {
		if a.SHA256 == sha {
			return a.BinaryPath, nil
		}
	}
	return "", pluginhost.ErrSlotNotFound
}

func (m mapSlotReader) SlotCommitSHA(string) (string, error) {
	return testCommitSHA, nil
}

const moduleSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func moduleSigil(sha string) *Sigil {
	return &Sigil{
		Alias:     "redis",
		Source:    moduleSourceURL,
		Ref:       "v1.0.0",
		Kind:      sharedplugin.SourceKindGit,
		Artifacts: []sharedhost.SigilArtifact{{SHA256: sha}},
		Schema:    []byte(soulModuleSchemaJSON),
	}
}

func moduleSlot(sha string) *pluginhost.SlotContents {
	return gitSlot("/cache/redis/current/redis", soulModuleSchemaJSON, sha)
}

func lookupService(t *testing.T, store Store, slots SlotReader) *Service {
	t.Helper()
	svc, err := NewService(ServiceDeps{Signer: testSigner(t), Store: store, Slots: slots})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestService_LookupModuleBinary_Allowed(t *testing.T) {
	svc := lookupService(t,
		&fakeStore{listResult: []*Sigil{moduleSigil(moduleSHA)}},
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"redis": moduleSlot(moduleSHA)}},
	)
	path, err := svc.LookupModuleBinary(context.Background(), moduleSHA)
	if err != nil {
		t.Fatalf("LookupModuleBinary: %v", err)
	}
	if path != "/cache/redis/current/redis" {
		t.Fatalf("path = %q", path)
	}
}

func TestService_LookupModuleBinary_NotAllowed(t *testing.T) {
	svc := lookupService(t,
		&fakeStore{listResult: []*Sigil{moduleSigil(moduleSHA)}},
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"redis": moduleSlot(moduleSHA)}},
	)
	_, err := svc.LookupModuleBinary(context.Background(), strings.Repeat("bb", 32))
	if !errors.Is(err, ErrModuleNotAllowed) {
		t.Fatalf("err = %v, want ErrModuleNotAllowed", err)
	}
}

func TestService_LookupModuleBinary_WrongKindRejected(t *testing.T) {
	// An active grant on the same sha but kind=ssh_provider — NOT a module, reject.
	// The kind is read from the grant's SIGNED schema, not from the cache: the grant is
	// what was approved, the cache is what a resolver last wrote there.
	rec := moduleSigil(moduleSHA)
	rec.Alias = "hetzner"
	rec.Schema = []byte(sshSchemaJSON)
	slot := slotFixture()
	slot.Artifacts[0].SHA256 = moduleSHA
	svc := lookupService(t,
		&fakeStore{listResult: []*Sigil{rec}},
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"hetzner": slot}},
	)
	_, err := svc.LookupModuleBinary(context.Background(), moduleSHA)
	if !errors.Is(err, ErrModuleNotAllowed) {
		t.Fatalf("err = %v, want ErrModuleNotAllowed (kind=ssh_provider)", err)
	}
}

func TestService_LookupModuleBinary_SlotDriftRejected(t *testing.T) {
	// current-symlink moved: slot carries DIFFERENT binary → allow sha no longer in cache,
	// fail-closed.
	driftSHA := strings.Repeat("cc", 32)
	svc := lookupService(t,
		&fakeStore{listResult: []*Sigil{moduleSigil(moduleSHA)}},
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"redis": moduleSlot(driftSHA)}},
	)
	_, err := svc.LookupModuleBinary(context.Background(), moduleSHA)
	if !errors.Is(err, ErrModuleNotAllowed) {
		t.Fatalf("err = %v, want ErrModuleNotAllowed (slot drift)", err)
	}
}

func TestService_LookupModuleBinary_StoreError(t *testing.T) {
	boom := errors.New("pg down")
	svc := lookupService(t, &fakeStore{listErr: boom}, fakeSlotReader{})
	_, err := svc.LookupModuleBinary(context.Background(), moduleSHA)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped store error", err)
	}
}

// Allow is kind-agnostic (guard): SoulModule-plugin allow follows same path
// as cloud/ssh — kind not restricted anywhere (live-verified 201 on bench).
func TestService_Allow_SoulModuleKindAgnostic(t *testing.T) {
	store := &fakeStore{}
	svc := lookupService(t, store, fakeSlotReader{slot: moduleSlot(moduleSHA), commit: testCommitSHA})
	approved, err := svc.Allow(context.Background(), AllowInput{
		Alias: "redis", Source: moduleSourceURL, Ref: "v1.0.0", CallerAID: "archon-ops",
	})
	if err != nil {
		t.Fatalf("Allow(kind=soul_module): %v", err)
	}
	if len(approved.Artifacts) != 1 || approved.Artifacts[0].SHA256 != moduleSHA {
		t.Fatalf("approved = %+v, want the one row %q", approved.Artifacts, moduleSHA)
	}
	if store.inserted == nil || store.inserted.Alias != "redis" || store.inserted.Source != moduleSourceURL {
		t.Fatalf("inserted = %+v", store.inserted)
	}
}

// narrowOnlySlotReader answers by digest and fails the test if anyone reaches for the
// whole-release read. The delivery path names a sha256 and streams one file
// (NIM-816); [pluginhost.ReadSlot] there re-derived every platform's digest, read
// every trailer and validated every schema document to answer about one of them.
type narrowOnlySlotReader struct {
	t     *testing.T
	slots map[string]*pluginhost.SlotContents
	asked []string
}

func (r *narrowOnlySlotReader) ReadSlot(alias string) (*pluginhost.SlotContents, error) {
	r.t.Helper()
	r.t.Errorf("the delivery path re-read the WHOLE release of %q to serve one file", alias)
	return nil, pluginhost.ErrSlotNotFound
}

func (r *narrowOnlySlotReader) ArtifactByDigest(alias, sha string) (string, error) {
	r.asked = append(r.asked, alias+"@"+sha)
	s, ok := r.slots[alias]
	if !ok {
		return "", pluginhost.ErrSlotNotFound
	}
	for _, a := range s.Artifacts {
		if a.SHA256 == sha {
			return a.BinaryPath, nil
		}
	}
	return "", pluginhost.ErrSlotNotFound
}

func (r *narrowOnlySlotReader) SlotCommitSHA(string) (string, error) {
	return testCommitSHA, nil
}

// The wiring guard for NIM-816: LookupModuleBinary reaches the cache by digest, once,
// for the alias whose grant approves that digest — and never through the whole-release
// read. Restore the ReadSlot call and this test says so by name.
func TestService_LookupModuleBinary_ReadsTheCacheByDigest(t *testing.T) {
	slots := &narrowOnlySlotReader{
		t:     t,
		slots: map[string]*pluginhost.SlotContents{"redis": moduleSlot(moduleSHA)},
	}
	svc := lookupService(t, &fakeStore{listResult: []*Sigil{moduleSigil(moduleSHA)}}, slots)

	path, err := svc.LookupModuleBinary(context.Background(), moduleSHA)
	if err != nil {
		t.Fatalf("LookupModuleBinary: %v", err)
	}
	if path != "/cache/redis/current/redis" {
		t.Fatalf("path = %q", path)
	}
	if len(slots.asked) != 1 {
		t.Fatalf("cache reads = %d, want 1 — one file is served, so one file is checked", len(slots.asked))
	}
	if slots.asked[0] != "redis@"+moduleSHA {
		t.Errorf("cache read = %q, want the digest asked for under the granting alias", slots.asked[0])
	}
}
