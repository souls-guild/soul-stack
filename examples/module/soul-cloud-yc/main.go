// soul-cloud-yc — реальный CloudDriver-плагин Soul Stack для Yandex Cloud
// (ADR-016 Фаза 4 cloud parity; тираж по pattern-у soul-cloud-aws).
//
// Собирается в статический бинарь `soul-cloud-yc`. Keeper-side модуль
// `core.cloud.provisioned` (ADR-017) запускает его как sub-process, делает
// gRPC-stdio handshake (sdk/handshake) и зовёт RPC CloudDriver.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper резолвит секрет из Vault и
// кладёт plain в CreateRequest.credentials / DestroyRequest.credentials; драйвер
// в Vault НЕ ходит. Yandex поддерживает три формы XOR: iam_token / oauth_token
// / service_account_key (JSON-blob). folder_id/zone приходят рядом, симметрично
// awsCredentials.Region. Cloud-init userdata для bootstrap soul-агента
// прокидывается в metadata["user-data"] (YC-конвенция).
//
// Shared-каркас (error-таксономия / wait-until-ready / retry-backoff) — из
// sdk/clouddriver, общий для всех драйверов тиража. Provider-specific здесь —
// только вызовы compute/v1 API и YC-классификатор ошибок (classify.go).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"time"

	computev1 "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON — profile_schema (JSON Schema draft 2020-12), embedded
// рядом с бинарём. Та же техника, что в soul-cloud-aws — отдельный файл,
// не хардкод в Go (легче держать в синхроне с manifest.spec.profile_schema).
//
//go:embed schema.json
var profileSchemaJSON []byte

// runLabelKey — label идемпотентности: значение = идентификатор прогона/
// incarnation (из profile.labels). YC labels поддерживают точечные ключи без
// специальных символов; имя ключа — kebab-case под YC-правила
// (`[a-z][-_./\\@0-9a-z]*`). Повторный Create по тому же label не плодит
// дубли — существующие живые VM (PROVISIONING/STARTING/RUNNING) переиспользуются.
const runLabelKey = "soulstack-run"

// userdataMetaKey — стандартный ключ YC instance metadata, который cloud-init
// внутри гостевой OS читает как user-data (YC-конвенция, симметрично EC2-
// userdata + GCP startup-script).
const userdataMetaKey = "user-data"

// defaultBackoff — фабрика [clouddriver.BackoffConfig] для wait/retry-фаз.
// Вынесена в переменную, чтобы L0-тесты могли подменить (быстрый MaxAttempts,
// короткие задержки) без поднятия таймера 1s→2s→4s. Та же техника, что и
// `newYcClient` (см. ycapi.go).
var defaultBackoff = clouddriver.DefaultBackoff

// YcDriver — реализация CloudDriver для Yandex Cloud.
type YcDriver struct {
	clouddriver.BaseDriver
}

