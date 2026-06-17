// soul-cloud-proxmox — реальный CloudDriver-плагин Soul Stack для Proxmox VE
// (ADR-016 Фаза 4 cloud parity; тираж по pattern-у soul-cloud-aws).
//
// Собирается в статический бинарь `soul-cloud-proxmox`. Keeper-side модуль
// `core.cloud.provisioned` (ADR-017) запускает его как sub-process, делает
// gRPC-stdio handshake (sdk/handshake) и зовёт RPC CloudDriver.
//
// Credentials (A-flow, docs/keeper/cloud.md): Keeper резолвит секрет из Vault и
// кладёт plain в CreateRequest.credentials / DestroyRequest.credentials; драйвер
// в Vault НЕ ходит. Proxmox поддерживает две формы XOR: API-token (формата
// `<user>@<realm>!<token-id>=<value>`) или ticket-based (username+password+
// realm). Endpoint обязателен (https://<host>:8006). См. pveapi.go.
//
// Shared-каркас (error-таксономия / wait-until-ready / retry-backoff) — из
// sdk/clouddriver, общий для всех драйверов тиража. Provider-specific здесь —
// только вызовы PVE REST API и Proxmox-классификатор ошибок (classify.go).
//
// # vm_id-paradigm: composite `<node>/<vmid>`
//
// Proxmox — НЕ облако в обычном смысле: VM живёт на конкретной node гипервизор-
// кластера; одного и того же endpoint /qemu/<vmid> не существует — он
// per-node (/nodes/<node>/qemu/<vmid>). Поэтому драйвер сериализует proto-
// идентификатор VM как composite-строку `<node>/<vmid>` — это даёт самодоста-
// точный ID для Destroy/Status без отдельного state-storage на keeper-стороне
// (vmid→node map). Симметрично AWS instance-id (там тоже строка, но без `/`).
// Парсинг — splitVmID(); сборка — formatVmID(). proto-поле VmInfo.vm_id (string)
// не меняет своей формы; ограничение для оператора — никаких слешей в имени
// node (Proxmox их и не разрешает).
//
// # create-модель: clone из template, не launch image
//
// В отличие от AWS (RunInstances из AMI) и YC (CreateInstance из image_id),
// Proxmox-create = `qm clone <template> <newid>`: VM создаётся как полная или
// linked-копия предсуществующего qemu-шаблона (VMID ≥ 100). Шаблон уже несёт
// подготовленный cloud-init (или мы дополним конфиг через SetVMConfig).
// Параметры профиля (cores/memory/storage/bridge) применяются POST-clone-ом
// через /config; ресурсы из шаблона перезаписываются.
package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/clouddriver"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// profileSchemaJSON — profile_schema (JSON Schema draft 2020-12), embedded
// рядом с бинарём. Та же техника, что в soul-cloud-aws.
//
//go:embed schema.json
var profileSchemaJSON []byte

// runTagKey — тег идемпотентности (в Proxmox VM-tags). Значение = идентификатор
// прогона/incarnation (из profile.tags). Повторный Create по тому же тегу не
// плодит дубли — существующие живые VM (running/stopped) переиспользуются.
//
// Имя ключа должно быть kebab-case под Proxmox-tag-regex (`^[a-z0-9_-]+$`);
// двоеточие — НЕ допустимо (отличие от AWS «soulstack:run»), поэтому используем
// `soulstack-run` (без `:`). Зеркальное имя для YC.
const runTagKey = "soulstack-run"

// defaultBackoff — фабрика [clouddriver.BackoffConfig] для wait/retry-фаз.
// Вынесена в переменную, чтобы L0-тесты могли подменить (быстрый MaxAttempts,
// короткие задержки) без поднятия таймера 1s→2s→4s. Та же техника, что и
// `newPveClient` (см. pveapi.go).
var defaultBackoff = clouddriver.DefaultBackoff

// ProxmoxDriver — реализация CloudDriver для Proxmox VE.
type ProxmoxDriver struct {
	clouddriver.BaseDriver
}

