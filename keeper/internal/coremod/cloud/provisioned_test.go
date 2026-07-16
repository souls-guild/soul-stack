package cloud_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	coremodcloud "github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/internaltest"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

type fakePlugins struct {
	createResp    []*pluginv1.VmInfo
	createErr     error
	destroyResp   []string
	destroyErr    error
	resizeResp    []*pluginv1.VmResizeResult
	resizeErr     error
	lastDriver    string
	lastProfile   map[string]any
	lastCreds     map[string]any
	lastUserdata  string
	lastName      string
	lastCount     int32
	lastDestroyed []string
	lastResizeIDs []string
	lastDesired   *pluginv1.ResizeSpec
	lastDowntime  bool
}

func (f *fakePlugins) Create(_ context.Context, driver string, profile, credentials map[string]any, count int32, userdata, name string) ([]*pluginv1.VmInfo, error) {
	f.lastDriver = driver
	f.lastProfile = profile
	f.lastCreds = credentials
	f.lastUserdata = userdata
	f.lastName = name
	f.lastCount = count
	if f.createErr != nil {
		return nil, f.createErr
	}
	return f.createResp, nil
}

func (f *fakePlugins) Destroy(_ context.Context, driver string, credentials map[string]any, ids []string) ([]string, error) {
	f.lastDriver = driver
	f.lastCreds = credentials
	f.lastDestroyed = append([]string(nil), ids...)
	if f.destroyErr != nil {
		return nil, f.destroyErr
	}
	return f.destroyResp, nil
}

// Status/List are not called by core.cloud.provisioned (created/destroyed);
// implemented as stub so fake satisfies expanded PluginHost after Status/List
// migration to credentials-Struct.
func (f *fakePlugins) Status(_ context.Context, _ string, _ map[string]any, _ string) (*pluginv1.StatusReply, error) {
	return nil, coremodcloud.ErrPluginHostNotImplemented
}

func (f *fakePlugins) List(_ context.Context, _ string, _, _ map[string]any) ([]*pluginv1.VmInfo, error) {
	return nil, coremodcloud.ErrPluginHostNotImplemented
}

func (f *fakePlugins) Resize(_ context.Context, driver string, credentials map[string]any, vmIDs []string, desired *pluginv1.ResizeSpec, allowDowntime bool) ([]*pluginv1.VmResizeResult, error) {
	f.lastDriver = driver
	f.lastCreds = credentials
	f.lastResizeIDs = append([]string(nil), vmIDs...)
	f.lastDesired = desired
	f.lastDowntime = allowDowntime
	if f.resizeErr != nil {
		return nil, f.resizeErr
	}
	return f.resizeResp, nil
}

// fakeResolver stubs ProviderResolver: maps param `provider` (=Provider name)
// to driver-name + credentials and param `profile` (=Profile name) to VM-spec params.
// By default driver == provider-name (as before A-flow so existing lastDriver checks remain valid).
type fakeResolver struct {
	driver       string // if "" — Resolve returns providerName as driver
	creds        map[string]any
	fqdnSuffix   string // self-onboard Variant T: FQDN prediction suffix
	err          error
	lastProvider string

	// profile-resolve (Variant A): profile-name → params. profileErr causes
	// ResolveProfile to return error (name not in registry).
	profileParams map[string]any
	profileErr    error
	lastProfile   string
}

func (r *fakeResolver) Resolve(_ context.Context, providerName string) (*coremodcloud.ResolvedProvider, error) {
	r.lastProvider = providerName
	if r.err != nil {
		return nil, r.err
	}
	driver := r.driver
	if driver == "" {
		driver = providerName
	}
	return &coremodcloud.ResolvedProvider{Driver: driver, Credentials: r.creds, FQDNSuffix: r.fqdnSuffix}, nil
}

func (r *fakeResolver) ResolveProfile(_ context.Context, profileName string) (map[string]any, error) {
	r.lastProfile = profileName
	if r.profileErr != nil {
		return nil, r.profileErr
	}
	return r.profileParams, nil
}

type fakeSouls struct {
	inserted     []*keepersoul.Soul
	deleted      []string // SID for which DeleteBySID was called (orphan-cleanup)
	updateCalls  []string
	updateStatus keepersoul.Status
	insertErr    error
	updateErr    error
	deleteErr    error
}

func (s *fakeSouls) Insert(_ context.Context, soul *keepersoul.Soul) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	cp := *soul
	s.inserted = append(s.inserted, &cp)
	return nil
}

func (s *fakeSouls) UpdateStatus(_ context.Context, sid string, status keepersoul.Status, _ *string) error {
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updateCalls = append(s.updateCalls, sid)
	s.updateStatus = status
	return nil
}

// remaining returns SIDs that were inserted and NOT rolled back (emulates
// registry contents after run: insert minus delete).
func (s *fakeSouls) remaining() []string {
	gone := make(map[string]bool, len(s.deleted))
	for _, sid := range s.deleted {
		gone[sid] = true
	}
	var out []string
	for _, soul := range s.inserted {
		if !gone[soul.SID] {
			out = append(out, soul.SID)
		}
	}
	return out
}

func (s *fakeSouls) DeleteBySID(_ context.Context, sid string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deleted = append(s.deleted, sid)
	return nil
}

