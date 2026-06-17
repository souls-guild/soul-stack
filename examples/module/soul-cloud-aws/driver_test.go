package main

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

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

// fakeEC2 — mock ec2API для L0-unit-тестов (без сети). Поведение
// настраивается per-метод; describeSeq позволяет смоделировать переход
// pending→running между раундами поллера.
//
// describeFn — опциональный override: получает 0-based номер вызова и волен
// вернуть свою пару (out, err) — для тестов transient-probe-error (ошибка
// классифицирована Transient → поллер проглатывает) и других сценариев,
// где плоского describeSeq не хватает.
type fakeEC2 struct {
	runOut  *ec2.RunInstancesOutput
	runErr  error
	runCall int

	describeSeq []*ec2.DescribeInstancesOutput // последовательные ответы Describe
	describeIdx int
	describeErr error
	describeFn  func(call int) (*ec2.DescribeInstancesOutput, error)
	describeN   int

	termOut *ec2.TerminateInstancesOutput
	termErr error

	lastRunInput  *ec2.RunInstancesInput
	lastTermInput *ec2.TerminateInstancesInput
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.runCall++
	f.lastRunInput = in
	if f.runErr != nil {
		return nil, f.runErr
	}
	return f.runOut, nil
}

func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.lastTermInput = in
	if f.termErr != nil {
		return nil, f.termErr
	}
	return f.termOut, nil
}

func (f *fakeEC2) DescribeInstances(_ context.Context, _ *ec2.DescribeInstancesInput, _ ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	call := f.describeN
	f.describeN++
	if f.describeFn != nil {
		return f.describeFn(call)
	}
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if len(f.describeSeq) == 0 {
		return &ec2.DescribeInstancesOutput{}, nil
	}
	out := f.describeSeq[f.describeIdx]
	if f.describeIdx < len(f.describeSeq)-1 {
		f.describeIdx++
	}
	return out, nil
}

func (f *fakeEC2) DescribeImages(_ context.Context, _ *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{}, nil
}

func (f *fakeEC2) DescribeSubnets(_ context.Context, _ *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	return &ec2.DescribeSubnetsOutput{}, nil
}

