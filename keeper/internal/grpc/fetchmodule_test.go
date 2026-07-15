package grpc

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/souls-guild/soul-stack/keeper/internal/sigil"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

const testModuleSHA = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

// fakeModuleBinaries — fake [ModuleBinarySource]: fixed path/error + records
// the last requested sha.
type fakeModuleBinaries struct {
	mu     sync.Mutex
	path   string
	err    error
	gotSHA string
}

func (f *fakeModuleBinaries) LookupModuleBinary(_ context.Context, sha256Hex string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotSHA = sha256Hex
	if f.err != nil {
		return "", f.err
	}
	return f.path, nil
}

func (f *fakeModuleBinaries) lastSHA() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotSHA
}

// fakePluginChunkStream — fake grpc.ServerStreamingServer[PluginChunk].
// onSend (optional) is called AFTER the chunk is recorded — a hook for
// cancel/blocking.
type fakePluginChunkStream struct {
	grpclib.ServerStream
	ctx    context.Context
	mu     sync.Mutex
	chunks [][]byte
	onSend func(chunkCount int) error
}

func (s *fakePluginChunkStream) Context() context.Context { return s.ctx }

func (s *fakePluginChunkStream) Send(c *keeperv1.PluginChunk) error {
	s.mu.Lock()
	cp := append([]byte(nil), c.GetData()...)
	s.chunks = append(s.chunks, cp)
	n := len(s.chunks)
	hook := s.onSend
	s.mu.Unlock()
	if hook != nil {
		return hook(n)
	}
	return nil
}

func (s *fakePluginChunkStream) assembled() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for _, c := range s.chunks {
		out = append(out, c...)
	}
	return out
}

func (s *fakePluginChunkStream) chunkCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chunks)
}

func newFetchHandler(t *testing.T, deps EventStreamDeps) *eventStreamHandler {
	t.Helper()
	if deps.SeedDB == nil {
		deps.SeedDB = &fakeSeedDB{}
	}
	if deps.AuditWriter == nil {
		deps.AuditWriter = &recordingAudit{}
	}
	if deps.KID == "" {
		deps.KID = "kid-test"
	}
	if err := deps.validate(); err != nil {
		t.Fatalf("deps validate: %v", err)
	}
	return newEventStreamHandler(deps, discardLogger(t))
}

func writeModuleFile(t *testing.T, size int) (path string, content []byte) {
	t.Helper()
	content = make([]byte, size)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("rand: %v", err)
	}
	path = filepath.Join(t.TempDir(), "soul-mod-test")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write module file: %v", err)
	}
	return path, content
}

func fetchCtx(sid string) context.Context {
	return withAuthenticatedSID(context.Background(), sid)
}

// TestFetchModule_AllowedSHA_StreamsChunks — allowed sha: a file bigger
// than a chunk goes out as several PluginChunks, the byte-for-byte
// concatenation equals the file; lookup is called with a lowercase sha.
func TestFetchModule_AllowedSHA_StreamsChunks(t *testing.T) {
	path, content := writeModuleFile(t, moduleFetchChunkSize*2+1234)
	src := &fakeModuleBinaries{path: path}
	h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src})
	stream := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}

	err := h.FetchModule(&keeperv1.PluginFetchRequest{
		Namespace:    "community",
		Name:         "mongo",
		BinarySha256: strings.ToUpper(testModuleSHA),
	}, stream)
	if err != nil {
		t.Fatalf("FetchModule: %v", err)
	}
	if got := stream.chunkCount(); got != 3 {
		t.Errorf("chunks = %d, want 3 (2 полных + хвост)", got)
	}
	if !bytes.Equal(stream.assembled(), content) {
		t.Error("собранные чанки != содержимое файла")
	}
	if src.lastSHA() != testModuleSHA {
		t.Errorf("lookup sha = %q, want lowercase %q", src.lastSHA(), testModuleSHA)
	}
}