// Schema публикует embedded profile_schema.
func (y *YcDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
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

// Validate не несёт credentials (ValidateProfileRequest), structural-проверки
// здесь; auth — на фазе Create.
func (y *YcDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "folder_id") == "" {
		errs = append(errs, "profile.folder_id is required")
	}
	if stringField(p, "zone") == "" {
		errs = append(errs, "profile.zone is required")
	}
	if stringField(p, "image_id") == "" {
		errs = append(errs, "profile.image_id is required")
	}
	if stringField(p, "subnet_id") == "" {
		errs = append(errs, "profile.subnet_id is required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile — параметры профиля, разобранные для CreateInstance.
type vmProfile struct {
	folderID         string
	zone             string
	platformID       string
	cores            int64
	memoryBytes      int64
	coreFraction     int64
	imageID          string
	diskSizeBytes    int64
	diskType         string
	subnetID         string
	securityGroupIDs []string
	nat              bool
	serviceAccountID string
	labels           map[string]string
	runLabel         string
}

const (
	defaultPlatformID = "standard-v3"
	defaultDiskSizeGB = 20
	defaultDiskType   = "network-ssd"
	gibibyte          = int64(1024 * 1024 * 1024)
	defaultCores      = int64(2)
	defaultMemoryGB   = int64(2)
)

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		folderID:   stringField(p, "folder_id"),
		zone:       stringField(p, "zone"),
		platformID: stringField(p, "platform_id"),
		imageID:    stringField(p, "image_id"),
		subnetID:   stringField(p, "subnet_id"),
		labels:     map[string]string{},
	}
	if prof.platformID == "" {
		prof.platformID = defaultPlatformID
	}
	if res, ok := p["resources"].(map[string]any); ok {
		prof.cores = intField(res, "cores")
		if mem := intField(res, "memory_gb"); mem > 0 {
			prof.memoryBytes = mem * gibibyte
		}
		prof.coreFraction = intField(res, "core_fraction")
	}
	if prof.cores == 0 {
		prof.cores = defaultCores
	}
	if prof.memoryBytes == 0 {
		prof.memoryBytes = defaultMemoryGB * gibibyte
	}
	if disk, ok := p["disk"].(map[string]any); ok {
		if sz := intField(disk, "size_gb"); sz > 0 {
			prof.diskSizeBytes = sz * gibibyte
		}
		prof.diskType = stringField(disk, "type")
	}
	if prof.diskSizeBytes == 0 {
		prof.diskSizeBytes = int64(defaultDiskSizeGB) * gibibyte
	}
	if prof.diskType == "" {
		prof.diskType = defaultDiskType
	}
	if sgs, ok := p["security_group_ids"].([]any); ok {
		for _, sg := range sgs {
			if s, ok := sg.(string); ok {
				prof.securityGroupIDs = append(prof.securityGroupIDs, s)
			}
		}
	}
	if v, ok := p["nat"].(bool); ok {
		prof.nat = v
	}
	prof.serviceAccountID = stringField(p, "service_account_id")
	if labels, ok := p["labels"].(map[string]any); ok {
		for k, v := range labels {
			if s, ok := v.(string); ok {
				prof.labels[k] = s
			}
		}
	}
	prof.runLabel = prof.labels[runLabelKey]
	return prof
}

func intField(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// Create: fail-closed без идентичности (нет name и нет run-label — иначе rerun
// плодит orphan-VM, NIM-16) → идемпотент-скан живых VM по label soulstack-run →
// добор недостающих с первыми свободными индексами → wait-until-ready → финальное
// событие с VmInfo (fqdn = YC internal-DNS). name без label становится run-label
// (стамп labels). См. doc-комментарий пакета.
func (y *YcDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	// folder_id/zone в credentials имеют приоритет; пустые — берём из profile.
	if creds.FolderID == "" {
		creds.FolderID = prof.folderID
	}
	if creds.Zone == "" {
		creds.Zone = prof.zone
	}
	// Эффективные значения для запроса (profile может перетереть credentials и
	// наоборот — обе стороны корректны, выбираем непустое).
	folderID := firstNonEmpty(prof.folderID, creds.FolderID)
	zone := firstNonEmpty(prof.zone, creds.Zone)

	// Fail-closed: без name и без run-label прогон неотличим от предыдущих →
	// idempotency-скан невозможен, rerun плодил бы orphan-VM (NIM-16).
	nameBase := req.GetName()
	if nameBase == "" && prof.runLabel == "" {
		return sendCreateFailed(stream, clouddriver.FailInvalidParams, "identity",
			fmt.Errorf("no run identity: set step param `name` or profile.labels[%q]", runLabelKey))
	}
	// name без явного label становится run-label → создаваемые VM всегда несут
	// label soulstack-run, будущие прогоны матчатся по нему.
	if prof.runLabel == "" {
		prof.runLabel = nameBase
	}

	cli, err := newYcClient(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "yc-client", err)
	}

	backoff := defaultBackoff()

	// Идемпотентность (NIM-16): скан живых VM батча по label soulstack-run —
	// всегда (после guard runLabel непуст). Есть живые — переиспользуем, добиваем
	// только недостающие.
	existing, err := y.findByRunLabel(ctx, cli, backoff, folderID, prof.runLabel)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.Classify(classifyYC, err), "list-existing", err)
	}
	if int32(len(existing)) >= count {
		_ = stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runLabel),
		})
		return y.finalizeCreate(ctx, cli, stream, backoff, instanceIDs(existing))
	}

	need := count - int32(len(existing))
	names := gapFillNames(existing, nameBase, prof.runLabel, need)
	if len(existing) > 0 {
		_ = stream.Send(&pluginv1.CreateEvent{
			Message: fmt.Sprintf("idempotent: reusing %d existing VM for run %q, creating %d more", len(existing), prof.runLabel, need),
		})
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("yc.InstanceService.Create count=%d zone=%s platform=%s image=%s", need, zone, prof.platformID, prof.imageID),
	}); err != nil {
		return err
	}

	newInstances, err := y.createInstances(ctx, cli, backoff, prof, folderID, zone, req.GetUserdata(), names)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.Classify(classifyYC, err), "CreateInstance", err)
	}

	allIDs := append(instanceIDs(existing), instanceIDs(newInstances)...)
	return y.finalizeCreate(ctx, cli, stream, backoff, allIDs)
}

