package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// withFastBackoff replaces defaultBackoff with "zero" delays + the given
// MaxAttempts. Used in wait-deadline / transient-probe tests where the default
// 1s->2s->4s would make the test slow.
func withFastBackoff(t *testing.T, maxAttempts int) {
	t.Helper()
	orig := defaultBackoff
	defaultBackoff = func() clouddriver.BackoffConfig {
		return clouddriver.BackoffConfig{
			Initial:     1 * time.Millisecond,
			Max:         1 * time.Millisecond,
			Factor:      1.0,
			MaxAttempts: maxAttempts,
		}
	}
	t.Cleanup(func() { defaultBackoff = orig })
}

// fakeOS is a mock osAPI for L0 unit tests (without network). Behavior is
// configured per method; getSeq models BUILD->ACTIVE transition between poller
// rounds. getFn is an override for transient-probe-error tests.
type fakeOS struct {
	createOut  *servers.Server
	createErr  error
	createCall int

	listOut  []servers.Server
	listErr  error
	listCall int

	getSeq []*servers.Server
	getIdx int
	getErr error
	getFn  func(call int) (*servers.Server, error)
	getN   int

	deleteErr error
	deleteN   int

	lastCreateOpts servers.CreateOptsBuilder
	lastDeleted    []string
}

func (f *fakeOS) CreateServer(_ context.Context, opts servers.CreateOptsBuilder) (*servers.Server, error) {
	f.createCall++
	f.lastCreateOpts = opts
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createOut != nil {
		return f.createOut, nil
	}
	// Default: fabricate a unique ID from the request name.
	if co, ok := opts.(servers.CreateOpts); ok {
		return &servers.Server{ID: co.Name, Name: co.Name, Metadata: co.Metadata}, nil
	}
	return &servers.Server{ID: "anon"}, nil
}

func (f *fakeOS) GetServer(_ context.Context, id string) (*servers.Server, error) {
	call := f.getN
	f.getN++
	if f.getFn != nil {
		return f.getFn(call)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if len(f.getSeq) == 0 {
		return &servers.Server{ID: id}, nil
	}
	out := f.getSeq[f.getIdx]
	if f.getIdx < len(f.getSeq)-1 {
		f.getIdx++
	}
	return out, nil
}

func (f *fakeOS) DeleteServer(_ context.Context, id string) error {
	f.deleteN++
	f.lastDeleted = append(f.lastDeleted, id)
	return f.deleteErr
}

func (f *fakeOS) ListServers(_ context.Context, _ servers.ListOptsBuilder) ([]servers.Server, error) {
	f.listCall++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listOut, nil
}

// withFakeOS replaces the client factory to return f and restores it after the
// test.
func withFakeOS(t *testing.T, f *fakeOS) {
	t.Helper()
	orig := newOsClient
	newOsClient = func(_ context.Context, _ osCredentials) (osAPI, error) { return f, nil }
	t.Cleanup(func() { newOsClient = orig })
}

type createStream struct {
	grpc.ServerStreamingServer[pluginv1.CreateEvent]
	ctx  context.Context
	sent []*pluginv1.CreateEvent
}

func (s *createStream) Send(e *pluginv1.CreateEvent) error { s.sent = append(s.sent, e); return nil }
func (s *createStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}
func (s *createStream) last() *pluginv1.CreateEvent {
	if len(s.sent) == 0 {
		return nil
	}
	return s.sent[len(s.sent)-1]
}

type destroyStream struct {
	grpc.ServerStreamingServer[pluginv1.DestroyEvent]
	sent []*pluginv1.DestroyEvent
}

func (s *destroyStream) Send(e *pluginv1.DestroyEvent) error { s.sent = append(s.sent, e); return nil }
func (s *destroyStream) Context() context.Context            { return context.Background() }

type listStream struct {
	grpc.ServerStreamingServer[pluginv1.VmInfo]
	ctx  context.Context
	sent []*pluginv1.VmInfo
}

func (s *listStream) Send(v *pluginv1.VmInfo) error { s.sent = append(s.sent, v); return nil }
func (s *listStream) Context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb: %v", err)
	}
	return s
}

