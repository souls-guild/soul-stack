package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	computev1 "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
)

// withFastBackoff replaces defaultBackoff with near-zero delays plus the given
// MaxAttempts. Used in wait-deadline / transient-probe tests where the default
// 1s->2s->4s backoff would make the test slow.
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

// fakeYC is a mock ycAPI for L0 unit tests (no network). Behavior is configured
// per method; getSeq models a PROVISIONING->RUNNING transition between poller
// rounds. getFn is an override for transient-probe-error tests.
type fakeYC struct {
	createOut  *computev1.Instance
	createErr  error
	createCall int

	listOut  *computev1.ListInstancesResponse
	listErr  error
	listCall int

	getSeq []*computev1.Instance
	getIdx int
	getErr error
	getFn  func(call int) (*computev1.Instance, error)
	getN   int

	deleteErr error
	deleteN   int

	lastCreateInput *computev1.CreateInstanceRequest
	lastListInput   *computev1.ListInstancesRequest
	lastDeleted     []string
}

func (f *fakeYC) CreateInstance(_ context.Context, in *computev1.CreateInstanceRequest, _ ...grpc.CallOption) (*computev1.Instance, error) {
	f.createCall++
	f.lastCreateInput = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createOut != nil {
		// Keep the ID returned by the CREATE call for later Get calls.
		// Callers fill getSeq separately to model the lifecycle.
		return f.createOut, nil
	}
	// Default: fabricate a unique ID from the request name.
	return &computev1.Instance{Id: in.GetName()}, nil
}

