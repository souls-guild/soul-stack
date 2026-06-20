// soul-cloud-azure — реальный CloudDriver-плагин Soul Stack для Azure VM
// (ADR-016 Фаза 4 cloud parity; тираж по reference-паттерну soul-cloud-aws).
//
// Собирается в статический бинарь `soul-cloud-azure`. Keeper-side модуль
// `core.cloud.provisioned` (ADR-017) запускает его как sub-process, делает
// gRPC-stdio handshake (sdk/handshake) и зовёт RPC CloudDriver.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper резолвит секрет
// service-principal-а из Vault и кладёт plain в CreateRequest.credentials /
// DestroyRequest.credentials (tenant_id/client_id/client_secret +
// subscription_id/resource_group/location); драйвер в Vault НЕ ходит.
// Cloud-init userdata для bootstrap soul-агента приходит в CreateRequest.userdata
// и кодируется драйвером в base64 (Azure-требование для osProfile.customData).
//
// Shared-каркас (error-таксономия / wait-until-ready / retry-backoff) — из
// sdk/clouddriver, общий для всех драйверов тиража. Provider-specific здесь —
// только вызовы Azure ARM-API и Azure-классификатор ошибок (classify.go).
//
// # Multi-resource транзакция Create
//
// Azure VM в отличие от EC2 — это композит: PublicIP + NIC + VM создаются
// тремя отдельными ARM-операциями. Драйвер делает их последовательно (NIC
// требует id-ссылку на готовый PublicIP, VM — на готовый NIC) и при фейле
// шага N откатывает шаги 1..N-1 в обратном порядке (best-effort).
//
// Композитный идентификатор: primary `vm_id` = VM-имя
// (`<runTag>-vm-<idx>` или `soul-vm-<short-rand>` без runTag). NIC/PIP имена —
// детерминированные производные (`<vm_name>-nic` / `<vm_name>-pip`) — это даёт
// Keeper-у возможность по одному `vm_id` восстановить все три ресурса и
// корректно Destroy их в обратном порядке, без хранения отдельного mapping-а.
package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	armcompute "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v5"
	armnetwork "github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON — profile_schema (JSON Schema draft 2020-12), embedded
// рядом с бинарём. Симметрично soul-cloud-aws.
//
//go:embed schema.json
var profileSchemaJSON []byte

// runTagKey — тег идемпотентности: значение = идентификатор прогона/incarnation.
// Имя тега в Azure следует snake_case-нотации; двоеточия в ключах тегов
// допустимы, но дефис безопаснее для CLI-фильтров и совместим со всеми
// провайдерами тиража (тот же тег у soul-cloud-aws).
const runTagKey = "soulstack-run"

// defaultBackoff — фабрика [clouddriver.BackoffConfig] для wait/retry-фаз
// (см. soul-cloud-aws). L0-тесты подменяют через withFastBackoff.
var defaultBackoff = clouddriver.DefaultBackoff

// randomSuffix — короткий случайный суффикс для VM-имени, если runTag не
// задан. Вынесен в переменную для детерминированных L0-тестов.
var randomSuffix = func() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AzureDriver — реализация CloudDriver для Azure.
type AzureDriver struct {
	clouddriver.BaseDriver
}

// Schema публикует embedded profile_schema (симметрично soul-cloud-aws).
func (a *AzureDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
	var raw map[string]any
	if err := json.Unmarshal(profileSchemaJSON, &raw); err != nil {
		return nil, fmt.Errorf("parse embedded schema.json: %w", err)
	}
	s, err := structpb.NewStruct(raw)
	if err != nil {
		return nil, fmt.Errorf("encode profile_schema: %w", err)
	}
	return &pluginv1.SchemaReply{ProfileSchema: s}, nil
}