func validKeystoneCreds() map[string]any {
	return map[string]any{
		"auth_url":            "https://keystone.example.com/v3",
		"username":            "soul",
		"password":            "s3cret",
		"user_domain_name":    "Default",
		"project_name":        "soul-stack",
		"project_domain_name": "Default",
		"region":              "RegionOne",
	}
}

// activeServer is a factory for a "ready" VM with the given ID and address (for
// probe happy path and final Get in finalizeCreate). Addresses shape in
// gophercloud is map[string]any -> []map{addr,version,...}; reproduce it exactly.
func activeServer(id, ip string) *servers.Server {
	return &servers.Server{
		ID:     id,
		Name:   id,
		Status: statusActive,
		Addresses: map[string]any{
			"private": []any{
				map[string]any{"addr": ip, "version": float64(4)},
			},
		},
		Flavor: map[string]any{"id": "m1.small"},
		Image:  map[string]any{"id": "img-1"},
	}
}

func TestSchema_ParsesEmbedded(t *testing.T) {
	d := &OpenstackDriver{}
	rep, err := d.Schema(context.Background(), &pluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	m := rep.ProfileSchema.AsMap()
	req, _ := m["required"].([]any)
	if len(req) != 3 {
		t.Errorf("schema required=%v, want 3 fields (image_id/flavor_id/network_id)", req)
	}
}

func TestValidate_MissingFields(t *testing.T) {
	d := &OpenstackDriver{}
	rep, err := d.Validate(context.Background(), &pluginv1.ValidateProfileRequest{
		Profile: mustStruct(t, map[string]any{"image_id": "img-1"}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Ok {
		t.Error("expected Ok=false on missing flavor_id/network_id")
	}
	if len(rep.Errors) != 2 {
		t.Errorf("errors=%v, want 2 (flavor_id, network_id)", rep.Errors)
	}
}

// Validate does NOT require region - private clouds without regions are ok.
func TestValidate_RegionOptional(t *testing.T) {
	d := &OpenstackDriver{}
	rep, err := d.Validate(context.Background(), &pluginv1.ValidateProfileRequest{
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			// region: absent
		}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !rep.Ok {
		t.Errorf("expected Ok=true without region, got errors=%v", rep.Errors)
	}
}

// TestVmName_Precedence - deterministic name (NIM-16): nameBase from
// CreateRequest.name gives `<nameBase>-<seq>` (keeper predicts FQDN from it),
// taking precedence over runLabel; without nameBase - `soul-<runLabel>-<seq>`
// (anon branch removed).
func TestVmName_Precedence(t *testing.T) {
	cases := []struct {
		name     string
		nameBase string
		runLabel string
		seq      int32
		want     string
	}{
		{name: "nameBase wins", nameBase: "redis", runLabel: "run-x", seq: 0, want: "redis-0"},
		{name: "nameBase index 2", nameBase: "redis", seq: 2, want: "redis-2"},
		{name: "runLabel when no nameBase", runLabel: "run-x", seq: 1, want: "soul-run-x-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := vmName(c.nameBase, c.runLabel, c.seq); got != c.want {
				t.Errorf("vmName(%q,%q,%d) = %q, want %q", c.nameBase, c.runLabel, c.seq, got, c.want)
			}
		})
	}
}

// TestCreate_HonorsRequestName - CreateRequest.name reaches servers.Create as
// `<name>-<seq>` + metadata stamp runMetaKey=<name> (the driver honors the
// keeper-provided name for predictable FQDN, self-onboard "Variant T").
func TestCreate_HonorsRequestName(t *testing.T) {
	f := &fakeOS{
		createOut: &servers.Server{ID: "srv-1", Name: "redis-0", Status: statusBuild},
		getSeq:    []*servers.Server{activeServer("srv-1", "10.0.0.5")},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
		Name:        "redis",
	}, s)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	co, ok := f.lastCreateOpts.(servers.CreateOpts)
	if !ok {
		t.Fatalf("CreateServer not called or opts type=%T", f.lastCreateOpts)
	}
	if co.Name != "redis-0" {
		t.Errorf("servers.Create name=%q, want redis-0 (honor CreateRequest.name)", co.Name)
	}
	if got := co.Metadata[runMetaKey]; got != "redis" {
		t.Errorf("metadata[%s]=%q, want redis (name stamped as run identity)", runMetaKey, got)
	}
}

// TestCreate_NameDerivedRerun_ReusesExisting - NIM-16 central scenario: rerun a
// step with name and WITHOUT a profile label. The previous run stamped
// metadata[runMetaKey]=<name> -> scan (runLabel=nameBase) finds a live VM and
// reuses it. Regression guard for stamp+always-scan coupling: red on rollback of
// fallback prof.runLabel=nameBase.
func TestCreate_NameDerivedRerun_ReusesExisting(t *testing.T) {
	existing := *activeServer("redis-0", "10.0.0.5")
	existing.Metadata = map[string]string{runMetaKey: "redis"} // previous run stamp
	f := &fakeOS{
		listOut: []servers.Server{existing},
		getSeq:  []*servers.Server{&existing},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{ // no labels - identity from Name
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
		Name:        "redis",
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 0 {
		t.Errorf("CreateServer called %d times; name-derived rerun must reuse stamped VM", f.createCall)
	}
	last := s.last()
	if last == nil || last.Failed {
		t.Fatalf("final=%+v, want success", last)
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "redis-0" {
		t.Errorf("reused vms=%+v, want [redis-0]", last.Vms)
	}
}

// TestCreate_NoIdentity_FailsClosed - NIM-16: without name and run label the run
// is indistinguishable from previous ones -> repeated Create would create orphan
// VMs. Fail-closed BEFORE any OpenStack API call (no create, no list, no
// Keystone auth).
func TestCreate_NoIdentity_FailsClosed(t *testing.T) {
	withFastBackoff(t, 2) // regression guard: without guard, test must not sleep until deadline
	f := &fakeOS{}
	withFakeOS(t, f)
	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{ // no labels, Name empty
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last == nil || !last.Failed {
		t.Fatalf("expected failed=true on missing identity, got %+v", last)
	}
	if !strings.HasPrefix(last.Message, "invalid_params:") {
		t.Errorf("message=%q, want invalid_params-class prefix", last.Message)
	}
	if f.createCall != 0 {
		t.Errorf("CreateServer called %d times; fail-closed must NOT touch OpenStack API", f.createCall)
	}
	if f.listCall != 0 {
		t.Errorf("ListServers called %d times; fail-closed must NOT touch OpenStack API", f.listCall)
	}
}

func TestCreate_HappyPath(t *testing.T) {
	f := &fakeOS{
		createOut: &servers.Server{ID: "srv-aaa", Name: "soul-run1-0", Status: statusBuild},
		getSeq:    []*servers.Server{activeServer("srv-aaa", "10.0.0.5")},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run1"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
		Userdata:    "#cloud-config\n",
	}, s)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last == nil || last.Failed {
		t.Fatalf("final event=%+v, want success", last)
	}
	if len(last.Vms) != 1 {
		t.Fatalf("vms=%d, want 1", len(last.Vms))
	}
	vm := last.Vms[0]
	if vm.VmId != "srv-aaa" {
		t.Errorf("vm_id=%q", vm.VmId)
	}
	if vm.Fqdn == "" {
		t.Error("fqdn empty (must be IP or hostname)")
	}
	if vm.PrimaryIp != "10.0.0.5" {
		t.Errorf("primary_ip=%q", vm.PrimaryIp)
	}
	// userdata is passed into CreateOpts as plain []byte (gophercloud encodes it).
	co, ok := f.lastCreateOpts.(servers.CreateOpts)
	if !ok {
		t.Fatalf("lastCreateOpts type=%T", f.lastCreateOpts)
	}
	if string(co.UserData) != "#cloud-config\n" {
		t.Errorf("UserData=%q, want plain cloud-init blob", co.UserData)
	}
}

func TestCreate_WaitsForActive(t *testing.T) {
	withFastBackoff(t, 8)
	f := &fakeOS{
		createOut: &servers.Server{ID: "srv-bbb", Status: statusBuild},
		getSeq: []*servers.Server{
			// round 1: BUILD without address
			{ID: "srv-bbb", Status: statusBuild},
			// round 2: ACTIVE with IP
			activeServer("srv-bbb", "10.0.0.9"),
		},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.last().Failed {
		t.Fatalf("final=%+v, want success after wait", s.last())
	}
	if s.last().Vms[0].Fqdn == "" {
		t.Error("fqdn empty after wait")
	}
}

// Keystone auth error comes FROM buildAuthOptions/newOsClient - the driver sends
// an auth event without calling compute API. Verify that `os-client` path raises
// FailAuth, not Transient.
func TestCreate_KeystoneAuthError(t *testing.T) {
	// Replace the factory to return an error (simulate 401 from Keystone).
	orig := newOsClient
	newOsClient = func(context.Context, osCredentials) (osAPI, error) {
		return nil, gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusUnauthorized, Body: []byte("bad creds")}
	}
	t.Cleanup(func() { newOsClient = orig })

	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on keystone-auth error")
	}
	if !strings.Contains(last.Message, "auth:") {
		t.Errorf("message=%q, want auth-class prefix", last.Message)
	}
}

// Idempotent path: findByRunLabel returned live VMs >= count -> driver does NOT
// call CreateServer, returns existing ones.
func TestCreate_Idempotent_ReusesExisting(t *testing.T) {
	existing := *activeServer("srv-existing", "10.1.1.1")
	existing.Metadata = map[string]string{runMetaKey: "run-42"}
	f := &fakeOS{
		listOut: []servers.Server{existing},
		getSeq:  []*servers.Server{&existing},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run-42"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 0 {
		t.Errorf("CreateServer called %d times; idempotent path must NOT launch new VM", f.createCall)
	}
	if s.last().Failed {
		t.Fatalf("idempotent final=%+v", s.last())
	}
	if s.last().Vms[0].VmId != "srv-existing" {
		t.Errorf("reused vm=%q, want srv-existing", s.last().Vms[0].VmId)
	}
}

// Anti-orphan: ctx-cancel during wait -> final event CARRIES vm_id for all
// created VMs with failed=true (Keeper-side will see them and can Destroy).
func TestCreate_CtxCancel_AntiOrphan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeOS{
		createOut: &servers.Server{ID: "srv-orphan", Status: statusBuild},
		// always BUILD -> poller runs until ctx is canceled
		getSeq: []*servers.Server{{ID: "srv-orphan", Status: statusBuild}},
	}
	withFakeOS(t, f)
	cancel() // cancel immediately - poller enters sleepCtx and returns ctx.Err

	d := &OpenstackDriver{}
	s := &createStream{ctx: ctx}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on ctx-cancel during wait")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "srv-orphan" {
		t.Errorf("anti-orphan: final event must carry vm_id srv-orphan, got %+v", last.Vms)
	}
}

// Wait-deadline (NOT ctx-cancel): MaxAttempts exhausted - failed event with vm_id
// + "max attempts exhausted" text.
func TestCreate_WaitDeadline_AntiOrphan(t *testing.T) {
	withFastBackoff(t, 2)
	f := &fakeOS{
		createOut: &servers.Server{ID: "srv-wait", Status: statusBuild},
		getSeq:    []*servers.Server{{ID: "srv-wait", Status: statusBuild}},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on wait-deadline exhaustion")
	}
	if !strings.Contains(last.Message, "max attempts exhausted") {
		t.Errorf("message=%q, want max-attempts-exhausted (ErrWaitDeadline)", last.Message)
	}
}

// Terminal status during wait (ERROR/SHUTOFF) - probe returns Err, poller stops
// polling, final event has failed=true + vm_id.
func TestCreate_TerminalStatusProbe(t *testing.T) {
	withFastBackoff(t, 4)
	cases := []struct {
		name   string
		status string
	}{
		{"error", "ERROR"},
		{"shutoff", "SHUTOFF"},
		{"deleted", "DELETED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeOS{
				createOut: &servers.Server{ID: "srv-term", Status: statusBuild},
				getSeq:    []*servers.Server{{ID: "srv-term", Status: tc.status}},
			}
			withFakeOS(t, f)
			d := &OpenstackDriver{}
			s := &createStream{}
			if err := d.Create(&pluginv1.CreateRequest{
				Count: 1,
				Profile: mustStruct(t, map[string]any{
					"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
					"labels": map[string]any{runMetaKey: "run"},
				}),
				Credentials: mustStruct(t, validKeystoneCreds()),
			}, s); err != nil {
				t.Fatalf("Create: %v", err)
			}
			last := s.last()
			if !last.Failed {
				t.Fatalf("terminal=%s: expected failed=true, got %+v", tc.status, last)
			}
			if len(last.Vms) != 1 || last.Vms[0].VmId != "srv-term" {
				t.Errorf("terminal=%s: final vms=%+v, want vm_id=srv-term", tc.status, last.Vms)
			}
			if last.Vms[0].Fqdn != "" {
				t.Errorf("terminal=%s: Fqdn=%q must be empty (probe failed)", tc.status, last.Vms[0].Fqdn)
			}
		})
	}
}