type fakeTokens struct {
	inserted  []string // SIDs for which tokens were issued
	deleted   []string // token-id for which DeleteByTokenID was called (orphan-cleanup)
	plain     bootstraptoken.PlainToken
	insertErr error
	genErr    error
	deleteErr error
}

func newFakeTokens() *fakeTokens {
	// Generate real plain-token in test (via bootstraptoken.Generate)
	// so Hash() works same path as in production.
	pt, err := bootstraptoken.Generate()
	if err != nil {
		panic(err)
	}
	return &fakeTokens{plain: pt}
}

func (t *fakeTokens) Generate() (bootstraptoken.PlainToken, error) {
	if t.genErr != nil {
		return bootstraptoken.PlainToken{}, t.genErr
	}
	return t.plain, nil
}

func (t *fakeTokens) Insert(_ context.Context, sid, _ string, _ *string) (*bootstraptoken.Record, error) {
	if t.insertErr != nil {
		return nil, t.insertErr
	}
	t.inserted = append(t.inserted, sid)
	// TokenID deterministically = sid: test verifies rollback by token-id against SIDs.
	return &bootstraptoken.Record{TokenID: sid, SID: sid}, nil
}

func (t *fakeTokens) DeleteByTokenID(_ context.Context, tokenID string) error {
	if t.deleteErr != nil {
		return t.deleteErr
	}
	t.deleted = append(t.deleted, tokenID)
	return nil
}

func (t *fakeTokens) DeleteByTokenID(_ context.Context, tokenID string) error {
	if t.deleteErr != nil {
		return t.deleteErr
	}
	t.deleted = append(t.deleted, tokenID)
	return nil
}

type fakeCascade struct {
	lastSids      []string
	lastUsedByKID string
	counts        coremodcloud.CascadeCounts
	err           error
	calls         int
}

func (c *fakeCascade) CascadeDestroy(_ context.Context, sids []string, usedByKID string) (coremodcloud.CascadeCounts, error) {
	c.calls++
	c.lastSids = append([]string(nil), sids...)
	c.lastUsedByKID = usedByKID
	if c.err != nil {
		return coremodcloud.CascadeCounts{}, c.err
	}
	return c.counts, nil
}

type fakeAudit struct {
	events []*audit.Event
	err    error
}

func (a *fakeAudit) Write(_ context.Context, e *audit.Event) error {
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, e)
	return nil
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func TestValidate_Created_Missing(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "created",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatal("expected errors on missing provider")
	}
}

func TestValidate_UnknownState(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "rebooted",
		Params: mustStruct(t, map[string]any{}),
	})
	if rep.Ok {
		t.Fatal("expected errors on unknown state")
	}
}

func TestValidate_Created_BadCount(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"count":    float64(0),
		}),
	})
	if rep.Ok {
		t.Fatal("expected error on count=0")
	}
}

func TestApply_Created_InsertsSoulsAndTokens(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com", PrimaryIp: "10.0.0.1"},
		{VmId: "i-bbb", Fqdn: "h2.example.com", PrimaryIp: "10.0.0.2"},
	}}
	fs := &fakeSouls{}
	ft := newFakeTokens()
	fa := &fakeAudit{}

	// profile = Profile name (Variant A); resolver yields its params which
	// should reach driver.
	fr := &fakeResolver{profileParams: map[string]any{"image_id": "ami-0001"}}
	m := coremodcloud.New(fp, fr, fs, ft, nil, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"profile":  "redis-small",
			"count":    float64(2),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("unexpected: %+v", ev)
	}
	if fr.lastProfile != "redis-small" {
		t.Errorf("ResolveProfile got %q, want redis-small", fr.lastProfile)
	}
	if fp.lastProfile["image_id"] != "ami-0001" {
		t.Errorf("resolved profile params not forwarded to driver: %v", fp.lastProfile)
	}
	if fp.lastDriver != "aws" || fp.lastCount != 2 {
		t.Errorf("driver=%q count=%d", fp.lastDriver, fp.lastCount)
	}
	if len(fs.inserted) != 2 {
		t.Fatalf("inserted souls = %d, want 2", len(fs.inserted))
	}
	if fs.inserted[0].Status != keepersoul.StatusPending {
		t.Errorf("status = %q, want pending", fs.inserted[0].Status)
	}
	if len(ft.inserted) != 2 {
		t.Errorf("inserted tokens = %d, want 2", len(ft.inserted))
	}
	if len(fa.events) != 1 || fa.events[0].EventType != audit.EventCloudProvisioned {
		t.Errorf("audit event = %v", fa.events)
	}

	out := ev.Output.AsMap()
	hosts := out["hosts"].([]any)
	if len(hosts) != 2 {
		t.Errorf("hosts len = %d, want 2", len(hosts))
	}
	h0 := hosts[0].(map[string]any)
	if h0["sid"] != "h1.example.com" || h0["vm_id"] != "i-aaa" {
		t.Errorf("h0=%v", h0)
	}
	if _, has := h0["bootstrap_token"]; !has {
		t.Error("bootstrap_token missing from output hosts[0]")
	}
}