// createInstances вызывает CreateInstance по одному разу на каждое имя. YC
// создаёт по одной VM за вызов (нет batch-Create как у EC2.RunInstances). Имена
// приходят готовыми из gap-fill — детерминированные и без коллизий с уже
// существующими VM (NIM-16).
func (y *YcDriver) createInstances(ctx context.Context, cli ycAPI, backoff clouddriver.BackoffConfig, prof vmProfile, folderID, zone, userdata string, names []string) ([]*computev1.Instance, error) {
	out := make([]*computev1.Instance, 0, len(names))
	for _, name := range names {
		in := buildCreateRequest(prof, folderID, zone, userdata, name)
		var inst *computev1.Instance
		err := clouddriver.Retry(ctx, backoff, classifyYC, func() error {
			var rerr error
			inst, rerr = cli.CreateInstance(ctx, in)
			return rerr
		})
		if err != nil {
			return out, err
		}
		out = append(out, inst)
	}
	return out, nil
}

// buildCreateRequest формирует CreateInstanceRequest с готовым именем VM
// (детерминированное `<nameBase>-<seq>` / `soul-<runLabel>-<seq>` из gap-fill,
// NIM-16). runLabel штампуется в labels[soulstack-run] — идентичность прогона.
func buildCreateRequest(prof vmProfile, folderID, zone, userdata, name string) *computev1.CreateInstanceRequest {
	metadata := map[string]string{}
	if userdata != "" {
		metadata[userdataMetaKey] = userdata
	}
	labels := make(map[string]string, len(prof.labels))
	for k, v := range prof.labels {
		labels[k] = v
	}
	if prof.runLabel != "" {
		labels[runLabelKey] = prof.runLabel
	}

	natSpec := (*computev1.OneToOneNatSpec)(nil)
	if prof.nat {
		natSpec = &computev1.OneToOneNatSpec{IpVersion: computev1.IpVersion_IPV4}
	}

	resources := &computev1.ResourcesSpec{
		Cores:  prof.cores,
		Memory: prof.memoryBytes,
	}
	if prof.coreFraction > 0 {
		resources.CoreFraction = prof.coreFraction
	}

	req := &computev1.CreateInstanceRequest{
		FolderId:      folderID,
		Name:          name,
		ZoneId:        zone,
		PlatformId:    prof.platformID,
		Labels:        labels,
		Metadata:      metadata,
		ResourcesSpec: resources,
		BootDiskSpec: &computev1.AttachedDiskSpec{
			AutoDelete: true,
			Disk: &computev1.AttachedDiskSpec_DiskSpec_{
				DiskSpec: &computev1.AttachedDiskSpec_DiskSpec{
					TypeId: prof.diskType,
					Size:   prof.diskSizeBytes,
					Source: &computev1.AttachedDiskSpec_DiskSpec_ImageId{ImageId: prof.imageID},
				},
			},
		},
		NetworkInterfaceSpecs: []*computev1.NetworkInterfaceSpec{{
			SubnetId: prof.subnetID,
			PrimaryV4AddressSpec: &computev1.PrimaryAddressSpec{
				OneToOneNatSpec: natSpec,
			},
			SecurityGroupIds: prof.securityGroupIDs,
		}},
	}
	if prof.serviceAccountID != "" {
		req.ServiceAccountId = prof.serviceAccountID
	}
	return req
}

// vmName — детерминированное имя i-й VM батча (NIM-16, чистая функция без
// time-компоненты — иначе idempotency-скан не находил бы свои VM): nameBase
// (CreateRequest.name, self-onboard «Вариант T» ADR-017(h)) → `<nameBase>-<seq>`
// (keeper предсказал FQDN и запёк per-VM токен под ним — имя ОБЯЗАНО совпасть);
// иначе `soul-<runLabel>-<seq>`. Валидность nameBase под YC-ограничение имени
// `[a-z][-a-z0-9]{1,62}` — ответственность keeper-стороны.
func vmName(nameBase, runLabel string, seq int32) string {
	if nameBase != "" {
		return fmt.Sprintf("%s-%d", nameBase, seq)
	}
	return fmt.Sprintf("soul-%s-%d", runLabel, seq)
}