// Validate — structural-проверки на стороне драйвера. Полная JSON Schema
// валидация делается Keeper-ом по publish-нутому Schema; здесь покрываем
// required-поля как защиту-в-глубину (Keeper не обязан её делать на каждый
// Create).
func (a *AzureDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "location") == "" {
		errs = append(errs, "profile.location is required")
	}
	if stringField(p, "vm_size") == "" {
		errs = append(errs, "profile.vm_size is required")
	}
	if stringField(p, "subnet_id") == "" {
		errs = append(errs, "profile.subnet_id is required")
	}
	img, _ := p["image"].(map[string]any)
	if stringField(img, "publisher") == "" || stringField(img, "offer") == "" || stringField(img, "sku") == "" {
		errs = append(errs, "profile.image.{publisher,offer,sku} are required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile — параметры профиля, разобранные для Create.
type vmProfile struct {
	location               string
	vmSize                 string
	imagePublisher         string
	imageOffer             string
	imageSKU               string
	imageVersion           string
	subnetID               string
	adminUsername          string
	sshPublicKey           string
	publicIPEnabled        bool
	publicIPSku            string
	publicIPDNSLabel       string
	diskSizeGB             int32
	diskType               string
	networkSecurityGroupID string
	tags                   map[string]string
	runTag                 string
}

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		location:      stringField(p, "location"),
		vmSize:        stringField(p, "vm_size"),
		subnetID:      stringField(p, "subnet_id"),
		adminUsername: stringField(p, "admin_username"),
		sshPublicKey:  stringField(p, "ssh_public_key"),
		// public_ip по умолчанию true — Azure-VM без публичного IP бесполезна для
		// soul-bootstrap-а (Keeper не сможет дозвониться); оператор явно выключает.
		publicIPEnabled: true,
		publicIPSku:     "Standard",
		tags:            map[string]string{},
	}
	if prof.adminUsername == "" {
		prof.adminUsername = "soul"
	}
	if img, ok := p["image"].(map[string]any); ok {
		prof.imagePublisher = stringField(img, "publisher")
		prof.imageOffer = stringField(img, "offer")
		prof.imageSKU = stringField(img, "sku")
		prof.imageVersion = stringField(img, "version")
		if prof.imageVersion == "" {
			prof.imageVersion = "latest"
		}
	}
	if pip, ok := p["public_ip"].(map[string]any); ok {
		if e, ok := pip["enabled"].(bool); ok {
			prof.publicIPEnabled = e
		}
		if s := stringField(pip, "sku"); s != "" {
			prof.publicIPSku = s
		}
		prof.publicIPDNSLabel = stringField(pip, "dns_label")
	}
	if disk, ok := p["disk"].(map[string]any); ok {
		if sz, ok := disk["size_gb"].(float64); ok {
			prof.diskSizeGB = int32(sz)
		}
		prof.diskType = stringField(disk, "type")
	}
	prof.networkSecurityGroupID = stringField(p, "network_security_group_id")
	if tags, ok := p["tags"].(map[string]any); ok {
		for k, v := range tags {
			if s, ok := v.(string); ok {
				prof.tags[k] = s
			}
		}
	}
	prof.runTag = prof.tags[runTagKey]
	return prof
}

// resourceNames — детерминированные имена тройки ресурсов VM/NIC/PIP по
// `vmName`. Один primary `vm_id` = VM-имя достаточно для Destroy: NIC/PIP-имена
// восстанавливаются по тем же правилам без хранения mapping-а.
type resourceNames struct {
	vm  string
	nic string
	pip string
}

func makeResourceNames(vmName string) resourceNames {
	return resourceNames{vm: vmName, nic: vmName + "-nic", pip: vmName + "-pip"}
}

// makeVMName — детерминированное имя VM:
//   - с runTag:  "<runTag>-vm-<idx>" (idx — 0-based порядковый внутри прогона);
//   - без runTag: "soul-vm-<3-byte-hex>" (через [randomSuffix], подменяется тестом).
func makeVMName(runTag string, idx int) string {
	if runTag != "" {
		return fmt.Sprintf("%s-vm-%d", runTag, idx)
	}
	return "soul-vm-" + randomSuffix()
}

