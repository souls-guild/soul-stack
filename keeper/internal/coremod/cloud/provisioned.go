// Package cloud реализует keeper-side core-модуль `core.cloud.provisioned`
// (ADR-017, docs/keeper/cloud.md).
//
// Состояния:
//   - created: CloudDriver.Create через PluginHost → []VmInfo → INSERT в
//     souls (status: pending) + INSERT bootstrap_tokens (один на VM).
//     Output: hosts: [{sid, vm_id, primary_ip, attributes}].
//   - destroyed: PluginHost.Destroy(vm_ids) → cascade одной PG-транзакцией
//     (ADR-017 cascade): souls→destroyed + active soul_seeds→orphaned +
//     active bootstrap_tokens→burned. Output: destroyed_vm_ids + sids +
//     cascade-counts.
//   - resized: PluginHost.Resize(vm_ids, desired) — драйвер расширяет ресурсы
//     VM (cpu/ram/disk, наши единицы). Keeper-агностичен к stop/start: всю
//     последовательность инкапсулирует драйвер. БД не трогается (resize не
//     меняет реестр souls). Output: results[{vm_id, caused_downtime, error}].
//     Драйвер без Resizable-capability → resize.unsupported.
//
// Заменяет паттерн «destiny `cloud-provision` с `on: keeper`» (ADR-017):
// это keeper-side операция, не пакет задач для Soul.
package cloud

