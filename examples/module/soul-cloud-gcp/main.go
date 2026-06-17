// soul-cloud-gcp — реальный CloudDriver-плагин Soul Stack для Google Cloud
// Compute Engine (ADR-016 Фаза 4 cloud parity; тираж по docs/keeper/plugins.md
// после AWS-пилота).
//
// Собирается в статический бинарь `soul-cloud-gcp`. Keeper-side модуль
// `core.cloud.provisioned` (ADR-017) запускает его как sub-process, делает
// gRPC-stdio handshake (sdk/handshake) и зовёт RPC CloudDriver.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper резолвит сервис-аккаунт
// JSON-key из Vault и кладёт plain в CreateRequest.credentials /
// DestroyRequest.credentials; драйвер в Vault НЕ ходит. Cloud-init userdata для
// bootstrap soul-агента приходит в CreateRequest.userdata и пробрасывается в
// metadata-item `user-data` (GCP-convention для cloud-init).
//
// Shared-каркас (error-таксономия / wait-until-ready / retry-backoff) — из
// sdk/clouddriver, общий для всех драйверов тиража. Provider-specific здесь —
// только вызовы Compute Engine API и GCP-классификатор ошибок (classify.go).
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON — profile_schema (JSON Schema draft 2020-12), embedded
// рядом с бинарём. Приём из AWS-пилота: схема — отдельный файл, не хардкод в
// Go (легче держать в синхроне с manifest.spec.profile_schema).
//
//go:embed schema.json
var profileSchemaJSON []byte

// runLabelKey — label идемпотентности: значение = идентификатор прогона/
// incarnation (из profile.labels). Повторный Create по тому же label не плодит
// дубли — существующие живые VM переиспользуются. GCP-labels ограничены
// `[a-z0-9_-]{0,63}` (slash недопустим, в отличие от AWS-tag), поэтому ключ —
// `soulstack_run` без двоеточия/слэша.
const runLabelKey = "soulstack_run"

// userDataMetadataKey — стандартное имя metadata-item, которое cloud-init
// читает на GCP-VM (https://cloudinit.readthedocs.io → DataSourceGCE).
const userDataMetadataKey = "user-data"

// defaultBackoff — фабрика [clouddriver.BackoffConfig] для wait/retry-фаз.
// Вынесена в переменную, чтобы L0-тесты подменяли (быстрый MaxAttempts,
// короткие задержки) без поднятия таймера 1s→2s→4s. Та же техника, что и
// `newGcpInstancesClient` (см. gcpapi.go).
var defaultBackoff = clouddriver.DefaultBackoff

// GcpDriver — реализация CloudDriver для Google Compute Engine.
type GcpDriver struct {
	clouddriver.BaseDriver
}

// Schema публикует embedded profile_schema.
func (g *GcpDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
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
func (g *GcpDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "project") == "" {
		errs = append(errs, "profile.project is required")
	}
	if stringField(p, "zone") == "" {
		errs = append(errs, "profile.zone is required")
	}
	if stringField(p, "machine_type") == "" {
		errs = append(errs, "profile.machine_type is required")
	}
	if stringField(p, "source_image") == "" {
		errs = append(errs, "profile.source_image is required")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile — параметры профиля, разобранные для Insert.
type vmProfile struct {
	project     string
	zone        string
	machineType string
	sourceImage string
	network     string
	subnet      string
	diskSizeGB  int64
	diskType    string
	labels      map[string]string
	runTag      string
}

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		project:     stringField(p, "project"),
		zone:        stringField(p, "zone"),
		machineType: stringField(p, "machine_type"),
		sourceImage: stringField(p, "source_image"),
		labels:      map[string]string{},
	}
	if net, ok := p["network"].(map[string]any); ok {
		prof.network = stringField(net, "network")
		prof.subnet = stringField(net, "subnet")
	}
	if disk, ok := p["disk"].(map[string]any); ok {
		if sz, ok := disk["size_gb"].(float64); ok {
			prof.diskSizeGB = int64(sz)
		}
		prof.diskType = stringField(disk, "type")
	}
	if labels, ok := p["labels"].(map[string]any); ok {
		for k, v := range labels {
			if s, ok := v.(string); ok {
				prof.labels[k] = s
			}
		}
	}
	prof.runTag = prof.labels[runLabelKey]
	return prof
}