// TestApply_Created_NoProfile_NilSpec — profile not specified (Variant A optional-
// semantics): ResolveProfile NOT called, driver receives nil profile-spec,
// step does NOT fail.
func TestApply_Created_NoProfile_NilSpec(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com", PrimaryIp: "10.0.0.1"},
	}}
	// profileErr set to prove: on empty profile ResolveProfile is NOT called
	// (else test would fail with this error).
	fr := &fakeResolver{profileErr: errors.New("must not be called")}
	m := coremodcloud.New(fp, fr, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "created",
		Params: mustStruct(t, map[string]any{"provider": "aws"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("unexpected failed (no profile must be ok): %+v", stream.Last())
	}
	if fr.lastProfile != "" {
		t.Errorf("ResolveProfile called with %q; must not be called when profile omitted", fr.lastProfile)
	}
	if fp.lastProfile != nil {
		t.Errorf("driver got non-nil profile-spec %v; want nil", fp.lastProfile)
	}
}

// TestApply_Created_ProfileNotFound_Fails — ★ profile-name not in registry:
// ResolveProfile returned error → clear SendFailed (failed-event), not
// nil-panic and not false success.
func TestApply_Created_ProfileNotFound_Fails(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com"},
	}}
	fr := &fakeResolver{profileErr: errors.New("resolve profile \"ghost\": profile: name not found")}
	m := coremodcloud.New(fp, fr, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"profile":  "ghost",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("expected failed=true when profile name not in registry")
	}
	if ev.Changed {
		t.Error("expected changed=false on profile-resolve failure")
	}
	if !strings.Contains(ev.Message, "ghost") {
		t.Errorf("failed-event must name the missing profile; got %q", ev.Message)
	}
	// Driver must not be called if profile did not resolve.
	if fp.lastDriver != "" {
		t.Errorf("driver Create called despite profile-resolve failure (driver=%q)", fp.lastDriver)
	}
}

// TestApply_Created_PassesDriverCredsUserdata — A-flow: module resolves
// provider to driver-name + credentials and passes them (plus userdata) to
// PluginHost.Create.
func TestApply_Created_PassesDriverCredsUserdata(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com", PrimaryIp: "10.0.0.1"},
	}}
	fr := &fakeResolver{
		driver: "aws", // Provider.Type
		creds:  map[string]any{"access_key_id": "AKIA", "region": "eu-west-1"},
	}
	m := coremodcloud.New(fp, fr, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "aws-prod", // Provider name
			"userdata": "#cloud-config\n",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("unexpected failed: %+v", stream.Last())
	}
	if fr.lastProvider != "aws-prod" {
		t.Errorf("resolver got provider=%q, want aws-prod", fr.lastProvider)
	}
	if fp.lastDriver != "aws" {
		t.Errorf("PluginHost driver=%q, want aws (= Provider.Type)", fp.lastDriver)
	}
	if fp.lastCreds["access_key_id"] != "AKIA" || fp.lastCreds["region"] != "eu-west-1" {
		t.Errorf("credentials not forwarded: %v", fp.lastCreds)
	}
	if fp.lastUserdata != "#cloud-config\n" {
		t.Errorf("userdata=%q, want cloud-config blob", fp.lastUserdata)
	}
}

// TestApply_Created_ResolverError_MasksVaultRef — resolve error carrying
// vault-ref does not leak plaintext in failed-event.
func TestApply_Created_ResolverError_MasksVaultRef(t *testing.T) {
	fr := &fakeResolver{err: errors.New("provider \"p\" credentials_ref: read vault:secret/cloud/aws-prod failed")}
	m := coremodcloud.New(&fakePlugins{}, fr, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "created",
		Params: mustStruct(t, map[string]any{"provider": "p"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("expected failed=true")
	}
	if strings.Contains(ev.Message, "vault:secret/") {
		t.Errorf("vault-ref leaked in failed-event message: %q", ev.Message)
	}
}

// TestApply_Created_NilResolver_Fails — defensive: resolver not configured.
func TestApply_Created_NilResolver_Fails(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, nil, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "created",
		Params: mustStruct(t, map[string]any{"provider": "p"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true when resolver is nil")
	}
}

func TestApply_Created_PluginError_FailsTask(t *testing.T) {
	fp := &fakePlugins{createErr: coremodcloud.ErrPluginHostNotImplemented}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on plugin error")
	}
}

// TestApply_Created_DriverFailure_NotFalseOperational — guard on core bug:
// driver reported failure (cluster read-only / quota), adapter propagates as
// error → core.cloud.created MUST yield failed-event (step failed →
// incarnation error_locked), NOT false success (0 VM, operational).
// Driver-message must reach failed-event for operator diagnostics.
func TestApply_Created_DriverFailure_NotFalseOperational(t *testing.T) {
	fp := &fakePlugins{createErr: errors.New("driver create failed: cluster is read-only")}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("expected failed=true on driver failure (must NOT be false operational with 0 VM)")
	}
	if ev.Changed {
		t.Error("expected changed=false on driver failure")
	}
	if !strings.Contains(ev.Message, "cluster is read-only") {
		t.Errorf("failed-event message = %q, want driver message surfaced", ev.Message)
	}
}

func TestApply_Created_VmWithoutFqdn(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa"}, // no fqdn
	}}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "created",
		Params: mustStruct(t, map[string]any{"provider": "aws"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true when VM has no fqdn")
	}
}

