package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// withFastBackoff подменяет defaultBackoff на «нулевые» задержки + указанный
// MaxAttempts. Используется в wait-deadline / transient-probe тестах, где
// дефолтный 1s→2s→4s сделал бы тест медленным.
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

// fakeOperation — синтетический gcpOperation, опционально возвращающий ошибку
// из Wait.
type fakeOperation struct{ waitErr error }

func (f *fakeOperation) Wait(_ context.Context) error { return f.waitErr }

// fakeInstances — mock gcpInstancesAPI для L0-unit-тестов (без сети). Поведение
// настраивается per-метод; getSeq позволяет смоделировать переход
// PROVISIONING→RUNNING между раундами поллера.
//
// getFn — опциональный override: получает 0-based номер вызова и волен
// вернуть свою пару (instance, err) — для тестов transient-probe-error
// (ошибка классифицирована Transient → поллер проглатывает) и сценариев, где
// плоского getSeq не хватает.
type fakeInstances struct {
	insertOp      gcpOperation
	insertErr     error
	insertCall    int
	lastInsertReq *computepb.InsertInstanceRequest

	deleteOp     gcpOperation
	deleteErr    error
	deleteCall   int
	lastDeleteVM string

	getSeq []*computepb.Instance
	getIdx int
	getErr error
	getFn  func(call int) (*computepb.Instance, error)
	getN   int

	listOut    []*computepb.Instance
	listErr    error
	lastListIn *computepb.ListInstancesRequest
}

func (f *fakeInstances) Insert(_ context.Context, in *computepb.InsertInstanceRequest) (gcpOperation, error) {
	f.insertCall++
	f.lastInsertReq = in
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	if f.insertOp == nil {
		return &fakeOperation{}, nil
	}
	return f.insertOp, nil
}

func (f *fakeInstances) Delete(_ context.Context, in *computepb.DeleteInstanceRequest) (gcpOperation, error) {
	f.deleteCall++
	f.lastDeleteVM = in.GetInstance()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if f.deleteOp == nil {
		return &fakeOperation{}, nil
	}
	return f.deleteOp, nil
}

