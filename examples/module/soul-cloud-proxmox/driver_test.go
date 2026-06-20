package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
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

// fakePVE — mock pveAPI для L0-unit-тестов (без сети). Поведение настраивается
// per-метод; statusSeq позволяет смоделировать переход locked→running между
// раундами поллера, agentSeq — DHCP-handshake (сначала "", потом IP).
type fakePVE struct {
	cloneUPID  string
	cloneErr   error
	cloneCalls int
	lastClone  CloneParams

	nextID    int
	nextIDErr error

	setConfigErr   error
	setConfigCalls int
	lastSetFields  map[string]string

	startErr   error
	startCalls int

	stopErr   error
	stopCalls int

	deleteErr   error
	deleteCalls int

	// statusSeq отдаёт по очереди для каждого вызова; последний элемент «залипает».
	statusSeq []VMStatus
	statusErr error
	statusIdx int
	statusFn  func(call int, node string, vmid int) (VMStatus, error)
	statusN   int

	clusterVMs   []ClusterVM
	clusterErr   error
	clusterCalls int

	agentSeq   []string // primaryIP по вызовам
	agentErr   error
	agentIdx   int
	agentFn    func(call int, node string, vmid int) (string, error)
	agentN     int
	agentCalls int
}

func (f *fakePVE) CloneVM(_ context.Context, p CloneParams) (string, error) {
	f.cloneCalls++
	f.lastClone = p
	if f.cloneErr != nil {
		return "", f.cloneErr
	}
	if f.cloneUPID != "" {
		return f.cloneUPID, nil
	}
	return "UPID:test:clone", nil
}

func (f *fakePVE) NextID(_ context.Context) (int, error) {
	if f.nextIDErr != nil {
		return 0, f.nextIDErr
	}
	id := f.nextID
	f.nextID++ // следующий VMID при последовательном clone
	if id == 0 {
		id = 10000
		f.nextID = 10001
	}
	return id, nil
}

func (f *fakePVE) SetVMConfig(_ context.Context, _ string, _ int, fields map[string]string) error {
	f.setConfigCalls++
	f.lastSetFields = fields
	return f.setConfigErr
}

func (f *fakePVE) StartVM(_ context.Context, _ string, _ int) (string, error) {
	f.startCalls++
	if f.startErr != nil {
		return "", f.startErr
	}
	return "UPID:test:start", nil
}

func (f *fakePVE) StopVM(_ context.Context, _ string, _ int) (string, error) {
	f.stopCalls++
	if f.stopErr != nil {
		return "", f.stopErr
	}
	return "UPID:test:stop", nil
}

func (f *fakePVE) DeleteVM(_ context.Context, _ string, _ int) (string, error) {
	f.deleteCalls++
	if f.deleteErr != nil {
		return "", f.deleteErr
	}
	return "UPID:test:delete", nil
}

func (f *fakePVE) GetVMStatus(_ context.Context, node string, vmid int) (VMStatus, error) {
	call := f.statusN
	f.statusN++
	if f.statusFn != nil {
		return f.statusFn(call, node, vmid)
	}
	if f.statusErr != nil {
		return VMStatus{}, f.statusErr
	}
	if len(f.statusSeq) == 0 {
		return VMStatus{Node: node, VMID: vmid}, nil
	}
	out := f.statusSeq[f.statusIdx]
	out.Node = node
	out.VMID = vmid
	if f.statusIdx < len(f.statusSeq)-1 {
		f.statusIdx++
	}
	return out, nil
}

func (f *fakePVE) ListClusterVMs(_ context.Context) ([]ClusterVM, error) {
	f.clusterCalls++
	if f.clusterErr != nil {
		return nil, f.clusterErr
	}
	return f.clusterVMs, nil
}

func (f *fakePVE) GetGuestAgentInterfaces(_ context.Context, node string, vmid int) (string, error) {
	f.agentCalls++
	call := f.agentN
	f.agentN++
	if f.agentFn != nil {
		return f.agentFn(call, node, vmid)
	}
	if f.agentErr != nil {
		return "", f.agentErr
	}
	if len(f.agentSeq) == 0 {
		return "", nil
	}
	out := f.agentSeq[f.agentIdx]
	if f.agentIdx < len(f.agentSeq)-1 {
		f.agentIdx++
	}
	return out, nil
}