func TestApply_Destroyed_CascadesSidsAndAuditsCounts(t *testing.T) {
	fp := &fakePlugins{destroyResp: []string{"i-aaa", "i-bbb"}}
	fs := &fakeSouls{}
	fa := &fakeAudit{}
	fc := &fakeCascade{counts: coremodcloud.CascadeCounts{SoulsUpdated: 2, SeedsOrphaned: 2, TokensBurned: 1}}
	m := coremodcloud.New(fp, &fakeResolver{}, fs, newFakeTokens(), fc, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "destroyed",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-aaa", "i-bbb"},
			"sids":     []any{"h1.example.com", "h2.example.com"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("unexpected: %+v", ev)
	}
	if fc.calls != 1 {
		t.Fatalf("Cascade.CascadeDestroy called %d times, want 1", fc.calls)
	}
	if !reflect.DeepEqual(fc.lastSids, []string{"h1.example.com", "h2.example.com"}) {
		t.Errorf("cascade sids=%v", fc.lastSids)
	}
	if fc.lastUsedByKID != bootstraptoken.SystemKIDCloudDestroy {
		t.Errorf("cascade usedByKID=%q, want %q", fc.lastUsedByKID, bootstraptoken.SystemKIDCloudDestroy)
	}
	if len(fs.updateCalls) != 0 {
		t.Errorf("SoulStore.UpdateStatus must NOT be called (cascade owns transitions); got %v", fs.updateCalls)
	}
	if len(fa.events) != 1 {
		t.Fatalf("audit events=%d", len(fa.events))
	}
	payload := fa.events[0].Payload
	for _, k := range []string{"souls_updated", "seeds_orphaned", "tokens_burned"} {
		if _, ok := payload[k]; !ok {
			t.Errorf("audit payload missing key %q: %v", k, payload)
		}
	}
}

func TestApply_Destroyed_CascadeError_Fails(t *testing.T) {
	fp := &fakePlugins{destroyResp: []string{"i-aaa"}}
	fc := &fakeCascade{err: errors.New("pg down")}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), fc, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "destroyed",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-aaa"},
			"sids":     []any{"h1.example.com"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on cascade error")
	}
}

func TestApply_Destroyed_NoCascader_WithSids_Fails(t *testing.T) {
	// Defensive: if caller wire-up forgot Cascader but sids specified —
	// destroyed-state fails scenario with clear error.
	fp := &fakePlugins{destroyResp: []string{"i-aaa"}}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "destroyed",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-aaa"},
			"sids":     []any{"h1.example.com"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true when Cascader is nil and sids non-empty")
	}
}

func TestApply_Destroyed_PluginError(t *testing.T) {
	fp := &fakePlugins{destroyErr: errors.New("forbidden")}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "destroyed",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-aaa"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true")
	}
}

func TestApply_Destroyed_NoSids_NoCascade(t *testing.T) {
	fp := &fakePlugins{destroyResp: []string{"i-aaa"}}
	fc := &fakeCascade{}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), fc, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "destroyed",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-aaa"},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if fc.calls != 0 {
		t.Errorf("CascadeDestroy called %d times without sids; want 0", fc.calls)
	}
}

// fakeUserdata stubs UserdataProvider for testing generate_userdata flow and
// self-onboard (Variant T).
type fakeUserdata struct {
	out            string
	err            error
	last           context.Context
	selfOnboardOut string
	selfOnboardErr error
	lastTokens     map[string]string // tokens passed to self-onboard render
}

func (u *fakeUserdata) GenerateUserdata(ctx context.Context) (string, error) {
	u.last = ctx
	return u.out, u.err
}

func (u *fakeUserdata) GenerateUserdataSelfOnboard(ctx context.Context, tokens map[string]string) (string, error) {
	u.last = ctx
	u.lastTokens = tokens
	if u.selfOnboardErr != nil {
		return "", u.selfOnboardErr
	}
	out := u.selfOnboardOut
	if out == "" {
		out = "#cloud-config\n# self-onboard\n"
	}
	return out, nil
}

// TestApply_Created_GenerateUserdataTrue — ADR-017(h) amendment 2026-05-27,
// B-flat: `generate_userdata: true` + empty `userdata` → driver Create-call
// receives rendered cloud-config from UserdataProvider.
func TestApply_Created_GenerateUserdataTrue(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com", PrimaryIp: "10.0.0.1"},
	}}
	rendered := "#cloud-config\nruncmd: [echo soul]\n"
	fu := &fakeUserdata{out: rendered}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{}).WithUserdata(fu)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":          "aws",
			"generate_userdata": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("unexpected failed: %+v", stream.Last())
	}
	if fp.lastUserdata != rendered {
		t.Errorf("driver got userdata=%q, want rendered cloud-config", fp.lastUserdata)
	}
	if fu.last == nil {
		t.Error("UserdataProvider.GenerateUserdata was not called")
	}
}