func (f *fakeInstances) Get(_ context.Context, _ *computepb.GetInstanceRequest) (*computepb.Instance, error) {
	call := f.getN
	f.getN++
	if f.getFn != nil {
		return f.getFn(call)
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	if len(f.getSeq) == 0 {
		return nil, &googleapi.Error{Code: 404, Message: "not found"}
	}
	out := f.getSeq[f.getIdx]
	if f.getIdx < len(f.getSeq)-1 {
		f.getIdx++
	}
	return out, nil
}

func (f *fakeInstances) List(_ context.Context, in *computepb.ListInstancesRequest) ([]*computepb.Instance, error) {
	f.lastListIn = in
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listOut, nil
}

// withFakeInstances подменяет фабрику клиента на возврат f, восстанавливает после теста.
func withFakeInstances(t *testing.T, f *fakeInstances) {
	t.Helper()
	orig := newGcpInstancesClient
	newGcpInstancesClient = func(_ context.Context, _ gcpCredentials) (gcpInstancesAPI, error) { return f, nil }
	t.Cleanup(func() { newGcpInstancesClient = orig })
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

// runningInstance собирает VM в состоянии RUNNING с заданным внутренним IP.
func runningInstance(name, ip string) *computepb.Instance {
	return &computepb.Instance{
		Name:        proto.String(name),
		Status:      proto.String("RUNNING"),
		Zone:        proto.String("https://www.googleapis.com/compute/v1/projects/p/zones/europe-west1-b"),
		MachineType: proto.String("https://www.googleapis.com/compute/v1/projects/p/zones/europe-west1-b/machineTypes/e2-medium"),
		NetworkInterfaces: []*computepb.NetworkInterface{{
			NetworkIP: proto.String(ip),
		}},
	}
}

// validProfile — минимально валидный profile для тестов (project/zone/
// machine_type/source_image).
func validProfile(extra map[string]any) map[string]any {
	p := map[string]any{
		"project":      "my-project",
		"zone":         "europe-west1-b",
		"machine_type": "e2-medium",
		"source_image": "projects/debian-cloud/global/images/family/debian-12",
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func TestSchema_ParsesEmbedded(t *testing.T) {
	d := &GcpDriver{}
	rep, err := d.Schema(context.Background(), &pluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	m := rep.ProfileSchema.AsMap()
	req, _ := m["required"].([]any)
	if len(req) != 4 {
		t.Errorf("schema required=%v, want 4 fields", req)
	}
}

func TestValidate_MissingFields(t *testing.T) {
	d := &GcpDriver{}
	rep, err := d.Validate(context.Background(), &pluginv1.ValidateProfileRequest{
		Profile: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Ok {
		t.Error("expected Ok=false on missing machine_type/source_image")
	}
	if len(rep.Errors) != 2 {
		t.Errorf("errors=%v, want 2 (machine_type, source_image)", rep.Errors)
	}
}

func TestCreate_HappyPath(t *testing.T) {
	f := &fakeInstances{
		getSeq: []*computepb.Instance{runningInstance("soul-run-42-0", "10.0.0.5")},
	}
	withFakeInstances(t, f)

	d := &GcpDriver{}
	s := &createStream{}
	err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, validProfile(map[string]any{
			"labels": map[string]any{runLabelKey: "run-42"},
		})),
		Credentials: mustStruct(t, map[string]any{
			"service_account_key": `{"type":"service_account"}`,
			"project":             "my-project",
			"zone":                "europe-west1-b",
		}),
		Userdata: "#cloud-config\n",
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
	if vm.VmId != "soul-run-42-0" {
		t.Errorf("vm_id=%q, want deterministic soul-run-42-0", vm.VmId)
	}
	if vm.Fqdn != "soul-run-42-0.europe-west1-b.c.my-project.internal" {
		t.Errorf("fqdn=%q, want GCP internal DNS format", vm.Fqdn)
	}
	if vm.PrimaryIp != "10.0.0.5" {
		t.Errorf("primary_ip=%q", vm.PrimaryIp)
	}
	// userdata прокинут в Insert как metadata.items[user-data]. GCP передаёт
	// metadata plain (НЕ base64, в отличие от EC2).
	if f.lastInsertReq == nil {
		t.Fatal("Insert not called")
	}
	md := f.lastInsertReq.GetInstanceResource().GetMetadata()
	if md == nil || len(md.Items) == 0 {
		t.Fatal("metadata.items empty; userdata not propagated")
	}
	found := false
	for _, item := range md.Items {
		if item.GetKey() == "user-data" {
			found = true
			if item.GetValue() != "#cloud-config\n" {
				t.Errorf("user-data=%q, want plain cloud-init blob (no base64)", item.GetValue())
			}
		}
	}
	if !found {
		t.Error("metadata.items[user-data] not found")
	}
}

func TestCreate_WaitsForRunning(t *testing.T) {
	withFastBackoff(t, 5)
	f := &fakeInstances{
		getSeq: []*computepb.Instance{
			// раунд 1: PROVISIONING без IP
			{Name: proto.String("soul-w-0"), Status: proto.String("PROVISIONING")},
			// раунд 2: RUNNING с IP
			runningInstance("soul-w-0", "10.0.0.9"),
		},
	}
	withFakeInstances(t, f)
	d := &GcpDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, validProfile(map[string]any{
			"labels": map[string]any{runLabelKey: "w"},
		})),
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
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

func TestCreate_InsertAuthError(t *testing.T) {
	f := &fakeInstances{insertErr: &googleapi.Error{Code: 403, Message: "forbidden"}}
	withFakeInstances(t, f)
	d := &GcpDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, validProfile(nil)),
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on auth error")
	}
	if !contains(last.Message, "auth:") {
		t.Errorf("message=%q, want auth-class prefix", last.Message)
	}
	if f.insertCall != 1 {
		t.Errorf("Insert called %d times; auth must NOT retry", f.insertCall)
	}
}

func TestCreate_Idempotent_ReusesExisting(t *testing.T) {
	existing := runningInstance("soul-run-42-0", "10.1.1.1")
	f := &fakeInstances{
		listOut: []*computepb.Instance{existing},
		// после findByRunLabel finalizeCreate вызывает Get для VmInfo.Fqdn:
		getSeq: []*computepb.Instance{existing},
	}
	withFakeInstances(t, f)
	d := &GcpDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, validProfile(map[string]any{
			"labels": map[string]any{runLabelKey: "run-42"},
		})),
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.insertCall != 0 {
		t.Errorf("Insert called %d times; idempotent path must NOT launch new VM", f.insertCall)
	}
	if s.last().Failed {
		t.Fatalf("idempotent final=%+v", s.last())
	}
	if s.last().Vms[0].VmId != "soul-run-42-0" {
		t.Errorf("reused vm=%q, want soul-run-42-0", s.last().Vms[0].VmId)
	}
}

func TestCreate_CtxCancel_AntiOrphan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeInstances{
		// always PROVISIONING → поллер крутится, пока ctx не отменят
		getSeq: []*computepb.Instance{{Name: proto.String("soul-orphan-0"), Status: proto.String("PROVISIONING")}},
	}
	withFakeInstances(t, f)
	cancel() // отменяем сразу — поллер уйдёт в sleepCtx и вернёт ctx.Err

	d := &GcpDriver{}
	s := &createStream{ctx: ctx}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, validProfile(map[string]any{
			"labels": map[string]any{runLabelKey: "orphan"},
		})),
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on ctx-cancel during wait")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "soul-orphan-0" {
		t.Errorf("anti-orphan: final event must carry vm_id soul-orphan-0, got %+v", last.Vms)
	}
}

