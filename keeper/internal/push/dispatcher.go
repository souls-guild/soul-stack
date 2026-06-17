package push

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/soul"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/encoding/protojson"
)

// SSHTarget — реквизиты SSH-подключения к push-хосту: host (= SID/FQDN), порт,
// пользователь, путь к уже установленному soul-бинарю.
//
// В пилоте soul-бинарь предполагается уже на хосте по известному пути
// (SHA-256-кеш доставки/sync — слайс S1), поэтому SoulPath приходит из резолва,
// а не вычисляется.
type SSHTarget struct {
	Host     string
	Port     int
	User     string
	SoulPath string
}

// TargetResolver резолвит SSH-реквизиты по SID. Внедряется как зависимость:
// пилот подставляет config-backed резолвер, S7-1 — PGFallbackTargetResolver.
type TargetResolver interface {
	Resolve(ctx context.Context, sid string) (SSHTarget, error)
}

// SoulLookup читает Soul по SID — нужен диспетчеру ТОЛЬКО для проверки
// предусловия transport=ssh (валидация входа). Сужено до одного метода, чтобы
// диспетчер мокался без PG. Реализуется обёрткой над [soul.SelectBySID].
type SoulLookup interface {
	SelectBySID(ctx context.Context, sid string) (*soul.Soul, error)
}

// Dialer открывает SSH-сессию по DialConfig. Production — [Dial]; тест —
// мок-функция, возвращающая фейковую [Session]. Тип-функция (а не интерфейс)
// держит wire-up тривиальным.
type Dialer func(ctx context.Context, cfg DialConfig) (Session, error)

// ProviderRespawner — узкая поверхность для runtime re-spawn SshProvider
// plugin-handle с обновлёнными env-payload params. Реализуется wire-up-ом в
// daemon (он держит pluginhost.Host + discovered + PGFallbackProviderResolver).
//
// Контракт: получив имя плагина, реализация резолвит свежие params из
// PG/legacy-fallback, spawn-ит новый plugin-handle с обновлённым env-payload
// SOUL_SSH_<UPPER_SNAKE(name)>_PARAMS и возвращает пару (SshProvider,
// io.Closer) — Closer закрывает spawned plugin-process (вызывается dispatcher-
// ом при следующем RefreshProvider либо в его собственном Close-path).
type ProviderRespawner interface {
	// RespawnProvider закрывает текущий plugin-handle (если передан oldCloser ≠
	// nil) и spawn-ит новый с обновлёнными params.
	//
	// Reentrant-инвариант: caller (SshDispatcher.RefreshProvider) держит mutex,
	// одновременных Respawn по тому же имени не будет.
	RespawnProvider(ctx context.Context, providerName string, oldCloser io.Closer) (SshProvider, io.Closer, error)
}

// ProviderEntry — один зарегистрированный SshProvider-плагин в карте
// диспетчера. Closer закрывает spawned plugin-process (typically
// *pluginhost.SshProviderPlugin); при подмене через RefreshProvider старый
// Closer закрывается respawner-ом, новый ставится на место.
//
// nil Closer допустим: unit-тесты подсовывают мок-Provider без spawn-а
// дочернего процесса.
type ProviderEntry struct {
	Provider SshProvider
	Closer   io.Closer
}