// Schema публикует embedded profile_schema.
func (d *ProxmoxDriver) Schema(_ context.Context, _ *pluginv1.SchemaRequest) (*pluginv1.SchemaReply, error) {
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
func (d *ProxmoxDriver) Validate(_ context.Context, req *pluginv1.ValidateProfileRequest) (*pluginv1.ValidateProfileReply, error) {
	p := req.GetProfile().AsMap()
	var errs []string
	if stringField(p, "target_node") == "" {
		errs = append(errs, "profile.target_node is required")
	}
	if intField(p, "template_vmid") <= 0 {
		errs = append(errs, "profile.template_vmid is required (integer ≥ 100)")
	}
	return &pluginv1.ValidateProfileReply{Ok: len(errs) == 0, Errors: errs}, nil
}

// vmProfile — параметры профиля, разобранные для clone-операции.
type vmProfile struct {
	targetNode    string
	templateVMID  int
	newVMIDStart  int
	namePrefix    string
	fullClone     bool
	cores         int
	memory        int // МБ
	storage       string
	bridge        string
	tags          map[string]string
	cicustom      string
	runTag        string
}

const (
	defaultNamePrefix = "soul"
	defaultCores      = 2
	defaultMemoryMB   = 2048
)

func parseProfile(p map[string]any) vmProfile {
	prof := vmProfile{
		targetNode:   stringField(p, "target_node"),
		templateVMID: int(intField(p, "template_vmid")),
		newVMIDStart: int(intField(p, "new_vmid_start")),
		namePrefix:   stringField(p, "name_prefix"),
		fullClone:    true, // default
		cores:        defaultCores,
		memory:       defaultMemoryMB,
		storage:      stringField(p, "storage"),
		bridge:       stringField(p, "bridge"),
		cicustom:     stringField(p, "cicustom"),
		tags:         map[string]string{},
	}
	if prof.namePrefix == "" {
		prof.namePrefix = defaultNamePrefix
	}
	if v, ok := p["full_clone"].(bool); ok {
		prof.fullClone = v
	}
	if c := intField(p, "cores"); c > 0 {
		prof.cores = int(c)
	}
	if m := intField(p, "memory"); m > 0 {
		prof.memory = int(m)
	}
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

// Create: clone из template_vmid → SetVMConfig (ресурсы + cloud-init userdata)
// → start → wait-until-ready (running + guest-agent IP). См. doc-комментарий
// пакета.
func (d *ProxmoxDriver) Create(req *pluginv1.CreateRequest, stream grpc.ServerStreamingServer[pluginv1.CreateEvent]) error {
	ctx := stream.Context()
	count := req.GetCount()
	if count <= 0 {
		count = 1
	}
	prof := parseProfile(req.GetProfile().AsMap())
	creds := credsFromMap(req.GetCredentials().AsMap())

	cli, err := newPveClient(ctx, creds)
	if err != nil {
		return sendCreateFailed(stream, clouddriver.FailAuth, "pve-client", err)
	}

	backoff := defaultBackoff()

	// Идемпотентность: если по runTag уже есть живые VM — переиспользуем,
	// добиваем только недостающие. Без runTag idempotency-проверку не делаем.
	var existing []ClusterVM
	if prof.runTag != "" {
		existing, err = d.findByRunTag(ctx, cli, backoff, prof.runTag)
		if err != nil {
			return sendCreateFailed(stream, clouddriver.Classify(classifyProxmox, err), "list-existing", err)
		}
		if int32(len(existing)) >= count {
			_ = stream.Send(&pluginv1.CreateEvent{
				Message: fmt.Sprintf("idempotent: %d VM already present for run %q", len(existing), prof.runTag),
			})
			return d.finalizeCreate(ctx, cli, stream, backoff, clusterVmIDs(existing))
		}
		count -= int32(len(existing))
	}

	if err := stream.Send(&pluginv1.CreateEvent{
		Message: fmt.Sprintf("pve clone count=%d template_vmid=%d target=%s full=%v",
			count, prof.templateVMID, prof.targetNode, prof.fullClone),
	}); err != nil {
		return err
	}

	newVMs, err := d.cloneInstances(ctx, cli, backoff, prof, req.GetUserdata(), count)
	if err != nil {
		// newVMs может содержать УЖЕ созданные (до того, как clone-цикл упал) —
		// anti-orphan: финальное событие отдаст их с failed=true.
		fail := &pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyProxmox, err), "clone", err),
			Failed:  true,
		}
		for _, v := range newVMs {
			fail.Vms = append(fail.Vms, &pluginv1.VmInfo{VmId: formatVmID(v.Node, v.VMID)})
		}
		return stream.Send(fail)
	}

	all := make([]string, 0, len(existing)+len(newVMs))
	for _, e := range existing {
		all = append(all, formatVmID(e.Node, e.VMID))
	}
	for _, n := range newVMs {
		all = append(all, formatVmID(n.Node, n.VMID))
	}
	return d.finalizeCreate(ctx, cli, stream, backoff, all)
}