// withFakePVE подменяет фабрику клиента на возврат f, восстанавливает после теста.
func withFakePVE(t *testing.T, f *fakePVE) {
	t.Helper()
	orig := newPveClient
	newPveClient = func(_ context.Context, _ pveCredentials) (pveAPI, error) { return f, nil }
	t.Cleanup(func() { newPveClient = orig })
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

// baseProfile — минимальный валидный profile для Create. Параметризуем
// доп-полями через add()-обёртку.
func baseProfile(extra map[string]any) map[string]any {
	p := map[string]any{
		"target_node":   "pve1",
		"template_vmid": 9000,
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// baseCreds — минимальный валидный credentials Struct (token-based).
func baseCreds() map[string]any {
	return map[string]any{
		"endpoint": "https://pve.example:8006",
		"token":    "root@pam!soul=00000000-0000-0000-0000-000000000000",
	}
}

func runningStatus(name string) VMStatus {
	return VMStatus{Status: "running", QmpStatus: "running", Name: name}
}

func lockedStatus(lock string) VMStatus {
	return VMStatus{Status: "stopped", Lock: lock}
}

func TestSchema_ParsesEmbedded(t *testing.T) {
	d := &ProxmoxDriver{}
	rep, err := d.Schema(context.Background(), &pluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	m := rep.ProfileSchema.AsMap()
	req, _ := m["required"].([]any)
	if len(req) != 2 {
		t.Errorf("schema required=%v, want 2 fields (target_node, template_vmid)", req)
	}
}

func TestValidate_MissingFields(t *testing.T) {
	d := &ProxmoxDriver{}
	rep, err := d.Validate(context.Background(), &pluginv1.ValidateProfileRequest{
		Profile: mustStruct(t, map[string]any{"target_node": "pve1"}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Ok {
		t.Error("expected Ok=false on missing template_vmid")
	}
	if len(rep.Errors) != 1 {
		t.Errorf("errors=%v, want 1 (template_vmid)", rep.Errors)
	}
}

// TestCreate_HappyPath — clone happy: NextID → CloneVM → SetVMConfig → StartVM
// → probe возвращает running + guest-agent IP сразу.
func TestCreate_HappyPath(t *testing.T) {
	withFastBackoff(t, 4)
	f := &fakePVE{
		statusSeq: []VMStatus{runningStatus("soul-10000")},
		agentSeq:  []string{"10.0.0.5"},
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &createStream{}
	err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, baseProfile(map[string]any{
			"name_prefix": "soul",
			"cores":       4,
			"memory":      4096,
		})),
		Credentials: mustStruct(t, baseCreds()),
		Userdata:    "#cloud-config\nhostname: test\n",
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
	if vm.VmId != "pve1/10000" {
		t.Errorf("vm_id=%q, want composite `pve1/10000`", vm.VmId)
	}
	if vm.PrimaryIp != "10.0.0.5" {
		t.Errorf("primary_ip=%q, want 10.0.0.5", vm.PrimaryIp)
	}
	if vm.Fqdn != "soul-10000" {
		t.Errorf("fqdn=%q, want VM name (soul-10000)", vm.Fqdn)
	}
	// CloneVM был вызван с правильными параметрами.
	if f.cloneCalls != 1 {
		t.Errorf("clone calls=%d, want 1", f.cloneCalls)
	}
	if f.lastClone.TemplateVMID != 9000 || f.lastClone.NewVMID != 10000 || f.lastClone.TargetNode != "pve1" {
		t.Errorf("clone params=%+v", f.lastClone)
	}
	if !f.lastClone.FullClone {
		t.Error("full_clone default must be true")
	}
	// SetVMConfig получил cores/memory/description (с base64 userdata).
	if f.lastSetFields["cores"] != "4" || f.lastSetFields["memory"] != "4096" {
		t.Errorf("set-config resources=%v", f.lastSetFields)
	}
	if desc, ok := f.lastSetFields["description"]; !ok || desc == "" {
		t.Error("expected description with base64 userdata (cicustom path)")
	}
	if f.startCalls != 1 {
		t.Errorf("start calls=%d, want 1", f.startCalls)
	}
}

// TestCreate_WaitsForRunning — VM сначала locked (clone), потом running без
// IP (DHCP-handshake), потом running + IP. Поллер должен переждать оба
// промежуточных раунда без failed-event.
func TestCreate_WaitsForRunning(t *testing.T) {
	withFastBackoff(t, 10)
	f := &fakePVE{
		statusSeq: []VMStatus{
			lockedStatus("clone"),       // раунд 1: locked
			{Status: "stopped"},         // раунд 2: не залочена, но ещё не стартанула
			runningStatus("soul-10000"), // раунд 3+: running
		},
		// agent: первый probe agent ещё не отвечает (по тексту 500 — Transient
		// classify через pveHTTPError); потом IP.
		agentFn: func(call int, _ string, _ int) (string, error) {
			if call <= 1 {
				// На промежуточные probe-раунды agent НЕ вызывается (status != running).
				// На первом running-раунде вернём "" — DHCP в полёте.
				return "", nil
			}
			return "10.0.0.9", nil
		},
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, baseProfile(nil)),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("final=%+v, want success after wait", last)
	}
	if last.Vms[0].PrimaryIp != "10.0.0.9" {
		t.Errorf("primary_ip=%q after wait", last.Vms[0].PrimaryIp)
	}
}

// TestCreate_AuthError — newPveClient падает при пустых credentials (нет ни
// token, ни ticket-парами) → failed-event с auth-class префиксом, без clone.
func TestCreate_AuthError(t *testing.T) {
	// НЕ подменяем newPveClient — используем реальный, который вернёт
	// auth-ошибку на пустых creds.
	d := &ProxmoxDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, baseProfile(nil)),
		Credentials: mustStruct(t, map[string]any{}), // никаких полей
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on missing credentials")
	}
	if !strings.HasPrefix(last.Message, "auth:") {
		t.Errorf("message=%q, want auth-class prefix", last.Message)
	}
}

// TestCreate_Idempotent_ReusesExisting — findByRunTag вернул живые VM по тегу
// → драйвер не зовёт Clone, идёт сразу в finalizeCreate.
func TestCreate_Idempotent_ReusesExisting(t *testing.T) {
	withFastBackoff(t, 4)
	f := &fakePVE{
		clusterVMs: []ClusterVM{
			{VMID: 12345, Node: "pve1", Status: "running", Name: "soul-12345",
				Tags: "soulstack-run=run-42;env=prod", Type: "qemu"},
		},
		statusSeq: []VMStatus{runningStatus("soul-12345")},
		agentSeq:  []string{"10.1.1.1"},
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, baseProfile(map[string]any{
			"tags": map[string]any{runTagKey: "run-42"},
		})),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.cloneCalls != 0 {
		t.Errorf("clone calls=%d; idempotent path must NOT clone", f.cloneCalls)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("idempotent final=%+v", last)
	}
	if last.Vms[0].VmId != "pve1/12345" {
		t.Errorf("reused vm_id=%q, want pve1/12345", last.Vms[0].VmId)
	}
}

// TestCreate_CtxCancel_AntiOrphan — ctx отменяется во время wait → финальный
// failed-event несёт vm_id уже-склонированных VM, чтобы Keeper мог их Destroy.
func TestCreate_CtxCancel_AntiOrphan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakePVE{
		// status навсегда locked (clone) — пуллер крутится, пока ctx не отменят.
		statusSeq: []VMStatus{lockedStatus("clone")},
	}
	withFakePVE(t, f)
	cancel() // отменяем сразу — поллер уйдёт в sleepCtx и вернёт ctx.Err

	d := &ProxmoxDriver{}
	s := &createStream{ctx: ctx}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, baseProfile(nil)),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on ctx-cancel during wait")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "pve1/10000" {
		t.Errorf("anti-orphan: final event must carry vm_id pve1/10000, got %+v", last.Vms)
	}
}

// TestCreate_WaitDeadline_AntiOrphan — wait-поллер упирается в MaxAttempts (НЕ
// ctx-cancel) → ErrWaitDeadline → failed-event с заполненным vm_id.
func TestCreate_WaitDeadline_AntiOrphan(t *testing.T) {
	withFastBackoff(t, 2)
	f := &fakePVE{
		statusSeq: []VMStatus{lockedStatus("clone")},
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, baseProfile(nil)),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on wait-deadline exhaustion")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "pve1/10000" {
		t.Errorf("anti-orphan: final event must carry vm_id pve1/10000, got %+v", last.Vms)
	}
	if !strings.Contains(last.Message, "max attempts exhausted") {
		t.Errorf("message=%q, want max-attempts-exhausted (ErrWaitDeadline)", last.Message)
	}
}

