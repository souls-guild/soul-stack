package module_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"
)

// --- fakes ---

type fakeLookup map[string]*sharedhost.SigilRecord

func (f fakeLookup) Get(namespace, name string) *sharedhost.SigilRecord {
	return f[namespace+"/"+name]
}

type fakeChunkStream struct {
	grpc.ServerStreamingClient[keeperv1.PluginChunk]
	chunks [][]byte
	err    error // returned after chunks are exhausted, instead of io.EOF
}

func (s *fakeChunkStream) Recv() (*keeperv1.PluginChunk, error) {
	if len(s.chunks) == 0 {
		if s.err != nil {
			return nil, s.err
		}
		return nil, io.EOF
	}
	c := s.chunks[0]
	s.chunks = s.chunks[1:]
	return &keeperv1.PluginChunk{Data: c}, nil
}

type fakeFetcher struct {
	calls  int
	gotReq *keeperv1.PluginFetchRequest
	stream *fakeChunkStream
	err    error
}

func (f *fakeFetcher) FetchModule(_ context.Context, req *keeperv1.PluginFetchRequest) (grpc.ServerStreamingClient[keeperv1.PluginChunk], error) {
	f.calls++
	f.gotReq = req
	if f.err != nil {
		return nil, f.err
	}
	return f.stream, nil
}

// --- fixture ---

const testManifest = "kind: soul_module\nnamespace: community\nname: redis\nprotocol_version: 1\n" +
	"spec:\n  states:\n    present:\n      description: test state\n      input: {}\n"

type fixture struct {
	mod     *installmod.Module
	deps    installmod.Deps
	rec     *sharedhost.SigilRecord
	fetcher *fakeFetcher
	root    string
	binData []byte
	binSHA  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	binData := []byte("#!/bin/sh\necho soul-mod-redis fake binary\n")
	sum := sha256.Sum256(binData)
	binSHA := hex.EncodeToString(sum[:])

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	manifestDigest := sha256.Sum256(sharedhost.NormalizeManifestBytes([]byte(testManifest)))
	block := sharedhost.BuildSigilBlock("community", "redis", "v1.2.0", sum[:], manifestDigest[:])
	rec := &sharedhost.SigilRecord{
		Namespace:       "community",
		Name:            "redis",
		Ref:             "v1.2.0",
		BinarySHA256hex: binSHA,
		Signature:       ed25519.Sign(priv, block),
		Manifest:        []byte(testManifest),
	}

	root := t.TempDir()
	fetcher := &fakeFetcher{stream: &fakeChunkStream{chunks: chunked(binData, 16)}}
	deps := installmod.Deps{
		Sigils:      fakeLookup{"community/redis": rec},
		Anchors:     sharedhost.NewAnchorSet([]ed25519.PublicKey{pub}),
		ModulesRoot: root,
	}
	return &fixture{mod: installmod.New(deps), deps: deps, rec: rec, fetcher: fetcher, root: root, binData: binData, binSHA: binSHA}
}

func chunked(data []byte, size int) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := min(size, len(data))
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

func (f *fixture) apply(t *testing.T, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	return f.applyCtx(t, installmod.WithFetcher(context.Background(), f.fetcher), params)
}

func (f *fixture) applyCtx(t *testing.T, ctx context.Context, params map[string]any) *pluginv1.ApplyEvent {
	t.Helper()
	p, err := structpb.NewStruct(params)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	stream := &internaltest.ApplyStream{Ctx: ctx}
	if err := f.mod.Apply(&pluginv1.ApplyRequest{State: "installed", Params: p}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	last := stream.Last()
	if last == nil {
		t.Fatal("Apply не прислал финального события")
	}
	return last
}

func (f *fixture) binPath() string {
	return filepath.Join(f.root, "community-redis", "soul-mod-redis")
}

func (f *fixture) manifestPath() string {
	return filepath.Join(f.root, "community-redis", "manifest.yaml")
}

func wantFailedReason(t *testing.T, ev *pluginv1.ApplyEvent, reason string) {
	t.Helper()
	if !ev.GetFailed() {
		t.Fatalf("ожидался failed, получено changed=%v message=%q", ev.GetChanged(), ev.GetMessage())
	}
	if !strings.HasPrefix(ev.GetMessage(), reason+":") {
		t.Fatalf("message = %q; ожидался префикс %q", ev.GetMessage(), reason+":")
	}
}

// --- Validate ---

func TestValidate(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name   string
		state  string
		params map[string]any
		wantOK bool
	}{
		{"valid", "installed", map[string]any{"name": "community.redis"}, true},
		{"valid with ref", "installed", map[string]any{"name": "community.redis", "ref": "v1.2.0"}, true},
		{"missing name", "installed", map[string]any{}, false},
		{"name without dot", "installed", map[string]any{"name": "redis"}, false},
		{"name uppercase", "installed", map[string]any{"name": "Community.Redis"}, false},
		{"name with state suffix", "installed", map[string]any{"name": "community.redis.installed"}, false},
		{"name not a string", "installed", map[string]any{"name": 7}, false},
		{"ref not a string", "installed", map[string]any{"name": "community.redis", "ref": 1.5}, false},
		{"unknown state", "present", map[string]any{"name": "community.redis"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := structpb.NewStruct(tc.params)
			if err != nil {
				t.Fatalf("structpb.NewStruct: %v", err)
			}
			reply, err := f.mod.Validate(context.Background(), &pluginv1.ValidateRequest{State: tc.state, Params: p})
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if reply.GetOk() != tc.wantOK {
				t.Errorf("Ok = %v, want %v (errors: %v)", reply.GetOk(), tc.wantOK, reply.GetErrors())
			}
		})
	}
}