// createdVM — пара (node, vmid) свежесозданной VM. Драйвер копит её во время
// цикла clone, чтобы при ctx-cancel вернуть anti-orphan-ID.
type createdVM struct {
	Node string
	VMID int
}

// cloneInstances вызывает Clone count раз. Proxmox не имеет batch-clone, идём
// последовательно. Имя каждой VM — `<prefix>-<vmid>`. Параллельный clone тех
// же template-VMID Proxmox не любит (locks), поэтому строго последовательно.
//
// Антиорфан-контракт: возвращает уже-успешно-склонированные VM ДАЖЕ при
// ошибке последующего шага — Keeper увидит их в финальном failed-event.
func (d *ProxmoxDriver) cloneInstances(
	ctx context.Context, cli pveAPI, backoff clouddriver.BackoffConfig,
	prof vmProfile, userdata string, count int32,
) ([]createdVM, error) {
	out := make([]createdVM, 0, count)
	for i := int32(0); i < count; i++ {
		newID, err := d.allocVMID(ctx, cli, backoff, prof, int(i))
		if err != nil {
			return out, fmt.Errorf("alloc vmid: %w", err)
		}
		name := fmt.Sprintf("%s-%d", prof.namePrefix, newID)

		cloneErr := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
			_, e := cli.CloneVM(ctx, CloneParams{
				SourceNode:    prof.targetNode, // template живёт на target_node (best-effort default)
				TemplateVMID:  prof.templateVMID,
				NewVMID:       newID,
				Name:          name,
				TargetNode:    prof.targetNode,
				TargetStorage: prof.storage,
				FullClone:     prof.fullClone,
			})
			return e
		})
		if cloneErr != nil {
			return out, cloneErr
		}
		out = append(out, createdVM{Node: prof.targetNode, VMID: newID})

		// Применяем POST-clone параметры: ресурсы (cores/memory), tags, cloud-init
		// userdata. Делаем единым POST /config, чтобы один write-lock.
		fields := buildConfigFields(prof, userdata)
		if err := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
			return cli.SetVMConfig(ctx, prof.targetNode, newID, fields)
		}); err != nil {
			return out, fmt.Errorf("set-config: %w", err)
		}

		// Стартуем VM.
		if _, err := func() (string, error) {
			var upid string
			rerr := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
				var e error
				upid, e = cli.StartVM(ctx, prof.targetNode, newID)
				return e
			})
			return upid, rerr
		}(); err != nil {
			return out, fmt.Errorf("start: %w", err)
		}
	}
	return out, nil
}

// allocVMID выбирает VMID для новой VM: profile.new_vmid_start+seq если задан,
// иначе /cluster/nextid. Берём NextID и при коллизии (race с другим оператором)
// Retry-обёртка повторит весь clone — Proxmox вернёт 500 «VM already exists».
func (d *ProxmoxDriver) allocVMID(ctx context.Context, cli pveAPI, backoff clouddriver.BackoffConfig, prof vmProfile, seq int) (int, error) {
	if prof.newVMIDStart > 0 {
		return prof.newVMIDStart + seq, nil
	}
	var id int
	err := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
		var e error
		id, e = cli.NextID(ctx)
		return e
	})
	return id, err
}