import (
	"context"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name — base-имя модуля без state-суффикса (ключ Registry). Author-форма
// адреса задачи — `core.cloud.created` / `core.cloud.destroyed` (base + state);
// state приходит в pluginv1.ApplyRequest.state и диспетчеризуется в Apply.
const Name = "core.cloud"

// Состояния модуля.
const (
	StateCreated   = "created"
	StateDestroyed = "destroyed"
	StateResized   = "resized"
)

// SoulStore — узкое подмножество для INSERT в souls.
type SoulStore interface {
	Insert(ctx context.Context, soul *keepersoul.Soul) error
	UpdateStatus(ctx context.Context, sid string, status keepersoul.Status, kid *string) error
}

// TokenStore — узкое подмножество для INSERT в bootstrap_tokens.
type TokenStore interface {
	Generate() (bootstraptoken.PlainToken, error)
	Insert(ctx context.Context, sid, tokenHash string, createdByAID *string) (*bootstraptoken.Record, error)
}

// AuditWriter — для audit-event-а `cloud.provisioned`.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Cascader — cascade-обработчик `destroyed`-state (ADR-017 cascade):
// одна PG-транзакция, переводящая souls/soul_seeds/bootstrap_tokens в
// терминальные состояния. Прод-реализация — [CascadePG] поверх pgxpool.Pool;
// для unit-тестов модуля — fake (см. provisioned_test.go).
//
// Может быть nil в wire-сборках без PG (тогда destroyed-state валит
// scenario с понятной ошибкой; см. applyDestroyed).
type Cascader interface {
	CascadeDestroy(ctx context.Context, sids []string, usedByKID string) (CascadeCounts, error)
}

// UserdataProvider — резолвер cloud-init userdata по параметру scenario
// `generate_userdata: true` (ADR-017(h) amendment 2026-05-27, B-flat).
// Реализация — keeper/internal/cloudinit.Resolver+GenerateUserdata в обёртке
// daemon-а: читает текущий snapshot KeeperConfig.CloudInit (hot-reload через
// config.Store.Get) и Vault.ReadKV для PEM CA. Возвращает готовый cloud-config
// YAML без секретов.
//
// Cross-package изоляция через interface: cloud-модуль не знает про cloudinit-
// пакет, тестируется на fake-провайдере.
//
// Может быть nil — тогда `generate_userdata: true` вернёт ошибку «не сконфигурирован»;
// явный `userdata: "..."` продолжает работать без UserdataProvider-а.
type UserdataProvider interface {
	GenerateUserdata(ctx context.Context) (string, error)
}

// Module — реализация sdk/module.SoulModule.
type Module struct {
	Plugins  PluginHost
	Resolver ProviderResolver
	Souls    SoulStore
	Tokens   TokenStore
	Cascade  Cascader
	Audit    AuditWriter
	Userdata UserdataProvider
}

// New — wire-helper. `cascade` может быть nil в тестовых сборках, где
// destroyed-state не используется; applyDestroyed отдаст явную ошибку.
// `resolver` обязателен: и created, и destroyed резолвят Provider-реестр в
// driver-имя + credentials (A-flow); nil даст явную ошибку при Apply.
//
// UserdataProvider не подаётся через New (опциональная зависимость,
// добавилась после фиксации первых 6 cloud-провайдеров) — wire-up идёт
// через прямое присваивание поля или через [Module.WithUserdata].
func New(p PluginHost, r ProviderResolver, s SoulStore, t TokenStore, c Cascader, a AuditWriter) *Module {
	return &Module{Plugins: p, Resolver: r, Souls: s, Tokens: t, Cascade: c, Audit: a}
}

// WithUserdata возвращает копию модуля с прокинутым UserdataProvider. Удобно
// в wire-up daemon-а (`coremod.Default(...).Lookup("core.cloud.provisioned")
// .WithUserdata(...)`), не ломая существующие callsite-ы [New].
func (m *Module) WithUserdata(p UserdataProvider) *Module {
	cp := *m
	cp.Userdata = p
	return &cp
}

func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	switch req.State {
	case StateCreated:
		if _, err := util.StringParam(req.Params, "provider"); err != nil {
			errs = append(errs, err.Error())
		}
		// profile = ИМЯ Profile-я в реестре profiles (Вариант A, ADR-017
		// amendment 2026-06-29). Опциональный; пустое/отсутствует → VM без
		// реестрового spec. Резолв имени → params — в applyCreated.
		if _, err := util.OptStringParam(req.Params, "profile"); err != nil {
			errs = append(errs, err.Error())
		}
		if n, ok, err := util.OptIntParam(req.Params, "count"); err != nil {
			errs = append(errs, err.Error())
		} else if ok && n < 1 {
			errs = append(errs, "param \"count\": must be >= 1")
		}
		// userdata + generate_userdata — type-check + mutually-exclusive проверка
		// (ADR-017(h) amendment 2026-05-27, B-flat).
		userdata, uerr := util.OptStringParam(req.Params, "userdata")
		if uerr != nil {
			errs = append(errs, uerr.Error())
		}
		gen, _, gerr := util.OptBoolParam(req.Params, "generate_userdata")
		if gerr != nil {
			errs = append(errs, gerr.Error())
		}
		if gen && userdata != "" {
			errs = append(errs, "params \"userdata\" and \"generate_userdata: true\" are mutually exclusive")
		}
	case StateDestroyed:
		if _, err := util.StringParam(req.Params, "provider"); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := util.StringSliceParam(req.Params, "vm_ids"); err != nil {
			errs = append(errs, err.Error())
		}
	case StateResized:
		if _, err := util.StringParam(req.Params, "provider"); err != nil {
			errs = append(errs, err.Error())
		}
		if _, err := util.StringSliceParam(req.Params, "vm_ids"); err != nil {
			errs = append(errs, err.Error())
		}
		// desired — обязательный объект; хотя бы одно измерение (cpu/ram/disk)
		// должно быть задано (>0), иначе resize — бессмысленный no-op.
		if _, _, _, derr := parseDesired(req.Params); derr != nil {
			errs = append(errs, derr.Error())
		}
		if _, _, aerr := util.OptBoolParam(req.Params, "allow_downtime"); aerr != nil {
			errs = append(errs, aerr.Error())
		}
	default:
		errs = append(errs, fmt.Sprintf("unknown state %q (want created/destroyed/resized)", req.State))
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	switch req.State {
	case StateCreated:
		return m.applyCreated(req, stream)
	case StateDestroyed:
		return m.applyDestroyed(req, stream)
	case StateResized:
		return m.applyResized(req, stream)
	default:
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
}

// maskErr маскирует возможный leak секретов в тексте ошибки перед выдачей в
// failed-event (он уходит в status_details/error_summary, наблюдаемый канал).
// Резолв credentials может породить error со встроенным vault-ref
// (`vault:secret/...`) — прогоняем строку через тот же substring-фильтр
// shared/audit, что чистит register-output. Ключ `_` несекретный — сработает
// только vault-ref-фильтр по значению.
func maskErr(err error) string {
	if err == nil {
		return ""
	}
	masked := audit.MaskSecrets(map[string]any{"_": err.Error()})
	if s, ok := masked["_"].(string); ok {
		return s
	}
	return "***MASKED***"
}

// applyCreated реализует state=created. См. doc-комментарий пакета.
func (m *Module) applyCreated(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	provider, err := util.StringParam(req.Params, "provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// profile — ИМЯ Profile-я в реестре profiles (Вариант A, ADR-017 amendment
	// 2026-06-29): keeper резолвит его в VM-spec params через
	// Resolver.ResolveProfile, симметрично provider→credentials. Опциональный:
	// пустое/отсутствует → VM создаётся без реестрового spec.
	profileName, err := util.OptStringParam(req.Params, "profile")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	count, ok, err := util.OptIntParam(req.Params, "count")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !ok {
		count = 1
	}
	if count < 1 {
		return util.SendFailed(stream, "count must be >= 1")
	}

	userdata, err := util.OptStringParam(req.Params, "userdata")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// generate_userdata: true — рендер cloud-init userdata из keeper.yml::cloud_init
	// (ADR-017(h) amendment 2026-05-27, B-flat). Mutually exclusive с явным
	// `userdata:` — иначе непонятно, что отправлять провайдеру.
	generate, _, err := util.OptBoolParam(req.Params, "generate_userdata")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if generate && userdata != "" {
		return util.SendFailed(stream, "cloud created: params \"userdata\" and \"generate_userdata: true\" are mutually exclusive (set one or the other)")
	}
	if generate {
		if m.Userdata == nil {
			return util.SendFailed(stream, "cloud created: generate_userdata=true but no UserdataProvider configured (set keeper.yml cloud_init block + wire cloudinit.Resolver in main)")
		}
		rendered, err := m.Userdata.GenerateUserdata(ctx)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: generate userdata: %s", maskErr(err)))
		}
		userdata = rendered
	}

	// A-flow: Keeper резолвит Provider-реестр в driver-имя + plain-credentials
	// (секрет из Vault по credentials_ref + region) и Profile-реестр (param
	// `profile` = ИМЯ) в VM-spec params. Драйвер в Vault/реестр не ходит.
	if m.Resolver == nil {
		return util.SendFailed(stream, "cloud created: provider resolver not configured (wire CredentialsResolverPG in main)")
	}

	// profile опционален: имя задано → резолвим в params; пусто → nil (VM без
	// реестрового spec, прежняя optional-семантика). Имя не в реестре →
	// SendFailed (не nil-panic).
	var profileMap map[string]any
	if profileName != "" {
		profileMap, err = m.Resolver.ResolveProfile(ctx, profileName)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("resolve profile %q: %s", profileName, maskErr(err)))
		}
	}

	resolved, err := m.Resolver.Resolve(ctx, provider)
	if err != nil {
		// Сообщение резолвера может нести vault-ref — маскируем перед выдачей.
		return util.SendFailed(stream, fmt.Sprintf("resolve provider %q: %s", provider, maskErr(err)))
	}

	vms, err := m.Plugins.Create(ctx, resolved.Driver, profileMap, resolved.Credentials, int32(count), userdata)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud create via provider %q: %s", provider, maskErr(err)))
	}

	hosts := make([]any, 0, len(vms))
	vmIDs := make([]any, 0, len(vms))
	for _, vm := range vms {
		sid := vm.GetFqdn()
		if sid == "" {
			return util.SendFailed(stream, fmt.Sprintf("provider %q returned VM %q without fqdn (cannot use as SID)", provider, vm.GetVmId()))
		}
		soul := &keepersoul.Soul{
			SID:       sid,
			Transport: keepersoul.TransportAgent,
			Status:    keepersoul.StatusPending,
		}
		if err := m.Souls.Insert(ctx, soul); err != nil {
			return util.SendFailed(stream, fmt.Sprintf("insert soul %q: %v", sid, err))
		}
		tok, err := m.Tokens.Generate()
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("generate bootstrap token for %q: %v", sid, err))
		}
		if _, err := m.Tokens.Insert(ctx, sid, tok.Hash(), nil); err != nil {
			return util.SendFailed(stream, fmt.Sprintf("insert bootstrap token for %q: %v", sid, err))
		}

		hostEntry := map[string]any{
			"sid":        sid,
			"vm_id":      vm.GetVmId(),
			"primary_ip": vm.GetPrimaryIp(),
		}
		if attrs := vm.GetAttributes(); attrs != nil {
			hostEntry["attributes"] = attrs.AsMap()
		}
		// ВНИМАНИЕ (security, H1): plain bootstrap-token намеренно кладётся в
		// register-output — cloud-init flow требует передать его на VM при
		// первичной загрузке. Это единственный момент, когда plain-token
		// виден; в БД хранится только hash, дальше токен не восстановим.
		//
		// Секретность ключа `bootstrap_token` обеспечивается на ВСЕХ выходах
		// register-output (audit-log / OTel / SSE / любые логи): ключ матчит
		// substring-фильтр [audit.MaskSecrets] (`token`-фрагмент). Любой новый
		// канал вывода register-output ОБЯЗАН прогонять payload через
		// audit.MaskSecrets — иначе action one-time token leak (см.
		// .pm/tasks/2026-05-22-security-review). Менять имя ключа без проверки
		// фильтра нельзя.
		hostEntry["bootstrap_token"] = tok.Reveal()
		hosts = append(hosts, hostEntry)
		vmIDs = append(vmIDs, vm.GetVmId())
	}

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventCloudProvisioned,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":   StateCreated,
				"provider": provider,
				"count":    float64(len(vms)),
				"vm_ids":   vmIDs,
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	return util.SendFinal(stream, true, map[string]any{
		"hosts":  hosts,
		"count":  float64(len(vms)),
		"vm_ids": vmIDs,
		"action": StateCreated,
	})
}