// Create: multi-resource транзакция PIP → NIC → VM с rollback при фейле.
// См. doc-комментарий пакета.
func (a *AzureDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	if creds.Location == "" {
		creds.Location = prof.location
	}

	cli, err := newAzureClients(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "azure-client", err)
	}

	backoff := defaultBackoff()

	// Идемпотентность: переиспользуем уже созданные VM с тем же runTag.
	var existing []*armcompute.VirtualMachine
	if prof.runTag != "" {
		existing, err = a.findByRunTag(ctx, cli, backoff, creds.ResourceGroup, prof.runTag)
		if err != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyAzure, err), "list-existing", err)
		}
		if int32(len(existing)) >= count {
			_ = stream.Send(&pluginv1.CreateEvent{
				Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runTag),
			})
			return a.finalizeCreate(ctx, cli, stream, backoff, creds, existingVMIDs(existing))
		}
		count -= int32(len(existing))
	}

	// Создаём `count` новых VM, каждую — multi-resource транзакцией.
	newIDs := make([]string, 0, count)
	startIdx := len(existing)
	for i := int32(0); i < count; i++ {
		name := makeVMName(prof.runTag, startIdx+int(i))
		if err := stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("azure.Create vm=%s size=%s image=%s/%s/%s",
				name, prof.vmSize, prof.imagePublisher, prof.imageOffer, prof.imageSKU),
		}); err != nil {
			return err
		}
		if err := a.createOneVM(ctx, cli, backoff, creds, prof, name, req.GetUserdata(), stream); err != nil {
			// createOneVM сам сделал rollback и отправил частичные events;
			// anti-orphan: имя уже созданной (но потом откатанной) VM в финальный
			// vm-список НЕ попадает (rollback успешен → ресурсов нет).
			//
			// Исключение: rollback частично провалился → имя пойдёт в final event
			// как failed (см. createOneVM).
			final := append(existingVMIDs(existing), newIDs...)
			return sendFinalRollbackFail(stream, clouddriver.Classify(classifyAzure, err), "create-vm", err, final)
		}
		newIDs = append(newIDs, name)
	}

	all := append(existingVMIDs(existing), newIDs...)
	return a.finalizeCreate(ctx, cli, stream, backoff, creds, all)
}

// createOneVM — multi-resource транзакция одной VM с rollback при фейле.
//
// Порядок: PIP → NIC → VM. Rollback идёт в обратном порядке (VM → NIC → PIP)
// и пропускает ресурсы, которые ещё не создались. Каждый шаг обёрнут в Retry
// (transient API-сбои не валят весь create — это критично для Azure, чья
// throttling-частота выше AWS).
func (a *AzureDriver) createOneVM(
	ctx context.Context, cli azureClients, backoff clouddriver.BackoffConfig,
	creds azureCredentials, prof vmProfile, vmName, userdata string,
	stream grpc.ServerStreamingServer[pluginv1.CreateEvent],
) error {
	names := makeResourceNames(vmName)

	// Список созданных ресурсов для rollback (last-in-first-out).
	var created []resourceRef
	rollback := func(opErr error) {
		_ = stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("rollback after %v: deleting %d resource(s)", opErr, len(created)),
		})
		// Обратный порядок: VM → NIC → PIP. ctx может быть уже отменён —
		// rollback использует Background для best-effort cleanup (anti-orphan),
		// иначе orphan-ресурсы останутся висеть в подписке.
		rbCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for i := len(created) - 1; i >= 0; i-- {
			r := created[i]
			if delErr := r.delete(rbCtx, cli, creds.ResourceGroup); delErr != nil {
				// Транзиентную ошибку rollback ретраим один-два раза (быстрый
				// backoff); упорная — пишем в event и идём дальше.
				_ = stream.Send(&pluginv1.CreateEvent{
					Message: fmt.Sprintf("rollback warn: delete %s %q: %v", r.kind, r.name, delErr),
				})
			}
		}
	}

	tagsForResource := mergeTags(prof.tags)

	// --- 1. PublicIP (опционально) ---
	var pipID string
	if prof.publicIPEnabled {
		pipParams := buildPublicIP(prof, tagsForResource)
		if err := clouddriver.Retry(ctx, backoff, classifyAzure, func() error {
			pip, perr := cli.pips.CreateAndWait(ctx, creds.ResourceGroup, names.pip, pipParams)
			if perr != nil {
				return perr
			}
			if pip.ID != nil {
				pipID = *pip.ID
			}
			return nil
		}); err != nil {
			// Rollback пуст — нечего откатывать.
			return fmt.Errorf("publicip create %s: %w", names.pip, err)
		}
		created = append(created, resourceRef{kind: "publicip", name: names.pip})
	}

	// --- 2. NIC ---
	var nicID string
	nicParams := buildNIC(prof, pipID, tagsForResource)
	if err := clouddriver.Retry(ctx, backoff, classifyAzure, func() error {
		nic, nerr := cli.nics.CreateAndWait(ctx, creds.ResourceGroup, names.nic, nicParams)
		if nerr != nil {
			return nerr
		}
		if nic.ID != nil {
			nicID = *nic.ID
		}
		return nil
	}); err != nil {
		rollback(err)
		return fmt.Errorf("nic create %s: %w", names.nic, err)
	}
	created = append(created, resourceRef{kind: "nic", name: names.nic})

	// --- 3. VM ---
	vmParams := buildVM(prof, nicID, userdata, tagsForResource)
	if err := clouddriver.Retry(ctx, backoff, classifyAzure, func() error {
		_, verr := cli.vms.CreateAndWait(ctx, creds.ResourceGroup, names.vm, vmParams)
		return verr
	}); err != nil {
		rollback(err)
		return fmt.Errorf("vm create %s: %w", names.vm, err)
	}
	// VM в `created` НЕ добавляем — успешно созданная VM не подлежит rollback
	// (она и есть конечная цель). Если упадёт wait-until-ready дальше, это
	// уже не rollback, а anti-orphan (см. finalizeCreate).
	return nil
}