// TestCreate_WaitDeadline_AntiOrphan — wait-поллер упирается в MaxAttempts
// (НЕ ctx-cancel) → возврат ErrWaitDeadline → failed-event с заполненным vm_id
// (anti-orphan ветка, отличная от ctx-cancel).
func TestCreate_WaitDeadline_AntiOrphan(t *testing.T) {
	withFastBackoff(t, 2)
	f := &fakeInstances{
		getSeq: []*computepb.Instance{{Name: proto.String("soul-wait-0"), Status: proto.String("PROVISIONING")}},
	}
	withFakeInstances(t, f)

	d := &GcpDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, validProfile(map[string]any{
			"labels": map[string]any{runLabelKey: "wait"},
		})),
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on wait-deadline exhaustion")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "soul-wait-0" {
		t.Errorf("anti-orphan: final event must carry vm_id soul-wait-0, got %+v", last.Vms)
	}
	if !contains(last.Message, "max attempts exhausted") {
		t.Errorf("message=%q, want max-attempts-exhausted (ErrWaitDeadline)", last.Message)
	}
}

// TestCreate_TerminalStateProbe — VM во время wait уходит в terminal-state →
// probe возвращает ProbeResult{Err} → поллер прекращает опрос этой VM,
// finalizeCreate шлёт failed-event с vm_id и описательным сообщением.
func TestCreate_TerminalStateProbe(t *testing.T) {
	withFastBackoff(t, 4)
	cases := []struct {
		name   string
		status string
	}{
		{"terminated", "TERMINATED"},
		{"stopping", "STOPPING"},
		{"stopped", "STOPPED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeInstances{
				getSeq: []*computepb.Instance{{Name: proto.String("soul-term-0"), Status: proto.String(tc.status)}},
			}
			withFakeInstances(t, f)
			d := &GcpDriver{}
			s := &createStream{}
			if err := d.Create(&pluginv1.CreateRequest{
				Count: 1,
				Profile: mustStruct(t, validProfile(map[string]any{
					"labels": map[string]any{runLabelKey: "term"},
				})),
				Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
			}, s); err != nil {
				t.Fatalf("Create: %v", err)
			}
			last := s.last()
			if !last.Failed {
				t.Fatalf("terminal=%s: expected failed=true, got %+v", tc.status, last)
			}
			if len(last.Vms) != 1 || last.Vms[0].VmId != "soul-term-0" {
				t.Errorf("terminal=%s: final vms=%+v, want vm_id=soul-term-0", tc.status, last.Vms)
			}
			if last.Vms[0].Fqdn != "" {
				t.Errorf("terminal=%s: Fqdn=%q must be empty (probe failed)", tc.status, last.Vms[0].Fqdn)
			}
		})
	}
}