// Deps — зависимости [SshDispatcher].
//
// Multi-provider routing (ADR-032 amendment 2026-05-27, P2 W-2): диспетчер
// держит карту провайдеров по имени `Providers map[string]ProviderEntry`.
// SendApply/Cleanup получают `providerName` от caller-а (pushorch.PushRun
// резолвит его через ProviderRouter) и делают lookup в карте под RLock.
type Deps struct {
	// Providers — карта зарегистрированных SshProvider-плагинов по имени
	// (manifest.Name). Пустая карта — программная ошибка: NewSshDispatcher
	// валит создание. Подменяется в рантайме через [SshDispatcher.
	// RefreshProvider] под d.mu (атомарная подмена одной записи без эффекта на
	// остальных).
	Providers map[string]ProviderEntry
	// Respawner — runtime re-spawn новой версии plugin-handle с обновлённым
	// env-payload. nil → [SshDispatcher.RefreshProvider] вернёт
	// ErrRespawnNotSupported.
	Respawner ProviderRespawner
	// Targets — резолв SSH-реквизитов по SID.
	Targets TargetResolver
	// Souls — проверка предусловия transport=ssh.
	Souls SoulLookup
	// HostAuthorities — multi-CA-набор для verify host-сертификатов (S7-3,
	// ADR-032 amendment 2026-05-26). Непустой; на handshake-е делается OR-
	// проверка по всем элементам через ssh.CertChecker.IsHostAuthority.
	HostAuthorities []NamedHostKeyAuthority
	// Metrics — опц. observability multi-CA (счётчик матчей по `ca_name`).
	// nil — no-op (unit-тесты без obs.Registry / push выключен).
	Metrics *Metrics
	// Dial — открытие SSH-сессии. nil → [Dial] (production).
	Dial Dialer
	// Logger обязателен.
	Logger *slog.Logger
	// DialTimeout — таймаут connect+handshake. 0 → defaultDialTimeout.
	DialTimeout time.Duration
	// Deliverer — доставка soul-бинаря и зарегистрированных модулей с SHA-256-
	// дедупом ПЕРЕД exec-ом `soul apply`. nil → доставка пропускается (BC с S0:
	// в пилоте бинарь уже на хосте по SoulPath).
	Deliverer Deliverer
	// SoulSpec — что доставлять (см. [SoulSpec]). Игнорируется при Deliverer=nil.
	SoulSpec SoulSpec
	// Cleaner — host-side чистка артефактов (`rm -rf /var/lib/soul-stack/{bin,
	// modules}/`). Используется методом [SshDispatcher.Cleanup]; SendApply его
	// не дергает (см. doc Cleaner).
	Cleaner Cleaner
}

const defaultDialTimeout = 30 * time.Second

// SshDispatcher — push-реализация диспетчера apply (ADR-004, агентless SSH).
// Метод [SshDispatcher.SendApply] повторяет сигнатуру-семантику pull-Outbound,
// чтобы позже стать его alt-реализацией (ветвление по transport на точке
// Outbound.SendApply). Отличие: push синхронный oneshot — возвращает
// *RunResult сразу (нет асинхронного EventStream-барьера).
//
// Multi-provider (P2 W-2): диспетчер держит карту `Deps.Providers` и lookup-ит
// провайдер по имени, переданному в SendApply/Cleanup. Routing-логика (per-SID
// → coven-default → cluster-default) — за рамками диспетчера, см.
// [ProviderRouter].
type SshDispatcher struct {
	// mu защищает deps.Providers при runtime re-spawn (RefreshProvider).
	// SendApply/Cleanup на горячем пути берут RLock-style snapshot ссылки на
	// конкретный ProviderEntry и доезжают до конца сессии без блокировки.
	// Используем sync.RWMutex.
	mu   sync.RWMutex
	deps Deps
}

// NewSshDispatcher собирает диспетчер. Providers / Targets / Souls / Logger
// обязательны; `HostAuthorities` непустой (без CA нет доверенного host-cert
// verification). Каждый элемент HostAuthorities обязан иметь непустой `Name`
// и непустой `CAPubKey` — это инвариант caller-а. Dial nil → production [Dial].
//
// Карта Providers обязана быть непустой и каждая запись — иметь non-nil
// Provider (Closer допустим nil — unit-тесты).
func NewSshDispatcher(deps Deps) (*SshDispatcher, error) {
	if len(deps.Providers) == 0 {
		return nil, errors.New("push: Deps.Providers must be non-empty (multi-provider map)")
	}
	for name, entry := range deps.Providers {
		if entry.Provider == nil {
			return nil, fmt.Errorf("push: Providers[%q].Provider is nil", name)
		}
	}
	if deps.Targets == nil {
		return nil, errors.New("push: TargetResolver обязателен")
	}
	if deps.Souls == nil {
		return nil, errors.New("push: SoulLookup обязателен")
	}
	if deps.Logger == nil {
		return nil, errors.New("push: logger обязателен")
	}
	if len(deps.HostAuthorities) == 0 {
		return nil, errors.New("push: HostAuthorities обязателен непустым (CA-signed host-cert verification)")
	}
	for i, ha := range deps.HostAuthorities {
		if ha.Name == "" {
			return nil, fmt.Errorf("push: HostAuthorities[%d].Name пуст", i)
		}
		if ha.CAPubKey == nil {
			return nil, fmt.Errorf("push: HostAuthorities[%d].CAPubKey nil (CA %q)", i, ha.Name)
		}
	}
	if deps.Dial == nil {
		deps.Dial = Dial
	}
	if deps.DialTimeout == 0 {
		deps.DialTimeout = defaultDialTimeout
	}
	return &SshDispatcher{deps: deps}, nil
}

