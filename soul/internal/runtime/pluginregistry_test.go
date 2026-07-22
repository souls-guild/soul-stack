package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
	"github.com/souls-guild/soul-stack/soul/internal/pluginhost"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

func TestPluginRegistry_LookupKnown(t *testing.T) {
	d := makeDiscovered("acme", "echo")
	r := NewPluginRegistry(&fakeSpawner{}, []pluginhost.Discovered{d}, nil)
	if _, ok := r.Lookup("acme.echo"); !ok {
		t.Fatal("Lookup(acme.echo): not found")
	}
	if _, ok := r.Lookup("acme.unknown"); ok {
		t.Fatal("Lookup(acme.unknown): unexpectedly found")
	}
}

func TestPluginRegistry_ApplySpawnsAndCloses(t *testing.T) {
	d := makeDiscovered("acme", "echo")
	spawner := &fakeSpawner{
		makeSession: func() *fakeSession {
			return &fakeSession{
				events: []*pluginv1.ApplyEvent{
					{Message: "starting"},
					{Changed: true, Output: mustStruct(nil, map[string]any{"hello": "world"})},
				},
			}
		},
	}
	r := NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil)
	mod, _ := r.Lookup("acme.echo")

	stream := newInProcApplyStream(context.Background())
	if err := mod.Apply(&pluginv1.ApplyRequest{State: "applied"}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(stream.events) != 2 {
		t.Fatalf("events received = %d, want 2", len(stream.events))
	}
	if spawner.spawnCount != 1 {
		t.Errorf("spawnCount = %d, want 1 (one-shot per Apply)", spawner.spawnCount)
	}
	if !spawner.lastSession.closed {
		t.Error("session not closed after Apply")
	}
}

func TestPluginRegistry_ApplyRpcErrorPropagates(t *testing.T) {
	d := makeDiscovered("acme", "echo")
	spawner := &fakeSpawner{
		makeSession: func() *fakeSession {
			return &fakeSession{applyErr: errors.New("rpc broken")}
		},
	}
	r := NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil)
	mod, _ := r.Lookup("acme.echo")

	stream := newInProcApplyStream(context.Background())
	err := mod.Apply(&pluginv1.ApplyRequest{}, stream)
	if err == nil {
		t.Fatal("expected error from RPC failure")
	}
}

func TestPluginRegistry_SpawnErrorPropagates(t *testing.T) {
	d := makeDiscovered("acme", "echo")
	spawner := &fakeSpawner{spawnErr: errors.New("plugin not found")}
	r := NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil)
	mod, _ := r.Lookup("acme.echo")

	err := mod.Apply(&pluginv1.ApplyRequest{}, newInProcApplyStream(context.Background()))
	if err == nil {
		t.Fatal("expected error from Spawn failure")
	}
}

func TestCompositeRegistry_CoreShadowsPlugin(t *testing.T) {
	core := mapRegistry{"core.pkg": &fakeModule{}}
	plug := mapRegistry{"core.pkg": &fakeModule{}, "acme.echo": &fakeModule{}}

	c := NewCompositeRegistry(core, plug)
	got, ok := c.Lookup("core.pkg")
	if !ok || got != core["core.pkg"] {
		t.Errorf("Lookup(core.pkg): expected core layer to win")
	}
	if _, ok := c.Lookup("acme.echo"); !ok {
		t.Error("Lookup(acme.echo): plugin layer should be reachable")
	}
	if _, ok := c.Lookup("core.frobnicate"); ok {
		t.Error("Lookup(unknown): should be false")
	}
}

func TestRun_DispatchesToPluginViaComposite(t *testing.T) {
	// Integration of applyrunner ↔ pluginregistry: module acme.echo isn't in
	// core but is in the plugin layer; ApplyRunner finds it via composite.
	d := makeDiscovered("acme", "echo")
	spawner := &fakeSpawner{
		makeSession: func() *fakeSession {
			return &fakeSession{events: []*pluginv1.ApplyEvent{{Changed: true}}}
		},
	}
	composite := NewCompositeRegistry(
		mapRegistry{},
		NewPluginRegistry(spawner, []pluginhost.Discovered{d}, nil),
	)
	r := NewApplyRunner(composite, nil)
	sink := &recordingSink{}

	err := r.Run(context.Background(), &keeperv1.ApplyRequest{
		ApplyId: "plug-1",
		Tasks:   []*keeperv1.RenderedTask{{Module: "acme.echo.applied"}},
	}, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(sink.taskEvents) != 1 {
		t.Fatalf("taskEvents = %d", len(sink.taskEvents))
	}
	if sink.taskEvents[0].GetStatus() != keeperv1.TaskStatus_TASK_STATUS_CHANGED {
		t.Errorf("status = %v", sink.taskEvents[0].GetStatus())
	}
	if spawner.spawnCount != 1 {
		t.Errorf("spawnCount = %d", spawner.spawnCount)
	}
}

// --- Rescan (hot-register, ADR-065(d)) ---

func TestPluginRegistry_RescanPicksUpNewModule(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "acme", "echo")
	discovered, _, err := pluginhost.Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	r := NewPluginRegistry(&fakeSpawner{}, discovered, nil)
	if _, ok := r.Lookup("acme.extra"); ok {
		t.Fatal("Lookup(acme.extra): found before installation")
	}

	writeSlot(t, root, "acme", "extra")
	if _, err := r.Rescan(root); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if _, ok := r.Lookup("acme.extra"); !ok {
		t.Error("Lookup(acme.extra) after Rescan: not found")
	}
	if _, ok := r.Lookup("acme.echo"); !ok {
		t.Error("Lookup(acme.echo): existing module lost after Rescan")
	}
}

