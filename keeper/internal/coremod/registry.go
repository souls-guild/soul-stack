// Package coremod — соединяет keeper-side core-модули (ADR-017,
// docs/keeper/modules.md) в единый Registry.
//
// Модули (ключ Registry = base-имя, author-форма = base + state в адресе):
// `core.soul` (`core.soul.registered`, docs/keeper/modules.md), `core.cloud`
// (`core.cloud.created`/`core.cloud.destroyed`, ADR-017(a), Plugin.d-pending),
// `core.vault` (`core.vault.kv-read`/`core.vault.kv-present`, ADR-017(b)) и `core.choir`
// (`core.choir.present`/`core.choir.absent`, ADR-044 — правка членства Voice в
// Choir-е инкарнации, регистрируется при наличии Deps.ChoirStore). Все
// исполняются на keeper-инстансе, диспетчер scenario-runner-а — `on: keeper`.
//
// Симметрично soul/internal/coremod (Soul-side, ADR-015): тот же интерфейс
// sdk/module.SoulModule, тот же Registry-pattern. Разница — где исполняется
// шаг и какие dep-ы (PG-pool / Vault / PluginHost vs apt/systemd).
package coremod

import (
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/bootstrap"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/choir"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/cloud"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/soul"
	"github.com/souls-guild/soul-stack/keeper/internal/coremod/vault"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/sdk/module"
)

// Registry — иммутабельный набор «base-имя модуля → реализация SoulModule».
//
// Симметрично soul/internal/coremod.Registry: ключ — base-имя модуля БЕЗ
// state-суффикса (`core.soul`, не `core.soul.registered`). Author-форма
// адреса задачи — base + state (`core.soul.registered`, `core.cloud.created`);
// config.SplitModuleAddr делит адрес на (base, state) в keeper_dispatch, base
// идёт в Lookup, state — в pluginv1.ApplyRequest.state и обрабатывается внутри
// реализации.
type Registry struct {
	mods map[string]module.SoulModule
}

// Deps — внешние зависимости keeper-side модулей. Поля обязательны
// (кроме `Audit` — может быть nil в тестовых сборках; в проде всегда есть
// auditmulti.Writer).
type Deps struct {
	// SoulStore — keeper/internal/coremod/soul.Store.
	SoulStore soul.Store

	// SoulPresence — presence-checker (Redis SID-lease) для барьера онбординга
	// `core.soul.registered` `await_online` (ADR-061). nil допустим (тестовые
	// сборки / dev без Redis): шаг с `await_online: true` тогда завершится failed
	// (барьер не может работать без источника presence). Прод — обёртка над
	// keeperredis.SoulsStreamAlive (тот же источник, что topology.SoulLeaseChecker).
	SoulPresence soul.PresenceChecker

	// MaxAwaitTimeout — провайдер строкового потолка keeper.yml::max_await_timeout
	// (ADR-061), hot-reload-aware (читается на каждом Apply). nil → дефолт
	// config.DefaultMaxAwaitTimeout. Прод — обёртка над config.Store.Get().
	MaxAwaitTimeout func() string

	// PluginHost — keeper/internal/coremod/cloud.PluginHost. До завершения
	// Plugin.d caller передаёт cloud.StubHost{}; интерфейс зафиксирован
	// заранее, чтобы Registry собирался без зависимости от Plugin.d.
	PluginHost cloud.PluginHost

	// CloudResolver резолвит param `provider` в driver-имя + plain-credentials
	// (A-flow): Provider-реестр + Vault. Прод — cloud.CredentialsResolverPG.
	CloudResolver cloud.ProviderResolver

	// CloudSouls / CloudTokens — узкие PG-adapter-ы для cloud-модуля.
	// Отделены от SoulStore: модуль `core.cloud` дёргает разные методы
	// (Insert + UpdateStatus), не пересекающиеся с `core.soul`
	// (SelectBySID + Insert + UpdateCoven).
	CloudSouls  cloud.SoulStore
	CloudTokens cloud.TokenStore

	// CloudCascade — cascade-обработчик `destroyed`-state (ADR-017).
	// Реализуется [cloud.CascadePG] поверх pgxpool.Pool. В тестовых
	// сборках без destroyed-сценариев допустим nil.
	CloudCascade cloud.Cascader

	// CloudUserdata — резолвер cloud-init userdata для scenario-параметра
	// `generate_userdata: true` (ADR-017(h) amendment 2026-05-27, B-flat).
	// Прод-реализация — обёртка над cloudinit.Resolver+GenerateUserdata в
	// daemon-е (читает текущий KeeperConfig.CloudInit snapshot + Vault.ReadKV).
	// nil допустим: `generate_userdata: true` тогда вернёт явную ошибку,
	// явный `userdata:` продолжает работать без изменений.
	CloudUserdata cloud.UserdataProvider

	// Vault — vault-client для `core.vault` (kv-read читает; kv-present
	// generate-if-absent читает+пишет). *vault.Client удовлетворяет обе операции;
	// kv-read write-путь не вызывает (read-state).
	Vault vault.VaultWriter

	// ChoirStore — choir-CRUD adapter (ADR-044) для `core.choir`:
	// AddVoice/RemoveVoice над incarnation_choir_voices + проверка
	// существования инкарнации. Прод — choir.NewPGStore(pool). nil допустим
	// в тестовых сборках без choir-сценариев (модуль тогда не регистрируется).
	ChoirStore choir.Store

	// BootstrapProviders / BootstrapHostCAs / BootstrapDial — зависимости
	// keeper-side core-модуля `core.bootstrap.delivered` (ADR-063, доставка
	// per-VM bootstrap-токена по SSH). Все три собираются wire-up-ом из той же
	// push-инфраструктуры, что и SshDispatcher (дискаверенные SshProvider-плагины
	// по manifest.Name + host-CA из Vault + push.Dial). Модуль регистрируется
	// только при непустых BootstrapProviders И непустых BootstrapHostCAs И
	// заданном BootstrapDial — иначе сборка без SSH-доступа (pull-only / нет
	// host-CA), и шаг с этим адресом упадёт «unknown keeper-side module».
	BootstrapProviders map[string]bootstrap.SshProviderHost
	BootstrapHostCAs   []push.NamedHostKeyAuthority
	BootstrapDial      push.Dialer

	// Audit — единый audit-writer для keeper-side модулей (cloud/vault
	// пишут audit-event-ы; soul/choir — нет). nil допустим (модули пропустят
	// запись и продолжат), но в проде wire-up из main должен подсовывать
	// настоящий keeper/internal/auditpg или auditmulti.
	Audit AuditWriter
}