// TestCreate_GuestAgentNotResponding — VM running, но guest-agent не настроен
// (4xx/5xx, не-transient). Probe возвращает Err → finalizeCreate шлёт failed
// для этой VM с заполненным vm_id (anti-orphan).
func TestCreate_GuestAgentNotResponding(t *testing.T) {
	withFastBackoff(t, 4)
	f := &fakePVE{
		statusSeq: []VMStatus{runningStatus("soul-10000")},
		agentErr:  &pveHTTPError{Status: 500, Body: "QEMU guest agent is not running"},
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, baseProfile(nil)),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	// 500 от Proxmox с текстом «is not running» классифицируется как not_found
	// (см. classify.go) — это НЕ transient. Probe вернёт Err → VM helmed
	// failed, но vm_id обязан быть.
	if !last.Failed {
		t.Fatal("expected failed=true when guest-agent fails fatally")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "pve1/10000" {
		t.Errorf("anti-orphan vm_id missing: %+v", last.Vms)
	}
	// fqdn/primary_ip НЕ должны быть заполнены при failed probe.
	if last.Vms[0].PrimaryIp != "" {
		t.Errorf("primary_ip=%q must be empty on probe failure", last.Vms[0].PrimaryIp)
	}
}

// TestCreate_TransientProbeError_SwallowAndRetry — agent временно отдаёт
// transient-ошибку (502) → probe-обёртка глотает её → следующий round успешен.
func TestCreate_TransientProbeError_SwallowAndRetry(t *testing.T) {
	withFastBackoff(t, 8)
	f := &fakePVE{
		// status стабильно running во всех раундах.
		statusSeq: []VMStatus{runningStatus("soul-10000")},
	}
	// agent: call 0 → 502 (transient), call 1+ → IP.
	f.agentFn = func(call int, _ string, _ int) (string, error) {
		if call == 0 {
			return "", &pveHTTPError{Status: 502, Body: "bad gateway"}
		}
		return "10.5.5.5", nil
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, baseProfile(nil)),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("transient probe-error must be swallowed; got failed: %+v", last)
	}
	if last.Vms[0].PrimaryIp != "10.5.5.5" {
		t.Errorf("primary_ip=%q after transient retry", last.Vms[0].PrimaryIp)
	}
}