// Create: проверка идемпотентности по label → Insert N штук → wait-until-ready →
// финальное событие с VmInfo (fqdn=GCP internal DNS, основной идентификатор).
// См. doc-комментарий пакета.
func (g *GcpDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())
	// project/zone могут быть в profile или в credentials — credentials имеют
	// приоритет (точно та же модель, что в AWS region).
	if creds.Project == "" {
		creds.Project = prof.project
	}
	if creds.Zone == "" {
		creds.Zone = prof.zone
	}
	// profile project/zone тоже подменяем (Insert берёт их из profile).
	if prof.project == "" {
		prof.project = creds.Project
	}
	if prof.zone == "" {
		prof.zone = creds.Zone
	}

	cli, err := newGcpInstancesClient(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "compute-client", err)
	}

	backoff := defaultBackoff()

	// Идемпотентность: если по runTag уже есть живые VM — переиспользуем их,
	// добиваем только недостающие. Без runTag idempotency-проверку не делаем
	// (некому соотнести прогон).
	var existing []*computepb.Instance
	if prof.runTag != "" {
		existing, err = g.findByRunLabel(ctx, cli, backoff, prof, prof.runTag)
		if err != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyGCP, err), "list-existing", err)
		}
		if int32(len(existing)) >= count {
			_ = stream.Send(&pluginv1.CreateEvent{
				Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runTag),
			})
			return g.finalizeCreate(ctx, cli, stream, backoff, prof, instanceNames(existing))
		}
		count -= int32(len(existing))
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("compute.Insert count=%d type=%s image=%s zone=%s", count, prof.machineType, prof.sourceImage, prof.zone),
	}); err != nil {
		return err
	}

	// Имена новых VM: детерминированные от runTag (для безопасной retry-логики),
	// либо timestamp-based если runTag пуст. Существующие VM продолжают свой
	// индекс (offset = len(existing)).
	offset := int32(len(existing))
	newNames := generateVMNames(prof.runTag, offset, count)
	for _, name := range newNames {
		if perr := g.insertOne(ctx, cli, backoff, prof, req.GetUserdata(), name); perr != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyGCP, perr), "Insert", perr)
		}
	}

	allNames := append(instanceNames(existing), newNames...)
	return g.finalizeCreate(ctx, cli, stream, backoff, prof, allNames)
}