// AuditWriter — общий тип для audit-пишущих модулей (cloud/vault/bootstrap);
// всё совпадает с shared/audit.Writer.
type AuditWriter interface {
	cloud.AuditWriter
	vault.AuditWriter
	bootstrap.AuditWriter
}

// Default собирает Registry с keeper-side core-модулями: безусловно
// core.soul / core.cloud / core.vault, плюс core.choir при наличии
// Deps.ChoirStore. Caller передаёт реальные dep-ы (PG-pool через
// cloud.NewSoulPG / cloud.NewTokenPG / soul.NewPGStore, vault-client из
// keeper/internal/vault, choir.NewPGStore).
func Default(d Deps) *Registry {
	cloudMod := cloud.New(d.PluginHost, d.CloudResolver, d.CloudSouls, d.CloudTokens, d.CloudCascade, d.Audit)
	if d.CloudUserdata != nil {
		cloudMod = cloudMod.WithUserdata(d.CloudUserdata)
	}
	// core.soul.registered с барьером онбординга (ADR-061): presence-checker +
	// провайдер потолка await_timeout подключаются опционально. nil presence —
	// шаг с await_online: true завершится failed (тестовые/dev-сборки без Redis).
	soulMod := soul.New(d.SoulStore).WithPresence(d.SoulPresence, d.MaxAwaitTimeout)
	mods := map[string]module.SoulModule{
		soul.Name:  soulMod,
		cloud.Name: cloudMod,
		vault.Name: vault.New(d.Vault, d.Audit),
	}
	// `core.choir` (ADR-044) — регистрируется только при наличии
	// ChoirStore. nil означает сборку без choir-сценариев; шаг с этим модулем
	// тогда упадёт «unknown keeper-side module» (как любой не подключённый).
	if d.ChoirStore != nil {
		mods[choir.Name] = choir.New(d.ChoirStore)
	}
	// `core.bootstrap.delivered` (ADR-063) — регистрируется только при полном
	// наборе SSH-зависимостей (провайдеры + host-CA + dialer). Любой пробел
	// означает сборку без push-доступа (pull-only / нет host-CA): шаг с этим
	// адресом тогда упадёт «unknown keeper-side module» (как любой не
	// подключённый). Симметрично условной регистрации `core.choir`.
	if len(d.BootstrapProviders) > 0 && len(d.BootstrapHostCAs) > 0 && d.BootstrapDial != nil {
		mods[bootstrap.Name] = &bootstrap.Module{
			Providers: d.BootstrapProviders,
			HostCAs:   d.BootstrapHostCAs,
			Dial:      d.BootstrapDial,
			Audit:     d.Audit,
		}
	}
	return NewRegistry(mods)
}

// NewRegistry собирает Registry из произвольного набора реализаций.
func NewRegistry(mods map[string]module.SoulModule) *Registry {
	cp := make(map[string]module.SoulModule, len(mods))
	for k, v := range mods {
		cp[k] = v
	}
	return &Registry{mods: cp}
}

// Lookup возвращает модуль по base-имени (без state-суффикса) и флаг наличия.
func (r *Registry) Lookup(name string) (module.SoulModule, bool) {
	m, ok := r.mods[name]
	return m, ok
}

// Names возвращает список зарегистрированных модулей в недетерминированном
// порядке (Go map iteration). Используется для diagnostic-вывода / healthz.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.mods))
	for k := range r.mods {
		out = append(out, k)
	}
	return out
}