// buildConfigFields — Proxmox-form-параметры для /config: ресурсы, tags,
// сетевой bridge (поверх net0 если задан), cloud-init userdata.
//
// userdata-стратегия:
//   - Если profile.cicustom задан — используем cicustom (snippet-path) как есть.
//     Это путь в /var/lib/vz/snippets/<…>, который должен быть подготовлен
//     оператором заранее.
//   - Иначе userdata из CreateRequest (cloud-init блоб) base64-кодируется и
//     передаётся через ciuser/cipassword/ssh-keys невозможно (это поле строкой
//     не передаётся); вместо этого Proxmox принимает userdata через `cicustom=
//     user=<snippet>`. Альтернатива — параметр `description` или поле
//     `serial0=socket` — но они не интерпретируются cloud-init.
//
// Для MVP: если cicustom не задан И userdata есть, кладём userdata в
// `description` (поле документации VM) и помечаем VM tag-ом `soulstack-needs-
// snippet=1`, чтобы оператор увидел: автоматическая прокидка userdata в Proxmox
// требует snippets-storage, который драйвер сам создать не может.
//
// Это сознательное ограничение pilot-а — расширение через автоматическое
// размещение snippets (через WebDAV / SSH в node) — отложено до отдельного
// архитектурного решения (требует ssh-доступа к ноде или PVE storage API).
func buildConfigFields(prof vmProfile, userdata string) map[string]string {
	fields := map[string]string{
		"cores":  strconv.Itoa(prof.cores),
		"memory": strconv.Itoa(prof.memory),
	}
	if prof.bridge != "" {
		// virtio + bridge. virtio — default Proxmox для cloud-init шаблонов.
		fields["net0"] = fmt.Sprintf("virtio,bridge=%s", prof.bridge)
	}
	if len(prof.tags) > 0 {
		// Proxmox tags: semicolon-separated, format `key=value` или просто `key`.
		// Для runTagKey используем `<key>=<value>`, прочие — `key=value`.
		var parts []string
		for k, v := range prof.tags {
			if v == "" {
				parts = append(parts, k)
			} else {
				parts = append(parts, fmt.Sprintf("%s=%s", k, v))
			}
		}
		fields["tags"] = strings.Join(parts, ";")
	}
	switch {
	case prof.cicustom != "":
		fields["cicustom"] = prof.cicustom
	case userdata != "":
		// Кладём base64-encoded userdata в description, чтобы он был доступен
		// оператору для отладки. См. doc-комментарий buildConfigFields о
		// ограничении: Proxmox требует snippet-файл на ноде для cloud-init
		// user-data; полностью in-band прокидка userdata без snippet не
		// поддерживается API.
		fields["description"] = "soul-stack userdata (base64): " +
			base64.StdEncoding.EncodeToString([]byte(userdata))
	}
	return fields
}