// withFakeEC2 подменяет фабрику клиента на возврат f, восстанавливает после теста.
func withFakeEC2(t *testing.T, f *fakeEC2) {
	t.Helper()
	orig := newEC2Client
	newEC2Client = func(_ context.Context, _ awsCredentials) (ec2API, error) { return f, nil }
	t.Cleanup(func() { newEC2Client = orig })
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

func runningInstance(id, ip, dns string) ec2types.Instance {
	return ec2types.Instance{
		InstanceId:       aws.String(id),
		PrivateIpAddress: aws.String(ip),
		PrivateDnsName:   aws.String(dns),
		State:            &ec2types.InstanceState{Name: ec2types.InstanceStateNameRunning},
		Placement:        &ec2types.Placement{AvailabilityZone: aws.String("eu-west-1a")},
	}
}

func describeOut(insts ...ec2types.Instance) *ec2.DescribeInstancesOutput {
	return &ec2.DescribeInstancesOutput{
		Reservations: []ec2types.Reservation{{Instances: insts}},
	}
}

func TestSchema_ParsesEmbedded(t *testing.T) {
	d := &AwsDriver{}
	rep, err := d.Schema(context.Background(), &pluginv1.SchemaRequest{})
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	m := rep.ProfileSchema.AsMap()
	req, _ := m["required"].([]any)
	if len(req) != 3 {
		t.Errorf("schema required=%v, want 3 fields", req)
	}
}

func TestValidate_MissingFields(t *testing.T) {
	d := &AwsDriver{}
	rep, err := d.Validate(context.Background(), &pluginv1.ValidateProfileRequest{
		Profile: mustStruct(t, map[string]any{"region": "eu-west-1"}),
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if rep.Ok {
		t.Error("expected Ok=false on missing ami/instance_type")
	}
	if len(rep.Errors) != 2 {
		t.Errorf("errors=%v, want 2 (ami, instance_type)", rep.Errors)
	}
}

func TestCreate_HappyPath(t *testing.T) {
	f := &fakeEC2{
		runOut: &ec2.RunInstancesOutput{Instances: []ec2types.Instance{
			{InstanceId: aws.String("i-aaa")},
		}},
		// первый Describe (поллер) сразу running с IP/DNS.
		describeSeq: []*ec2.DescribeInstancesOutput{
			describeOut(runningInstance("i-aaa", "10.0.0.5", "ip-10-0-0-5.eu-west-1.compute.internal")),
		},
	}
	withFakeEC2(t, f)

	d := &AwsDriver{}
	s := &createStream{}
	err := d.Create(&pluginv1.CreateRequest{
		Count:   1,
		Profile: mustStruct(t, map[string]any{"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium"}),
		Credentials: mustStruct(t, map[string]any{
			"access_key_id": "AKIA", "secret_access_key": "x", "region": "eu-west-1",
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
	if vm.VmId != "i-aaa" {
		t.Errorf("vm_id=%q", vm.VmId)
	}
	if vm.Fqdn == "" {
		t.Error("fqdn empty (must be set = SID)")
	}
	if vm.PrimaryIp != "10.0.0.5" {
		t.Errorf("primary_ip=%q", vm.PrimaryIp)
	}
	// userdata прокинут в RunInstances в виде base64 (EC2-требование):
	// aws-sdk-go-v2 поле UserData НЕ кодирует, драйвер кодирует сам.
	if f.lastRunInput == nil {
		t.Fatal("RunInstances not called")
	}
	decoded, derr := base64.StdEncoding.DecodeString(aws.ToString(f.lastRunInput.UserData))
	if derr != nil {
		t.Fatalf("UserData not valid base64: %v", derr)
	}
	if string(decoded) != "#cloud-config\n" {
		t.Errorf("decoded userdata=%q, want raw cloud-init blob", decoded)
	}
}

func TestCreate_WaitsForRunning(t *testing.T) {
	f := &fakeEC2{
		runOut: &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-bbb")}}},
		describeSeq: []*ec2.DescribeInstancesOutput{
			// раунд 1: pending, без IP
			describeOut(ec2types.Instance{
				InstanceId: aws.String("i-bbb"),
				State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending},
			}),
			// раунд 2: running с IP
			describeOut(runningInstance("i-bbb", "10.0.0.9", "ip-10-0-0-9.internal")),
		},
	}
	withFakeEC2(t, f)
	d := &AwsDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, map[string]any{"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium"}),
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
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

func TestCreate_RunInstancesAuthError(t *testing.T) {
	f := &fakeEC2{runErr: &smithy.GenericAPIError{Code: "AuthFailure", Message: "bad creds"}}
	withFakeEC2(t, f)
	d := &AwsDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, map[string]any{"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium"}),
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
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
	if f.runCall != 1 {
		t.Errorf("RunInstances called %d times; auth must NOT retry", f.runCall)
	}
}

func TestCreate_Idempotent_ReusesExisting(t *testing.T) {
	f := &fakeEC2{
		describeSeq: []*ec2.DescribeInstancesOutput{
			// findByRunTag → одна живая VM (>= count=1) → переиспользуем
			describeOut(runningInstance("i-existing", "10.1.1.1", "ip-10-1-1-1.internal")),
		},
	}
	withFakeEC2(t, f)
	d := &AwsDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 1,
		Profile: mustStruct(t, map[string]any{
			"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium",
			"tags": map[string]any{runTagKey: "run-42"},
		}),
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.runCall != 0 {
		t.Errorf("RunInstances called %d times; idempotent path must NOT launch new VM", f.runCall)
	}
	if s.last().Failed {
		t.Fatalf("idempotent final=%+v", s.last())
	}
	if s.last().Vms[0].VmId != "i-existing" {
		t.Errorf("reused vm=%q, want i-existing", s.last().Vms[0].VmId)
	}
}

func TestCreate_CtxCancel_AntiOrphan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeEC2{
		runOut: &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-orphan")}}},
		// всегда pending → поллер крутится, пока ctx не отменят
		describeSeq: []*ec2.DescribeInstancesOutput{
			describeOut(ec2types.Instance{
				InstanceId: aws.String("i-orphan"),
				State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending},
			}),
		},
	}
	withFakeEC2(t, f)
	cancel() // отменяем сразу — поллер уйдёт в sleepCtx и вернёт ctx.Err

	d := &AwsDriver{}
	s := &createStream{ctx: ctx}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, map[string]any{"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium"}),
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on ctx-cancel during wait")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "i-orphan" {
		t.Errorf("anti-orphan: final event must carry vm_id i-orphan, got %+v", last.Vms)
	}
}

// TestCreate_WaitDeadline_AntiOrphan — wait-поллер упирается в MaxAttempts
// (НЕ ctx-cancel) → возврат ErrWaitDeadline → failed-event с заполненным vm_id
// (anti-orphan ветка, отличная от ctx-cancel). Все probe-запросы возвращают
// pending, поэтому WaitUntilReady исчерпает попытки и вернёт deadline-ошибку.
func TestCreate_WaitDeadline_AntiOrphan(t *testing.T) {
	withFastBackoff(t, 2)
	f := &fakeEC2{
		runOut: &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-wait")}}},
		describeSeq: []*ec2.DescribeInstancesOutput{
			describeOut(ec2types.Instance{
				InstanceId: aws.String("i-wait"),
				State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending},
			}),
		},
	}
	withFakeEC2(t, f)

	d := &AwsDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, map[string]any{"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium"}),
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if !last.Failed {
		t.Fatal("expected failed=true on wait-deadline exhaustion")
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "i-wait" {
		t.Errorf("anti-orphan: final event must carry vm_id i-wait, got %+v", last.Vms)
	}
	// Anti-orphan ветка отличается от ctx-cancel: ctx тут НЕ отменялся,
	// failed-message приходит из ErrWaitDeadline (transient-класс по Classify-
	// преобразованию, но это особенность таксономии — детерминанта здесь именно
	// «не дождались за MaxAttempts»).
	if !contains(last.Message, "max attempts exhausted") {
		t.Errorf("message=%q, want max-attempts-exhausted (ErrWaitDeadline)", last.Message)
	}
}