// finalizeCreate ждёт готовности VM (ProvisioningState=Succeeded + powerState=
// running) и шлёт финальное событие.
func (a *AzureDriver) finalizeCreate(
	ctx context.Context, cli azureClients,
	stream grpc.ServerStreamingServer[pluginv1.CreateEvent],
	backoff clouddriver.BackoffConfig, creds azureCredentials, vmIDs []string,
) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		ready, perr := a.probeVMReady(pctx, cli, creds.ResourceGroup, vmID)
		if perr != nil {
			if clouddriver.Classify(classifyAzure, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		return clouddriver.ProbeResult{Ready: ready}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmIDs, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			a.fillVMInfo(ctx, cli, creds, wr.VMID, vi)
		}
		if !wr.Ready {
			anyFailed = true
		}
		vms = append(vms, vi)
	}

	if waitErr != nil {
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyAzure, waitErr), "wait-until-ready", waitErr),
			Vms:     vms,
			Failed:  true,
		})
	}
	return stream.Send(&pluginv1.CreateEvent{
		Message: "completed",
		Vms:     vms,
		Failed:  anyFailed,
	})
}

// probeVMReady — true, если VM в ProvisioningState=Succeeded И InstanceView
// содержит PowerState/running. Через один Get с Expand=InstanceView, чтобы не
// дёргать API дважды.
func (a *AzureDriver) probeVMReady(ctx context.Context, cli azureClients, rg, vmName string) (bool, error) {
	resp, err := cli.vms.Get(ctx, rg, vmName, &armcompute.VirtualMachinesClientGetOptions{
		Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView),
	})
	if err != nil {
		return false, err
	}
	if resp.Properties == nil {
		return false, nil
	}
	if resp.Properties.ProvisioningState == nil || *resp.Properties.ProvisioningState != "Succeeded" {
		// Failed-ProvisioningState — terminal, не транзиент: пробрасываем как error,
		// поллер закроет эту VM.
		if resp.Properties.ProvisioningState != nil && *resp.Properties.ProvisioningState == "Failed" {
			return false, fmt.Errorf("vm %s provisioning failed", vmName)
		}
		return false, nil
	}
	iv := resp.Properties.InstanceView
	if iv == nil {
		return false, nil
	}
	for _, st := range iv.Statuses {
		if st == nil || st.Code == nil {
			continue
		}
		// PowerState статус — формат "PowerState/<state>".
		if *st.Code == "PowerState/running" {
			return true, nil
		}
		if *st.Code == "PowerState/deallocated" || *st.Code == "PowerState/stopped" {
			return false, fmt.Errorf("vm %s entered terminal power state %q", vmName, *st.Code)
		}
	}
	return false, nil
}