func (f *fakeYC) GetInstance(_ context.Context, id string, _ ...grpc.CallOption) (*computev1.Instance, error) {
	call := f.getN
	f.getN++
	if f.getFn != nil {
		return f.getFn(call)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if len(f.getSeq) == 0 {
		return &computev1.Instance{Id: id}, nil
	}
	out := f.getSeq[f.getIdx]
	if f.getIdx < len(f.getSeq)-1 {
		f.getIdx++
	}
	return out, nil
}

func (f *fakeYC) DeleteInstance(_ context.Context, id string, _ ...grpc.CallOption) error {
	f.deleteN++
	f.lastDeleted = append(f.lastDeleted, id)
	return f.deleteErr
}

func (f *fakeYC) ListInstances(_ context.Context, in *computev1.ListInstancesRequest, _ ...grpc.CallOption) (*computev1.ListInstancesResponse, error) {
	f.listCall++
	f.lastListInput = in
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listOut == nil {
		return &computev1.ListInstancesResponse{}, nil
	}
	return f.listOut, nil
}

// withFakeYC replaces the client factory so it returns f.
func withFakeYC(t *testing.T, f *fakeYC) {
	t.Helper()
	orig := newYcClient
	newYcClient = func(_ context.Context, _ ycCredentials) (ycAPI, error) { return f, nil }
	t.Cleanup(func() { newYcClient = orig })
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

func runningInstance(id, ip, fqdn string) *computev1.Instance {
	return &computev1.Instance{
		Id:     id,
		Name:   id, // name = id: gap-fill (NIM-16) computes occupancy via GetName()
		Fqdn:   fqdn,
		Status: computev1.Instance_RUNNING,
		ZoneId: "ru-central1-a",
		NetworkInterfaces: []*computev1.NetworkInterface{{
			PrimaryV4Address: &computev1.PrimaryAddress{Address: ip},
		}},
	}
}

func validProfile() map[string]any {
	return map[string]any{
		"folder_id": "b1g111111111",
		"zone":      "ru-central1-a",
		"image_id":  "fd811111111111111111",
		"subnet_id": "e9b111111111",
		"resources": map[string]any{"cores": 2, "memory_gb": 2},
	}
}

func validCredsIAM() map[string]any {
	return map[string]any{
		"iam_token": "t1.fake-iam-token",
		"folder_id": "b1g111111111",
		"zone":      "ru-central1-a",
	}
}

// labeledProfile is validProfile plus the identity run-label (Create fails
// closed without name and label, NIM-16).
func labeledProfile(run string) map[string]any {
	p := validProfile()
	p["labels"] = map[string]any{runLabelKey: run}
	return p
}

// TestVmName_Precedence checks deterministic naming (NIM-16): nameBase from
// CreateRequest.name yields `<nameBase>-<seq>` (Keeper predicts FQDN from it)
// and wins over runLabel; without nameBase it is `soul-<runLabel>-<seq>` (anon
// branch removed).
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

func TestSchema_ParsesEmbedded(t *testing.T) {
	d := &YcDriver{}
	rep, err := d.Schema(context.Background(), &pluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	m := rep.ProfileSchema.AsMap()
	req, _ := m["required"].([]any)
	if len(req) != 4 {
		t.Errorf("schema required=%v, want 4 fields (folder_id/zone/image_id/subnet_id)", req)
	}
}

func TestValidate_MissingFields(t *testing.T) {
	d := &YcDriver{}
	rep, err := d.Validate(context.Background(), &pluginv1.ValidateProfileRequest{
		Profile: mustStruct(t, map[string]any{"folder_id": "b1g"}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Ok {
		t.Error("expected Ok=false on missing zone/image_id/subnet_id")
	}
	if len(rep.Errors) != 3 {
		t.Errorf("errors=%v, want 3 (zone, image_id, subnet_id)", rep.Errors)
	}
}

func TestValidate_AllRequired(t *testing.T) {
	d := &YcDriver{}
	rep, err := d.Validate(context.Background(), &pluginv1.ValidateProfileRequest{
		Profile: mustStruct(t, validProfile()),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !rep.Ok {
		t.Errorf("expected Ok=true, errors=%v", rep.Errors)
	}
}

func TestCreate_HappyPath(t *testing.T) {
	f := &fakeYC{
		createOut: &computev1.Instance{Id: "epd-aaa"},
		getSeq: []*computev1.Instance{
			runningInstance("epd-aaa", "10.0.0.5", "soul-run1-0.ru-central1.internal"),
		},
	}
	withFakeYC(t, f)

	d := &YcDriver{}
	s := &createStream{}
	err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, labeledProfile("run1")),
		Credentials: mustStruct(t, validCredsIAM()),
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
	if vm.VmId != "epd-aaa" {
		t.Errorf("vm_id=%q", vm.VmId)
	}
	if vm.Fqdn == "" {
		t.Error("fqdn empty (must be YC internal-DNS = SID)")
	}
	if vm.PrimaryIp != "10.0.0.5" {
		t.Errorf("primary_ip=%q", vm.PrimaryIp)
	}
	// userdata is passed into Metadata["user-data"] (YC convention, plain
	// string, no base64 unlike EC2).
	if f.lastCreateInput == nil {
		t.Fatal("CreateInstance not called")
	}
	if got := f.lastCreateInput.GetMetadata()[userdataMetaKey]; got != "#cloud-config\n" {
		t.Errorf("metadata[user-data]=%q, want raw cloud-init blob", got)
	}
}

func TestCreate_PassesProfileFieldsToYC(t *testing.T) {
	f := &fakeYC{
		createOut: &computev1.Instance{Id: "epd-prof"},
		getSeq: []*computev1.Instance{
			runningInstance("epd-prof", "10.0.0.7", "soul-x-0.ru-central1.internal"),
		},
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	prof := validProfile()
	prof["platform_id"] = "standard-v2"
	prof["resources"] = map[string]any{"cores": 4, "memory_gb": 8, "core_fraction": 50}
	prof["disk"] = map[string]any{"size_gb": 30, "type": "network-ssd"}
	prof["security_group_ids"] = []any{"enp-sg-1"}
	prof["nat"] = true
	prof["labels"] = map[string]any{runLabelKey: "run-x"}
	prof["service_account_id"] = "ajex-sa"

	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, prof),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	in := f.lastCreateInput
	if in == nil {
		t.Fatal("CreateInstance not called")
	}
	if in.GetPlatformId() != "standard-v2" {
		t.Errorf("platform_id=%q", in.GetPlatformId())
	}
	if in.GetResourcesSpec().GetCores() != 4 {
		t.Errorf("cores=%d", in.GetResourcesSpec().GetCores())
	}
	wantMemBytes := int64(8) * gibibyte
	if in.GetResourcesSpec().GetMemory() != wantMemBytes {
		t.Errorf("memory=%d, want %d", in.GetResourcesSpec().GetMemory(), wantMemBytes)
	}
	if in.GetResourcesSpec().GetCoreFraction() != 50 {
		t.Errorf("core_fraction=%d", in.GetResourcesSpec().GetCoreFraction())
	}
	disk := in.GetBootDiskSpec().GetDiskSpec()
	if disk.GetTypeId() != "network-ssd" || disk.GetSize() != int64(30)*gibibyte || disk.GetImageId() != "fd811111111111111111" {
		t.Errorf("boot-disk spec mismatch: %+v", disk)
	}
	nis := in.GetNetworkInterfaceSpecs()
	if len(nis) != 1 || nis[0].GetSubnetId() != "e9b111111111" {
		t.Fatalf("net interfaces=%+v", nis)
	}
	if nis[0].GetPrimaryV4AddressSpec().GetOneToOneNatSpec() == nil {
		t.Error("NAT requested but OneToOneNatSpec is nil")
	}
	if len(nis[0].GetSecurityGroupIds()) != 1 {
		t.Errorf("security_group_ids=%v", nis[0].GetSecurityGroupIds())
	}
	if in.GetServiceAccountId() != "ajex-sa" {
		t.Errorf("service_account_id=%q", in.GetServiceAccountId())
	}
	if in.GetLabels()[runLabelKey] != "run-x" {
		t.Errorf("labels[%s]=%q", runLabelKey, in.GetLabels()[runLabelKey])
	}
	if !strings.HasPrefix(in.GetName(), "soul-run-x-") {
		t.Errorf("name=%q, want soul-<runLabel>-<seq>", in.GetName())
	}
}

func TestCreate_WaitsForRunning(t *testing.T) {
	withFastBackoff(t, 4)
	f := &fakeYC{
		createOut: &computev1.Instance{Id: "epd-bbb"},
		getSeq: []*computev1.Instance{
			// round 1: PROVISIONING, no fqdn
			{Id: "epd-bbb", Status: computev1.Instance_PROVISIONING},
			// round 2: RUNNING with fqdn
			runningInstance("epd-bbb", "10.0.0.9", "soul-run-0.ru-central1.internal"),
		},
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, labeledProfile("run")),
		Credentials: mustStruct(t, validCredsIAM()),
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

func TestCreate_AuthError(t *testing.T) {
	// Auth failure surfaces at newYcClient: empty credentials make
	// resolveCredentials return "no credentials provided". Replace the client
	// factory so it runs the real resolve branch.
	orig := newYcClient
	newYcClient = func(ctx context.Context, c ycCredentials) (ycAPI, error) {
		_, err := resolveCredentials(c)
		if err != nil {
			return nil, err
		}
		return &fakeYC{}, nil
	}
	t.Cleanup(func() { newYcClient = orig })

	d := &YcDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, labeledProfile("run")),
		Credentials: mustStruct(t, map[string]any{"folder_id": "b1g", "zone": "ru-central1-a"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on auth error")
	}
	if !strings.HasPrefix(last.Message, "auth:") {
		t.Errorf("message=%q, want auth-class prefix", last.Message)
	}
}

func TestCreate_AmbiguousCredentials(t *testing.T) {
	orig := newYcClient
	newYcClient = func(ctx context.Context, c ycCredentials) (ycAPI, error) {
		_, err := resolveCredentials(c)
		if err != nil {
			return nil, err
		}
		return &fakeYC{}, nil
	}
	t.Cleanup(func() { newYcClient = orig })

	d := &YcDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:   1,
		Profile: mustStruct(t, labeledProfile("run")),
		Credentials: mustStruct(t, map[string]any{
			"iam_token":   "t1.x",
			"oauth_token": "y0_x",
			"folder_id":   "b1g",
			"zone":        "ru-central1-a",
		}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !s.last().Failed || !strings.Contains(s.last().Message, "ambiguous") {
		t.Errorf("ambiguous-auth event=%+v", s.last())
	}
}

// TestCreate_NoIdentity_FailsClosed - NIM-16: without name and run-label the
// run is indistinguishable from previous runs, so a repeated Create would spawn
// orphan VMs. Fail closed before any YC API call (neither create nor list).
func TestCreate_NoIdentity_FailsClosed(t *testing.T) {
	withFastBackoff(t, 2) // regression guard: without the guard the test must not sleep until deadline
	f := &fakeYC{}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, validProfile()), // no labels, Name is empty
		Credentials: mustStruct(t, validCredsIAM()),
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
		t.Errorf("CreateInstance called %d times; fail-closed must NOT touch YC API", f.createCall)
	}
	if f.listCall != 0 {
		t.Errorf("ListInstances called %d times; fail-closed must NOT touch YC API", f.listCall)
	}
}

// TestCreate_HonorsRequestName - NIM-16: CreateRequest.name reaches
// CreateInstance as `<name>-<seq>` (the driver honors the Keeper-provided name
// for predictable FQDN) and is stamped into labels[soulstack-run] (future runs
// match by label, not only by name).
func TestCreate_HonorsRequestName(t *testing.T) {
	f := &fakeYC{
		createOut: &computev1.Instance{Id: "epd-1"},
		getSeq: []*computev1.Instance{
			runningInstance("epd-1", "10.0.0.5", "redis-0.ru-central1.internal"),
		},
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, validProfile()),
		Credentials: mustStruct(t, validCredsIAM()),
		Name:        "redis",
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.lastCreateInput == nil {
		t.Fatal("CreateInstance not called")
	}
	if f.lastCreateInput.GetName() != "redis-0" {
		t.Errorf("CreateInstance name = %q, want redis-0 (honor CreateRequest.name)", f.lastCreateInput.GetName())
	}
	if got := f.lastCreateInput.GetLabels()[runLabelKey]; got != "redis" {
		t.Errorf("labels[%s]=%q, want redis (name stamped as run-label)", runLabelKey, got)
	}
}

// TestCreate_NameDerivedRerun_ReusesExisting - NIM-16 central scenario: rerun
// by name-derived identity. The first run's VM carries stamped
// labels[soulstack-run]="redis"; repeated Create{Name:"redis"} without labels
// must scan by this label (filter assert guards against stamp rollback: fake
// does not filter itself) and reuse the live VM instead of creating a duplicate.
func TestCreate_NameDerivedRerun_ReusesExisting(t *testing.T) {
	existing := runningInstance("redis-0", "10.0.0.5", "redis-0.ru-central1.internal")
	existing.Labels = map[string]string{runLabelKey: "redis"}
	f := &fakeYC{
		listOut: &computev1.ListInstancesResponse{Instances: []*computev1.Instance{existing}},
		getSeq:  []*computev1.Instance{existing},
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, validProfile()), // no labels; identity comes from Name
		Credentials: mustStruct(t, validCredsIAM()),
		Name:        "redis",
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 0 {
		t.Errorf("CreateInstance called %d times; name-derived rerun must reuse existing VM", f.createCall)
	}
	wantFilter := `labels.` + runLabelKey + `="redis"`
	if !strings.Contains(f.lastListInput.GetFilter(), wantFilter) {
		t.Errorf("ListInstances filter=%q, want %q (name stamped as run-label)", f.lastListInput.GetFilter(), wantFilter)
	}
	last := s.last()
	if last == nil || last.Failed {
		t.Fatalf("final=%+v, want success", last)
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "redis-0" {
		t.Errorf("reused vms=%+v, want [redis-0]", last.Vms)
	}
}

// TestCreate_PartialRerun_NoIndexCollision - NIM-16: label path, partial rerun.
// Existing "soul-run-delta-0" (index 0 is occupied), count=2 means the new VM
// takes the first free index "soul-run-delta-1", not a duplicate "-0".
func TestCreate_PartialRerun_NoIndexCollision(t *testing.T) {
	withFastBackoff(t, 2)
	existing := runningInstance("soul-run-delta-0", "10.1.0.1", "soul-run-delta-0.ru-central1.internal")
	f := &fakeYC{
		listOut:   &computev1.ListInstancesResponse{Instances: []*computev1.Instance{existing}},
		createOut: &computev1.Instance{Id: "epd-new"},
		getFn: func(_ int) (*computev1.Instance, error) {
			return runningInstance("soul-run-delta-1", "10.1.0.2", "soul-run-delta-1.ru-central1.internal"), nil
		},
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	prof := validProfile()
	prof["labels"] = map[string]any{runLabelKey: "run-delta"}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       2,
		Profile:     mustStruct(t, prof),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 1 {
		t.Errorf("CreateInstance called %d times, want 1 (gap = count - existing)", f.createCall)
	}
	if f.lastCreateInput == nil || f.lastCreateInput.GetName() != "soul-run-delta-1" {
		t.Errorf("gap-fill name=%q, want soul-run-delta-1 (no -0 collision)", f.lastCreateInput.GetName())
	}
}

func TestCreate_Idempotent_ReusesExisting(t *testing.T) {
	existing := runningInstance("epd-existing", "10.1.1.1", "soul-run-42-0.ru-central1.internal")
	f := &fakeYC{
		listOut: &computev1.ListInstancesResponse{
			Instances: []*computev1.Instance{existing},
		},
		getSeq: []*computev1.Instance{existing},
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	prof := validProfile()
	prof["labels"] = map[string]any{runLabelKey: "run-42"}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, prof),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 0 {
		t.Errorf("CreateInstance called %d times; idempotent path must NOT launch new VM", f.createCall)
	}
	if s.last().Failed {
		t.Fatalf("idempotent final=%+v", s.last())
	}
	if s.last().Vms[0].VmId != "epd-existing" {
		t.Errorf("reused vm=%q, want epd-existing", s.last().Vms[0].VmId)
	}
}

func TestCreate_Idempotent_OverCount(t *testing.T) {
	withFastBackoff(t, 2)
	existing := []*computev1.Instance{
		runningInstance("epd-1", "10.1.0.1", "soul-over-0.ru-central1.internal"),
		runningInstance("epd-2", "10.1.0.2", "soul-over-1.ru-central1.internal"),
		runningInstance("epd-3", "10.1.0.3", "soul-over-2.ru-central1.internal"),
	}
	f := &fakeYC{
		listOut: &computev1.ListInstancesResponse{Instances: existing},
		getSeq:  existing,
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	prof := validProfile()
	prof["labels"] = map[string]any{runLabelKey: "run-over"}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       2, // less than the actual number of existing VMs
		Profile:     mustStruct(t, prof),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 0 {
		t.Errorf("CreateInstance called %d times; over-count idempotent path must NOT launch new VM", f.createCall)
	}
	if len(s.last().Vms) != 3 {
		t.Errorf("vms=%d, want 3 (all existing returned, not truncated to count)", len(s.last().Vms))
	}
}

func TestCreate_Idempotent_FilterIgnoresDeadStatuses(t *testing.T) {
	// List returned both live and DELETING VMs; the driver must filter them.
	live := runningInstance("epd-live", "10.2.0.1", "soul-mix-0.ru-central1.internal")
	dead := &computev1.Instance{Id: "epd-dead", Status: computev1.Instance_DELETING}
	f := &fakeYC{
		listOut: &computev1.ListInstancesResponse{Instances: []*computev1.Instance{live, dead}},
		// After filtering only live remains; for it the poller immediately sees RUNNING.
		getSeq: []*computev1.Instance{live},
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	prof := validProfile()
	prof["labels"] = map[string]any{runLabelKey: "run-mix"}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, prof),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.createCall != 0 {
		t.Errorf("CreateInstance must NOT be called; live VM satisfies count")
	}
	if len(s.last().Vms) != 1 || s.last().Vms[0].VmId != "epd-live" {
		t.Errorf("reused VM mismatch: %+v", s.last().Vms)
	}
}

func TestCreate_CtxCancel_AntiOrphan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeYC{
		createOut: &computev1.Instance{Id: "epd-orphan"},
		getSeq: []*computev1.Instance{
			// Always PROVISIONING: the poller loops until ctx is canceled.
			{Id: "epd-orphan", Status: computev1.Instance_PROVISIONING},
		},
	}
	withFakeYC(t, f)
	cancel()

	d := &YcDriver{}
	s := &createStream{ctx: ctx}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, labeledProfile("run")),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on ctx-cancel during wait")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "epd-orphan" {
		t.Errorf("anti-orphan: final event must carry vm_id epd-orphan, got %+v", last.Vms)
	}
}

func TestCreate_WaitDeadline_AntiOrphan(t *testing.T) {
	withFastBackoff(t, 2)
	f := &fakeYC{
		createOut: &computev1.Instance{Id: "epd-wait"},
		getSeq: []*computev1.Instance{
			{Id: "epd-wait", Status: computev1.Instance_PROVISIONING},
		},
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, labeledProfile("run")),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on wait-deadline exhaustion")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "epd-wait" {
		t.Errorf("anti-orphan: final event must carry vm_id epd-wait, got %+v", last.Vms)
	}
	if !strings.Contains(last.Message, "max attempts exhausted") {
		t.Errorf("message=%q, want max-attempts-exhausted (ErrWaitDeadline)", last.Message)
	}
}

func TestCreate_TerminalStateProbe(t *testing.T) {
	withFastBackoff(t, 4)
	cases := []struct {
		name  string
		state computev1.Instance_Status
	}{
		{"stopping", computev1.Instance_STOPPING},
		{"stopped", computev1.Instance_STOPPED},
		{"error", computev1.Instance_ERROR},
		{"crashed", computev1.Instance_CRASHED},
		{"deleting", computev1.Instance_DELETING},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeYC{
				createOut: &computev1.Instance{Id: "epd-term"},
				getSeq: []*computev1.Instance{
					{Id: "epd-term", Status: tc.state},
				},
			}
			withFakeYC(t, f)
			d := &YcDriver{}
			s := &createStream{}
			if err := d.Create(&pluginv1.CreateRequest{
				Count:       1,
				Profile:     mustStruct(t, labeledProfile("run")),
				Credentials: mustStruct(t, validCredsIAM()),
			}, s); err != nil {
				t.Fatalf("Create: %v", err)
			}
			last := s.last()
			if !last.Failed {
				t.Fatalf("terminal=%s: expected failed=true, got %+v", tc.state, last)
			}
			if len(last.Vms) != 1 || last.Vms[0].VmId != "epd-term" {
				t.Errorf("terminal=%s: final vms=%+v, want vm_id=epd-term", tc.state, last.Vms)
			}
			if last.Vms[0].Fqdn != "" {
				t.Errorf("terminal=%s: Fqdn=%q must be empty (probe failed)", tc.state, last.Vms[0].Fqdn)
			}
		})
	}
}

func TestCreate_TransientProbeError_SwallowAndRetry(t *testing.T) {
	withFastBackoff(t, 8)
	f := &fakeYC{
		createOut: &computev1.Instance{Id: "epd-trans"},
	}
	// call 0 - PROVISIONING (first probe round);
	// call 1 - Unavailable (transient grpc, swallowed);
	// call 2 — RUNNING + IP/FQDN → Ready.
	f.getFn = func(call int) (*computev1.Instance, error) {
		switch call {
		case 0:
			return &computev1.Instance{Id: "epd-trans", Status: computev1.Instance_PROVISIONING}, nil
		case 1:
			return nil, status.Error(codes.Unavailable, "transient")
		default:
			return runningInstance("epd-trans", "10.5.5.5", "soul-tr-0.ru-central1.internal"), nil
		}
	}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, labeledProfile("run")),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("transient probe-error must be swallowed; got failed: %+v", last)
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "epd-trans" || last.Vms[0].Fqdn == "" {
		t.Errorf("vms after transient retry = %+v", last.Vms)
	}
}

func TestStatus_UsesCredentials(t *testing.T) {
	inst := runningInstance("epd-stat", "10.6.6.6", "soul-stat-0.ru-central1.internal")
	f := &fakeYC{getSeq: []*computev1.Instance{inst}}
	withFakeYC(t, f)

	d := &YcDriver{}
	rep, err := d.Status(context.Background(), &pluginv1.StatusRequest{
		VmId:        "epd-stat",
		Credentials: mustStruct(t, validCredsIAM()),
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.State != computev1.Instance_RUNNING.String() {
		t.Errorf("state=%q, want RUNNING", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes must be populated")
	}
}

func TestList_UsesCredentialsField(t *testing.T) {
	f := &fakeYC{
		listOut: &computev1.ListInstancesResponse{Instances: []*computev1.Instance{
			runningInstance("epd-l-1", "10.7.7.1", "soul-l-0.ru-central1.internal"),
			runningInstance("epd-l-2", "10.7.7.2", "soul-l-1.ru-central1.internal"),
		}},
	}
	withFakeYC(t, f)

	d := &YcDriver{}
	s := &listStream{ctx: context.Background()}
	if err := d.List(&pluginv1.ListRequest{
		Filter:      mustStruct(t, map[string]any{runLabelKey: "run-list", "folder_id": "b1g111111111"}),
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(s.sent) != 2 {
		t.Errorf("list events=%d, want 2", len(s.sent))
	}
}

func TestList_RequiresFolderID(t *testing.T) {
	withFakeYC(t, &fakeYC{})
	d := &YcDriver{}
	s := &listStream{ctx: context.Background()}
	// folder_id is absent from both filter and credentials, so this errors.
	if err := d.List(&pluginv1.ListRequest{
		Filter:      mustStruct(t, map[string]any{}),
		Credentials: mustStruct(t, map[string]any{"iam_token": "t1.x"}),
	}, s); err == nil {
		t.Fatal("expected error without folder_id")
	}
}

func TestDestroy_PerVM(t *testing.T) {
	f := &fakeYC{}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"epd-1", "epd-2"},
		Credentials: mustStruct(t, validCredsIAM()),
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
		t.Errorf("DeleteInstance called %d times, want 2", f.deleteN)
	}
}

func TestDestroy_NotFoundIsIdempotent(t *testing.T) {
	f := &fakeYC{deleteErr: status.Error(codes.NotFound, "gone")}
	withFakeYC(t, f)
	d := &YcDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"epd-gone"},
		Credentials: mustStruct(t, validCredsIAM()),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.sent) != 1 || s.sent[0].Failed {
		t.Errorf("not-found destroy must be idempotent (success), got %+v", s.sent)
	}
	if !strings.Contains(s.sent[0].Message, "absent") {
		t.Errorf("message=%q, want 'absent'-style", s.sent[0].Message)
	}
}

func TestClassifyYC_Codes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want clouddriver.FailClass
	}{
		{"Unauthenticated", status.Error(codes.Unauthenticated, "bad token"), clouddriver.FailAuth},
		{"PermissionDenied", status.Error(codes.PermissionDenied, "no rights"), clouddriver.FailAuth},
		{"NotFound", status.Error(codes.NotFound, "no such vm"), clouddriver.FailNotFound},
		{"ResourceExhausted_quota", status.Error(codes.ResourceExhausted, "quota for instances in folder exceeded"), clouddriver.FailQuota},
		{"ResourceExhausted_throttle", status.Error(codes.ResourceExhausted, "Too many requests, please slow down"), clouddriver.FailTransient},
		{"InvalidArgument", status.Error(codes.InvalidArgument, "bad spec"), clouddriver.FailInvalidParams},
		{"FailedPrecondition", status.Error(codes.FailedPrecondition, "wrong state"), clouddriver.FailInvalidParams},
		{"AlreadyExists", status.Error(codes.AlreadyExists, "name clash"), clouddriver.FailInvalidParams},
		{"Unavailable", status.Error(codes.Unavailable, "503"), clouddriver.FailTransient},
		{"DeadlineExceeded", status.Error(codes.DeadlineExceeded, "deadline"), clouddriver.FailTransient},
		{"Aborted", status.Error(codes.Aborted, "retry"), clouddriver.FailTransient},
		{"Internal", status.Error(codes.Internal, "500"), clouddriver.FailTransient},
		{"non-grpc", errors.New("dial tcp: timeout"), clouddriver.FailTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyYC(tc.err); got != tc.want {
				t.Errorf("classifyYC(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestResolveCredentials(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		_, err := resolveCredentials(ycCredentials{FolderID: "b1g", Zone: "ru-central1-a"})
		if err == nil || !strings.Contains(err.Error(), "no credentials") {
			t.Fatalf("err=%v, want no-credentials", err)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		_, err := resolveCredentials(ycCredentials{IAMToken: "t", OAuthToken: "y"})
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("err=%v, want ambiguous", err)
		}
	})
	t.Run("iam-only", func(t *testing.T) {
		creds, err := resolveCredentials(ycCredentials{IAMToken: "t1.x"})
		if err != nil || creds == nil {
			t.Fatalf("iam-token resolve err=%v creds=%v", err, creds)
		}
	})
	t.Run("oauth-only", func(t *testing.T) {
		creds, err := resolveCredentials(ycCredentials{OAuthToken: "y0_x"})
		if err != nil || creds == nil {
			t.Fatalf("oauth resolve err=%v creds=%v", err, creds)
		}
	})
	t.Run("sa-key-invalid", func(t *testing.T) {
		_, err := resolveCredentials(ycCredentials{ServiceAccountKey: []byte("{not json")})
		if err == nil || !strings.Contains(err.Error(), "service_account_key") {
			t.Fatalf("err=%v, want parse error", err)
		}
	})
}