// Transient probe-error (5xx during Get) is swallowed by the probe wrapper; next
// round succeeds.
func TestCreate_TransientProbeError_SwallowAndRetry(t *testing.T) {
	withFastBackoff(t, 8)
	f := &fakeOS{
		createOut: &servers.Server{ID: "srv-trans", Status: statusBuild},
	}
	// call 0 - BUILD (first probe round);
	// call 1 - 503 (transient, swallowed);
	// call 2 — ACTIVE + IP → Ready.
	f.getFn = func(call int) (*servers.Server, error) {
		switch call {
		case 0:
			return &servers.Server{ID: "srv-trans", Status: statusBuild}, nil
		case 1:
			return nil, gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusServiceUnavailable}
		default:
			return activeServer("srv-trans", "10.5.5.5"), nil
		}
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("transient probe-error must be swallowed; got failed: %+v", last)
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "srv-trans" || last.Vms[0].Fqdn == "" {
		t.Errorf("vms after transient retry = %+v", last.Vms)
	}
}

// Idempotent over-count: list returned more VMs than count -> return all found
// VMs, without new Create calls.
func TestCreate_Idempotent_OverCount(t *testing.T) {
	withFastBackoff(t, 2)
	existing := []servers.Server{
		*activeServer("srv-old-1", "10.1.0.1"),
		*activeServer("srv-old-2", "10.1.0.2"),
		*activeServer("srv-old-3", "10.1.0.3"),
	}
	for i := range existing {
		existing[i].Metadata = map[string]string{runMetaKey: "run-over"}
	}
	f := &fakeOS{
		listOut: existing,
		getSeq:  []*servers.Server{&existing[0], &existing[1], &existing[2]},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 2,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run-over"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 0 {
		t.Errorf("CreateServer called %d times; over-count idempotent path must NOT launch new VM", f.createCall)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("over-count idempotent: final=%+v, want success", last)
	}
	if len(last.Vms) != 3 {
		t.Errorf("vms=%d, want 3 (all existing returned, not truncated to count)", len(last.Vms))
	}
}