// gapFillNames выдаёт need имён для недостающих VM, беря первые свободные индексы
// seq=0,1,2,… (занятые = имена existing) → при частичном rerun новые VM не
// коллидируют по имени с уже существующими (NIM-16). Занятость считается по живым
// existing: индекс VM в DELETING может переиспользоваться — YC ответит
// AlreadyExists, громко и без orphan-ов.
func gapFillNames(existing []*computev1.Instance, nameBase, runLabel string, need int32) []string {
	occupied := make(map[string]bool, len(existing))
	for _, inst := range existing {
		occupied[inst.GetName()] = true
	}
	names := make([]string, 0, need)
	for seq := int32(0); int32(len(names)) < need; seq++ {
		n := vmName(nameBase, runLabel, seq)
		if occupied[n] {
			continue
		}
		names = append(names, n)
	}
	return names
}

// finalizeCreate ждёт готовности VM (RUNNING + FQDN/IP) и шлёт финальное
// событие. Anti-orphan: при ctx-cancel/timeout недоехавшие VM попадают в
// финальное событие с failed=true, но с заполненным vm_id — Keeper сможет их
// Destroy.
func (y *YcDriver) finalizeCreate(ctx context.Context, cli ycAPI, stream grpc.ServerStreamingServer[pluginv1.CreateEvent], backoff clouddriver.BackoffConfig, vmIDs []string) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		inst, perr := cli.GetInstance(pctx, vmID)
		if perr != nil {
			if clouddriver.Classify(classifyYC, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		switch inst.GetStatus() {
		case computev1.Instance_RUNNING:
			if inst.GetFqdn() != "" || primaryIP(inst) != "" {
				return clouddriver.ProbeResult{Ready: true}
			}
			return clouddriver.ProbeResult{}
		case computev1.Instance_STOPPING, computev1.Instance_STOPPED,
			computev1.Instance_ERROR, computev1.Instance_CRASHED, computev1.Instance_DELETING:
			return clouddriver.ProbeResult{Err: fmt.Errorf("instance %s entered terminal state %q", vmID, inst.GetStatus())}
		default:
			return clouddriver.ProbeResult{}
		}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmIDs, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			if inst, derr := cli.GetInstance(ctx, wr.VMID); derr == nil {
				vi.Fqdn = inst.GetFqdn()
				vi.PrimaryIp = primaryIP(inst)
				vi.Attributes = instanceAttributes(inst)
			}
		}
		if !wr.Ready {
			anyFailed = true
		}
		vms = append(vms, vi)
	}

	if waitErr != nil {
		// ctx-cancel / deadline: финальное событие НЕСЁТ vm_id всех созданных VM
		// с failed=true (anti-orphan) — Keeper увидит instance-id и сможет Destroy.
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyYC, waitErr), "wait-until-ready", waitErr),
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

// findByRunLabel перечисляет живые (не STOPPED/ERROR/CRASHED/DELETING) VM с
// заданным runLabel в folder-е (NIM-16: скан всегда — идентичность гарантирована
// guard-ом в Create). YC ListInstances поддерживает простой DSL фильтра
// `labels.<key>="<value>"`; статус фильтруем после, чтобы не зависеть от вариаций
// синтаксиса DSL у разных версий API. В отличие от wb-драйвера name-матч-ветки
// (усыновление до-фиксовых unlabeled VM по имени) здесь НЕТ: anon-имена были
// рандомными (time-seed), детерминированного паттерна для матча не существует,
// live-прогонов не было.
func (y *YcDriver) findByRunLabel(ctx context.Context, cli ycAPI, backoff clouddriver.BackoffConfig, folderID, runLabel string) ([]*computev1.Instance, error) {
	in := &computev1.ListInstancesRequest{
		FolderId: folderID,
		Filter:   fmt.Sprintf(`labels.%s="%s"`, runLabelKey, runLabel),
		PageSize: 1000,
	}
	var out *computev1.ListInstancesResponse
	err := clouddriver.Retry(ctx, backoff, classifyYC, func() error {
		var rerr error
		out, rerr = cli.ListInstances(ctx, in)
		return rerr
	})
	if err != nil {
		return nil, err
	}
	live := make([]*computev1.Instance, 0, len(out.GetInstances()))
	for _, inst := range out.GetInstances() {
		if isLiveStatus(inst.GetStatus()) {
			live = append(live, inst)
		}
	}
	return live, nil
}

func isLiveStatus(s computev1.Instance_Status) bool {
	switch s {
	case computev1.Instance_PROVISIONING, computev1.Instance_STARTING,
		computev1.Instance_RUNNING, computev1.Instance_RESTARTING,
		computev1.Instance_UPDATING:
		return true
	}
	return false
}