// providerEntry возвращает зарегистрированный ProviderEntry по имени под
// RLock. SendApply/Cleanup на горячем пути берут snapshot одной записи и
// держат её до конца прогона. RefreshProvider под Lock подменяет запись
// атомарно — параллельные SendApply на другие имена не блокируются.
//
// Возвращает (ProviderEntry{}, false) если имя не зарегистрировано — caller
// (SendApply/Cleanup) маппит это в ErrProviderUnknown.
func (d *SshDispatcher) providerEntry(name string) (ProviderEntry, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	e, ok := d.deps.Providers[name]
	return e, ok
}

// HasProvider — операторская диагностика: «зарегистрирован ли SshProvider с
// этим именем». Используется тестами и invalidation-listener-ом, чтобы не
// дёргать RefreshProvider на чужие имена.
func (d *SshDispatcher) HasProvider(name string) bool {
	_, ok := d.providerEntry(name)
	return ok
}

// ProviderNames — снимок имён зарегистрированных провайдеров (диагностика,
// логи). Порядок недетерминирован.
func (d *SshDispatcher) ProviderNames() []string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]string, 0, len(d.deps.Providers))
	for name := range d.deps.Providers {
		out = append(out, name)
	}
	return out
}

// RefreshProvider — runtime re-spawn SshProvider plugin-handle с обновлёнными
// env-payload params (S7-2 hot-reload, ADR-032 amendment 2026-05-26;
// расширено amendment 2026-05-27 P2 W-2 для multi-provider map).
//
// Алгоритм:
//
//  1. Имя пустое → массовая инвалидация: re-spawn ВСЕХ зарегистрированных
//     провайдеров (последовательно, под общим Lock). Используется при
//     неизвестной точке мутации.
//  2. Имя задано, в карте отсутствует → no-op без ошибки (это не наш провайдер,
//     pub/sub-сообщение пришло от другого кластера / другой ноды с иным
//     каталогом плагинов).
//  3. Имя задано, в карте есть → под Lock зовёт respawner.RespawnProvider:
//     тот закрывает старый handle и spawn-ит новый. При успехе подменяет
//     запись; при ошибке очищает её (degraded state — последующий SendApply
//     на этот provider вернёт ErrProviderUnknown / nil-provider).
//
// Возвращает ErrRespawnNotSupported, если Respawner не сконфигурирован.
func (d *SshDispatcher) RefreshProvider(ctx context.Context, providerName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.deps.Respawner == nil {
		return ErrRespawnNotSupported
	}

	if providerName == "" {
		// Массовая инвалидация: проходим по всем именам. Ошибка на одном
		// провайдере не прерывает остальных — каждый идёт независимо.
		var firstErr error
		for name := range d.deps.Providers {
			if err := d.respawnOneLocked(ctx, name); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	// Не наш провайдер — no-op (multi-provider routing: другие SshDispatcher-ы
	// не существуют в текущей раскладке, но защитимся от чужих pub/sub-сообщений).
	if _, ok := d.deps.Providers[providerName]; !ok {
		return nil
	}
	return d.respawnOneLocked(ctx, providerName)
}

// respawnOneLocked — re-spawn одной записи карты. Caller обязан держать
// d.mu.Lock (не RLock).
func (d *SshDispatcher) respawnOneLocked(ctx context.Context, name string) error {
	old := d.deps.Providers[name]
	newProv, newCloser, err := d.deps.Respawner.RespawnProvider(ctx, name, old.Closer)
	if err != nil {
		// degraded state: удаляем запись, чтобы последующий SendApply вернул
		// ErrProviderUnknown с понятным сообщением (а не nil-deref). respawner
		// уже закрыл старый handle (документированный контракт).
		delete(d.deps.Providers, name)
		return fmt.Errorf("push: re-spawn provider %q: %w", name, err)
	}
	d.deps.Providers[name] = ProviderEntry{Provider: newProv, Closer: newCloser}
	d.deps.Logger.Info("push: plugin re-spawned with fresh params",
		slog.String("provider", name))
	return nil
}

// ErrRespawnNotSupported — sentinel-ошибка [SshDispatcher.RefreshProvider]:
// диспетчер собран без ProviderRespawner (single-instance dev / unit-тесты).
// Caller (daemon listener) трактует её как «нечего обновлять» и продолжает.
var ErrRespawnNotSupported = errors.New("push: ProviderRespawner not configured")

// ErrProviderUnknown — sentinel-ошибка SendApply/Cleanup: providerName не
// зарегистрирован в карте (routing-промах, либо предыдущий RefreshProvider
// перевёл запись в degraded state из-за spawn-fail).
//
// pushorch.PushRun на этой ошибке помечает per-host status="error" с
// error_code="ssh_provider_unavailable: <name>".
var ErrProviderUnknown = errors.New("push: SshProvider not registered")

// SendApply исполняет прогон на push-хосте по SSH синхронно: lookup provider
// по имени → резолв target → проверка transport=ssh → ephemeral keypair →
// Authorize → Sign(pubkey) → connect (CA-host-cert verify) → `soul apply`
// со stdin=ApplyRequest → разбор NDJSON-stdout → RunResult.
//
// providerName — имя SshProvider-плагина (резолвится pushorch.ProviderRouter-ом
// до вызова). Пустая строка либо не зарегистрированное имя → ErrProviderUnknown.
//
// Возврат:
//   - (*RunResult, nil) — прогон доехал до RunResult (его status может быть
//     FAILED — это валидный итог, не ошибка транспорта).
//   - (nil, ошибка) — сбой ДО RunResult: ErrProviderUnknown, deny Authorize,
//     fail connect/Sign, обрыв до RunResult, битый NDJSON.
func (d *SshDispatcher) SendApply(ctx context.Context, sid string, providerName string, req *keeperv1.ApplyRequest) (*keeperv1.RunResult, error) {
	if req == nil {
		return nil, errors.New("push: ApplyRequest is nil")
	}
	if providerName == "" {
		return nil, fmt.Errorf("push: providerName is empty for sid=%s", sid)
	}
	entry, ok := d.providerEntry(providerName)
	if !ok || entry.Provider == nil {
		return nil, fmt.Errorf("%w: %s", ErrProviderUnknown, providerName)
	}

	log := d.deps.Logger.With(
		slog.String("sid", sid),
		slog.String("apply_id", req.GetApplyId()),
		slog.String("ssh_provider", providerName),
	)

	// Предусловие: диспетчер обслуживает ТОЛЬКО transport=ssh.
	s, err := d.deps.Souls.SelectBySID(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("push: резолв soul %s: %w", sid, err)
	}
	if s.Transport != soul.TransportSSH {
		return nil, fmt.Errorf("push: soul %s имеет transport=%q, ожидался ssh", sid, s.Transport)
	}

	target, err := d.deps.Targets.Resolve(ctx, sid)
	if err != nil {
		return nil, fmt.Errorf("push: резолв ssh-target %s: %w", sid, err)
	}

	prov := entry.Provider

	// Authorize — fail-closed: deny прекращает прогон до connect-а.
	authReply, err := prov.Authorize(ctx, &pluginv1.AuthorizeRequest{
		Host: target.Host,
		User: target.User,
	})
	if err != nil {
		return nil, fmt.Errorf("push: Authorize %s@%s: %w", target.User, target.Host, err)
	}
	if !authReply.GetAllowed() {
		return nil, fmt.Errorf("push: Authorize отказал для %s@%s: %s", target.User, target.Host, authReply.GetReason())
	}

	// Ephemeral keypair: Keeper-side ed25519-пара per-session. Pubkey уезжает
	// в SignRequest для CA-провайдеров. Приватник НИКОГДА не покидает Keeper.
	ephSigner, ephPubAuthorized, err := newEphemeralEd25519()
	if err != nil {
		return nil, fmt.Errorf("push: генерация ephemeral keypair %s: %w", sid, err)
	}

	signReply, err := prov.Sign(ctx, &pluginv1.SignRequest{
		Host:      target.Host,
		User:      target.User,
		PublicKey: ephPubAuthorized,
	})
	if err != nil {
		return nil, fmt.Errorf("push: Sign %s@%s: %w", target.User, target.Host, err)
	}
	auth, err := authMethodsFromSign(signReply, ephSigner)
	if err != nil {
		return nil, fmt.Errorf("push: подготовка SSH-auth %s: %w", sid, err)
	}

	sess, err := d.deps.Dial(ctx, DialConfig{
		Host:            target.Host,
		Port:            target.Port,
		User:            target.User,
		Auth:            auth,
		HostAuthorities: d.deps.HostAuthorities,
		OnHostCAMatch:   d.onHostCAMatch,
		ProxyJump:       signReply.GetProxyJump(),
		Timeout:         d.deps.DialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("push: connect %s: %w", sid, err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			log.Warn("push: закрытие SSH-сессии с ошибкой", slog.Any("error", cerr))
		}
	}()

	if d.deps.Deliverer != nil {
		if err := d.deps.Deliverer.Deliver(ctx, sess, d.deps.SoulSpec); err != nil {
			return nil, fmt.Errorf("push: доставка артефактов %s: %w", sid, err)
		}
	}

	stdin, err := protojson.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("push: marshal ApplyRequest %s: %w", sid, err)
	}

	cmd := soulApplyCommand(target.SoulPath)
	stdout, runErr := sess.Run(ctx, cmd, stdin)

	rr, parseErr := ParseStream(strings.NewReader(stdout), func(ev *keeperv1.TaskEvent) {
		log.Debug("push: TaskEvent",
			slog.Int("task_idx", int(ev.GetTaskIdx())),
			slog.String("status", ev.GetStatus().String()))
	})
	if parseErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("push: прогон %s без RunResult (exit: %v): %w", sid, runErr, parseErr)
		}
		return nil, fmt.Errorf("push: прогон %s: %w", sid, parseErr)
	}

	log.Info("push: прогон завершён", slog.String("status", rr.GetStatus().String()))
	return rr, nil
}