// TestApply_Created_BothUserdataAndGenerate_Error — explicit `userdata:` AND
// `generate_userdata: true` simultaneously → failed=true (mutually exclusive).
func TestApply_Created_BothUserdataAndGenerate_Error(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com"},
	}}
	fu := &fakeUserdata{out: "#cloud-config\n"}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{}).WithUserdata(fu)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":          "aws",
			"userdata":          "#cloud-config\nmanual\n",
			"generate_userdata": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on mutually-exclusive params")
	}
	if !strings.Contains(stream.Last().Message, "mutually exclusive") {
		t.Errorf("error message does not explain conflict: %q", stream.Last().Message)
	}
}

// TestApply_Created_ExplicitUserdata — passthrough as before (without UserdataProvider).
func TestApply_Created_ExplicitUserdata(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com"},
	}}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"userdata": "#cloud-config\nmanual\n",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("unexpected failed: %+v", stream.Last())
	}
	if fp.lastUserdata != "#cloud-config\nmanual\n" {
		t.Errorf("explicit userdata not passed through: %q", fp.lastUserdata)
	}
}

// TestApply_Created_GenerateUserdata_NoProvider — generate_userdata: true,
// but UserdataProvider not configured → clear error.
func TestApply_Created_GenerateUserdata_NoProvider(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com"},
	}}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":          "aws",
			"generate_userdata": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true when UserdataProvider is nil")
	}
}

// TestApply_Created_GenerateUserdata_ProviderError — UserdataProvider returns
// error → failed=true; error is masked (vault-ref filter).
func TestApply_Created_GenerateUserdata_ProviderError(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "h1.example.com"},
	}}
	fu := &fakeUserdata{err: errors.New("read vault:secret/keeper/ca failed")}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{}).WithUserdata(fu)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":          "aws",
			"generate_userdata": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on userdata error")
	}
	if strings.Contains(stream.Last().Message, "vault:secret/") {
		t.Errorf("vault-ref leaked in failed-event: %q", stream.Last().Message)
	}
}

// --- self-onboard (Variant T, ADR-017(h) amendment) ---

// TestApply_Created_SelfOnboard_PredictsFQDNAndBakesTokens — self_onboard=true:
// keeper predicts FQDN for each VM (`<name>-<i>.<suffix>`), issues per-VM
// tokens, bakes them into userdata (map FQDN→token), passes base-name to
// CreateRequest.name. Plain-tokens NOT placed in register.
func TestApply_Created_SelfOnboard_PredictsFQDNAndBakesTokens(t *testing.T) {
	// Driver returns VM with FQDN matching predicted (honor name).
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "redis-0.ns.vm.clv3", PrimaryIp: "10.0.0.1"},
		{VmId: "i-bbb", Fqdn: "redis-1.ns.vm.clv3", PrimaryIp: "10.0.0.2"},
	}}
	fs := &fakeSouls{}
	ft := newFakeTokens()
	fu := &fakeUserdata{selfOnboardOut: "#cloud-config\n# baked\n"}
	fr := &fakeResolver{driver: "wb", fqdnSuffix: "ns.vm.clv3"}
	m := coremodcloud.New(fp, fr, fs, ft, nil, &fakeAudit{}).WithUserdata(fu)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"name":         "redis",
			"count":        float64(2),
			"self_onboard": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("unexpected: %+v", ev)
	}

	// base-name passed to driver in CreateRequest.name.
	if fp.lastName != "redis" {
		t.Errorf("driver got name=%q, want redis (CreateRequest.name)", fp.lastName)
	}
	// userdata = self-onboard render (not generic).
	if fp.lastUserdata != "#cloud-config\n# baked\n" {
		t.Errorf("driver got userdata=%q, want self-onboard baked", fp.lastUserdata)
	}
	// Tokens baked under PREDICTED FQDNs (not under actual from response —
	// they are issued BEFORE create).
	if len(fu.lastTokens) != 2 {
		t.Fatalf("baked tokens = %d, want 2", len(fu.lastTokens))
	}
	for _, fqdn := range []string{"redis-0.ns.vm.clv3", "redis-1.ns.vm.clv3"} {
		if _, ok := fu.lastTokens[fqdn]; !ok {
			t.Errorf("predicted FQDN %q missing from baked token map %v", fqdn, keysOf(fu.lastTokens))
		}
	}
	// Souls + tokens issued for predicted FQDNs.
	if len(fs.inserted) != 2 || len(ft.inserted) != 2 {
		t.Fatalf("souls=%d tokens=%d, want 2/2", len(fs.inserted), len(ft.inserted))
	}
	if fs.inserted[0].SID != "redis-0.ns.vm.clv3" {
		t.Errorf("first soul SID=%q, want predicted redis-0.ns.vm.clv3", fs.inserted[0].SID)
	}

	// ★ Plain bootstrap_token NOT in register (no delivery, VM self-onboards).
	out := ev.Output.AsMap()
	hosts := out["hosts"].([]any)
	if len(hosts) != 2 {
		t.Fatalf("hosts=%d, want 2", len(hosts))
	}
	for i, h := range hosts {
		hm := h.(map[string]any)
		if _, has := hm["bootstrap_token"]; has {
			t.Errorf("hosts[%d] must NOT carry bootstrap_token in self-onboard (token in userdata, no delivery step)", i)
		}
	}
	if so, _ := out["self_onboard"].(bool); !so {
		t.Error("output must mark self_onboard=true")
	}
}