// Destroy: per-vm DeleteInstance, стрим per-vm событий. YC DeleteInstance не
// батчевый (как Salt-style API), поэтому идём списком; для not_found
// возвращаем success (идемпотентность destroy).
func (y *YcDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newYcClient(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "yc-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, id := range req.GetVmIds() {
		vmID := id
		err := clouddriver.Retry(ctx, backoff, classifyYC, func() error {
			return cli.DeleteInstance(ctx, vmID)
		})
		if err == nil {
			_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmID, Message: "deleted"})
			continue
		}
		class := clouddriver.Classify(classifyYC, err)
		if class == clouddriver.FailNotFound {
			// not_found на Destroy — это идемпотент-успех (VM уже нет).
			_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmID, Message: "already absent"})
			continue
		}
		_ = stream.Send(&pluginv1.DestroyEvent{
			VmId:    vmID,
			Message: clouddriver.FailMessage(class, "DeleteInstance", err),
			Failed:  true,
		})
	}
	return nil
}

// Status — опрос одной VM (GetInstance). credentials приходят отдельным полем
// StatusRequest.credentials (A-flow, симметрично Create/Destroy).
func (y *YcDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newYcClient(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("yc-client: %w", err)
	}
	inst, err := cli.GetInstance(ctx, req.GetVmId())
	if err != nil {
		return nil, fmt.Errorf("GetInstance %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      inst.GetStatus().String(),
		Attributes: instanceAttributes(inst),
	}, nil
}

// List — стрим инвентаря VM в folder-е (опц. отфильтрованный по runLabel).
// credentials приходят отдельным полем ListRequest.credentials (A-flow,
// симметрично Create/Destroy/Status).
func (y *YcDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newYcClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("yc-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	folderID := firstNonEmpty(stringField(filter, "folder_id"), creds.FolderID)
	if folderID == "" {
		return fmt.Errorf("folder_id is required (in filter or credentials)")
	}
	in := &computev1.ListInstancesRequest{FolderId: folderID, PageSize: 1000}
	if runLabel := stringField(filter, runLabelKey); runLabel != "" {
		in.Filter = fmt.Sprintf(`labels.%s="%s"`, runLabelKey, runLabel)
	}
	out, err := cli.ListInstances(ctx, in)
	if err != nil {
		return fmt.Errorf("ListInstances: %w", err)
	}
	for _, inst := range out.GetInstances() {
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       inst.GetId(),
			Fqdn:       inst.GetFqdn(),
			PrimaryIp:  primaryIP(inst),
			Attributes: instanceAttributes(inst),
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&YcDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-yc:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

func instanceIDs(insts []*computev1.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		out = append(out, i.GetId())
	}
	return out
}

// primaryIP — приватный IP первой network-interface; если есть NAT (публичный
// адрес) — используется он как fallback, чтобы Soul-host был достижим
// извне VPC при таком профиле.
func primaryIP(inst *computev1.Instance) string {
	for _, ni := range inst.GetNetworkInterfaces() {
		if pa := ni.GetPrimaryV4Address(); pa != nil {
			if pa.GetAddress() != "" {
				return pa.GetAddress()
			}
			if nat := pa.GetOneToOneNat(); nat != nil && nat.GetAddress() != "" {
				return nat.GetAddress()
			}
		}
	}
	return ""
}

func instanceAttributes(inst *computev1.Instance) *structpb.Struct {
	m := map[string]any{
		"platform_id": inst.GetPlatformId(),
		"zone":        inst.GetZoneId(),
	}
	if ca := inst.GetCreatedAt(); ca != nil {
		m["created_at"] = ca.AsTime().UTC().Format(time.RFC3339)
	}
	if res := inst.GetResources(); res != nil {
		m["cores"] = res.GetCores()
		m["memory_bytes"] = res.GetMemory()
		if res.GetCoreFraction() > 0 {
			m["core_fraction"] = res.GetCoreFraction()
		}
	}
	// Публичный IP — отдельным атрибутом для прозрачности (в primary_ip он
	// уже мог осесть как fallback).
	for _, ni := range inst.GetNetworkInterfaces() {
		if pa := ni.GetPrimaryV4Address(); pa != nil {
			if nat := pa.GetOneToOneNat(); nat != nil && nat.GetAddress() != "" {
				m["public_ip"] = nat.GetAddress()
				break
			}
		}
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