// TestCreate_TerminalStateProbe — VM во время wait уходит в terminal-state
// (stopped/stopping/terminated) → probe возвращает ProbeResult{Err} →
// поллер прекращает опрос этой VM, finalizeCreate шлёт failed-event с vm_id
// и описательным сообщением. Этот случай ОТЛИЧАЕТСЯ от wait-deadline тем, что
// сам WaitUntilReady НЕ возвращает ошибку (probe-Err per-VM), но drvier видит
// !Ready во всех wr и выставляет anyFailed=true.
func TestCreate_TerminalStateProbe(t *testing.T) {
	withFastBackoff(t, 4)
	cases := []struct {
		name  string
		state ec2types.InstanceStateName
	}{
		{"stopping", ec2types.InstanceStateNameStopping},
		{"stopped", ec2types.InstanceStateNameStopped},
		{"terminated", ec2types.InstanceStateNameTerminated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeEC2{
				runOut: &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-term")}}},
				describeSeq: []*ec2.DescribeInstancesOutput{
					describeOut(ec2types.Instance{
						InstanceId: aws.String("i-term"),
						State:      &ec2types.InstanceState{Name: tc.state},
					}),
				},
			}
			withFakeEC2(t, f)
			d := &AwsDriver{}
			s := &createStream{}
			if err := d.Create(&pluginv1.CreateRequest{
				Count:       1,
				Profile:     mustStruct(t, map[string]any{"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium"}),
				Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
			}, s); err != nil {
				t.Fatalf("Create: %v", err)
			}
			last := s.last()
			if !last.Failed {
				t.Fatalf("terminal=%s: expected failed=true, got %+v", tc.state, last)
			}
			// WaitUntilReady вернул nil (per-VM Err внутри results), поэтому
			// сообщение — обычное "completed" с failed=true, не FailMessage.
			// vm_id ОБЯЗАН быть в финальном событии (anti-orphan).
			if len(last.Vms) != 1 || last.Vms[0].VmId != "i-term" {
				t.Errorf("terminal=%s: final vms=%+v, want vm_id=i-term", tc.state, last.Vms)
			}
			// wr.Ready=false → finalizeCreate НЕ заполняет Fqdn (probe failed).
			if last.Vms[0].Fqdn != "" {
				t.Errorf("terminal=%s: Fqdn=%q must be empty (probe failed)", tc.state, last.Vms[0].Fqdn)
			}
		})
	}
}