// fillVMInfo дозаполняет VmInfo (fqdn/primary_ip/attributes) по готовой VM.
func (a *AzureDriver) fillVMInfo(ctx context.Context, cli azureClients, creds azureCredentials, vmName string, vi *pluginv1.VmInfo) {
	names := makeResourceNames(vmName)
	// VM нужна только для attributes (vm_size/state) — без InstanceView.
	vmResp, err := cli.vms.Get(ctx, creds.ResourceGroup, vmName, nil)
	if err == nil {
		vi.Attributes = vmAttributes(vmResp.VirtualMachine, creds.Location)
	}
	// Primary IP / FQDN — из PublicIP, если есть.
	if pip, err := cli.pips.Get(ctx, creds.ResourceGroup, names.pip); err == nil {
		if pip.Properties != nil {
			if pip.Properties.IPAddress != nil {
				vi.PrimaryIp = *pip.Properties.IPAddress
			}
			if pip.Properties.DNSSettings != nil && pip.Properties.DNSSettings.Fqdn != nil {
				vi.Fqdn = *pip.Properties.DNSSettings.Fqdn
			}
		}
	}
	// Без PublicIP / без DNS-label: fallback FQDN = VM-имя (internal-name,
	// уникальный в пределах resource-group). Это `SID` для bootstrap-а.
	if vi.Fqdn == "" {
		vi.Fqdn = vmName
	}
}

// findByRunTag перечисляет VM с заданным runTag в resource-group.
func (a *AzureDriver) findByRunTag(ctx context.Context, cli azureClients, backoff clouddriver.BackoffConfig, rg, runTag string) ([]*armcompute.VirtualMachine, error) {
	var out []*armcompute.VirtualMachine
	err := clouddriver.Retry(ctx, backoff, classifyAzure, func() error {
		v, rerr := cli.vms.ListByRunTag(ctx, rg, runTag)
		if rerr != nil {
			return rerr
		}
		out = v
		return nil
	})
	return out, err
}

// Destroy: VM → NIC → PIP (обратный порядок Create). Per-vm события.
func (a *AzureDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newAzureClients(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "azure-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, vmName := range req.GetVmIds() {
		names := makeResourceNames(vmName)
		allMissing, stepErr := a.destroyOne(ctx, cli, backoff, creds.ResourceGroup, names)
		if stepErr != nil {
			class := clouddriver.Classify(classifyAzure, stepErr)
			_ = stream.Send(&pluginv1.DestroyEvent{
				VmId:    vmName,
				Message: clouddriver.FailMessage(class, "destroy", stepErr),
				Failed:  true,
			})
			continue
		}
		// Все три шага вернули not-found ⇒ ресурсов уже нет ⇒ идемпотентно.
		// Иначе хотя бы один реально удалили ⇒ "terminated".
		msg := "terminated"
		if allMissing {
			msg = "already absent"
		}
		_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmName, Message: msg})
	}
	return nil
}

// destroyOne — VM → NIC → PIP, каждый через Retry. not-found на любом шаге —
// продолжаем (идемпотентность); возвращает allMissing=true только если ВСЕ три
// шага оказались not-found (для корректной "already absent"-семантики).
func (a *AzureDriver) destroyOne(ctx context.Context, cli azureClients, backoff clouddriver.BackoffConfig, rg string, names resourceNames) (bool, error) {
	steps := []struct {
		kind   string
		delete func() error
	}{
		{"vm", func() error { return cli.vms.DeleteAndWait(ctx, rg, names.vm) }},
		{"nic", func() error { return cli.nics.DeleteAndWait(ctx, rg, names.nic) }},
		{"pip", func() error { return cli.pips.DeleteAndWait(ctx, rg, names.pip) }},
	}
	allMissing := true
	for _, s := range steps {
		err := clouddriver.Retry(ctx, backoff, classifyAzure, s.delete)
		if err == nil {
			allMissing = false
			continue
		}
		if clouddriver.Classify(classifyAzure, err) == clouddriver.FailNotFound {
			continue
		}
		return false, fmt.Errorf("%s %s: %w", s.kind, names.vm, err)
	}
	return allMissing, nil
}

// Status — опрос одной VM с Expand=InstanceView (state + powerState).
func (a *AzureDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newAzureClients(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("azure-client: %w", err)
	}
	resp, err := cli.vms.Get(ctx, creds.ResourceGroup, req.GetVmId(),
		&armcompute.VirtualMachinesClientGetOptions{Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView)})
	if err != nil {
		return nil, fmt.Errorf("vms.Get %s: %w", req.GetVmId(), err)
	}
	state := derivePowerState(resp.VirtualMachine)
	return &pluginv1.StatusReply{
		State:      state,
		Attributes: vmAttributes(resp.VirtualMachine, creds.Location),
	}, nil
}