// TestCreate_OverCount_Idempotent — findByRunTag вернул БОЛЬШЕ VM, чем
// запрошенный count. Драйвер обязан вернуть всё найденное (не падать).
func TestCreate_OverCount_Idempotent(t *testing.T) {
	withFastBackoff(t, 4)
	existing := []ClusterVM{
		{VMID: 101, Node: "pve1", Status: "running", Name: "soul-101", Tags: "soulstack-run=run-over", Type: "qemu"},
		{VMID: 102, Node: "pve1", Status: "running", Name: "soul-102", Tags: "soulstack-run=run-over", Type: "qemu"},
		{VMID: 103, Node: "pve2", Status: "running", Name: "soul-103", Tags: "soulstack-run=run-over", Type: "qemu"},
	}
	f := &fakePVE{
		clusterVMs: existing,
		statusSeq:  []VMStatus{runningStatus("soul-101")},
		agentSeq:   []string{"10.1.0.1"},
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 2, // меньше, чем реальное число существующих VM
		Profile: mustStruct(t, baseProfile(map[string]any{
			"tags": map[string]any{runTagKey: "run-over"},
		})),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.cloneCalls != 0 {
		t.Errorf("clone calls=%d; over-count idempotent path must NOT clone", f.cloneCalls)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("over-count idempotent final=%+v", last)
	}
	if len(last.Vms) != 3 {
		t.Errorf("vms=%d, want 3 (all existing returned, not truncated to count)", len(last.Vms))
	}
}

// TestCreate_NewVMIDStart — profile.new_vmid_start задан → драйвер НЕ зовёт
// NextID, использует start+seq.
func TestCreate_NewVMIDStart(t *testing.T) {
	withFastBackoff(t, 4)
	f := &fakePVE{
		statusSeq: []VMStatus{runningStatus("soul-15000")},
		agentSeq:  []string{"10.0.0.5"},
		// nextID 0 — если драйвер ошибочно вызовет, тест поймает по lastClone.NewVMID.
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, baseProfile(map[string]any{
			"new_vmid_start": 15000,
		})),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.lastClone.NewVMID != 15000 {
		t.Errorf("NewVMID=%d, want 15000 from new_vmid_start", f.lastClone.NewVMID)
	}
}

func TestDestroy_PerVM(t *testing.T) {
	f := &fakePVE{}
	withFakePVE(t, f)
	d := &ProxmoxDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"pve1/100", "pve2/200"},
		Credentials: mustStruct(t, baseCreds()),
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
	if f.stopCalls != 2 || f.deleteCalls != 2 {
		t.Errorf("stop=%d delete=%d, want 2 each", f.stopCalls, f.deleteCalls)
	}
}