// TestCreate_TransientProbeError_SwallowAndRetry — Get между раундами
// возвращает классифицируемую как Transient() ошибку → probe-обёртка глотает
// её (ProbeResult{}) → следующий round успешен.
func TestCreate_TransientProbeError_SwallowAndRetry(t *testing.T) {
	withFastBackoff(t, 8)
	f := &fakeInstances{}
	f.getFn = func(call int) (*computepb.Instance, error) {
		switch call {
		case 0:
			return &computepb.Instance{Name: proto.String("soul-trans-0"), Status: proto.String("PROVISIONING")}, nil
		case 1:
			// 503 — transient: probe должен проглотить и продолжить.
			return nil, &googleapi.Error{Code: 503, Message: "service unavailable"}
		default:
			return runningInstance("soul-trans-0", "10.5.5.5"), nil
		}
	}
	withFakeInstances(t, f)

	d := &GcpDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, validProfile(map[string]any{
			"labels": map[string]any{runLabelKey: "trans"},
		})),
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("transient probe-error must be swallowed; got failed: %+v", last)
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "soul-trans-0" || last.Vms[0].Fqdn == "" {
		t.Errorf("vms after transient retry = %+v", last.Vms)
	}
}

// TestCreate_Idempotent_OverCount — findByRunLabel вернул БОЛЬШЕ VM, чем
// запрошенный count. Драйвер обязан вернуть все найденные (не падать, не
// плодить дубли).
func TestCreate_Idempotent_OverCount(t *testing.T) {
	withFastBackoff(t, 2)
	existing := []*computepb.Instance{
		runningInstance("soul-over-0", "10.1.0.1"),
		runningInstance("soul-over-1", "10.1.0.2"),
		runningInstance("soul-over-2", "10.1.0.3"),
	}
	f := &fakeInstances{
		listOut: existing,
		getSeq:  existing, // последовательные Get для finalizeCreate VmInfo-заполнения
	}
	withFakeInstances(t, f)

	d := &GcpDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 2, // меньше реального инвентаря
		Profile: mustStruct(t, validProfile(map[string]any{
			"labels": map[string]any{runLabelKey: "over"},
		})),
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.insertCall != 0 {
		t.Errorf("Insert called %d times; over-count idempotent path must NOT launch new VM", f.insertCall)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("over-count idempotent: final=%+v, want success", last)
	}
	if len(last.Vms) != 3 {
		t.Errorf("vms=%d, want 3 (all existing returned, not truncated to count)", len(last.Vms))
	}
}