// TestCreate_TransientProbeError_SwallowAndRetry — describeInstances между
// раундами возвращает классифицируемую как Transient() ошибку → probe-обёртка
// глотает её (ProbeResult{}) → следующий round успешен (running + IP).
// Покрывает контракт «transient error в probe — поллер повторяет, не плодит
// failed-event».
func TestCreate_TransientProbeError_SwallowAndRetry(t *testing.T) {
	withFastBackoff(t, 8)
	f := &fakeEC2{
		runOut: &ec2.RunInstancesOutput{Instances: []ec2types.Instance{{InstanceId: aws.String("i-trans")}}},
	}
	// call 0 — pending (первый probe round);
	// call 1 — Throttling (transient, проглатывается);
	// call 2 — running + IP/DNS → Ready.
	f.describeFn = func(call int) (*ec2.DescribeInstancesOutput, error) {
		switch call {
		case 0:
			return describeOut(ec2types.Instance{
				InstanceId: aws.String("i-trans"),
				State:      &ec2types.InstanceState{Name: ec2types.InstanceStateNamePending},
			}), nil
		case 1:
			return nil, &smithy.GenericAPIError{Code: "Throttling", Message: "slow down"}
		default:
			return describeOut(runningInstance("i-trans", "10.5.5.5", "ip-10-5-5-5.internal")), nil
		}
	}
	withFakeEC2(t, f)

	d := &AwsDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count:       1,
		Profile:     mustStruct(t, map[string]any{"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium"}),
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("transient probe-error must be swallowed; got failed: %+v", last)
	}
	if len(last.Vms) != 1 || last.Vms[0].VmId != "i-trans" || last.Vms[0].Fqdn == "" {
		t.Errorf("vms after transient retry = %+v", last.Vms)
	}
}

// TestCreate_Idempotent_OverCount — findByRunTag вернул БОЛЬШЕ VM, чем
// запрошенный count. Драйвер обязан вернуть все найденные (не падать, не плодить
// дубли). Защита от «после crash-а Keeper-а провижининг создал лишние VM, при
// retry-е счётчик count меньше реального инвентаря».
func TestCreate_Idempotent_OverCount(t *testing.T) {
	withFastBackoff(t, 2)
	// findByRunTag возвращает 3 живые VM (count=2 → over-count на 1);
	// далее probe для каждой возвращает running с IP.
	existing := []ec2types.Instance{
		runningInstance("i-old-1", "10.1.0.1", "ip-10-1-0-1.internal"),
		runningInstance("i-old-2", "10.1.0.2", "ip-10-1-0-2.internal"),
		runningInstance("i-old-3", "10.1.0.3", "ip-10-1-0-3.internal"),
	}
	f := &fakeEC2{
		describeSeq: []*ec2.DescribeInstancesOutput{
			// первый Describe = findByRunTag → 3 VM;
			// далее describeOne по каждой → их статус running (последний out
			// «залипает», см. fakeEC2.DescribeInstances).
			{Reservations: []ec2types.Reservation{{Instances: existing}}},
			describeOut(existing[0]),
			describeOut(existing[1]),
			describeOut(existing[2]),
		},
	}
	withFakeEC2(t, f)

	d := &AwsDriver{}
	s := &createStream{}
	if err := d.Create(&pluginv1.CreateRequest{
		Count: 2, // меньше, чем реальное число существующих VM
		Profile: mustStruct(t, map[string]any{
			"region": "eu-west-1", "ami": "ami-0abc1234", "instance_type": "t3.medium",
			"tags": map[string]any{runTagKey: "run-over"},
		}),
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
	}, s); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.runCall != 0 {
		t.Errorf("RunInstances called %d times; over-count idempotent path must NOT launch new VM", f.runCall)
	}
	last := s.last()
	if last.Failed {
		t.Fatalf("over-count idempotent: final=%+v, want success", last)
	}
	if len(last.Vms) != 3 {
		t.Errorf("vms=%d, want 3 (all existing returned, not truncated to count)", len(last.Vms))
	}
}