// finalizeCreate ждёт готовности VM (RUNNING + internal IP) и шлёт финальное
// событие. Anti-orphan: при ctx-cancel/timeout недоехавшие VM попадают в
// финальное событие с failed=true, но с заполненным vm_id — Keeper сможет их
// Destroy.
func (g *GcpDriver) finalizeCreate(ctx context.Context, cli gcpInstancesAPI, stream grpc.ServerStreamingServer[pluginv1.CreateEvent], backoff clouddriver.BackoffConfig, prof vmProfile, vmNames []string) error {
	probe := func(pctx context.Context, name string) clouddriver.ProbeResult {
		inst, perr := cli.Get(pctx, &computepb.GetInstanceRequest{
			Project:  prof.project,
			Zone:     prof.zone,
			Instance: name,
		})
		if perr != nil {
			// Транзиентную ошибку опроса глотаем — поллер повторит.
			if clouddriver.Classify(classifyGCP, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		status := inst.GetStatus()
		switch status {
		case "RUNNING":
			if primaryIP(inst) != "" {
				return clouddriver.ProbeResult{Ready: true}
			}
			return clouddriver.ProbeResult{}
		case "TERMINATED", "STOPPING", "STOPPED", "SUSPENDED", "SUSPENDING":
			return clouddriver.ProbeResult{Err: fmt.Errorf("instance %s entered terminal state %q", name, status)}
		default:
			// PROVISIONING / STAGING / REPAIRING — ждём дальше.
			return clouddriver.ProbeResult{}
		}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmNames, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmNames))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			if inst, derr := cli.Get(ctx, &computepb.GetInstanceRequest{
				Project: prof.project, Zone: prof.zone, Instance: wr.VMID,
			}); derr == nil {
				vi.Fqdn = internalFQDN(wr.VMID, prof.zone, prof.project)
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
		// с failed=true (anti-orphan) — Keeper увидит instance-name и сможет Destroy.
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyGCP, waitErr), "wait-until-ready", waitErr),
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

// insertOne собирает Instance-resource и вызывает Insert + Operation.Wait для
// одной VM. Insert обёрнут в retry/backoff (throttling-safe).
func (g *GcpDriver) insertOne(ctx context.Context, cli gcpInstancesAPI, backoff clouddriver.BackoffConfig, prof vmProfile, userdata, name string) error {
	inst := buildInstance(prof, userdata, name)
	in := &computepb.InsertInstanceRequest{
		Project:          prof.project,
		Zone:             prof.zone,
		InstanceResource: inst,
	}
	var op gcpOperation
	if err := clouddriver.Retry(ctx, backoff, classifyGCP, func() error {
		var rerr error
		op, rerr = cli.Insert(ctx, in)
		return rerr
	}); err != nil {
		return err
	}
	// Wait блокирует до DONE; ошибку Operation возвращает как googleapi.Error.
	return op.Wait(ctx)
}

// findByRunLabel перечисляет живые (не terminated) VM с заданным runTag.
// GCP-filter-синтаксис: `labels.<k>=<v> AND (status="RUNNING" OR status="PROVISIONING"…)`.
func (g *GcpDriver) findByRunLabel(ctx context.Context, cli gcpInstancesAPI, backoff clouddriver.BackoffConfig, prof vmProfile, runTag string) ([]*computepb.Instance, error) {
	filter := fmt.Sprintf(`labels.%s=%s`, runLabelKey, runTag)
	in := &computepb.ListInstancesRequest{
		Project: prof.project,
		Zone:    prof.zone,
		Filter:  proto.String(filter),
	}
	var out []*computepb.Instance
	err := clouddriver.Retry(ctx, backoff, classifyGCP, func() error {
		var rerr error
		out, rerr = cli.List(ctx, in)
		return rerr
	})
	if err != nil {
		return nil, err
	}
	// Фильтруем terminal-VM на стороне клиента (GCP filter синтаксис для OR
	// громоздкий, дешевле отфильтровать после List).
	alive := out[:0]
	for _, inst := range out {
		switch inst.GetStatus() {
		case "TERMINATED", "STOPPING", "STOPPED", "SUSPENDED", "SUSPENDING":
			// skip
		default:
			alive = append(alive, inst)
		}
	}
	return alive, nil
}

// Destroy: Delete для каждой VM, стрим per-vm событий. Идемпотентность: 404
// (NotFound) для отдельной VM — это «уже нет», шлём успешное событие.
func (g *GcpDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newGcpInstancesClient(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "compute-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, name := range req.GetVmIds() {
		err := clouddriver.Retry(ctx, backoff, classifyGCP, func() error {
			op, derr := cli.Delete(ctx, &computepb.DeleteInstanceRequest{
				Project:  creds.Project,
				Zone:     creds.Zone,
				Instance: name,
			})
			if derr != nil {
				return derr
			}
			return op.Wait(ctx)
		})
		if err != nil {
			class := clouddriver.Classify(classifyGCP, err)
			if class == clouddriver.FailNotFound {
				// not_found на Destroy — это успех-как-таковой (идемпотентность).
				_ = stream.Send(&pluginv1.DestroyEvent{VmId: name, Message: "already absent"})
				continue
			}
			_ = stream.Send(&pluginv1.DestroyEvent{
				VmId:    name,
				Message: clouddriver.FailMessage(class, "Delete", err),
				Failed:  true,
			})
			continue
		}
		_ = stream.Send(&pluginv1.DestroyEvent{VmId: name, Message: "terminated"})
	}
	return nil
}

// Status — опрос одной VM (Get). credentials приходят отдельным полем
// StatusRequest.credentials (A-flow, симметрично Create/Destroy).
func (g *GcpDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newGcpInstancesClient(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("compute-client: %w", err)
	}
	inst, err := cli.Get(ctx, &computepb.GetInstanceRequest{
		Project: creds.Project, Zone: creds.Zone, Instance: req.GetVmId(),
	})
	if err != nil {
		return nil, fmt.Errorf("Get %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      inst.GetStatus(),
		Attributes: instanceAttributes(inst),
	}, nil
}

// List — стрим инвентаря VM по фильтру. credentials приходят отдельным полем
// ListRequest.credentials (A-flow, симметрично Create/Destroy/Status). Фильтр-
// поле `soulstack_run` поднимается до GCP-filter `labels.soulstack_run=<v>`.
func (g *GcpDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newGcpInstancesClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("compute-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	in := &computepb.ListInstancesRequest{
		Project: creds.Project,
		Zone:    creds.Zone,
	}
	if runTag := stringField(filter, runLabelKey); runTag != "" {
		in.Filter = proto.String(fmt.Sprintf(`labels.%s=%s`, runLabelKey, runTag))
	}
	insts, err := cli.List(ctx, in)
	if err != nil {
		return fmt.Errorf("List: %w", err)
	}
	for _, inst := range insts {
		name := inst.GetName()
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       name,
			Fqdn:       internalFQDN(name, creds.Zone, creds.Project),
			PrimaryIp:  primaryIP(inst),
			Attributes: instanceAttributes(inst),
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&GcpDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-gcp:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

// buildInstance собирает Instance-resource из profile + userdata. machineType
// и sourceImage могут прийти короткой или полной формой; для machineType
// GCP-API требует полный путь `zones/<zone>/machineTypes/<type>` — нормализуем
// здесь.
func buildInstance(prof vmProfile, userdata, name string) *computepb.Instance {
	mt := prof.machineType
	if !strings.Contains(mt, "/") {
		mt = fmt.Sprintf("zones/%s/machineTypes/%s", prof.zone, mt)
	}
	disk := &computepb.AttachedDisk{
		Boot:       proto.Bool(true),
		AutoDelete: proto.Bool(true),
		InitializeParams: &computepb.AttachedDiskInitializeParams{
			SourceImage: proto.String(prof.sourceImage),
		},
	}
	if prof.diskSizeGB > 0 {
		disk.InitializeParams.DiskSizeGb = proto.Int64(prof.diskSizeGB)
	}
	if prof.diskType != "" {
		disk.InitializeParams.DiskType = proto.String(fmt.Sprintf("zones/%s/diskTypes/%s", prof.zone, prof.diskType))
	}
	network := prof.network
	if network == "" {
		network = "global/networks/default"
	} else if !strings.Contains(network, "/") {
		network = "global/networks/" + network
	}
	ni := &computepb.NetworkInterface{Network: proto.String(network)}
	if prof.subnet != "" {
		sn := prof.subnet
		if !strings.Contains(sn, "/") {
			// Subnet регионален, но zone-region строго детерминирован: regional-
			// часть = zone без суффикса `-<letter>`.
			region := zoneRegion(prof.zone)
			sn = fmt.Sprintf("regions/%s/subnetworks/%s", region, sn)
		}
		ni.Subnetwork = proto.String(sn)
	}
	inst := &computepb.Instance{
		Name:              proto.String(name),
		MachineType:       proto.String(mt),
		Disks:             []*computepb.AttachedDisk{disk},
		NetworkInterfaces: []*computepb.NetworkInterface{ni},
	}
	if len(prof.labels) > 0 {
		inst.Labels = make(map[string]string, len(prof.labels))
		for k, v := range prof.labels {
			inst.Labels[k] = v
		}
	}
	// cloud-init userdata: GCP-convention — metadata-item с ключом `user-data`
	// (cloud-init DataSourceGCE). НЕ base64 (в отличие от EC2): GCP передаёт
	// metadata как plain string.
	if userdata != "" {
		inst.Metadata = &computepb.Metadata{
			Items: []*computepb.Items{{
				Key:   proto.String(userDataMetadataKey),
				Value: proto.String(userdata),
			}},
		}
	}
	return inst
}

// generateVMNames — детерминированные имена для VM в одном прогоне. При retry
// после crash-а Keeper-а имена совпадут с предыдущими, и findByRunLabel
// найдёт существующие. Если runTag пуст — используем timestamp (одноразовый
// прогон без idempotency-гарантии).
//
// GCP-name требования: lowercase letters/digits/hyphens, начинается с буквы,
// заканчивается буквой/цифрой, max 63 символа.
func generateVMNames(runTag string, offset, count int32) []string {
	out := make([]string, 0, count)
	prefix := "soul-" + sanitizeName(runTag)
	if runTag == "" {
		prefix = fmt.Sprintf("soul-%d", time.Now().UnixNano())
	}
	for i := int32(0); i < count; i++ {
		out = append(out, fmt.Sprintf("%s-%d", prefix, offset+i))
	}
	return out
}

// sanitizeName приводит произвольную строку к GCP-name-формату: lowercase,
// hyphen-separated, max 50 символов (резерв на суффикс-индекс).
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	out := b.String()
	out = strings.Trim(out, "-")
	if len(out) > 50 {
		out = out[:50]
	}
	if out == "" {
		out = "run"
	}
	return out
}

// zoneRegion возвращает GCP-регион для зоны (europe-west1-b → europe-west1).
func zoneRegion(zone string) string {
	if i := strings.LastIndex(zone, "-"); i > 0 {
		return zone[:i]
	}
	return zone
}

// internalFQDN — GCP internal DNS-имя VM. Используется как SID.
func internalFQDN(name, zone, project string) string {
	return fmt.Sprintf("%s.%s.c.%s.internal", name, zone, project)
}

// primaryIP — первый internal-IP VM (private IP первого NIC); если пусто —
// первый external (access-config NatIP).
func primaryIP(inst *computepb.Instance) string {
	for _, ni := range inst.NetworkInterfaces {
		if ip := ni.GetNetworkIP(); ip != "" {
			return ip
		}
	}
	for _, ni := range inst.NetworkInterfaces {
		for _, ac := range ni.AccessConfigs {
			if ip := ac.GetNatIP(); ip != "" {
				return ip
			}
		}
	}
	return ""
}

func instanceNames(insts []*computepb.Instance) []string {
	out := make([]string, 0, len(insts))
	for _, i := range insts {
		out = append(out, i.GetName())
	}
	return out
}

func instanceAttributes(inst *computepb.Instance) *structpb.Struct {
	m := map[string]any{
		"machine_type": shortRef(inst.GetMachineType()),
		"zone":         shortRef(inst.GetZone()),
	}
	if ct := inst.GetCreationTimestamp(); ct != "" {
		m["creation_timestamp"] = ct
	}
	for _, ni := range inst.NetworkInterfaces {
		for _, ac := range ni.AccessConfigs {
			if ip := ac.GetNatIP(); ip != "" {
				m["public_ip"] = ip
				break
			}
		}
		if _, ok := m["public_ip"]; ok {
			break
		}
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}

// shortRef обрезает GCP-resource URL до последнего сегмента (полный URL вида
// `https://www.googleapis.com/compute/v1/projects/p/zones/europe-west1-b` →
// `europe-west1-b`).
func shortRef(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}