// TestApply_Created_SelfOnboard_NoFQDNSuffix_Fails — provider without fqdn_suffix:
// keeper cannot predict FQDN → clear failed (not silent provision).
func TestApply_Created_SelfOnboard_NoFQDNSuffix_Fails(t *testing.T) {
	fp := &fakePlugins{}
	fu := &fakeUserdata{}
	fr := &fakeResolver{driver: "wb", fqdnSuffix: ""} // no suffix
	m := coremodcloud.New(fp, fr, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{}).WithUserdata(fu)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"name":         "redis",
			"count":        float64(1),
			"self_onboard": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true when provider has no fqdn_suffix")
	}
	if !strings.Contains(stream.Last().Message, "fqdn_suffix") {
		t.Errorf("error must name fqdn_suffix requirement; got %q", stream.Last().Message)
	}
	// Driver must not be called without predictable FQDN.
	if fp.lastDriver != "" {
		t.Errorf("driver called despite missing fqdn_suffix (driver=%q)", fp.lastDriver)
	}
}

// TestApply_Created_SelfOnboard_FQDNMismatch_Fails — driver named VM differently than
// keeper predicted (not honor name): token in userdata under predicted FQDN,
// VM has different hostname → self-onboard broken silently. Fail-fast.
func TestApply_Created_SelfOnboard_FQDNMismatch_Fails(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "soul-anon-999-0.ns.vm.clv3"}, // NOT redis-0.*
	}}
	fu := &fakeUserdata{selfOnboardOut: "#cloud-config\n"}
	fr := &fakeResolver{driver: "wb", fqdnSuffix: "ns.vm.clv3"}
	m := coremodcloud.New(fp, fr, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{}).WithUserdata(fu)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"name":         "redis",
			"count":        float64(1),
			"self_onboard": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true when actual FQDN != predicted (driver ignored name)")
	}
	if !strings.Contains(stream.Last().Message, "predicted") {
		t.Errorf("error must explain FQDN mismatch; got %q", stream.Last().Message)
	}
}

// TestApply_Created_SelfOnboard_CreateFails_CleansOrphanedSoulsAndTokens —
// ★ guard on major review risk: souls (pending) + tokens issued BEFORE
// PluginHost.Create. If Create fails, inserted records MUST be
// rolled back — else presence-barrier await_online hangs on onboarding
// nonexistent VMs, and rerun-last hits PK-conflict on insert soul under
// same predicted FQDN (self-onboard not idempotent).
func TestApply_Created_SelfOnboard_CreateFails_CleansOrphanedSoulsAndTokens(t *testing.T) {
	fp := &fakePlugins{createErr: errors.New("driver create failed: quota exceeded")}
	fs := &fakeSouls{}
	ft := newFakeTokens()
	fu := &fakeUserdata{selfOnboardOut: "#cloud-config\n"}
	fr := &fakeResolver{driver: "wb", fqdnSuffix: "ns.vm.clv3"}
	m := coremodcloud.New(fp, fr, fs, ft, nil, &fakeAudit{}).WithUserdata(fu)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"name":         "redis",
			"count":        float64(2),
			"self_onboard": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on create failure")
	}
	// Registry souls empty: all inserted predicted-FQDNs rolled back.
	if rem := fs.remaining(); len(rem) != 0 {
		t.Errorf("orphaned souls after create-fail: %v — rerun will hit PK conflict", rem)
	}
	// Tokens rolled back: DeleteByTokenID called for each issued (token-id=SID
	// in fake). Else bootstrap-capability hangs for nonexistent VM.
	if len(ft.deleted) != len(ft.inserted) {
		t.Errorf("orphaned tokens: inserted %v, deleted %v", ft.inserted, ft.deleted)
	}
	for _, sid := range ft.inserted {
		found := false
		for _, del := range ft.deleted {
			if del == sid {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("token for %q not rolled back (deleted=%v)", sid, ft.deleted)
		}
	}
}

// TestApply_Created_SelfOnboard_FQDNMismatch_CleansOrphaned — same orphan-risk,
// but failure happens AFTER successful Create (driver named VM not as
// predicted). Inserted souls/tokens still rolled back.
func TestApply_Created_SelfOnboard_FQDNMismatch_CleansOrphaned(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "soul-anon-999-0.ns.vm.clv3"}, // NOT redis-0.*
	}}
	fs := &fakeSouls{}
	ft := newFakeTokens()
	fu := &fakeUserdata{selfOnboardOut: "#cloud-config\n"}
	fr := &fakeResolver{driver: "wb", fqdnSuffix: "ns.vm.clv3"}
	m := coremodcloud.New(fp, fr, fs, ft, nil, &fakeAudit{}).WithUserdata(fu)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"name":         "redis",
			"count":        float64(1),
			"self_onboard": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on FQDN mismatch")
	}
	if rem := fs.remaining(); len(rem) != 0 {
		t.Errorf("orphaned souls after FQDN-mismatch fail: %v", rem)
	}
	if len(ft.deleted) != len(ft.inserted) {
		t.Errorf("orphaned tokens after FQDN-mismatch: inserted %v, deleted %v", ft.inserted, ft.deleted)
	}
}