// TestStatus_UsesCredentials — Status с credentials в новом StatusRequest-поле
// успешно опрашивает VM (не возвращает «requires credentials»-ошибку workaround-
// версии). Покрывает only-add StatusRequest.credentials.
func TestStatus_UsesCredentials(t *testing.T) {
	inst := runningInstance("i-stat", "10.6.6.6", "ip-10-6-6-6.internal")
	f := &fakeEC2{
		describeSeq: []*ec2.DescribeInstancesOutput{describeOut(inst)},
	}
	withFakeEC2(t, f)

	d := &AwsDriver{}
	rep, err := d.Status(context.Background(), &pluginv1.StatusRequest{
		VmId:        "i-stat",
		Credentials: mustStruct(t, map[string]any{"access_key_id": "AKIA", "secret_access_key": "x", "region": "eu-west-1"}),
	})
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if rep.State != string(ec2types.InstanceStateNameRunning) {
		t.Errorf("state=%q, want running", rep.State)
	}
	if rep.Attributes == nil {
		t.Error("attributes must be populated")
	}
}

// TestList_UsesCredentialsField — List с credentials в новом ListRequest-поле
// (не workaround «creds внутри filter»). Покрывает миграцию filter-Struct →
// credentials-Struct.
func TestList_UsesCredentialsField(t *testing.T) {
	f := &fakeEC2{
		describeSeq: []*ec2.DescribeInstancesOutput{
			describeOut(
				runningInstance("i-l-1", "10.7.7.1", "ip-10-7-7-1.internal"),
				runningInstance("i-l-2", "10.7.7.2", "ip-10-7-7-2.internal"),
			),
		},
	}
	withFakeEC2(t, f)

	d := &AwsDriver{}
	s := &listStream{ctx: context.Background()}
	if err := d.List(&pluginv1.ListRequest{
		Filter:      mustStruct(t, map[string]any{runTagKey: "run-list"}),
		Credentials: mustStruct(t, map[string]any{"access_key_id": "AKIA", "secret_access_key": "x", "region": "eu-west-1"}),
	}, s); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(s.sent) != 2 {
		t.Errorf("list events=%d, want 2", len(s.sent))
	}
}

func TestDestroy_PerVM(t *testing.T) {
	f := &fakeEC2{termOut: &ec2.TerminateInstancesOutput{
		TerminatingInstances: []ec2types.InstanceStateChange{
			{InstanceId: aws.String("i-1"), CurrentState: &ec2types.InstanceState{Name: ec2types.InstanceStateNameShuttingDown}},
			{InstanceId: aws.String("i-2"), CurrentState: &ec2types.InstanceState{Name: ec2types.InstanceStateNameShuttingDown}},
		},
	}}
	withFakeEC2(t, f)
	d := &AwsDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"i-1", "i-2"},
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
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
	f := &fakeEC2{termErr: &smithy.GenericAPIError{Code: "InvalidInstanceID.NotFound", Message: "gone"}}
	withFakeEC2(t, f)
	d := &AwsDriver{}
	s := &destroyStream{}
	if err := d.Destroy(&pluginv1.DestroyRequest{
		VmIds:       []string{"i-gone"},
		Credentials: mustStruct(t, map[string]any{"region": "eu-west-1"}),
	}, s); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(s.sent) != 1 || s.sent[0].Failed {
		t.Errorf("not-found destroy must be idempotent (success), got %+v", s.sent)
	}
}

func TestClassifyAWS_Codes(t *testing.T) {
	cases := map[string]clouddriver.FailClass{
		"AuthFailure":              clouddriver.FailAuth,
		"UnauthorizedOperation":    clouddriver.FailAuth,
		"InstanceLimitExceeded":    clouddriver.FailQuota,
		"InvalidAMIID.NotFound":    clouddriver.FailNotFound,
		"InvalidSubnetID.NotFound": clouddriver.FailNotFound,
		"InvalidParameterValue":    clouddriver.FailInvalidParams,
		"Throttling":               clouddriver.FailTransient,
		"RequestLimitExceeded":     clouddriver.FailTransient,
	}
	for code, want := range cases {
		got := classifyAWS(&smithy.GenericAPIError{Code: code})
		if got != want {
			t.Errorf("classifyAWS(%q)=%v, want %v", code, got, want)
		}
	}
	// не-API ошибка → transient
	if got := classifyAWS(errors.New("dial tcp: timeout")); got != clouddriver.FailTransient {
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
