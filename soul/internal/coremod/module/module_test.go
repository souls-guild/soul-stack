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
	"github.com/souls-guild/soul-stack/sdk/schema"
	sharedhost "github.com/souls-guild/soul-stack/shared/pluginhost"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	installmod "github.com/souls-guild/soul-stack/soul/internal/coremod/module"
)

// --- fakes ---

type fakeLookup map[string]*sharedhost.SigilRecord

func (f fakeLookup) Get(alias string) *sharedhost.SigilRecord { return f[alias] }

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

// The registration the test slot is named by, and the source the grant is signed over.
// They are separate on purpose: the alias is an operator's local choice, the source is
// what a signature can be about.
const (
	testAlias  = "redis"
	testSource = "https://github.com/souls-guild/soul-mod-redis"
	testRef    = "v1.2.0"
)

// testSchemaDoc is the canonical schema document the grant carries — a bundle serving
// two modules, so the fixture is the real shape rather than a degenerate single-module
// one.
func testSchemaDoc(t *testing.T) []byte {
	t.Helper()
	doc := schema.Document{
		Kind:            schema.KindSoulModule,
		ProtocolVersion: 1,
		Modules: []schema.Module{
			{Name: "acl", Description: "acl", States: map[string]schema.State{"present": {Description: "test state"}}},
			{Name: "config", Description: "config", States: map[string]schema.State{"present": {Description: "test state"}}},
		},
	}
	out, err := schema.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	return out
}

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
	schemaDoc := testSchemaDoc(t)
	schemaDigest := sharedhost.SchemaDigest(schemaDoc)
	block := sharedhost.BuildSigilBlock(testSource, testRef, sum[:], schemaDigest[:])
	rec := &sharedhost.SigilRecord{
		Alias:           testAlias,
		Source:          testSource,
		Ref:             testRef,
		BinarySHA256hex: binSHA,
		Signature:       ed25519.Sign(priv, block),
		Schema:          schemaDoc,
	}

	root := t.TempDir()
	fetcher := &fakeFetcher{stream: &fakeChunkStream{chunks: chunked(binData, 16)}}
	deps := installmod.Deps{
		Sigils:      fakeLookup{testAlias: rec},
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
		t.Fatal("Apply did not send a final event")
	}
	return last
}

// The slot is named by the alias, and so is the single executable inside it: the
// artifact has no name of its own to use.
func (f *fixture) binPath() string {
	return filepath.Join(f.root, testAlias, testAlias)
}