// TestStatus_UsesCredentials — Status с credentials в StatusRequest-поле
// успешно опрашивает VM (не возвращает «requires credentials»-ошибку
// workaround-версии).
func TestStatus_UsesCredentials(t *testing.T) {
	inst := runningInstance("soul-stat-0", "10.6.6.6")
	f := &fakeInstances{getSeq: []*computepb.Instance{inst}}
	withFakeInstances(t, f)

	d := &GcpDriver{}
	rep, err := d.Status(context.Background(), &pluginv1.StatusRequest{
		VmId: "soul-stat-0",
		Credentials: mustStruct(t, map[string]any{
			"service_account_key": `{"type":"service_account"}`,
			"project":             "my-project",
			"zone":                "europe-west1-b",
		}),
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.State != "RUNNING" {
		t.Errorf("state=%q, want RUNNING", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes must be populated")
	}
}

// TestList_UsesCredentialsField — List с credentials в ListRequest-поле.
func TestList_UsesCredentialsField(t *testing.T) {
	f := &fakeInstances{
		listOut: []*computepb.Instance{
			runningInstance("soul-l-0", "10.7.7.1"),
			runningInstance("soul-l-1", "10.7.7.2"),
		},
	}
	withFakeInstances(t, f)

	d := &GcpDriver{}
	s := &listStream{ctx: context.Background()}
	if err := d.List(&pluginv1.ListRequest{
		Filter: mustStruct(t, map[string]any{runLabelKey: "run-list"}),
		Credentials: mustStruct(t, map[string]any{
			"service_account_key": `{"type":"service_account"}`,
			"project":             "my-project",
			"zone":                "europe-west1-b",
		}),
	}, s); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(s.sent) != 2 {
		t.Errorf("list events=%d, want 2", len(s.sent))
	}
	// filter поднят до GCP-filter-синтаксиса `labels.<k>=<v>`.
	if f.lastListIn == nil || f.lastListIn.GetFilter() != `labels.soulstack_run=run-list` {
		t.Errorf("List filter=%q, want labels.soulstack_run=run-list", f.lastListIn.GetFilter())
	}
}

func TestDestroy_PerVM(t *testing.T) {
	f := &fakeInstances{}
	withFakeInstances(t, f)
	d := &GcpDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"soul-d-0", "soul-d-1"},
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
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
}

func TestDestroy_NotFoundIsIdempotent(t *testing.T) {
	f := &fakeInstances{deleteErr: &googleapi.Error{Code: 404, Message: "gone"}}
	withFakeInstances(t, f)
	d := &GcpDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"soul-gone-0"},
		Credentials: mustStruct(t, map[string]any{"project": "my-project", "zone": "europe-west1-b"}),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.sent) != 1 || s.sent[0].Failed {
		t.Errorf("not-found destroy must be idempotent (success), got %+v", s.sent)
	}
}

func TestClassifyGCP_Codes(t *testing.T) {
	cases := []struct {
		name string
		err  *googleapi.Error
		want clouddriver.FailClass
	}{
		{"401-auth", &googleapi.Error{Code: 401}, clouddriver.FailAuth},
		{"403-auth-default", &googleapi.Error{Code: 403, Message: "forbidden"}, clouddriver.FailAuth},
		{"403-quota-by-reason", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "quotaExceeded"}}}, clouddriver.FailQuota},
		{"403-rateLimit-transient", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}}, clouddriver.FailTransient},
		{"404-not-found", &googleapi.Error{Code: 404}, clouddriver.FailNotFound},
		{"400-invalid", &googleapi.Error{Code: 400}, clouddriver.FailInvalidParams},
		{"409-conflict", &googleapi.Error{Code: 409}, clouddriver.FailInvalidParams},
		{"412-precondition", &googleapi.Error{Code: 412}, clouddriver.FailInvalidParams},
		{"429-transient", &googleapi.Error{Code: 429}, clouddriver.FailTransient},
		{"500-transient", &googleapi.Error{Code: 500}, clouddriver.FailTransient},
		{"503-transient", &googleapi.Error{Code: 503}, clouddriver.FailTransient},
		{"418-unknown", &googleapi.Error{Code: 418}, clouddriver.FailUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyGCP(tc.err)
			if got != tc.want {
				t.Errorf("classifyGCP(%+v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// не-API ошибка → transient
	if got := classifyGCP(errors.New("dial tcp: timeout")); got != clouddriver.FailTransient {
		t.Errorf("non-API err class=%v, want transient", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