// List — стрим инвентаря VM (опц. фильтр по runTag).
func (a *AzureDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newAzureClients(ctx, creds)
	if err != nil {
		return fmt.Errorf("azure-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	runTag := stringField(filter, runTagKey)
	if runTag == "" {
		// Без фильтра — пустой список (защита от случайного «вытащи всю
		// подписку»). Симметрично AWS-варианту, где filter обязателен в
		// сценариях.
		return nil
	}
	vms, err := cli.vms.ListByRunTag(ctx, creds.ResourceGroup, runTag)
	if err != nil {
		return fmt.Errorf("vms.List: %w", err)
	}
	for _, vm := range vms {
		if vm == nil || vm.Name == nil {
			continue
		}
		vi := &pluginv1.VmInfo{VmId: *vm.Name}
		a.fillVMInfo(ctx, cli, creds, *vm.Name, vi)
		if serr := stream.Send(vi); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&AzureDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-azure:", err)
		os.Exit(1)
	}
}

// --- ARM-параметры (builders) ---

func buildPublicIP(prof vmProfile, tags map[string]*string) armnetwork.PublicIPAddress {
	pip := armnetwork.PublicIPAddress{
		Location: &prof.location,
		SKU:      &armnetwork.PublicIPAddressSKU{Name: skuName(prof.publicIPSku)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
		Tags: tags,
	}
	if prof.publicIPDNSLabel != "" {
		pip.Properties.DNSSettings = &armnetwork.PublicIPAddressDNSSettings{
			DomainNameLabel: &prof.publicIPDNSLabel,
		}
	}
	return pip
}

func skuName(s string) *armnetwork.PublicIPAddressSKUName {
	switch s {
	case "Basic":
		return to.Ptr(armnetwork.PublicIPAddressSKUNameBasic)
	default:
		return to.Ptr(armnetwork.PublicIPAddressSKUNameStandard)
	}
}

func buildNIC(prof vmProfile, pipID string, tags map[string]*string) armnetwork.Interface {
	ipCfg := &armnetwork.InterfaceIPConfiguration{
		Name: to.Ptr("ipconfig1"),
		Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
			PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
			Subnet:                    &armnetwork.Subnet{ID: &prof.subnetID},
		},
	}
	if pipID != "" {
		ipCfg.Properties.PublicIPAddress = &armnetwork.PublicIPAddress{ID: &pipID}
	}
	nic := armnetwork.Interface{
		Location: &prof.location,
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{ipCfg},
		},
		Tags: tags,
	}
	if prof.networkSecurityGroupID != "" {
		nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{ID: &prof.networkSecurityGroupID}
	}
	return nic
}

func buildVM(prof vmProfile, nicID, userdata string, tags map[string]*string) armcompute.VirtualMachine {
	vm := armcompute.VirtualMachine{
		Location: &prof.location,
		Tags:     tags,
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(prof.vmSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: &prof.imagePublisher,
					Offer:     &prof.imageOffer,
					SKU:       &prof.imageSKU,
					Version:   &prof.imageVersion,
				},
				OSDisk: buildOSDisk(prof),
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  to.Ptr(prof.adminUsername + "-vm"), // hostname внутри VM
				AdminUsername: &prof.adminUsername,
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
					ID: &nicID,
					Properties: &armcompute.NetworkInterfaceReferenceProperties{
						Primary: to.Ptr(true),
					},
				}},
			},
		},
	}
	// SSH-публичный ключ → Linux profile. Без него Azure требует password,
	// что нам не подходит (cloud-init runs SSH-only).
	if prof.sshPublicKey != "" {
		vm.Properties.OSProfile.LinuxConfiguration = &armcompute.LinuxConfiguration{
			DisablePasswordAuthentication: to.Ptr(true),
			SSH: &armcompute.SSHConfiguration{
				PublicKeys: []*armcompute.SSHPublicKey{{
					Path:    to.Ptr("/home/" + prof.adminUsername + "/.ssh/authorized_keys"),
					KeyData: &prof.sshPublicKey,
				}},
			},
		}
	}
	// cloud-init userdata → customData (base64, Azure-требование).
	if userdata != "" {
		vm.Properties.OSProfile.CustomData = to.Ptr(base64.StdEncoding.EncodeToString([]byte(userdata)))
	}
	return vm
}