// applyDestroyed реализует state=destroyed. См. doc-комментарий пакета.
//
// Cascade (ADR-017) выполняется ПОСЛЕ успешного PluginHost.Destroy: если
// cloud-destroy провалился, реестры остаются нетронутыми — хост ещё «жив»
// с точки зрения cloud-провайдера, переводить souls→destroyed преждевременно.
func (m *Module) applyDestroyed(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	provider, err := util.StringParam(req.Params, "provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	vmIDs, err := util.StringSliceParam(req.Params, "vm_ids")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	// Опциональный список SID для cascade (per-VM связка sid↔vm_id
	// держится caller-ом: cloud-driver не знает наш SID, мы не знаем
	// provider-vm-id-mapping).
	sids, err := util.OptStringSliceParam(req.Params, "sids")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	if m.Resolver == nil {
		return util.SendFailed(stream, "cloud destroyed: provider resolver not configured (wire CredentialsResolverPG in main)")
	}
	resolved, err := m.Resolver.Resolve(ctx, provider)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("resolve provider %q: %s", provider, maskErr(err)))
	}

	destroyed, err := m.Plugins.Destroy(ctx, resolved.Driver, resolved.Credentials, vmIDs)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud destroy via provider %q: %s", provider, maskErr(err)))
	}

	var counts CascadeCounts
	if len(sids) > 0 {
		if m.Cascade == nil {
			return util.SendFailed(stream, "cloud destroyed: cascade store not configured (wire CascadePG in main)")
		}
		counts, err = m.Cascade.CascadeDestroy(ctx, sids, bootstraptoken.SystemKIDCloudDestroy)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("cascade destroy: %v", err))
		}
	}

	destroyedAny := make([]any, len(destroyed))
	for i, id := range destroyed {
		destroyedAny[i] = id
	}
	sidsAny := make([]any, len(sids))
	for i, s := range sids {
		sidsAny[i] = s
	}

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventCloudProvisioned,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":         StateDestroyed,
				"provider":       provider,
				"vm_ids":         destroyedAny,
				"sids":           sidsAny,
				"souls_updated":  float64(counts.SoulsUpdated),
				"seeds_orphaned": float64(counts.SeedsOrphaned),
				"tokens_burned":  float64(counts.TokensBurned),
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	return util.SendFinal(stream, len(destroyed) > 0, map[string]any{
		"action":         StateDestroyed,
		"vm_ids":         destroyedAny,
		"sids":           sidsAny,
		"destroyed_n":    float64(len(destroyed)),
		"souls_updated":  float64(counts.SoulsUpdated),
		"seeds_orphaned": float64(counts.SeedsOrphaned),
		"tokens_burned":  float64(counts.TokensBurned),
	})
}

