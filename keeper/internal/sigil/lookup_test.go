package sigil

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
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

func (m mapSlotReader) SlotCommitSHA(string) (string, error) {
	return testCommitSHA, nil
}

const moduleSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func moduleSigil(sha string) *Sigil {
	return &Sigil{
		Alias:  "redis",
		Source: moduleSourceURL,
		Ref:    "v1.0.0",
		SHA256: sha,
		Schema: []byte(soulModuleSchemaJSON),
	}
}

func moduleSlot(sha string) *pluginhost.SlotContents {
	return &pluginhost.SlotContents{
		BinaryPath:   "/cache/redis/current/redis",
		SchemaBytes:  []byte(soulModuleSchemaJSON),
		BinarySHA256: sha,
	}
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
	slot.BinarySHA256 = moduleSHA
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
	sha, err := svc.Allow(context.Background(), AllowInput{
		Alias: "redis", Source: moduleSourceURL, Ref: "v1.0.0", CallerAID: "archon-ops",
	})
	if err != nil {
		t.Fatalf("Allow(kind=soul_module): %v", err)
	}
	if sha != moduleSHA {
		t.Fatalf("sha = %q, want %q", sha, moduleSHA)
	}
	if store.inserted == nil || store.inserted.Alias != "redis" || store.inserted.Source != moduleSourceURL {
		t.Fatalf("inserted = %+v", store.inserted)
	}
}