// TestApply_Created_SelfOnboard_UserdataFails_CleansOrphaned — failure happens
// AFTER souls/tokens insertion, but BEFORE create: self-onboard userdata render
// failed (e.g. vault-ref in template). Provision not yet reached provider, but
// registry records exist → defer-cleanup MUST roll them back (else rerun hits
// PK-conflict on insert soul under same predicted FQDN, token hangs capability
// for nonexistent VM). Driver must not be called.
func TestApply_Created_SelfOnboard_UserdataFails_CleansOrphaned(t *testing.T) {
	fp := &fakePlugins{}
	fs := &fakeSouls{}
	ft := newFakeTokens()
	fu := &fakeUserdata{selfOnboardErr: errors.New("render self-onboard userdata: read vault:secret/keeper/ca failed")}
	fr := &fakeResolver{driver: "wb", fqdnSuffix: "ns.vm.clv3"}
	m := coremodcloud.New(fp, fr, fs, ft, nil, &fakeAudit{}).WithUserdata(fu)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"name":         "redis",
			"count":        float64(2),
			"self_onboard": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on self-onboard userdata render error")
	}
	// vault-ref did not leak into failed-event.
	// Souls/tokens issued BEFORE userdata-render — rolled back by defer on render failure.
	if strings.Contains(stream.Last().Message, "vault:secret/") {
		t.Errorf("vault-ref leaked in failed-event: %q", stream.Last().Message)
	}
	if rem := fs.remaining(); len(rem) != 0 {
		t.Errorf("orphaned souls after userdata-render fail: %v — rerun will hit PK conflict", rem)
	}
	if len(ft.deleted) != len(ft.inserted) {
		t.Errorf("orphaned tokens after userdata-render fail: inserted %v, deleted %v", ft.inserted, ft.deleted)
	}
	// Provision failed before create — driver not called.
	if fp.lastDriver != "" {
		t.Errorf("driver Create called despite userdata-render failure (driver=%q)", fp.lastDriver)
	}
}

// TestApply_Created_SelfOnboard_EmptyFQDN_CleansOrphaned — Create passed, but
// provider returned VM without Fqdn (provisioned.go: sid == "" → SendFailed). Same
// orphan-risk: souls/tokens issued BEFORE create, rolled back by defer. Differs
// from FQDNMismatch in hitting branch of empty (not mismatched) FQDN.
func TestApply_Created_SelfOnboard_EmptyFQDN_CleansOrphaned(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa"}, // Create ok, but Fqdn empty
	}}
	fs := &fakeSouls{}
	ft := newFakeTokens()
	fu := &fakeUserdata{selfOnboardOut: "#cloud-config\n"}
	fr := &fakeResolver{driver: "wb", fqdnSuffix: "ns.vm.clv3"}
	m := coremodcloud.New(fp, fr, fs, ft, nil, &fakeAudit{}).WithUserdata(fu)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"name":         "redis",
			"count":        float64(1),
			"self_onboard": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true when provider returns VM without fqdn")
	}
	if rem := fs.remaining(); len(rem) != 0 {
		t.Errorf("orphaned souls after empty-fqdn fail: %v", rem)
	}
	if len(ft.deleted) != len(ft.inserted) {
		t.Errorf("orphaned tokens after empty-fqdn fail: inserted %v, deleted %v", ft.inserted, ft.deleted)
	}
}

// TestApply_Created_SelfOnboard_Success_NoCleanup — on success path cleanup does NOT
// trigger: inserted souls/tokens remain (await_online waits for them).
func TestApply_Created_SelfOnboard_Success_NoCleanup(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "redis-0.ns.vm.clv3", PrimaryIp: "10.0.0.1"},
		{VmId: "i-bbb", Fqdn: "redis-1.ns.vm.clv3", PrimaryIp: "10.0.0.2"},
	}}
	fs := &fakeSouls{}
	ft := newFakeTokens()
	fu := &fakeUserdata{selfOnboardOut: "#cloud-config\n"}
	fr := &fakeResolver{driver: "wb", fqdnSuffix: "ns.vm.clv3"}
	m := coremodcloud.New(fp, fr, fs, ft, nil, &fakeAudit{}).WithUserdata(fu)

	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"name":         "redis",
			"count":        float64(2),
			"self_onboard": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("unexpected failed: %+v", stream.Last())
	}
	if len(fs.deleted) != 0 || len(ft.deleted) != 0 {
		t.Errorf("cleanup must NOT run on success: souls-deleted=%v tokens-deleted=%v", fs.deleted, ft.deleted)
	}
	if len(fs.remaining()) != 2 {
		t.Errorf("both souls must remain on success; remaining=%v", fs.remaining())
	}
}

// TestValidate_Created_SelfOnboard_RequiresName — self_onboard: true without name →
// validate error (cannot predict FQDN without base name).
func TestValidate_Created_SelfOnboard_RequiresName(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider":     "wb-prod",
			"self_onboard": true,
		}),
	})
	if rep.Ok {
		t.Fatal("expected validate error: self_onboard requires name")
	}
}

