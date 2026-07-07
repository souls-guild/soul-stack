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
	"regexp"

	"github.com/souls-guild/soul-stack/keeper/internal/bootstraptoken"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	keepersoul "github.com/souls-guild/soul-stack/keeper/internal/soul"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// VMNameBasePattern — форма base-имени VM (param `name`, self-onboard Вариант T).
// Единый источник паттерна: NIM-58 guard-assert в provision-телах сверяет
// incarnation.name байт-в-байт против этого литерала (nameguard_pin_test).
// lowercase-alnum + дефисы, старт/конец alnum, 1..50 — драйвер добавит `-<index>`,
// FQDN=`<name>-<index>.<suffix>` обязан пройти SID-валидацию.
const VMNameBasePattern = `^[a-z][a-z0-9-]{0,48}[a-z0-9]$`

var VMNameBaseRe = regexp.MustCompile(VMNameBasePattern)

// ValidVMNameBase проверяет base-имя VM на соответствие [VMNameBasePattern].
func ValidVMNameBase(name string) bool { return VMNameBaseRe.MatchString(name) }

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

// SoulStore — узкое подмножество для INSERT в souls. DeleteBySID нужен для
// orphan-cleanup self-onboard (Вариант T): souls вставляются ДО create, и при
// провале create/сверки их надо откатить (иначе rerun упрётся в PK-конфликт).
type SoulStore interface {
	Insert(ctx context.Context, soul *keepersoul.Soul) error
	UpdateStatus(ctx context.Context, sid string, status keepersoul.Status, kid *string) error
	DeleteBySID(ctx context.Context, sid string) error
}