// Cleanup открывает SSH-сессию к хосту sid и удаляет соул-артефакты
// (`/var/lib/soul-stack/{bin,modules}/`) через [Cleaner].
//
// providerName — то же имя SshProvider-плагина, что использовалось в
// предшествующем SendApply (caller сохраняет соответствие в push_runs.summary).
func (d *SshDispatcher) Cleanup(ctx context.Context, sid string, providerName string) error {
	if d.deps.Cleaner == nil {
		return errors.New("push: Cleaner не сконфигурирован")
	}
	if providerName == "" {
		return fmt.Errorf("push: providerName is empty for sid=%s (cleanup)", sid)
	}
	entry, ok := d.providerEntry(providerName)
	if !ok || entry.Provider == nil {
		return fmt.Errorf("%w: %s (cleanup)", ErrProviderUnknown, providerName)
	}

	log := d.deps.Logger.With(
		slog.String("sid", sid),
		slog.String("op", "cleanup"),
		slog.String("ssh_provider", providerName),
	)

	s, err := d.deps.Souls.SelectBySID(ctx, sid)
	if err != nil {
		return fmt.Errorf("push: резолв soul %s: %w", sid, err)
	}
	if s.Transport != soul.TransportSSH {
		return fmt.Errorf("push: soul %s имеет transport=%q, ожидался ssh", sid, s.Transport)
	}

	target, err := d.deps.Targets.Resolve(ctx, sid)
	if err != nil {
		return fmt.Errorf("push: резолв ssh-target %s: %w", sid, err)
	}

	prov := entry.Provider

	authReply, err := prov.Authorize(ctx, &pluginv1.AuthorizeRequest{
		Host: target.Host,
		User: target.User,
	})
	if err != nil {
		return fmt.Errorf("push: Authorize %s@%s: %w", target.User, target.Host, err)
	}
	if !authReply.GetAllowed() {
		return fmt.Errorf("push: Authorize отказал для %s@%s: %s", target.User, target.Host, authReply.GetReason())
	}

	ephSigner, ephPubAuthorized, err := newEphemeralEd25519()
	if err != nil {
		return fmt.Errorf("push: генерация ephemeral keypair %s: %w", sid, err)
	}

	signReply, err := prov.Sign(ctx, &pluginv1.SignRequest{
		Host:      target.Host,
		User:      target.User,
		PublicKey: ephPubAuthorized,
	})
	if err != nil {
		return fmt.Errorf("push: Sign %s@%s: %w", target.User, target.Host, err)
	}
	auth, err := authMethodsFromSign(signReply, ephSigner)
	if err != nil {
		return fmt.Errorf("push: подготовка SSH-auth %s: %w", sid, err)
	}

	sess, err := d.deps.Dial(ctx, DialConfig{
		Host:            target.Host,
		Port:            target.Port,
		User:            target.User,
		Auth:            auth,
		HostAuthorities: d.deps.HostAuthorities,
		OnHostCAMatch:   d.onHostCAMatch,
		ProxyJump:       signReply.GetProxyJump(),
		Timeout:         d.deps.DialTimeout,
	})
	if err != nil {
		return fmt.Errorf("push: connect %s: %w", sid, err)
	}
	defer func() {
		if cerr := sess.Close(); cerr != nil {
			log.Warn("push: закрытие SSH-сессии с ошибкой", slog.Any("error", cerr))
		}
	}()

	if err := d.deps.Cleaner.Cleanup(ctx, sess); err != nil {
		return fmt.Errorf("push: cleanup %s: %w", sid, err)
	}
	log.Info("push: host-side cleanup выполнен")
	return nil
}