// TestApply_Created_Name_PassedThroughNonSelfOnboard — name passed to driver
// even outside self-onboard (meaningful name instead of auto-slug), B-flat tokens as before.
func TestApply_Created_Name_PassedThroughNonSelfOnboard(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa", Fqdn: "redis-0.ns.vm.clv3"},
	}}
	m := coremodcloud.New(fp, &fakeResolver{driver: "wb"}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "wb-prod",
			"name":     "redis",
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("unexpected failed: %+v", stream.Last())
	}
	if fp.lastName != "redis" {
		t.Errorf("driver got name=%q, want redis (passed through non-self-onboard)", fp.lastName)
	}
	// B-flat: plain-token in register (delivery by separate step).
	hosts := stream.Last().Output.AsMap()["hosts"].([]any)
	if _, has := hosts[0].(map[string]any)["bootstrap_token"]; !has {
		t.Error("non-self-onboard must keep bootstrap_token in register (B-flat)")
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestApply_StubHost_FailsCleanly(t *testing.T) {
	// Integration-like smoke: StubHost returns ErrPluginHostNotImplemented,
	// module must turn it into failed=true, not crash.
	m := coremodcloud.New(coremodcloud.StubHost{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "created",
		Params: mustStruct(t, map[string]any{"provider": "aws"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true from StubHost")
	}
}

// --- resized (state) ---

func TestValidate_Resized_Ok(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "resized",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-1"},
			"desired":  map[string]any{"cpu_cores": float64(4), "ram_mb": float64(8192)},
		}),
	})
	if !rep.Ok {
		t.Fatalf("expected ok, got errors: %v", rep.Errors)
	}
}

func TestValidate_Resized_MissingDesired(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "resized",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-1"},
		}),
	})
	if rep.Ok {
		t.Fatal("expected error on missing desired")
	}
}

func TestValidate_Resized_AllZeroDesired(t *testing.T) {
	m := coremodcloud.New(&fakePlugins{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), &fakeCascade{}, &fakeAudit{})
	rep, _ := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State: "resized",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-1"},
			"desired":  map[string]any{"cpu_cores": float64(0), "ram_mb": float64(0), "disk_gb": float64(0)},
		}),
	})
	if rep.Ok {
		t.Fatal("expected error on all-zero desired (no dimension to change)")
	}
}

func TestApply_Resized_PassesDesiredAndAggregates(t *testing.T) {
	fp := &fakePlugins{resizeResp: []*pluginv1.VmResizeResult{
		{VmId: "i-1", CausedDowntime: true},
		{VmId: "i-2", CausedDowntime: false},
	}}
	m := coremodcloud.New(fp, &fakeResolver{driver: "wb"}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "resized",
		Params: mustStruct(t, map[string]any{
			"provider":       "prox",
			"vm_ids":         []any{"i-1", "i-2"},
			"desired":        map[string]any{"cpu_cores": float64(4), "ram_mb": float64(8192), "disk_gb": float64(100)},
			"allow_downtime": true,
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("unexpected failed: %+v", ev)
	}
	if !ev.Changed {
		t.Fatal("resize must report changed=true")
	}
	// Our units forwarded to ResizeSpec without conversion (conversion to WB-bytes — driver).
	if fp.lastDesired.GetCpuCores() != 4 || fp.lastDesired.GetRamMb() != 8192 || fp.lastDesired.GetDiskGb() != 100 {
		t.Fatalf("desired not propagated: %+v", fp.lastDesired)
	}
	if !fp.lastDowntime {
		t.Fatal("allow_downtime not propagated")
	}
	if fp.lastDriver != "wb" {
		t.Fatalf("driver=%q, want resolved wb", fp.lastDriver)
	}
	out := ev.Output.AsMap()
	if dt, _ := out["caused_downtime"].(bool); !dt {
		t.Fatal("aggregate caused_downtime must be true (one VM had downtime)")
	}
	results, _ := out["results"].([]any)
	if len(results) != 2 {
		t.Fatalf("results=%v, want 2", results)
	}
}

func TestApply_Resized_PerVmErrorInOutput(t *testing.T) {
	fp := &fakePlugins{resizeResp: []*pluginv1.VmResizeResult{
		{VmId: "i-1"},
		{VmId: "i-2", Error: "quota_exceeded: disk"},
	}}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "resized",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-1", "i-2"},
			"desired":  map[string]any{"disk_gb": float64(200)},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("per-VM error must NOT fail whole step: %+v", ev)
	}
	out := ev.Output.AsMap()
	errs, _ := out["errors"].([]any)
	if len(errs) != 1 {
		t.Fatalf("errors=%v, want 1 per-VM error surfaced", errs)
	}
}

func TestApply_Resized_PluginError_FailsTask(t *testing.T) {
	fp := &fakePlugins{resizeErr: errors.New("provider down")}
	m := coremodcloud.New(fp, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "resized",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-1"},
			"desired":  map[string]any{"cpu_cores": float64(2)},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true on resize plugin error")
	}
}

func TestApply_Resized_StubHost_FailsCleanly(t *testing.T) {
	// StubHost returns ErrPluginHostNotImplemented → failed-event, not crash.
	m := coremodcloud.New(coremodcloud.StubHost{}, &fakeResolver{}, &fakeSouls{}, newFakeTokens(), nil, &fakeAudit{})
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "resized",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"vm_ids":   []any{"i-1"},
			"desired":  map[string]any{"ram_mb": float64(4096)},
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("expected failed=true from StubHost")
	}
}

var _ context.Context = context.Background() // guard against unused-import on rearrange