// parseDesired извлекает целевые ресурсы из params.desired (наши единицы:
// cpu=ядра / ram_mb=МБ / disk_gb=ГБ). Все поля опциональные, но хотя бы одно
// должно быть задано (>0) — иначе resize бессмысленен. Возвращает значения
// (0 = не менять) + ошибку валидации (отсутствует desired / неверный тип /
// все нули / отрицательные).
func parseDesired(params *structpb.Struct) (cpu int32, ramMB, diskGB int64, err error) {
	desired, derr := util.OptStructParam(params, "desired")
	if derr != nil {
		return 0, 0, 0, derr
	}
	if desired == nil {
		return 0, 0, 0, fmt.Errorf("param %q: missing (resize requires target resources)", "desired")
	}
	cpu64, _, cerr := util.OptIntParam(desired, "cpu_cores")
	if cerr != nil {
		return 0, 0, 0, cerr
	}
	ramMB, _, rerr := util.OptIntParam(desired, "ram_mb")
	if rerr != nil {
		return 0, 0, 0, rerr
	}
	diskGB, _, gerr := util.OptIntParam(desired, "disk_gb")
	if gerr != nil {
		return 0, 0, 0, gerr
	}
	if cpu64 < 0 || ramMB < 0 || diskGB < 0 {
		return 0, 0, 0, fmt.Errorf("param %q: cpu_cores/ram_mb/disk_gb must be >= 0", "desired")
	}
	if cpu64 == 0 && ramMB == 0 && diskGB == 0 {
		return 0, 0, 0, fmt.Errorf("param %q: at least one of cpu_cores/ram_mb/disk_gb must be > 0", "desired")
	}
	return int32(cpu64), ramMB, diskGB, nil
}