// authMethodsFromSign конвертирует SignReply в ssh.AuthMethod-ы. Поддерживает
// два режима (PM-decision SSH key-ownership):
//
//   - Keeper-ephemeral (Vault SSH CA, канонический для CA-провайдеров): плагин
//     возвращает только certificate, private_key="". Подписант — ephemeral
//     keypair Keeper-а. Cert + ephSigner → ssh.NewCertSigner.
//
//   - Static-flow (соул-ssh-static): плагин владеет ключом и возвращает готовую
//     пару (private_key непуст). ephSigner игнорируется.
func authMethodsFromSign(reply *pluginv1.SignReply, ephSigner ssh.Signer) ([]ssh.AuthMethod, error) {
	if reply.GetPrivateKey() != "" {
		signer, err := ssh.ParsePrivateKey([]byte(reply.GetPrivateKey()))
		if err != nil {
			return nil, fmt.Errorf("разбор private_key: %w", err)
		}
		if cert := reply.GetCertificate(); cert != "" {
			certSigner, cerr := certSignerFrom(cert, signer)
			if cerr != nil {
				return nil, cerr
			}
			return []ssh.AuthMethod{ssh.PublicKeys(certSigner)}, nil
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}

	cert := reply.GetCertificate()
	if cert == "" {
		return nil, errors.New("SignReply: оба поля пусты (нужен certificate для ephemeral-режима либо private_key для static-режима)")
	}
	if ephSigner == nil {
		return nil, errors.New("ephemeral signer не передан, а private_key пуст — нечем подписать handshake")
	}
	certSigner, err := certSignerFrom(cert, ephSigner)
	if err != nil {
		return nil, err
	}
	return []ssh.AuthMethod{ssh.PublicKeys(certSigner)}, nil
}

// certSignerFrom разбирает OpenSSH-cert в текстовом виде и склеивает с signer-ом
// в ssh.CertSigner. Используется обоими flow-ами (static с cert / ephemeral).
func certSignerFrom(certText string, signer ssh.Signer) (ssh.Signer, error) {
	pub, _, _, _, perr := ssh.ParseAuthorizedKey([]byte(certText))
	if perr != nil {
		return nil, fmt.Errorf("разбор certificate: %w", perr)
	}
	sshCert, ok := pub.(*ssh.Certificate)
	if !ok {
		return nil, errors.New("certificate не является SSH-сертификатом")
	}
	certSigner, cerr := ssh.NewCertSigner(sshCert, signer)
	if cerr != nil {
		return nil, fmt.Errorf("сборка cert-signer: %w", cerr)
	}
	return certSigner, nil
}

// newEphemeralEd25519 генерирует свежий ed25519-keypair per-session и
// возвращает (signer, marshaled-pubkey в OpenSSH authorized_keys-формате).
//
// SENSITIVE: приватник остаётся ТОЛЬКО внутри возвращённого signer-а.
func newEphemeralEd25519() (ssh.Signer, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("ed25519 GenerateKey: %w", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", fmt.Errorf("ssh signer from ed25519: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", fmt.Errorf("ssh pubkey from ed25519: %w", err)
	}
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	return signer, authorized, nil
}

// soulApplyCommand строит команду запуска oneshot-applier на хосте.
// stdin=protojson ApplyRequest подаётся отдельно (Session.Run), stdout=NDJSON.
func soulApplyCommand(soulPath string) string {
	return soulPath + " apply"
}

// onHostCAMatch — callback из `hostCertCallback` при матче host-CA.
func (d *SshDispatcher) onHostCAMatch(caName string) {
	if d.deps.Logger != nil {
		d.deps.Logger.Debug("push: host CA matched", slog.String("ca_name", caName))
	}
	d.deps.Metrics.ObserveHostCAUsed(caName)
}
