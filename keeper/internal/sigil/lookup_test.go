package sigil

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/pluginhost"
)

const soulModuleManifestYAML = `kind: soul_module
protocol_version: 1
namespace: community
name: redis
spec:
  states:
    pinged: {}
`

// mapSlotReader is SlotReader keyed by "<ns>-<name>" (for lookup tests with
// multiple slots; fakeSlotReader returns a single fixed slot).
type mapSlotReader struct {
	slots map[string]*pluginhost.SlotContents
}

func (m mapSlotReader) ReadSlot(ns, name string) (*pluginhost.SlotContents, error) {
	if s, ok := m.slots[ns+"-"+name]; ok {
		return s, nil
	}
	return nil, pluginhost.ErrSlotNotFound
}

func (m mapSlotReader) SlotCommitSHA(string, string) (string, error) {
	return testCommitSHA, nil
}

const moduleSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func moduleSigil(sha string) *Sigil {
	return &Sigil{
		Namespace:   "community",
		Name:        "redis",
		Ref:         "v1.0.0",
		SHA256:      sha,
		ManifestRaw: []byte(soulModuleManifestYAML),
	}
}

func moduleSlot(sha string) *pluginhost.SlotContents {
	return &pluginhost.SlotContents{
		BinaryPath:    "/cache/community-redis/current/soul-mod-redis",
		ManifestBytes: []byte(soulModuleManifestYAML),
		BinarySHA256:  sha,
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
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"community-redis": moduleSlot(moduleSHA)}},
	)
	path, err := svc.LookupModuleBinary(context.Background(), moduleSHA)
	if err != nil {
		t.Fatalf("LookupModuleBinary: %v", err)
	}
	if path != "/cache/community-redis/current/soul-mod-redis" {
		t.Fatalf("path = %q", path)
	}
}

func TestService_LookupModuleBinary_NotAllowed(t *testing.T) {
	svc := lookupService(t,
		&fakeStore{listResult: []*Sigil{moduleSigil(moduleSHA)}},
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"community-redis": moduleSlot(moduleSHA)}},
	)
	_, err := svc.LookupModuleBinary(context.Background(), strings.Repeat("bb", 32))
	if !errors.Is(err, ErrModuleNotAllowed) {
		t.Fatalf("err = %v, want ErrModuleNotAllowed", err)
	}
}

func TestService_LookupModuleBinary_WrongKindRejected(t *testing.T) {
	// Active allow with same sha but kind=cloud_driver — NOT a module, reject.
	rec := moduleSigil(moduleSHA)
	rec.Namespace, rec.Name = "cloud", "hetzner"
	rec.ManifestRaw = []byte(cloudManifestYAML)
	slot := slotFixture()
	slot.BinarySHA256 = moduleSHA
	svc := lookupService(t,
		&fakeStore{listResult: []*Sigil{rec}},
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"cloud-hetzner": slot}},
	)
	_, err := svc.LookupModuleBinary(context.Background(), moduleSHA)
	if !errors.Is(err, ErrModuleNotAllowed) {
		t.Fatalf("err = %v, want ErrModuleNotAllowed (kind=cloud_driver)", err)
	}
}

func TestService_LookupModuleBinary_SlotDriftRejected(t *testing.T) {
	// current-symlink moved: slot carries DIFFERENT binary → allow sha no longer in cache,
	// fail-closed.
	driftSHA := strings.Repeat("cc", 32)
	svc := lookupService(t,
		&fakeStore{listResult: []*Sigil{moduleSigil(moduleSHA)}},
		mapSlotReader{slots: map[string]*pluginhost.SlotContents{"community-redis": moduleSlot(driftSHA)}},
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
		Namespace: "community", Name: "redis", Ref: "v1.0.0", CallerAID: "archon-ops",
	})
	if err != nil {
		t.Fatalf("Allow(kind=soul_module): %v", err)
	}
	if sha != moduleSHA {
		t.Fatalf("sha = %q, want %q", sha, moduleSHA)
	}
	if store.inserted == nil || store.inserted.Namespace != "community" || store.inserted.Name != "redis" {
		t.Fatalf("inserted = %+v", store.inserted)
	}
}