// TestDestroy_NotFoundIsIdempotent — Proxmox 500 «does not exist» на stop →
// идемпотент-успех, без передачи в delete.
func TestDestroy_NotFoundIsIdempotent(t *testing.T) {
	f := &fakePVE{
		stopErr: &pveHTTPError{Status: 500, Body: "VM 999 does not exist"},
	}
	withFakePVE(t, f)
	d := &ProxmoxDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"pve1/999"},
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.sent) != 1 || s.sent[0].Failed {
		t.Errorf("not-found destroy must be idempotent (success), got %+v", s.sent)
	}
	if f.deleteCalls != 0 {
		t.Errorf("delete called %d times after not-found-stop; want 0", f.deleteCalls)
	}
}

// TestDestroy_AlreadyStopped — Proxmox 500 «not running» на stop → драйвер
// продолжает к delete (VM остановлена, но существует).
func TestDestroy_AlreadyStopped(t *testing.T) {
	withFastBackoff(t, 2)
	f := &fakePVE{
		stopErr: &pveHTTPError{Status: 500, Body: "VM is not running"},
	}
	withFakePVE(t, f)
	d := &ProxmoxDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"pve1/777"},
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if f.deleteCalls != 1 {
		t.Errorf("delete called %d times; want 1 (continue after `not running`)", f.deleteCalls)
	}
	if len(s.sent) != 1 || s.sent[0].Failed {
		t.Errorf("destroy events=%+v, want success", s.sent)
	}
}

// TestDestroy_InvalidVmID — vm_id не в формате `<node>/<vmid>` → invalid_params
// per-event, остальные VM в списке продолжают обрабатываться.
func TestDestroy_InvalidVmID(t *testing.T) {
	f := &fakePVE{}
	withFakePVE(t, f)
	d := &ProxmoxDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"i-aws-style", "pve1/200"},
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.sent) != 2 {
		t.Fatalf("events=%d, want 2", len(s.sent))
	}
	if !s.sent[0].Failed {
		t.Errorf("first vm_id (invalid) must be failed, got %+v", s.sent[0])
	}
	if !strings.HasPrefix(s.sent[0].Message, "invalid_params:") {
		t.Errorf("first message=%q, want invalid_params prefix", s.sent[0].Message)
	}
	if s.sent[1].Failed {
		t.Errorf("second vm_id (valid) must succeed, got %+v", s.sent[1])
	}
}