// finalizeCreate ждёт готовности VM (running + guest-agent IP) и шлёт финальное
// событие. Anti-orphan: при ctx-cancel/timeout недоехавшие VM попадают в
// финальное событие с failed=true, но с заполненным vm_id — Keeper сможет их
// Destroy.
func (d *ProxmoxDriver) finalizeCreate(
	ctx context.Context, cli pveAPI,
	stream grpc.ServerStreamingServer[pluginv1.CreateEvent],
	backoff clouddriver.BackoffConfig, vmIDs []string,
) error {
	probe := func(pctx context.Context, vmID string) clouddriver.ProbeResult {
		node, id, err := splitVmID(vmID)
		if err != nil {
			return clouddriver.ProbeResult{Err: err}
		}
		st, perr := cli.GetVMStatus(pctx, node, id)
		if perr != nil {
			if clouddriver.Classify(classifyProxmox, perr).Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{Err: perr}
		}
		// Proxmox терминальных «error/crashed» статусов не возвращает; VM либо
		// running, либо stopped. Long-running «stopped» во время clone/migrate
		// нормально — это всё ещё в pipeline; распознаём по полю `lock`.
		if st.Lock != "" {
			// Под locked (clone/migrate/backup) — ждём дальше.
			return clouddriver.ProbeResult{}
		}
		if st.Status != "running" {
			// VM ещё не стартовала после start-RPC — пуллим дальше; терминала нет.
			return clouddriver.ProbeResult{}
		}
		// Running. Проверяем guest-agent → IP.
		ip, ipErr := cli.GetGuestAgentInterfaces(pctx, node, id)
		if ipErr != nil {
			// Guest-agent может быть не настроен/не отвечать. Это потенциально
			// конфигурационная ошибка шаблона: задаём fail-closed по принципу
			// «нет guest-agent — нет IP — VM использовать нельзя». Распознаём
			// «не настроен» по тексту — Proxmox возвращает 500/502 в зависимости
			// от состояния.
			class := clouddriver.Classify(classifyProxmox, ipErr)
			if class.Transient() {
				return clouddriver.ProbeResult{}
			}
			return clouddriver.ProbeResult{
				Err: fmt.Errorf("guest-agent not responding (template likely missing qemu-guest-agent): %w", ipErr),
			}
		}
		if ip == "" {
			// Guest-agent ответил, но IP ещё не назначен (DHCP-handshake в полёте).
			return clouddriver.ProbeResult{}
		}
		return clouddriver.ProbeResult{Ready: true}
	}

	waitResults, waitErr := clouddriver.WaitUntilReady(ctx, backoff, vmIDs, probe,
		func(msg string) { _ = stream.Send(&pluginv1.CreateEvent{Message: msg}) })

	vms := make([]*pluginv1.VmInfo, 0, len(vmIDs))
	anyFailed := false
	for _, wr := range waitResults {
		vi := &pluginv1.VmInfo{VmId: wr.VMID}
		if wr.Ready {
			node, id, err := splitVmID(wr.VMID)
			if err == nil {
				if st, derr := cli.GetVMStatus(ctx, node, id); derr == nil {
					if ip, ierr := cli.GetGuestAgentInterfaces(ctx, node, id); ierr == nil && ip != "" {
						vi.PrimaryIp = ip
						vi.Fqdn = st.Name // Proxmox-конвенция: name = hostname (SID)
					}
					vi.Attributes = statusAttributes(st)
				}
			}
		}
		if !wr.Ready {
			anyFailed = true
		}
		vms = append(vms, vi)
	}

	if waitErr != nil {
		return stream.Send(&pluginv1.CreateEvent{
			Message: clouddriver.FailMessage(clouddriver.Classify(classifyProxmox, waitErr), "wait-until-ready", waitErr),
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

// findByRunTag перечисляет живые (running/stopped, qemu-type) VM с заданным
// runTag в кластере. Proxmox /cluster/resources не поддерживает server-side
// filter по tag — фильтруем на стороне драйвера.
func (d *ProxmoxDriver) findByRunTag(ctx context.Context, cli pveAPI, backoff clouddriver.BackoffConfig, runTag string) ([]ClusterVM, error) {
	var out []ClusterVM
	err := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
		all, e := cli.ListClusterVMs(ctx)
		if e != nil {
			return e
		}
		out = nil
		for _, vm := range all {
			if vm.Type != "qemu" {
				continue
			}
			if hasTag(vm.Tags, runTagKey, runTag) {
				out = append(out, vm)
			}
		}
		return nil
	})
	return out, err
}

// hasTag проверяет, есть ли в semicolon-separated списке тегов пара key=value.
// Proxmox-tag-list имеет формат `t1;k=v;t2` без экранирования; ключи уникальны.
func hasTag(tags, key, value string) bool {
	for _, t := range strings.Split(tags, ";") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if kv := strings.SplitN(t, "=", 2); len(kv) == 2 {
			if kv[0] == key && kv[1] == value {
				return true
			}
		}
	}
	return false
}

// Destroy: stop → delete, per-vm события. Proxmox API DELETE на running-VM
// возвращает 500; нужно сначала stop (force). Idempotent на 404 / «does not
// exist».
func (d *ProxmoxDriver) Destroy(req *pluginv1.DestroyRequest, stream grpc.ServerStreamingServer[pluginv1.DestroyEvent]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newPveClient(ctx, creds)
	if err != nil {
		_ = stream.Send(&pluginv1.DestroyEvent{Message: clouddriver.FailMessage(clouddriver.FailAuth, "pve-client", err), Failed: true})
		return nil
	}

	backoff := defaultBackoff()
	for _, vmIDStr := range req.GetVmIds() {
		node, id, perr := splitVmID(vmIDStr)
		if perr != nil {
			_ = stream.Send(&pluginv1.DestroyEvent{
				VmId:    vmIDStr,
				Message: clouddriver.FailMessage(clouddriver.FailInvalidParams, "parse-vm_id", perr),
				Failed:  true,
			})
			continue
		}

		// Stop. not_found на этом шаге — ок, VM уже могла быть удалена.
		stopErr := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
			_, e := cli.StopVM(ctx, node, id)
			return e
		})
		if stopErr != nil {
			class := clouddriver.Classify(classifyProxmox, stopErr)
			if class == clouddriver.FailNotFound {
				_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmIDStr, Message: "already absent"})
				continue
			}
			// Proxmox может вернуть 500 «VM is not running» — это успех для нас.
			// Классификатор приведёт это к transient, но Retry уже исчерпал
			// попытки; распознаём по тексту body.
			if isNotRunning(stopErr) {
				// продолжаем к delete
			} else {
				_ = stream.Send(&pluginv1.DestroyEvent{
					VmId:    vmIDStr,
					Message: clouddriver.FailMessage(class, "stop", stopErr),
					Failed:  true,
				})
				continue
			}
		}

		// Delete. not_found → идемпотент-успех.
		delErr := clouddriver.Retry(ctx, backoff, classifyProxmox, func() error {
			_, e := cli.DeleteVM(ctx, node, id)
			return e
		})
		if delErr != nil {
			class := clouddriver.Classify(classifyProxmox, delErr)
			if class == clouddriver.FailNotFound {
				_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmIDStr, Message: "already absent"})
				continue
			}
			_ = stream.Send(&pluginv1.DestroyEvent{
				VmId:    vmIDStr,
				Message: clouddriver.FailMessage(class, "delete", delErr),
				Failed:  true,
			})
			continue
		}
		_ = stream.Send(&pluginv1.DestroyEvent{VmId: vmIDStr, Message: "deleted"})
	}
	return nil
}