// TestFetchModule_NotAllowed_NotFound — ErrModuleNotAllowed → NotFound,
// with no filesystem path leaking into the message sent to the client.
func TestFetchModule_NotAllowed_NotFound(t *testing.T) {
	src := &fakeModuleBinaries{err: fmt.Errorf("%w: %s", sigil.ErrModuleNotAllowed, testModuleSHA)}
	h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src})
	stream := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}

	err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, stream)
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("code = %v, want NotFound; err = %v", got, err)
	}
	if stream.chunkCount() != 0 {
		t.Error("не должно быть чанков при отказе")
	}
}

// TestFetchModule_LookupStoreError_Unavailable — a DB/store lookup error
// (not ErrModuleNotAllowed) → Unavailable (transient, retryable for the
// Soul).
func TestFetchModule_LookupStoreError_Unavailable(t *testing.T) {
	src := &fakeModuleBinaries{err: errors.New("pg down")}
	h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src})
	stream := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}

	err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, stream)
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable; err = %v", got, err)
	}
}

// TestFetchModule_NoAuthenticatedSID_Internal — a call with no SID in the
// context (the interceptor didn't run) → Internal, lookup isn't touched.
// Same pattern as EventStream: without the interceptor we don't know who's
// on the other end.
func TestFetchModule_NoAuthenticatedSID_Internal(t *testing.T) {
	src := &fakeModuleBinaries{path: "/nonexistent"}
	h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src})
	stream := &fakePluginChunkStream{ctx: context.Background()}

	err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, stream)
	if got := status.Code(err); got != codes.Internal {
		t.Fatalf("code = %v, want Internal; err = %v", got, err)
	}
	if src.lastSHA() != "" {
		t.Error("lookup не должен вызываться без authenticated SID")
	}
}

// TestFetchModule_NilSource_Unavailable — Sigil disabled (ModuleBinaries
// nil) → Unavailable.
func TestFetchModule_NilSource_Unavailable(t *testing.T) {
	h := newFetchHandler(t, EventStreamDeps{})
	stream := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}

	err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, stream)
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable; err = %v", got, err)
	}
}

// TestFetchModule_InvalidSHA_InvalidArgument — a non-hex / wrong-length /
// empty sha is rejected before lookup.
func TestFetchModule_InvalidSHA_InvalidArgument(t *testing.T) {
	cases := map[string]string{
		"empty":    "",
		"short":    "abc123",
		"long":     testModuleSHA + "00",
		"non_hex":  strings.Repeat("zz", 32),
		"with_sep": testModuleSHA[:62] + ":a",
	}
	for name, sha := range cases {
		t.Run(name, func(t *testing.T) {
			src := &fakeModuleBinaries{path: "/nonexistent"}
			h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src})
			stream := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}

			err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: sha}, stream)
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument; err = %v", got, err)
			}
			if src.lastSHA() != "" {
				t.Error("lookup не должен вызываться на невалидном sha")
			}
		})
	}
}

// TestFetchModule_FileTooLarge_FailedPrecondition — a file bigger than
// ModuleFetchMaxBytes (plugins.max_artifact_size_mb) → FailedPrecondition,
// not a single chunk sent.
func TestFetchModule_FileTooLarge_FailedPrecondition(t *testing.T) {
	path, _ := writeModuleFile(t, 4096)
	src := &fakeModuleBinaries{path: path}
	h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src, ModuleFetchMaxBytes: 1024})
	stream := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}

	err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, stream)
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition; err = %v", got, err)
	}
	if stream.chunkCount() != 0 {
		t.Error("не должно быть чанков при превышении лимита")
	}
}

// TestFetchModule_MissingFile_NotFound — lookup returned a path, but the
// bytes are gone (the slot moved between lookup and open) → NotFound, same
// as not-allowed.
func TestFetchModule_MissingFile_NotFound(t *testing.T) {
	src := &fakeModuleBinaries{path: filepath.Join(t.TempDir(), "gone")}
	h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src})
	stream := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}

	err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, stream)
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("code = %v, want NotFound; err = %v", got, err)
	}
}