func TestCompositeRegistry_RescanKeepsCoreLayer(t *testing.T) {
	root := t.TempDir()
	plug := NewPluginRegistry(&fakeSpawner{}, nil, nil)
	core := mapRegistry{"core.pkg": &fakeModule{}}
	c := NewCompositeRegistry(core, plug)

	writeSlot(t, root, "acme", "extra")
	if _, err := plug.Rescan(root); err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	got, ok := c.Lookup("core.pkg")
	if !ok || got != core["core.pkg"] {
		t.Error("core-layer composite affected by plugin-layer Rescan")
	}
	if _, ok := c.Lookup("acme.extra"); !ok {
		t.Error("new module unavailable via composite after Rescan")
	}
}

func TestPluginRegistry_ConcurrentLookupAndRescan(t *testing.T) {
	root := t.TempDir()
	writeSlot(t, root, "acme", "echo")
	discovered, _, err := pluginhost.Discover(root)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	r := NewPluginRegistry(&fakeSpawner{}, discovered, nil)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					r.Lookup("acme.echo")
					r.Names()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if _, err := r.Rescan(root); err != nil {
			t.Errorf("Rescan #%d: %v", i, err)
			break
		}
	}
	close(stop)
	wg.Wait()

	if _, ok := r.Lookup("acme.echo"); !ok {
		t.Error("Lookup(acme.echo) after concurrent Rescans: not found")
	}
}

// --- helpers ---

// writeSlot materializes a valid directory slot `<root>/<ns>-<name>/`
// (manifest.yaml + executable binary) — the core.module.installed format.
func writeSlot(t *testing.T, root, namespace, name string) {
	t.Helper()
	dir := filepath.Join(root, namespace+"-"+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf("kind: soul_module\nnamespace: %s\nname: %s\nprotocol_version: 1\n"+
		"spec:\n  states:\n    applied:\n      description: test state\n      input: {}\n", namespace, name)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "soul-mod-"+name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func makeDiscovered(namespace, name string) pluginhost.Discovered {
	return pluginhost.Discovered{
		Manifest: &sharedplugin.Manifest{
			Kind:            sharedplugin.KindSoulModule,
			ProtocolVersion: 1,
			Namespace:       namespace,
			Name:            name,
		},
		BinaryPath: "/bogus/soul-mod-" + name,
		Dir:        "/bogus",
	}
}

type fakeSpawner struct {
	makeSession func() *fakeSession
	spawnErr    error
	spawnCount  int
	lastSession *fakeSession
}

func (f *fakeSpawner) Spawn(ctx context.Context, d pluginhost.Discovered) (PluginSession, error) {
	if f.spawnErr != nil {
		return nil, f.spawnErr
	}
	f.spawnCount++
	sess := f.makeSession()
	f.lastSession = sess
	return sess, nil
}

// fakeSession implements PluginSession over an in-memory list of ApplyEvents.
type fakeSession struct {
	events   []*pluginv1.ApplyEvent
	applyErr error
	closed   bool
}

func (f *fakeSession) Apply(ctx context.Context, req *pluginv1.ApplyRequest) (grpc.ServerStreamingClient[pluginv1.ApplyEvent], error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	return &fakeApplyClient{events: f.events}, nil
}

func (f *fakeSession) Close() error {
	f.closed = true
	return nil
}

type fakeApplyClient struct {
	grpc.ClientStream
	events []*pluginv1.ApplyEvent
	idx    int
}

func (c *fakeApplyClient) Recv() (*pluginv1.ApplyEvent, error) {
	if c.idx >= len(c.events) {
		return nil, io.EOF
	}
	ev := c.events[c.idx]
	c.idx++
	return ev, nil
}

func (c *fakeApplyClient) Header() (metadata.MD, error) { return nil, nil }
func (c *fakeApplyClient) Trailer() metadata.MD         { return nil }
func (c *fakeApplyClient) CloseSend() error             { return nil }
func (c *fakeApplyClient) Context() context.Context     { return context.Background() }
func (c *fakeApplyClient) SendMsg(any) error            { return nil }
func (c *fakeApplyClient) RecvMsg(any) error {
	// apply-cycle only calls Recv(); RecvMsg stays a no-op to satisfy the
	// ClientStream interface — copying the proto message (which contains
	// sync.Mutex protoimpl.MessageState) would trip go vet.
	return nil
}

var _ = structpb.NewStruct // keep import even if unused after rebases