// applyResized реализует state=resized. См. doc-комментарий пакета. БД не
// трогается: resize меняет ресурсы VM, но не реестр souls.
func (m *Module) applyResized(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	provider, err := util.StringParam(req.Params, "provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	vmIDs, err := util.StringSliceParam(req.Params, "vm_ids")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	cpu, ramMB, diskGB, err := parseDesired(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	allowDowntime, _, err := util.OptBoolParam(req.Params, "allow_downtime")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}

	if m.Resolver == nil {
		return util.SendFailed(stream, "cloud resized: provider resolver not configured (wire CredentialsResolverPG in main)")
	}
	resolved, err := m.Resolver.Resolve(ctx, provider)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("resolve provider %q: %s", provider, maskErr(err)))
	}

	desired := &pluginv1.ResizeSpec{CpuCores: cpu, RamMb: ramMB, DiskGb: diskGB}
	results, err := m.Plugins.Resize(ctx, resolved.Driver, resolved.Credentials, vmIDs, desired, allowDowntime)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud resize via provider %q: %s", provider, maskErr(err)))
	}

	resultsOut := make([]any, 0, len(results))
	causedDowntime := false
	var perVMErrors []string
	for _, r := range results {
		entry := map[string]any{
			"vm_id":           r.GetVmId(),
			"caused_downtime": r.GetCausedDowntime(),
		}
		if e := r.GetError(); e != "" {
			entry["error"] = e
			perVMErrors = append(perVMErrors, fmt.Sprintf("%s: %s", r.GetVmId(), e))
		}
		if r.GetCausedDowntime() {
			causedDowntime = true
		}
		resultsOut = append(resultsOut, entry)
	}

	if m.Audit != nil {
		ev := &audit.Event{
			EventType: audit.EventCloudProvisioned,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":          StateResized,
				"provider":        provider,
				"vm_ids":          toAnySlice(vmIDs),
				"cpu_cores":       float64(cpu),
				"ram_mb":          float64(ramMB),
				"disk_gb":         float64(diskGB),
				"caused_downtime": causedDowntime,
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	// changed=true: resize всегда меняет ресурс (идемпотентность к целевому
	// размеру — задача драйвера; на уровне модуля считаем resize изменяющей
	// операцией). Per-VM ошибки не валят весь шаг (часть VM могла зарезайзиться),
	// но попадают в output для наблюдаемости.
	out := map[string]any{
		"action":          StateResized,
		"vm_ids":          toAnySlice(vmIDs),
		"results":         resultsOut,
		"caused_downtime": causedDowntime,
	}
	if len(perVMErrors) > 0 {
		out["errors"] = toAnySlice(perVMErrors)
	}
	return util.SendFinal(stream, true, out)
}

// toAnySlice конвертирует []string в []any для structpb-output.
func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