// --- Apply: allow-check BEFORE fetch ---

func TestApplyNotAllowedNoSigil(t *testing.T) {
	f := newFixture(t)
	ev := f.apply(t, map[string]any{"name": "community.mongo"})
	wantFailedReason(t, ev, "module_not_allowed")
	if f.fetcher.calls != 0 {
		t.Errorf("fetch вызван %d раз(а) до allow-check; должен не вызываться", f.fetcher.calls)
	}
}

func TestApplyNotAllowedRefMismatch(t *testing.T) {
	f := newFixture(t)
	ev := f.apply(t, map[string]any{"name": "community.redis", "ref": "v9.9.9"})
	wantFailedReason(t, ev, "module_not_allowed")
	if f.fetcher.calls != 0 {
		t.Errorf("fetch вызван при ref-mismatch; pin-сверка должна отказывать до fetch")
	}
}

// --- Apply: idempotency ---

func TestApplyIdempotentSkipsFetch(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Dir(f.binPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.binPath(), f.binData, 0o755); err != nil {
		t.Fatal(err)
	}

	ev := f.apply(t, map[string]any{"name": "community.redis"})
	if ev.GetFailed() {
		t.Fatalf("ожидался успех, получен failed: %q", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Error("changed = true; слот с тем же sha должен давать changed=false")
	}
	if f.fetcher.calls != 0 {
		t.Errorf("fetch вызван %d раз(а) при совпавшем sha; должен пропускаться", f.fetcher.calls)
	}
}

// --- Apply: fetch → verify → atomic install ---

func TestApplyInstallHappyPath(t *testing.T) {
	f := newFixture(t)
	ev := f.apply(t, map[string]any{"name": "community.redis", "ref": "v1.2.0"})
	if ev.GetFailed() {
		t.Fatalf("ожидался успех, получен failed: %q", ev.GetMessage())
	}
	if !ev.GetChanged() {
		t.Error("changed = false; установка нового модуля должна давать changed=true")
	}

	if f.fetcher.gotReq.GetNamespace() != "community" || f.fetcher.gotReq.GetName() != "redis" {
		t.Errorf("PluginFetchRequest ns/name = %q/%q", f.fetcher.gotReq.GetNamespace(), f.fetcher.gotReq.GetName())
	}
	if f.fetcher.gotReq.GetBinarySha256() != f.binSHA {
		t.Errorf("PluginFetchRequest.binary_sha256 = %q, want %q", f.fetcher.gotReq.GetBinarySha256(), f.binSHA)
	}

	got, err := os.ReadFile(f.binPath())
	if err != nil {
		t.Fatalf("бинарь не материализован: %v", err)
	}
	if string(got) != string(f.binData) {
		t.Error("содержимое установленного бинаря не совпало с fetch-байтами")
	}
	st, err := os.Stat(f.binPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("бинарь не исполняемый: mode %o", st.Mode().Perm())
	}

	mf, err := os.ReadFile(f.manifestPath())
	if err != nil {
		t.Fatalf("manifest.yaml не материализован: %v", err)
	}
	if string(mf) != testManifest {
		t.Errorf("manifest.yaml = %q; ожидались сырые байты manifest_raw допуска", string(mf))
	}
}

func TestApplyVerifyFailedWrongBytes(t *testing.T) {
	f := newFixture(t)
	f.fetcher.stream = &fakeChunkStream{chunks: [][]byte{[]byte("malicious payload")}}

	ev := f.apply(t, map[string]any{"name": "community.redis"})
	wantFailedReason(t, ev, "module_verify_failed")
	if _, err := os.Stat(f.binPath()); !os.IsNotExist(err) {
		t.Errorf("бинарь материализован при проваленном verify (stat err=%v)", err)
	}
}

func TestApplyVerifyFailedBadSignature(t *testing.T) {
	f := newFixture(t)
	f.rec.Signature = make([]byte, ed25519.SignatureSize)

	ev := f.apply(t, map[string]any{"name": "community.redis"})
	wantFailedReason(t, ev, "module_verify_failed")
	if _, err := os.Stat(f.binPath()); !os.IsNotExist(err) {
		t.Errorf("бинарь материализован при невалидной подписи (stat err=%v)", err)
	}
}

func TestApplyFetchErrorNotFound(t *testing.T) {
	f := newFixture(t)
	f.fetcher.err = status.Error(codes.NotFound, "module is not allowed")

	ev := f.apply(t, map[string]any{"name": "community.redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
}

func TestApplyFetchStreamBroken(t *testing.T) {
	f := newFixture(t)
	f.fetcher.stream = &fakeChunkStream{
		chunks: [][]byte{f.binData[:8]},
		err:    status.Error(codes.Unavailable, "stream reset"),
	}

	ev := f.apply(t, map[string]any{"name": "community.redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
	if _, err := os.Stat(f.binPath()); !os.IsNotExist(err) {
		t.Errorf("бинарь материализован при оборванном fetch (stat err=%v)", err)
	}
}

func TestApplyNoFetcherInContext(t *testing.T) {
	f := newFixture(t)
	ev := f.applyCtx(t, context.Background(), map[string]any{"name": "community.redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
}

func TestApplyUnknownState(t *testing.T) {
	f := newFixture(t)
	stream := &internaltest.ApplyStream{}
	if err := f.mod.Apply(&pluginv1.ApplyRequest{State: "absent"}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || !last.GetFailed() {
		t.Fatalf("ожидался failed на unknown state, получено %v", last)
	}
}