func wantFailedReason(t *testing.T, ev *pluginv1.ApplyEvent, reason string) {
	t.Helper()
	if !ev.GetFailed() {
		t.Fatalf("expected failed, got changed=%v message=%q", ev.GetChanged(), ev.GetMessage())
	}
	if !strings.HasPrefix(ev.GetMessage(), reason+":") {
		t.Fatalf("message = %q; expected prefix %q", ev.GetMessage(), reason+":")
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
		{"valid", "installed", map[string]any{"name": "redis"}, true},
		{"valid with ref", "installed", map[string]any{"name": "redis", "ref": "v1.2.0"}, true},
		{"missing name", "installed", map[string]any{}, false},
		{"name is an address, not an alias", "installed", map[string]any{"name": "redis.instance"}, false},
		{"name uppercase", "installed", map[string]any{"name": "Redis"}, false},
		{"name with state suffix", "installed", map[string]any{"name": "redis.acl.present"}, false},
		{"name not a string", "installed", map[string]any{"name": 7}, false},
		{"ref not a string", "installed", map[string]any{"name": "redis", "ref": 1.5}, false},
		{"unknown state", "present", map[string]any{"name": "redis"}, false},
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
	ev := f.apply(t, map[string]any{"name": "mongo"})
	wantFailedReason(t, ev, "module_not_allowed")
	if f.fetcher.calls != 0 {
		t.Errorf("fetch called %d time(s) before allow-check; must not be called", f.fetcher.calls)
	}
}

func TestApplyNotAllowedRefMismatch(t *testing.T) {
	f := newFixture(t)
	ev := f.apply(t, map[string]any{"name": "redis", "ref": "v9.9.9"})
	wantFailedReason(t, ev, "module_not_allowed")
	if f.fetcher.calls != 0 {
		t.Errorf("fetch called on ref-mismatch; pin check must reject before fetch")
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

	ev := f.apply(t, map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("expected success, got failed: %q", ev.GetMessage())
	}
	if ev.GetChanged() {
		t.Error("changed = true; a slot with the same sha should give changed=false")
	}
	if f.fetcher.calls != 0 {
		t.Errorf("fetch called %d time(s) with a matching sha; should be skipped", f.fetcher.calls)
	}
}

// --- Apply: fetch → verify → atomic install ---

func TestApplyInstallHappyPath(t *testing.T) {
	f := newFixture(t)
	ev := f.apply(t, map[string]any{"name": "redis", "ref": "v1.2.0"})
	if ev.GetFailed() {
		t.Fatalf("expected success, got failed: %q", ev.GetMessage())
	}
	if !ev.GetChanged() {
		t.Error("changed = false; installing a new module should give changed=true")
	}

	if f.fetcher.gotReq.GetAlias() != testAlias {
		t.Errorf("PluginFetchRequest.alias = %q, want %q", f.fetcher.gotReq.GetAlias(), testAlias)
	}
	if f.fetcher.gotReq.GetBinarySha256() != f.binSHA {
		t.Errorf("PluginFetchRequest.binary_sha256 = %q, want %q", f.fetcher.gotReq.GetBinarySha256(), f.binSHA)
	}

	got, err := os.ReadFile(f.binPath())
	if err != nil {
		t.Fatalf("binary not materialized: %v", err)
	}
	if string(got) != string(f.binData) {
		t.Error("installed binary content did not match fetch bytes")
	}
	st, err := os.Stat(f.binPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("binary is not executable: mode %o", st.Mode().Perm())
	}

	// The slot holds exactly one file: the artifact. No schema file is written —
	// the schema rides in the artifact's trailer, which is what discovery reads.
	entries, err := os.ReadDir(filepath.Dir(f.binPath()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != testAlias {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("slot contents = %v, want just the artifact %q", names, testAlias)
	}
}

// GUARD: installing over a slot that already holds a differently-named executable
// leaves exactly one behind. Two would make the slot ambiguous and discovery would
// refuse it — the install would appear to succeed and the module would vanish.
func TestApplyInstallClearsAForeignArtifact(t *testing.T) {
	f := newFixture(t)
	slotDir := filepath.Dir(f.binPath())
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(slotDir, "soul-mod-redis")
	if err := os.WriteFile(stale, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ev := f.apply(t, map[string]any{"name": "redis"})
	if ev.GetFailed() {
		t.Fatalf("expected success, got failed: %q", ev.GetMessage())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("the stale artifact survived the install (stat err=%v)", err)
	}
	if _, err := os.Stat(f.binPath()); err != nil {
		t.Errorf("the new artifact was not installed: %v", err)
	}
}

// GUARD: the grant is looked up by the alias the task names. A grant for another
// registration is no grant, and nothing is fetched.
func TestApplyLookupIsByAlias(t *testing.T) {
	f := newFixture(t)
	f.deps.Sigils = fakeLookup{"redis-community": f.rec}
	f.mod = installmod.New(f.deps)

	ev := f.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_not_allowed")
	if f.fetcher.calls != 0 {
		t.Errorf("fetch called %d time(s) without a matching grant", f.fetcher.calls)
	}
}

// The kind comes from the grant's schema bytes — the ones the signature covers —
// because at allow-check time the artifact has not been fetched, let alone verified.
func TestApplyRefusesGrantOfAnotherKind(t *testing.T) {
	f := newFixture(t)
	sshDoc, err := schema.Marshal(schema.Document{
		Kind:            schema.KindSSHProvider,
		ProtocolVersion: 1,
		ProviderKind:    "vault_ssh_ca",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.rec.Schema = sshDoc

	ev := f.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_not_allowed")
	if f.fetcher.calls != 0 {
		t.Errorf("fetch called %d time(s) for a grant of the wrong kind", f.fetcher.calls)
	}
}

func TestApplyVerifyFailedWrongBytes(t *testing.T) {
	f := newFixture(t)
	f.fetcher.stream = &fakeChunkStream{chunks: [][]byte{[]byte("malicious payload")}}

	ev := f.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_verify_failed")
	if _, err := os.Stat(f.binPath()); !os.IsNotExist(err) {
		t.Errorf("binary materialized on failed verify (stat err=%v)", err)
	}
}

func TestApplyVerifyFailedBadSignature(t *testing.T) {
	f := newFixture(t)
	f.rec.Signature = make([]byte, ed25519.SignatureSize)

	ev := f.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_verify_failed")
	if _, err := os.Stat(f.binPath()); !os.IsNotExist(err) {
		t.Errorf("binary materialized with an invalid signature (stat err=%v)", err)
	}
}

func TestApplyFetchErrorNotFound(t *testing.T) {
	f := newFixture(t)
	f.fetcher.err = status.Error(codes.NotFound, "module is not allowed")

	ev := f.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
}

func TestApplyFetchStreamBroken(t *testing.T) {
	f := newFixture(t)
	f.fetcher.stream = &fakeChunkStream{
		chunks: [][]byte{f.binData[:8]},
		err:    status.Error(codes.Unavailable, "stream reset"),
	}

	ev := f.apply(t, map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
	if _, err := os.Stat(f.binPath()); !os.IsNotExist(err) {
		t.Errorf("binary materialized on an aborted fetch (stat err=%v)", err)
	}
}

func TestApplyNoFetcherInContext(t *testing.T) {
	f := newFixture(t)
	ev := f.applyCtx(t, context.Background(), map[string]any{"name": "redis"})
	wantFailedReason(t, ev, "module_fetch_failed")
}

func TestApplyUnknownState(t *testing.T) {
	f := newFixture(t)
	stream := &internaltest.ApplyStream{}
	if err := f.mod.Apply(&pluginv1.ApplyRequest{State: "absent"}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if last := stream.Last(); last == nil || !last.GetFailed() {
		t.Fatalf("expected failed on unknown state, got %v", last)
	}
}