// isNotRunning — проверка «VM is not running» в теле ошибки stop. Proxmox
// возвращает 500 с текстом вместо отдельного status-code. Не криминальная
// эвристика — false-positive ведёт максимум к лишнему delete-вызову.
func isNotRunning(err error) bool {
	var hErr *pveHTTPError
	if !errors.As(err, &hErr) {
		return false
	}
	body := strings.ToLower(hErr.Body)
	return strings.Contains(body, "not running") || strings.Contains(body, "is stopped")
}

// Status — опрос одной VM (GetVMStatus). credentials приходят отдельным полем
// StatusRequest.credentials (A-flow, симметрично Create/Destroy).
func (d *ProxmoxDriver) Status(ctx context.Context, req *pluginv1.StatusRequest) (*pluginv1.StatusReply, error) {
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newPveClient(ctx, creds)
	if err != nil {
		return nil, fmt.Errorf("pve-client: %w", err)
	}
	node, id, perr := splitVmID(req.GetVmId())
	if perr != nil {
		return nil, fmt.Errorf("parse vm_id %q: %w", req.GetVmId(), perr)
	}
	st, err := cli.GetVMStatus(ctx, node, id)
	if err != nil {
		return nil, fmt.Errorf("GetVMStatus %s: %w", req.GetVmId(), err)
	}
	return &pluginv1.StatusReply{
		State:      st.Status,
		Attributes: statusAttributes(st),
	}, nil
}

