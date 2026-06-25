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
	lastCount     int32
	lastDestroyed []string
	lastResizeIDs []string
	lastDesired   *pluginv1.ResizeSpec
	lastDowntime  bool
}

func (f *fakePlugins) Create(_ context.Context, driver string, profile, credentials map[string]any, count int32, userdata string) ([]*pluginv1.VmInfo, error) {
	f.lastDriver = driver
	f.lastProfile = profile
	f.lastCreds = credentials
	f.lastUserdata = userdata
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

// Status/List — модуль `core.cloud.provisioned` (created/destroyed) их не
// вызывает; реализованы как stub, чтобы fake удовлетворял расширенному
// PluginHost после миграции Status/List на credentials-Struct.
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

// fakeResolver — стаб ProviderResolver: маппит param `provider` (= имя Provider)
// в driver-имя + credentials. По умолчанию driver == имя provider-а (как было
// до A-flow, чтобы существующие проверки lastDriver не ломались).
type fakeResolver struct {
	driver       string // если "" — Resolve вернёт providerName как driver
	creds        map[string]any
	err          error
	lastProvider string
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
	return &coremodcloud.ResolvedProvider{Driver: driver, Credentials: r.creds}, nil
}

type fakeSouls struct {
	inserted     []*keepersoul.Soul
	updateCalls  []string
	updateStatus keepersoul.Status
	insertErr    error
	updateErr    error
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

type fakeTokens struct {
	inserted  []string // SIDs, для которых выпустили
	plain     bootstraptoken.PlainToken
	insertErr error
	genErr    error
}

func newFakeTokens() *fakeTokens {
	// Сгенерируем реальный plain-токен в тесте (через bootstraptoken.Generate),
	// чтобы Hash() работал по тому же пути, что и в проде.
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
	return &bootstraptoken.Record{SID: sid}, nil
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

	m := coremodcloud.New(fp, &fakeResolver{}, fs, ft, nil, fa)
	stream := internaltest.NewApplyStream()
	if err := m.Apply(&pluginv1.ApplyRequest{
		State: "created",
		Params: mustStruct(t, map[string]any{
			"provider": "aws",
			"profile":  map[string]any{"image_id": "ami-0001"},
			"count":    float64(2),
		}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed || !ev.Changed {
		t.Fatalf("unexpected: %+v", ev)
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

// TestApply_Created_PassesDriverCredsUserdata — A-flow: модуль резолвит
// provider в driver-имя + credentials и прокидывает их (плюс userdata) в
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
			"provider": "aws-prod", // имя Provider-а
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

// TestApply_Created_ResolverError_MasksVaultRef — ошибка резолва, несущая
// vault-ref, не утекает plaintext в failed-event.
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

// TestApply_Created_NilResolver_Fails — defensive: resolver не сконфигурирован.
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

func TestApply_Created_VmWithoutFqdn(t *testing.T) {
	fp := &fakePlugins{createResp: []*pluginv1.VmInfo{
		{VmId: "i-aaa"}, // нет fqdn
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
	// Defensive: если caller wire-up забыл Cascader, но sids указаны —
	// destroyed-state валит scenario с понятной ошибкой.
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

// fakeUserdata — стаб UserdataProvider для тестов generate_userdata-флоу.
type fakeUserdata struct {
	out  string
	err  error
	last context.Context
}

func (u *fakeUserdata) GenerateUserdata(ctx context.Context) (string, error) {
	u.last = ctx
	return u.out, u.err
}

// TestApply_Created_GenerateUserdataTrue — ADR-017(h) amendment 2026-05-27,
// B-flat: `generate_userdata: true` + пустой `userdata` → driver Create-call
// получает rendered cloud-config от UserdataProvider.
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

// TestApply_Created_BothUserdataAndGenerate_Error — explicit `userdata:` И
// `generate_userdata: true` одновременно → failed=true (mutually exclusive).
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

// TestApply_Created_ExplicitUserdata — passthrough как раньше (без UserdataProvider).
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
// но UserdataProvider не сконфигурирован → внятная ошибка.
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

// TestApply_Created_GenerateUserdata_ProviderError — UserdataProvider возвращает
// ошибку → failed=true; ошибка маскируется (vault-ref-фильтр).
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

func TestApply_StubHost_FailsCleanly(t *testing.T) {
	// Integration-like smoke: StubHost возвращает ErrPluginHostNotImplemented,
	// модуль обязан превратить его в failed=true, а не crash.
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
	// Наши единицы прокинуты в ResizeSpec без конверсии (конверсия в WB-байты — драйвер).
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
	// StubHost возвращает ErrPluginHostNotImplemented → failed-event, не crash.
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

var _ context.Context = context.Background() // защита от unused-import при rearrange