func buildOSDisk(prof vmProfile) *armcompute.OSDisk {
	od := &armcompute.OSDisk{
		CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
	}
	if prof.diskSizeGB > 0 {
		od.DiskSizeGB = &prof.diskSizeGB
	}
	if prof.diskType != "" {
		od.ManagedDisk = &armcompute.ManagedDiskParameters{
			StorageAccountType: to.Ptr(armcompute.StorageAccountTypes(prof.diskType)),
		}
	}
	return od
}

// derivePowerState вытаскивает power-state из InstanceView, fallback на
// ProvisioningState. Возвращает строку для StatusReply.State.
func derivePowerState(vm armcompute.VirtualMachine) string {
	if vm.Properties != nil && vm.Properties.InstanceView != nil {
		for _, st := range vm.Properties.InstanceView.Statuses {
			if st == nil || st.Code == nil {
				continue
			}
			if len(*st.Code) > len("PowerState/") && (*st.Code)[:len("PowerState/")] == "PowerState/" {
				return (*st.Code)[len("PowerState/"):]
			}
		}
	}
	if vm.Properties != nil && vm.Properties.ProvisioningState != nil {
		return *vm.Properties.ProvisioningState
	}
	return ""
}

// vmAttributes — снапшот VM-полей для StatusReply.Attributes / VmInfo.Attributes.
func vmAttributes(vm armcompute.VirtualMachine, locationFallback string) *structpb.Struct {
	m := map[string]any{}
	if vm.Properties != nil && vm.Properties.HardwareProfile != nil && vm.Properties.HardwareProfile.VMSize != nil {
		m["vm_size"] = string(*vm.Properties.HardwareProfile.VMSize)
	}
	loc := locationFallback
	if vm.Location != nil && *vm.Location != "" {
		loc = *vm.Location
	}
	if loc != "" {
		m["location"] = loc
	}
	if vm.Properties != nil && vm.Properties.ProvisioningState != nil {
		m["provisioning_state"] = *vm.Properties.ProvisioningState
	}
	if vm.Properties != nil && vm.Properties.TimeCreated != nil {
		m["created_at"] = vm.Properties.TimeCreated.UTC().Format(time.RFC3339)
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

// sendFinalRollbackFail — финальное событие с уже-созданными vm_id (anti-orphan)
// и failed=true. После rollback ресурсов конкретной упавшей VM нет, но
// остальные `successful` VM (если они уже успели создаться) должны попасть в
// final event, чтобы Keeper мог их Destroy при необходимости.
func sendFinalRollbackFail(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error, vmIDs []string) error {
	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	for _, id := range vmIDs {
		vms = append(vms, &pluginv1.VmInfo{VmId: id})
	}
	return stream.Send(&pluginv1.CreateEvent{
		Message: clouddriver.FailMessage(class, op, err),
		Vms:     vms,
		Failed:  true,
	})
}

func mergeTags(in map[string]string) map[string]*string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*string, len(in))
	for k, v := range in {
		v := v
		out[k] = &v
	}
	return out
}

func existingVMIDs(vms []*armcompute.VirtualMachine) []string {
	out := make([]string, 0, len(vms))
	for _, v := range vms {
		if v != nil && v.Name != nil {
			out = append(out, *v.Name)
		}
	}
	return out
}

// resourceRef — запись об уже-созданном ресурсе для rollback-цепочки.
type resourceRef struct {
	kind string // "publicip" | "nic" | "vm"
	name string
}

func (r resourceRef) delete(ctx context.Context, cli azureClients, rg string) error {
	switch r.kind {
	case "publicip":
		return cli.pips.DeleteAndWait(ctx, rg, r.name)
	case "nic":
		return cli.nics.DeleteAndWait(ctx, rg, r.name)
	case "vm":
		return cli.vms.DeleteAndWait(ctx, rg, r.name)
	}
	return errors.New("unknown resource kind: " + r.kind)
}