// TokenStore — узкое подмножество для INSERT в bootstrap_tokens. DeleteByTokenID
// нужен для orphan-cleanup self-onboard (см. SoulStore): токены выписываются ДО
// create и откатываются при провале.
type TokenStore interface {
	Generate() (bootstraptoken.PlainToken, error)
	Insert(ctx context.Context, sid, tokenHash string, createdByAID *string) (*bootstraptoken.Record, error)
	DeleteByTokenID(ctx context.Context, tokenID string) error
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
//
// GenerateUserdataSelfOnboard — рендер userdata с запечёнными per-VM токенами
// для self-onboard «Вариант T» (ADR-017(h) amendment): keeper предсказывает FQDN
// VM ДО create и передаёт map FQDN→plain-token; cloud-init на VM выбирает свой
// токен по hostname и онбордится в один цикл (без claim/keeper.push).
type UserdataProvider interface {
	GenerateUserdata(ctx context.Context) (string, error)
	GenerateUserdataSelfOnboard(ctx context.Context, tokens map[string]string) (string, error)
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
		// name — базовое имя VM-батча (self-onboard Вариант T, ADR-017(h)): keeper
		// передаёт его в CreateRequest.name, драйвер именует `<name>-<index>`,
		// FQDN=`<name>-<index>.<suffix>` предсказуем. Валидируется как имя-фрагмент
		// (тот же паттерн, что SID-labels; провайдер-специфичные ограничения
		// проверяет драйвер).
		name, nerr := util.OptStringParam(req.Params, "name")
		if nerr != nil {
			errs = append(errs, nerr.Error())
		} else if name != "" && !ValidVMNameBase(name) {
			errs = append(errs, fmt.Sprintf("param %q: %q must match %s (VM-name base for predictable FQDN)", "name", name, VMNameBasePattern))
		}
		// self_onboard: true — VM онбордится сама из cloud-init (Вариант T):
		// требует и name (для предсказания FQDN), и generate_userdata-путь (токены
		// в userdata). Явный `userdata:` с self_onboard несовместим (нам надо
		// самим запечь токены). generate_userdata тут НЕ обязателен как флаг —
		// self_onboard подразумевает рендер userdata с токенами (см. applyCreated).
		selfOnboard, _, serr := util.OptBoolParam(req.Params, "self_onboard")
		if serr != nil {
			errs = append(errs, serr.Error())
		}
		if selfOnboard {
			if name == "" {
				errs = append(errs, "param \"self_onboard: true\" requires \"name\" (base VM name for predictable FQDN)")
			}
			if userdata != "" {
				errs = append(errs, "params \"userdata\" and \"self_onboard: true\" are mutually exclusive (self-onboard renders userdata with per-VM tokens)")
			}
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
//
// Два режима userdata-доставки токена:
//   - B-flat (default): create → per-VM токены выписываются ПОСЛЕ (SID=FQDN из
//     ответа драйвера) → plain кладётся в register (доставка отдельным шагом).
//   - self-onboard «Вариант T» (ADR-017(h) amendment, `self_onboard: true`):
//     keeper ПРЕДСКАЗЫВАЕТ FQDN (`<name>-<i>.<suffix>`) ДО create, выписывает
//     токены и запекает их в userdata; VM онбордится сама. Plain в register НЕ
//     кладётся (доставки нет). CreateRequest.name = base-имя (драйвер именует
//     `<name>-<i>`).
func (m *Module) applyCreated(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()
	provider, err := util.StringParam(req.Params, "provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
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
	generate, _, err := util.OptBoolParam(req.Params, "generate_userdata")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	name, err := util.OptStringParam(req.Params, "name")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	selfOnboard, _, err := util.OptBoolParam(req.Params, "self_onboard")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if generate && userdata != "" {
		return util.SendFailed(stream, "cloud created: params \"userdata\" and \"generate_userdata: true\" are mutually exclusive (set one or the other)")
	}
	if selfOnboard {
		if name == "" {
			return util.SendFailed(stream, "cloud created: self_onboard=true requires \"name\" (base VM name for predictable FQDN)")
		}
		if userdata != "" {
			return util.SendFailed(stream, "cloud created: params \"userdata\" and \"self_onboard: true\" are mutually exclusive")
		}
	}
	if generate && !selfOnboard {
		if m.Userdata == nil {
			return util.SendFailed(stream, "cloud created: generate_userdata=true but no UserdataProvider configured (set keeper.yml cloud_init block + wire cloudinit.Resolver in main)")
		}
		rendered, gerr := m.Userdata.GenerateUserdata(ctx)
		if gerr != nil {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: generate userdata: %s", maskErr(gerr)))
		}
		userdata = rendered
	}

	// A-flow: Keeper резолвит Provider-реестр в driver-имя + plain-credentials
	// (+ FQDNSuffix для self-onboard-предсказания) и Profile-реестр в VM-spec params.
	if m.Resolver == nil {
		return util.SendFailed(stream, "cloud created: provider resolver not configured (wire CredentialsResolverPG in main)")
	}
	var profileMap map[string]any
	if profileName != "" {
		profileMap, err = m.Resolver.ResolveProfile(ctx, profileName)
		if err != nil {
			return util.SendFailed(stream, fmt.Sprintf("resolve profile %q: %s", profileName, maskErr(err)))
		}
	}
	resolved, err := m.Resolver.Resolve(ctx, provider)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("resolve provider %q: %s", provider, maskErr(err)))
	}

	if selfOnboard {
		return m.applyCreatedSelfOnboard(ctx, stream, provider, name, int(count), resolved, profileMap)
	}

	vms, err := m.Plugins.Create(ctx, resolved.Driver, profileMap, resolved.Credentials, int32(count), userdata, name)
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

	if werr := m.writeCreatedAudit(ctx, provider, len(vms), vmIDs); werr != nil {
		return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
	}

	return util.SendFinal(stream, true, map[string]any{
		"hosts":  hosts,
		"count":  float64(len(vms)),
		"vm_ids": vmIDs,
		"action": StateCreated,
	})
}

// applyCreatedSelfOnboard — ветка self_onboard=true (Вариант T, ADR-017(h)):
// keeper предсказывает FQDN VM ДО create, выписывает per-VM токены, запекает их в
// userdata, создаёт VM (передавая base-имя в CreateRequest.name) и сверяет
// фактические FQDN с предсказанными. Plain-токены в register НЕ кладутся —
// доставки нет, VM онбордится из cloud-init.
func (m *Module) applyCreatedSelfOnboard(ctx context.Context, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent], provider, name string, count int, resolved *ResolvedProvider, profileMap map[string]any) error {
	if resolved.FQDNSuffix == "" {
		return util.SendFailed(stream, fmt.Sprintf("cloud created: self_onboard requires provider %q to have fqdn_suffix (keeper predicts FQDN=<name>-<i>.<suffix>); set providers.fqdn_suffix", provider))
	}
	if m.Userdata == nil {
		return util.SendFailed(stream, "cloud created: self_onboard=true but no UserdataProvider configured (set keeper.yml cloud_init block + wire cloudinit.Resolver in main)")
	}

	// Souls (pending) + токены выписываются ДО create — потому любой провал ПОСЛЕ
	// вставки (create-fail, пустой/несовпавший FQDN, ошибка userdata-render)
	// оставил бы осиротевшие записи: presence-барьер await_online завис бы на
	// онбординге несуществующих VM, а rerun-last упёрся бы в PK-конфликт insert
	// soul под тем же предсказанным FQDN. Накопленные записи откатываем defer-ом,
	// если не дошли до успешного финала (флаг success). На успехе — оставляем.
	//
	// DeleteBySID каскадит на bootstrap_tokens (FK ON DELETE CASCADE, миграции
	// 008/009), но токен откатываем и явно — не полагаемся на схему БД и покрываем
	// случай soul-insert-ok / token-insert-fail.
	type provisionedRecord struct{ sid, tokenID string }
	var provisioned []provisionedRecord
	success := false
	defer func() {
		if success {
			return
		}
		for _, rec := range provisioned {
			// Best-effort: terminal failed-event уже отправлен, ошибки отката
			// пойти в стрим не могут.
			// TODO(prod): логировать неудачный откат в OTel/лог daemon-а — на
			// unit-уровне модуль не имеет logger-а; орфан после провала отката
			// подберёт Reaper (purge_souls) по возрасту pending-записи.
			_ = m.Tokens.DeleteByTokenID(ctx, rec.tokenID)
			_ = m.Souls.DeleteBySID(ctx, rec.sid)
		}
	}()

	// Предсказываем FQDN каждой VM и выписываем токен. Souls (pending) + токены
	// (hash в БД) создаются ДО create — presence-барьер await_online потом ждёт
	// их онбординга. Токены копим в map FQDN→plain для запекания в userdata.
	predicted := make([]string, count)
	tokens := make(map[string]string, count)
	for i := 0; i < count; i++ {
		sid := fmt.Sprintf("%s-%d.%s", name, i, resolved.FQDNSuffix)
		if !keepersoul.ValidSID(sid) {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: predicted FQDN %q is not a valid SID (check name/fqdn_suffix)", sid))
		}
		predicted[i] = sid

		soul := &keepersoul.Soul{SID: sid, Transport: keepersoul.TransportAgent, Status: keepersoul.StatusPending}
		if err := m.Souls.Insert(ctx, soul); err != nil {
			return util.SendFailed(stream, fmt.Sprintf("insert soul %q: %v", sid, err))
		}
		tok, err := m.Tokens.Generate()
		if err != nil {
			// soul уже вставлен — откатим его defer-ом (token-id пуст, DeleteByTokenID
			// на несуществующий id безопасен).
			provisioned = append(provisioned, provisionedRecord{sid: sid})
			return util.SendFailed(stream, fmt.Sprintf("generate bootstrap token for %q: %v", sid, err))
		}
		rec, err := m.Tokens.Insert(ctx, sid, tok.Hash(), nil)
		if err != nil {
			provisioned = append(provisioned, provisionedRecord{sid: sid})
			return util.SendFailed(stream, fmt.Sprintf("insert bootstrap token for %q: %v", sid, err))
		}
		provisioned = append(provisioned, provisionedRecord{sid: sid, tokenID: rec.TokenID})
		tokens[sid] = tok.Reveal()
	}

	userdata, err := m.Userdata.GenerateUserdataSelfOnboard(ctx, tokens)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud created: generate self-onboard userdata: %s", maskErr(err)))
	}

	vms, err := m.Plugins.Create(ctx, resolved.Driver, profileMap, resolved.Credentials, int32(count), userdata, name)
	if err != nil {
		return util.SendFailed(stream, fmt.Sprintf("cloud create via provider %q: %s", provider, maskErr(err)))
	}

	// Сверяем фактические FQDN с предсказанными: если провайдер назвал VM иначе,
	// self-onboard сломан молча (токен в userdata под предсказанным FQDN, а VM
	// имеет другой hostname → soul init не найдёт токен). Fail-fast, чтобы не
	// уходить в вечное ожидание presence-барьера.
	predictedSet := make(map[string]bool, len(predicted))
	for _, f := range predicted {
		predictedSet[f] = true
	}
	hosts := make([]any, 0, len(vms))
	vmIDs := make([]any, 0, len(vms))
	for _, vm := range vms {
		sid := vm.GetFqdn()
		if sid == "" {
			return util.SendFailed(stream, fmt.Sprintf("provider %q returned VM %q without fqdn (cannot use as SID)", provider, vm.GetVmId()))
		}
		if !predictedSet[sid] {
			return util.SendFailed(stream, fmt.Sprintf("cloud created: self_onboard: provider %q named VM %q, not among predicted FQDN %v — token in userdata will not match VM hostname (driver must honor CreateRequest.name)", provider, sid, predicted))
		}
		// БЕЗ bootstrap_token в register: self-onboard доставил токен через userdata,
		// доставки отдельным шагом нет. Онбординг идёт из cloud-init на VM.
		hostEntry := map[string]any{
			"sid":        sid,
			"vm_id":      vm.GetVmId(),
			"primary_ip": vm.GetPrimaryIp(),
		}
		if attrs := vm.GetAttributes(); attrs != nil {
			hostEntry["attributes"] = attrs.AsMap()
		}
		hosts = append(hosts, hostEntry)
		vmIDs = append(vmIDs, vm.GetVmId())
	}

	if werr := m.writeCreatedAudit(ctx, provider, len(vms), vmIDs); werr != nil {
		return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
	}

	// Дошли до успешного финала — souls/токены остаются, defer-cleanup не трогает.
	success = true
	return util.SendFinal(stream, true, map[string]any{
		"hosts":        hosts,
		"count":        float64(len(vms)),
		"vm_ids":       vmIDs,
		"action":       StateCreated,
		"self_onboard": true,
	})
}

// writeCreatedAudit пишет audit-event `cloud.provisioned` для created-фазы
// (общий для B-flat и self-onboard путей). nil Audit → no-op.
func (m *Module) writeCreatedAudit(ctx context.Context, provider string, n int, vmIDs []any) error {
	if m.Audit == nil {
		return nil
	}
	return m.Audit.Write(ctx, &audit.Event{
		EventType: audit.EventCloudProvisioned,
		Source:    audit.SourceKeeperInternal,
		Payload: map[string]any{
			"action":   StateCreated,
			"provider": provider,
			"count":    float64(n),
			"vm_ids":   vmIDs,
		},
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