func TestStatus_UsesCredentials(t *testing.T) {
	f := &fakePVE{statusSeq: []VMStatus{runningStatus("soul-100")}}
	withFakePVE(t, f)
	d := &ProxmoxDriver{}
	rep, err := d.Status(context.Background(), &pluginv1.StatusRequest{
		VmId:        "pve1/100",
		Credentials: mustStruct(t, baseCreds()),
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.State != "running" {
		t.Errorf("state=%q, want running", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes must be populated")
	}
}

// TestStatus_InvalidVmID — Status с vm_id без `/` → ошибка invalid_params.
func TestStatus_InvalidVmID(t *testing.T) {
	f := &fakePVE{}
	withFakePVE(t, f)
	d := &ProxmoxDriver{}
	_, err := d.Status(context.Background(), &pluginv1.StatusRequest{
		VmId:        "12345",
		Credentials: mustStruct(t, baseCreds()),
	})
	if err == nil {
		t.Fatal("expected error on bare vmid (no node)")
	}
}

func TestList_UsesCredentialsField(t *testing.T) {
	f := &fakePVE{
		clusterVMs: []ClusterVM{
			{VMID: 201, Node: "pve1", Name: "soul-201", Tags: "soulstack-run=run-list;t1", Type: "qemu"},
			{VMID: 202, Node: "pve2", Name: "soul-202", Tags: "soulstack-run=run-list", Type: "qemu"},
			// LXC type — должен быть отфильтрован.
			{VMID: 999, Node: "pve1", Name: "ct-999", Tags: "soulstack-run=run-list", Type: "lxc"},
			// Другой тег — не пройдёт filter.
			{VMID: 888, Node: "pve1", Name: "other", Tags: "soulstack-run=other-run", Type: "qemu"},
		},
	}
	withFakePVE(t, f)

	d := &ProxmoxDriver{}
	s := &listStream{ctx: context.Background()}
	if err := d.List(&pluginv1.ListRequest{
		Filter:      mustStruct(t, map[string]any{runTagKey: "run-list"}),
		Credentials: mustStruct(t, baseCreds()),
	}, s); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(s.sent) != 2 {
		t.Errorf("list events=%d, want 2 (qemu+run-list match)", len(s.sent))
	}
}

func TestClassifyProxmox_Codes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want clouddriver.FailClass
	}{
		{"401 unauthorized", &pveHTTPError{Status: 401, Body: "no ticket"}, clouddriver.FailAuth},
		{"403 forbidden", &pveHTTPError{Status: 403, Body: "Permission check failed"}, clouddriver.FailAuth},
		{"404 not_found", &pveHTTPError{Status: 404, Body: "no such endpoint"}, clouddriver.FailNotFound},
		{"400 invalid", &pveHTTPError{Status: 400, Body: "parameter verification failed"}, clouddriver.FailInvalidParams},
		{"500 does-not-exist→not_found", &pveHTTPError{Status: 500, Body: "VM 999 does not exist"}, clouddriver.FailNotFound},
		{"500 transient (lock)", &pveHTTPError{Status: 500, Body: "can't acquire lock"}, clouddriver.FailTransient},
		{"502 bad gateway transient", &pveHTTPError{Status: 502, Body: "upstream"}, clouddriver.FailTransient},
		{"503 svc unavail transient", &pveHTTPError{Status: 503, Body: ""}, clouddriver.FailTransient},
		{"429 throttle transient", &pveHTTPError{Status: 429, Body: ""}, clouddriver.FailTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyProxmox(tc.err)
			if got != tc.want {
				t.Errorf("classifyProxmox(%v)=%v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// не-HTTP ошибка → transient (сеть/DNS/TLS)
	if got := classifyProxmox(errors.New("dial tcp: timeout")); got != clouddriver.FailTransient {
		t.Errorf("non-HTTP err class=%v, want transient", got)
	}
}

// TestSplitFormatVmID_RoundTrip — формат vm_id `<node>/<vmid>` обратим.
func TestSplitFormatVmID_RoundTrip(t *testing.T) {
	cases := []struct {
		node string
		vmid int
	}{
		{"pve1", 100},
		{"pve-cluster-01", 999999},
	}
	for _, tc := range cases {
		s := formatVmID(tc.node, tc.vmid)
		n, v, err := splitVmID(s)
		if err != nil {
			t.Errorf("splitVmID(%q): %v", s, err)
			continue
		}
		if n != tc.node || v != tc.vmid {
			t.Errorf("round-trip %q: got (%q, %d), want (%q, %d)", s, n, v, tc.node, tc.vmid)
		}
	}
}

// TestSplitVmID_Errors — некорректные формы vm_id отбраковываются.
func TestSplitVmID_Errors(t *testing.T) {
	bad := []string{"", "100", "pve1/", "/100", "pve1/abc", "pve1//100"}
	for _, s := range bad {
		if _, _, err := splitVmID(s); err == nil {
			t.Errorf("splitVmID(%q) expected error, got nil", s)
		}
	}
}

// TestHasTag — фильтр по теге игнорирует whitespace и принимает только точное
// `key=value` совпадение.
func TestHasTag(t *testing.T) {
	cases := []struct {
		tags  string
		key   string
		value string
		want  bool
	}{
		{"soulstack-run=run-1;env=prod", "soulstack-run", "run-1", true},
		{"env=prod;soulstack-run=run-1", "soulstack-run", "run-1", true},
		{"soulstack-run=run-2", "soulstack-run", "run-1", false},
		{"", "soulstack-run", "run-1", false},
		{"env=prod", "soulstack-run", "run-1", false},
		{"  soulstack-run=run-1  ", "soulstack-run", "run-1", true},
	}
	for _, tc := range cases {
		if got := hasTag(tc.tags, tc.key, tc.value); got != tc.want {
			t.Errorf("hasTag(%q, %q, %q)=%v, want %v", tc.tags, tc.key, tc.value, got, tc.want)
		}
	}
}