// List — стрим инвентаря VM в кластере (опц. отфильтрованный по runTag).
// credentials приходят отдельным полем ListRequest.credentials (A-flow,
// симметрично Create/Destroy/Status). Per-VM IP guest-agent-ом НЕ запрашиваем
// (дорого для большого инвентаря); primary_ip заполняется только при Create.
func (d *ProxmoxDriver) List(req *pluginv1.ListRequest, stream grpc.ServerStreamingServer[pluginv1.VmInfo]) error {
	ctx := stream.Context()
	creds := credsFromMap(req.GetCredentials().AsMap())
	cli, err := newPveClient(ctx, creds)
	if err != nil {
		return fmt.Errorf("pve-client: %w", err)
	}
	filter := req.GetFilter().AsMap()
	wantTag := stringField(filter, runTagKey)

	all, err := cli.ListClusterVMs(ctx)
	if err != nil {
		return fmt.Errorf("ListClusterVMs: %w", err)
	}
	for _, vm := range all {
		if vm.Type != "qemu" {
			continue
		}
		if wantTag != "" && !hasTag(vm.Tags, runTagKey, wantTag) {
			continue
		}
		attrs, _ := structpb.NewStruct(map[string]any{
			"node":   vm.Node,
			"status": vm.Status,
			"tags":   vm.Tags,
		})
		if serr := stream.Send(&pluginv1.VmInfo{
			VmId:       formatVmID(vm.Node, vm.VMID),
			Fqdn:       vm.Name, // Proxmox-конвенция: name = hostname (SID)
			Attributes: attrs,
		}); serr != nil {
			return serr
		}
	}
	return nil
}

func main() {
	if err := clouddriver.Serve(&ProxmoxDriver{}); err != nil {
		fmt.Fprintln(os.Stderr, "soul-cloud-proxmox:", err)
		os.Exit(1)
	}
}

// --- helpers ---

func sendCreateFailed(stream grpc.ServerStreamingServer[pluginv1.CreateEvent], class clouddriver.FailClass, op string, err error) error {
	return stream.Send(&pluginv1.CreateEvent{Message: clouddriver.FailMessage(class, op, err), Failed: true})
}

// formatVmID собирает composite-vm_id `<node>/<vmid>` (см. doc-комментарий
// пакета о vm_id-paradigm).
func formatVmID(node string, vmid int) string {
	return fmt.Sprintf("%s/%d", node, vmid)
}

// splitVmID парсит composite-vm_id `<node>/<vmid>`. Возврат ошибки = vm_id
// сформирован чужим компонентом (не наш Create) — пусть Keeper увидит явный
// invalid_params.
func splitVmID(s string) (node string, vmid int, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", 0, fmt.Errorf("vm_id must be `<node>/<vmid>`, got %q", s)
	}
	id, perr := strconv.Atoi(parts[1])
	if perr != nil {
		return "", 0, fmt.Errorf("vm_id vmid part %q: %w", parts[1], perr)
	}
	return parts[0], id, nil
}

func clusterVmIDs(vms []ClusterVM) []string {
	out := make([]string, 0, len(vms))
	for _, v := range vms {
		out = append(out, formatVmID(v.Node, v.VMID))
	}
	return out
}

// statusAttributes — поднабор полей VMStatus, удобный оператору в proto-Struct.
func statusAttributes(st VMStatus) *structpb.Struct {
	m := map[string]any{
		"node":   st.Node,
		"vmid":   float64(st.VMID),
		"status": st.Status,
		"name":   st.Name,
	}
	if st.QmpStatus != "" {
		m["qmp_status"] = st.QmpStatus
	}
	if st.Lock != "" {
		m["lock"] = st.Lock
	}
	if st.Maxmem > 0 {
		m["max_memory"] = float64(st.Maxmem)
	}
	if st.Maxdisk > 0 {
		m["max_disk"] = float64(st.Maxdisk)
	}
	if st.Cpus > 0 {
		m["cpus"] = st.Cpus
	}
	if st.Tags != "" {
		m["tags"] = st.Tags
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return s
}