// TestFetchModule_ContextCancel_StopsStream — canceling the context after
// the first chunk aborts the stream (Canceled); the rest of the file is
// never sent.
func TestFetchModule_ContextCancel_StopsStream(t *testing.T) {
	path, _ := writeModuleFile(t, moduleFetchChunkSize*4)
	src := &fakeModuleBinaries{path: path}
	h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src})

	ctx, cancel := context.WithCancel(fetchCtx("host.example.com"))
	stream := &fakePluginChunkStream{ctx: ctx}
	stream.onSend = func(int) error {
		cancel()
		return nil
	}

	err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, stream)
	if got := status.Code(err); got != codes.Canceled {
		t.Fatalf("code = %v, want Canceled; err = %v", got, err)
	}
	if got := stream.chunkCount(); got >= 4 {
		t.Errorf("chunks = %d — стрим не прервался после cancel", got)
	}
}

// TestFetchModule_PerSIDLimit — limit=1: while the first fetch for a SID
// holds the slot, a second fetch for the same SID gets ResourceExhausted;
// a different SID goes through; the slot frees up once the first one
// finishes.
func TestFetchModule_PerSIDLimit(t *testing.T) {
	path, _ := writeModuleFile(t, 128)
	src := &fakeModuleBinaries{path: path}
	h := newFetchHandler(t, EventStreamDeps{ModuleBinaries: src, ModuleFetchPerSID: 1})

	holdStarted := make(chan struct{})
	holdRelease := make(chan struct{})
	holder := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}
	holder.onSend = func(int) error {
		close(holdStarted)
		<-holdRelease
		return nil
	}

	holderDone := make(chan error, 1)
	go func() {
		holderDone <- h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, holder)
	}()

	select {
	case <-holdStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("первый fetch не дошёл до Send")
	}

	// Same SID on top of an occupied slot → ResourceExhausted.
	sameSID := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}
	err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, sameSID)
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Fatalf("same-SID code = %v, want ResourceExhausted; err = %v", got, err)
	}

	// A different SID is unaffected by the first one's limit.
	otherSID := &fakePluginChunkStream{ctx: fetchCtx("other.example.com")}
	if err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, otherSID); err != nil {
		t.Fatalf("other-SID FetchModule: %v", err)
	}

	close(holdRelease)
	if err := <-holderDone; err != nil {
		t.Fatalf("holder FetchModule: %v", err)
	}

	// The slot freed up — a retry fetch with the same SID goes through.
	retry := &fakePluginChunkStream{ctx: fetchCtx("host.example.com")}
	if err := h.FetchModule(&keeperv1.PluginFetchRequest{BinarySha256: testModuleSHA}, retry); err != nil {
		t.Fatalf("retry FetchModule после release: %v", err)
	}
}

// TestKeeperServiceDesc_OnlyAdd — forward-compat guard for ADR-012: the
// prior RPCs are still there, FetchModule was added as server-streaming.
// Catches a method being removed/renamed from service Keeper (breaking for
// older Souls).
func TestKeeperServiceDesc_OnlyAdd(t *testing.T) {
	desc := keeperv1.Keeper_ServiceDesc
	methods := map[string]bool{}
	for _, m := range desc.Methods {
		methods[m.MethodName] = true
	}
	for _, want := range []string{"Ping", "Bootstrap"} {
		if !methods[want] {
			t.Errorf("unary RPC %q пропал из service Keeper (only-add нарушен)", want)
		}
	}
	streams := map[string]grpclib.StreamDesc{}
	for _, s := range desc.Streams {
		streams[s.StreamName] = s
	}
	es, ok := streams["EventStream"]
	if !ok || !es.ClientStreams || !es.ServerStreams {
		t.Errorf("EventStream должен остаться bidi-стримом: %+v", es)
	}
	fm, ok := streams["FetchModule"]
	if !ok || fm.ClientStreams || !fm.ServerStreams {
		t.Errorf("FetchModule должен быть server-streaming: %+v", fm)
	}
}