// TestCreate_PartialRerun_NoIndexCollision - NIM-16: partial rerun by metadata
// label. existing "soul-run-delta-0" (index 0 occupied), count=2 -> new VM takes
// first free index "soul-run-delta-1", not duplicate "-0".
func TestCreate_PartialRerun_NoIndexCollision(t *testing.T) {
	withFastBackoff(t, 2)
	existing := *activeServer("soul-run-delta-0", "10.1.0.1")
	existing.Metadata = map[string]string{runMetaKey: "run-delta"}
	f := &fakeOS{
		listOut:   []servers.Server{existing},
		createOut: &servers.Server{ID: "srv-new", Status: statusBuild},
		getFn: func(_ int) (*servers.Server, error) {
			return activeServer("srv-any", "10.1.0.2"), nil
		},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 2,
		Profile: mustStruct(t, map[string]any{
			"image_id": "img-1", "flavor_id": "m1.small", "network_id": "net-1",
			"labels": map[string]any{runMetaKey: "run-delta"},
		}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 1 {
		t.Errorf("CreateServer called %d times, want 1 (gap = count - existing)", f.createCall)
	}
	co, ok := f.lastCreateOpts.(servers.CreateOpts)
	if !ok {
		t.Fatalf("lastCreateOpts type=%T", f.lastCreateOpts)
	}
	if co.Name != "soul-run-delta-1" {
		t.Errorf("gap-fill name=%q, want soul-run-delta-1 (no -0 collision)", co.Name)
	}
}

func TestStatus_UsesCredentials(t *testing.T) {
	inst := activeServer("srv-stat", "10.6.6.6")
	f := &fakeOS{getSeq: []*servers.Server{inst}}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	rep, err := d.Status(context.Background(), &pluginv1.StatusRequest{
		VmId:        "srv-stat",
		Credentials: mustStruct(t, validKeystoneCreds()),
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.State != statusActive {
		t.Errorf("state=%q, want ACTIVE", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes must be populated")
	}
}

func TestList_UsesCredentialsField(t *testing.T) {
	f := &fakeOS{
		listOut: []servers.Server{
			*activeServer("srv-l-1", "10.7.7.1"),
			*activeServer("srv-l-2", "10.7.7.2"),
		},
	}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &listStream{ctx: context.Background()}
	if err := d.List(&pluginv1.ListRequest{
		Filter:      mustStruct(t, map[string]any{}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(s.sent) != 2 {
		t.Errorf("list events=%d, want 2", len(s.sent))
	}
}

// List with runLabel filter drops non-matching VMs.
func TestList_FiltersByRunLabel(t *testing.T) {
	a := *activeServer("srv-a", "10.7.7.1")
	a.Metadata = map[string]string{runMetaKey: "run-x"}
	b := *activeServer("srv-b", "10.7.7.2")
	b.Metadata = map[string]string{runMetaKey: "run-y"}
	f := &fakeOS{listOut: []servers.Server{a, b}}
	withFakeOS(t, f)

	d := &OpenstackDriver{}
	s := &listStream{ctx: context.Background()}
	if err := d.List(&pluginv1.ListRequest{
		Filter:      mustStruct(t, map[string]any{runMetaKey: "run-x"}),
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(s.sent) != 1 || s.sent[0].VmId != "srv-a" {
		t.Errorf("filtered list = %+v, want only srv-a", s.sent)
	}
}

func TestDestroy_PerVM(t *testing.T) {
	f := &fakeOS{}
	withFakeOS(t, f)
	d := &OpenstackDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"srv-1", "srv-2"},
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.sent) != 2 {
		t.Fatalf("destroy events=%d, want 2", len(s.sent))
	}
	for _, ev := range s.sent {
		if ev.Failed {
			t.Errorf("unexpected failed: %+v", ev)
		}
	}
	if f.deleteN != 2 {
		t.Errorf("DeleteServer called %d, want 2", f.deleteN)
	}
}

func TestDestroy_NotFoundIsIdempotent(t *testing.T) {
	f := &fakeOS{deleteErr: gophercloud.ErrUnexpectedResponseCode{Actual: http.StatusNotFound}}
	withFakeOS(t, f)
	d := &OpenstackDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"srv-gone"},
		Credentials: mustStruct(t, validKeystoneCreds()),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.sent) != 1 || s.sent[0].Failed {
		t.Errorf("not-found destroy must be idempotent (success), got %+v", s.sent)
	}
	if !strings.Contains(s.sent[0].Message, "already absent") {
		t.Errorf("message=%q, want already-absent", s.sent[0].Message)
	}
}

// buildAuthOptions: validity-checks Keystone credentials form.
func TestBuildAuthOptions(t *testing.T) {
	good := osCredentials{
		AuthURL:           "https://k/v3",
		Username:          "u",
		Password:          "p",
		UserDomainName:    "Default",
		ProjectName:       "proj",
		ProjectDomainName: "Default",
	}
	if _, err := buildAuthOptions(good); err != nil {
		t.Errorf("good keystone creds: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(c *osCredentials)
		wantSub string
	}{
		{"no_auth_url", func(c *osCredentials) { c.AuthURL = "" }, "auth_url"},
		{"no_username", func(c *osCredentials) { c.Username = "" }, "username/password"},
		{"no_password", func(c *osCredentials) { c.Password = "" }, "username/password"},
		{"no_user_domain", func(c *osCredentials) { c.UserDomainName = "" }, "user_domain"},
		{"no_project", func(c *osCredentials) { c.ProjectName = "" }, "project_name or project_id"},
		{"no_project_domain", func(c *osCredentials) { c.ProjectDomainName = "" }, "project_domain"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := good
			tc.mutate(&c)
			_, err := buildAuthOptions(c)
			if err == nil {
				t.Fatalf("want error containing %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err=%v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

// buildAuthOptions accepts project_id instead of project_name (XOR).
func TestBuildAuthOptions_AcceptsIDs(t *testing.T) {
	creds := osCredentials{
		AuthURL:         "https://k/v3",
		Username:        "u",
		Password:        "p",
		UserDomainID:    "default",
		ProjectID:       "proj-uuid",
		ProjectDomainID: "default",
	}
	if _, err := buildAuthOptions(creds); err != nil {
		t.Errorf("creds with IDs must be accepted: %v", err)
	}
}

// classifyOS - taxonomy by HTTP codes and heuristics.
func TestClassifyOS_HTTPCodes(t *testing.T) {
	cases := map[int]clouddriver.FailClass{
		http.StatusUnauthorized:          clouddriver.FailAuth,
		http.StatusForbidden:             clouddriver.FailAuth,
		http.StatusNotFound:              clouddriver.FailNotFound,
		http.StatusConflict:              clouddriver.FailInvalidParams,
		http.StatusBadRequest:            clouddriver.FailInvalidParams,
		http.StatusRequestEntityTooLarge: clouddriver.FailTransient,
		http.StatusTooManyRequests:       clouddriver.FailTransient,
		http.StatusInternalServerError:   clouddriver.FailTransient,
		http.StatusServiceUnavailable:    clouddriver.FailTransient,
	}
	for code, want := range cases {
		got := classifyOS(gophercloud.ErrUnexpectedResponseCode{Actual: code})
		if got != want {
			t.Errorf("classifyOS(%d)=%v, want %v", code, got, want)
		}
	}
	// non-HTTP error -> transient
	if got := classifyOS(errors.New("dial tcp: timeout")); got != clouddriver.FailTransient {
		t.Errorf("non-API err class=%v, want transient", got)
	}
	// text heuristic: "quota exceeded" -> quota; "throttling" -> transient
	if got := classifyOS(errors.New("Quota exceeded for instances")); got != clouddriver.FailQuota {
		t.Errorf("quota heuristic class=%v, want quota", got)
	}
	if got := classifyOS(errors.New("Throttling: too many requests")); got != clouddriver.FailTransient {
		t.Errorf("throttle heuristic class=%v, want transient", got)
	}
}
