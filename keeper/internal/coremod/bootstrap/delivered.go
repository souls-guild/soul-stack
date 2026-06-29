// Package bootstrap реализует keeper-side core-модуль `core.bootstrap.delivered`
// (ADR-063, docs/keeper/modules.md) — тонкую доставку per-VM bootstrap-токена по
// SSH на свежесозданные cloud-init-VM.
//
// Закрывает BUG#2 cloud-provision: до этого scenario нёс адрес-заглушку
// `keeper.push.applied`, который keeper-dispatch отвергал как unknown module —
// созданная VM никогда не получала токен и не онбордилась.
//
// Дизайн A1 («тонкая доставка»): cloud-init (B-flat, ADR-017(h)) уже поставил на
// VM soul-бинарь + CA + systemd-unit. Модуль кладёт ТОЛЬКО токен и опционально
// `systemctl start soul`. Поток per-host (последовательно): Authorize (deny →
// fail-closed) → ephemeral keypair + Sign → Dial (CA-signed host-cert verify) →
// запись токена в `token_path` (★токен в STDIN, не в argv) → опц. start soul.
// Ошибка любого хоста прерывает шаг (B1-strict): state не коммитится, прогон
// уходит в error_locked.
//
// Транспортные режимы (ADR-063 amendment «Teleport by-name transport»):
//   - direct (default): generic push.Dial по primary_ip — Authorize/Sign/
//     ephemeral + CA-signed host-cert verify (host-CA из Vault), C1.
//   - teleport: доставка через Teleport Proxy BY-NAME (target = SID/FQDN, НЕ
//     primary_ip). Транспорт + user-auth + host-verify целиком через Teleport
//     identity-file (keeper-side Teleport-Dialer, push.NewTeleportDialer):
//     Authorize/Sign/ephemeral НЕ вызываются, Vault host-CA НЕ требуется
//     (host-verify через Teleport CA, C1 неприменим). Свежая VM появляется в
//     Teleport только через ~3-5мин после создания → connect оборачивается
//     retry-with-backoff до `join_wait_timeout` (по deadline — failed, B1-strict).
//
// Границы MVP (ADR-063): один key-based SshProvider, доставка только токена,
// хосты обрабатываются последовательно. Cloud-init CA-signed host-key (C1) —
// required-для-live, следующий слайс: до него Dial реджектит host-cert свежей VM
// (голый host-key без подписи CA), и live-e2e не пройдёт.
//
// Симметрично keeper/internal/coremod/cloud: тот же интерфейс sdk/module,
// тот же Registry-pattern, тот же audit-маскинг секретов.
package bootstrap

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/coremod/util"
	"github.com/souls-guild/soul-stack/keeper/internal/push"
	"github.com/souls-guild/soul-stack/shared/audit"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// Name — base-имя модуля без state-суффикса (ключ Registry). Author-форма
// адреса задачи — `core.bootstrap.delivered` (base + state `delivered`); state
// приходит в pluginv1.ApplyRequest.state и проверяется в Apply.
const Name = "core.bootstrap"

// StateDelivered — единственное состояние модуля.
const StateDelivered = "delivered"

// Дефолты опциональных параметров.
const (
	defaultTokenPath = "/etc/soul/token"
	defaultSSHUser   = "root"
	defaultSSHPort   = 22
)

// Транспортные режимы доставки (поле Module.Transport, источник —
// keeper.yml::push.transport). Пустая строка трактуется как TransportDirect
// (backward-compat: существующая generic-доставка).
const (
	// TransportDirect — generic push.Dial по primary_ip (Authorize/Sign/CA-host-cert).
	TransportDirect = "direct"
	// TransportTeleport — by-name через Teleport Proxy (target = SID), без
	// Authorize/Sign, host-verify через Teleport identity-file.
	TransportTeleport = "teleport"
)

// Параметры retry-with-backoff connect-а в teleport-режиме (свежая VM join-ится
// в Teleport через ~3-5мин). В direct-режиме retry не применяется (хост уже
// существует на момент шага).
const (
	// defaultJoinWaitTimeout — потолок ожидания Teleport-join по умолчанию
	// (опц. param `join_wait_timeout`). 6 мин с запасом над типовыми 3-5мин.
	defaultJoinWaitTimeout = 6 * time.Minute
	// joinRetryBase — базовый интервал между попытками connect-а.
	joinRetryBase = 12 * time.Second
	// joinRetryJitter — верхняя граница случайной добавки к интервалу (anti-
	// thundering-herd при пакетной доставке на N VM).
	joinRetryJitter = 4 * time.Second
)

// deliverScriptFmt — shell-команда записи токена на VM. Токен подаётся в STDIN
// (`cat > path`), НЕ в argv: argv утёк бы в `ps`/audit.log/journald на самой VM.
// `install -d -m 0700 /etc/soul` создаёт каталог с приватными правами (umask 077
// дополнительно страхует от race-окна между cat и chmod), `chmod 0400` —
// read-only владельцу. token_path подставляется напрямую: источник — рендер
// scenario (доверенный keeper-side вход, не Soul-reported), shell-escape не нужен.
const deliverScriptFmt = "install -d -m 0700 /etc/soul && umask 077 && cat > %s && chmod 0400 %s"

// startSoulCmd — запуск soul-агента после доставки токена (start_soul: true).
const startSoulCmd = "systemctl start soul"

// SshProviderHost — узкая поверхность SSH-аутентификации, нужная модулю:
// Authorize (право Keeper-а ходить на хост) + Sign (выпуск SSH-credentials на
// сессию). Это в точности [push.SshProvider] — тот же host-side потребительский
// контракт из двух методов, которым пользуется SshDispatcher.
//
// Переиспользуется (а не дублируется) намеренно: прод-реализация —
// дискаверенный SshProvider-плагин (`*pluginhost.SshProviderPlugin`), который
// уже реализует Authorize/Sign и подаётся в SshDispatcher тем же типом. Карта
// провайдеров собирается wire-up-ом daemon-а из тех же spawned-плагинов; в
// unit-тестах модуля мокается struct-ом. nil-карта/пустая → модуль не
// регистрируется (см. registry).
type SshProviderHost = push.SshProvider

// AuditWriter — для audit-event-а `bootstrap.delivered`.
type AuditWriter interface {
	Write(ctx context.Context, event *audit.Event) error
}

// Module — реализация sdk/module.SoulModule.
//
// Обязательность зависит от транспорта (поле Transport):
//   - direct (default): Providers + HostCAs + Dial обязательны (nil/пусто → явная
//     ошибка Apply либо не-регистрация в Registry).
//   - teleport: достаточно Dial (Teleport-Dialer); Providers/HostCAs не
//     используются (Authorize/Sign не вызываются, host-verify через Teleport).
//
// Audit опционален (nil → запись пропускается).
type Module struct {
	// Transport — режим доставки: TransportDirect (default при "") или
	// TransportTeleport. Источник — keeper.yml::push.transport (wire-up daemon-а),
	// НЕ scenario-param: режим — свойство инсталляции keeper-а, не отдельной задачи.
	Transport string

	// Provider — резолв SSH-провайдера по имени param `ssh_provider`. MVP —
	// single-provider: модуль держит карту, собранную wire-up-ом из
	// дискаверенных SshProvider-плагинов (по manifest.Name). В teleport-режиме
	// НЕ используется (Authorize/Sign не вызываются).
	Providers map[string]SshProviderHost

	// HostCAs — multi-CA-набор для verify host-cert целевых VM (тот же
	// push.LoadHostCAs, что у SshDispatcher). В direct-режиме непустой; пустой →
	// Apply вернёт явную ошибку (CA-signed host-cert verify обязателен,
	// fail-closed). В teleport-режиме НЕ используется (host-verify через Teleport
	// identity-file, C1 неприменим).
	HostCAs []push.NamedHostKeyAuthority

	// Dial — открытие SSH-сессии. direct — push.Dial; teleport —
	// push.NewTeleportDialer; тест — мок-Dialer.
	Dial push.Dialer

	// RetryBase / RetryJitter — параметры backoff teleport-connect-retry. 0 →
	// дефолты joinRetryBase / joinRetryJitter. Поля существуют ради unit-тестов
	// (короткий backoff, чтобы retry-до-join не спал реальные ~12с); прод-wire-up
	// их не задаёт.
	RetryBase   time.Duration
	RetryJitter time.Duration

	// Audit — audit-writer (`bootstrap.delivered`). nil → запись пропускается.
	Audit AuditWriter
}

// retryBackoff возвращает (base, jitter) с подстановкой дефолтов при нулях.
func (m *Module) retryBackoff() (base, jitter time.Duration) {
	base = m.RetryBase
	if base <= 0 {
		base = joinRetryBase
	}
	jitter = m.RetryJitter
	if jitter <= 0 {
		jitter = joinRetryJitter
	}
	return base, jitter
}

// teleport сообщает, работает ли модуль в by-name Teleport-режиме.
func (m *Module) teleport() bool { return m.Transport == TransportTeleport }

// hostInput — одна VM из param `hosts` (= register.<provision>.hosts от
// core.cloud.created): SID, IP для коннекта и plain bootstrap-токен.
type hostInput struct {
	sid       string
	primaryIP string
	token     string
}

// connectTarget — адрес коннекта для диагностики (direct → primary_ip; teleport
// → SID/node-name). Используется в тексте failed-event, чтобы оператор видел, по
// чему именно шла доставка.
func (h hostInput) connectTarget(teleport bool) string {
	if teleport {
		return h.sid
	}
	return h.primaryIP
}

// Validate проверяет params без транспорт-зависимости (режим — свойство
// инсталляции, не задачи). ★ `ssh_provider` required в обоих режимах, но в
// transport: teleport он НЕ определяет транспорт: Authorize/Sign не вызываются,
// имя уходит ТОЛЬКО в audit-payload `bootstrap.delivered`. Смена required-статуса
// по транспорту — пост-MVP опционально.
func (m *Module) Validate(_ context.Context, req *pluginv1.ValidateRequest) (*pluginv1.ValidateReply, error) {
	var errs []string
	if req.State != StateDelivered {
		errs = append(errs, fmt.Sprintf("unknown state %q (want %q)", req.State, StateDelivered))
		return &pluginv1.ValidateReply{Ok: false, Errors: errs}, nil
	}
	if _, err := util.StringParam(req.Params, "ssh_provider"); err != nil {
		errs = append(errs, err.Error())
	}
	// hosts валидируется на структуру в Apply (per-element list-of-objects);
	// здесь проверяем лишь присутствие и тип через accessor.
	if _, ok := req.Params.GetFields()["hosts"]; !ok {
		errs = append(errs, "param \"hosts\": missing")
	}
	if _, err := util.OptStringParam(req.Params, "token_path"); err != nil {
		errs = append(errs, err.Error())
	}
	if _, err := util.OptStringParam(req.Params, "ssh_user"); err != nil {
		errs = append(errs, err.Error())
	}
	if n, ok, err := util.OptIntParam(req.Params, "ssh_port"); err != nil {
		errs = append(errs, err.Error())
	} else if ok && (n < 1 || n > 65535) {
		errs = append(errs, "param \"ssh_port\": must be in 1..65535")
	}
	if n, ok, err := util.OptIntParam(req.Params, "join_wait_timeout"); err != nil {
		errs = append(errs, err.Error())
	} else if ok && n < 0 {
		errs = append(errs, "param \"join_wait_timeout\": must be >= 0 (seconds)")
	}
	if _, _, err := util.OptBoolParam(req.Params, "start_soul"); err != nil {
		errs = append(errs, err.Error())
	}
	return &pluginv1.ValidateReply{Ok: len(errs) == 0, Errors: errs}, nil
}

func (m *Module) Plan(_ *pluginv1.PlanRequest, _ grpc.ServerStreamingServer[pluginv1.PlanEvent]) error {
	return nil
}

func (m *Module) Apply(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	if req.State != StateDelivered {
		return util.SendFailed(stream, fmt.Sprintf("unknown state %q", req.State))
	}
	return m.applyDelivered(req, stream)
}

// applyDelivered реализует state=delivered. См. doc-комментарий пакета.
func (m *Module) applyDelivered(req *pluginv1.ApplyRequest, stream grpc.ServerStreamingServer[pluginv1.ApplyEvent]) error {
	ctx := stream.Context()

	providerName, err := util.StringParam(req.Params, "ssh_provider")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	hosts, err := parseHosts(req.Params)
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	tokenPath, err := util.OptStringParam(req.Params, "token_path")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if tokenPath == "" {
		tokenPath = defaultTokenPath
	}
	sshUser, err := util.OptStringParam(req.Params, "ssh_user")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if sshUser == "" {
		sshUser = defaultSSHUser
	}
	sshPort, ok, err := util.OptIntParam(req.Params, "ssh_port")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !ok {
		sshPort = defaultSSHPort
	}
	startSoul, hasStart, err := util.OptBoolParam(req.Params, "start_soul")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	if !hasStart {
		startSoul = true // default: запускаем soul после доставки токена
	}
	joinWaitSec, hasJoinWait, err := util.OptIntParam(req.Params, "join_wait_timeout")
	if err != nil {
		return util.SendFailed(stream, err.Error())
	}
	joinWait := defaultJoinWaitTimeout
	if hasJoinWait {
		joinWait = time.Duration(joinWaitSec) * time.Second
	}

	// Конфигурационные предусловия — явная ошибка вместо невнятного nil-panic.
	if m.Dial == nil {
		return util.SendFailed(stream, "bootstrap delivered: dialer not configured (wire push.Dial / push.NewTeleportDialer in main)")
	}

	// prov резолвится только для direct-режима: teleport не вызывает Authorize/Sign
	// (транспорт+auth целиком через identity-file), карта Providers пуста.
	var prov SshProviderHost
	if !m.teleport() {
		if len(m.HostCAs) == 0 {
			return util.SendFailed(stream, "bootstrap delivered: host CAs not configured (set keeper.yml::push.host_ca_refs[] — CA-signed host-cert verify обязателен)")
		}
		p, ok := m.Providers[providerName]
		if !ok || p == nil {
			return util.SendFailed(stream, fmt.Sprintf("bootstrap delivered: ssh_provider %q not registered (known: %v)", providerName, m.providerNames()))
		}
		prov = p
	}

	script := fmt.Sprintf(deliverScriptFmt, tokenPath, tokenPath)

	results := make([]any, 0, len(hosts))
	sids := make([]any, 0, len(hosts))
	for _, h := range hosts {
		started, err := m.deliverHost(ctx, prov, h, sshUser, int(sshPort), script, startSoul, joinWait)
		if err != nil {
			// B1-strict: ошибка любого хоста валит весь шаг. maskErr страхует
			// от утечки vault-ref/токена в текст ошибки (failed-event уходит в
			// status_details — наблюдаемый канал).
			return util.SendFailed(stream, fmt.Sprintf("deliver token to %q (%s): %s", h.sid, h.connectTarget(m.teleport()), maskErr(err)))
		}
		results = append(results, map[string]any{
			"sid":       h.sid,
			"delivered": true,
			"started":   started,
		})
		sids = append(sids, h.sid)
	}

	if m.Audit != nil {
		// Audit-payload — БЕЗ токенов (как cloud.provisioned-маскинг): только
		// count + sids. Токен plain виден лишь в register предыдущего шага и
		// маскируется на всех его выходах; здесь его нет вовсе.
		ev := &audit.Event{
			EventType: audit.EventBootstrapDelivered,
			Source:    audit.SourceKeeperInternal,
			Payload: map[string]any{
				"action":       StateDelivered,
				"ssh_provider": providerName,
				"count":        float64(len(hosts)),
				"sids":         sids,
			},
		}
		if werr := m.Audit.Write(ctx, ev); werr != nil {
			return util.SendFailed(stream, fmt.Sprintf("audit write: %v", werr))
		}
	}

	// ★ БЕЗ токена в output: register.<имя>.hosts[] несёт только {sid, delivered,
	// started}. count — число успешно обработанных хостов (== len(hosts), иначе
	// шаг бы упал).
	return util.SendFinal(stream, true, map[string]any{
		"hosts": results,
		"count": float64(len(hosts)),
	})
}

// deliverHost обрабатывает один хост: открытие SSH-сессии (transport-зависимо) →
// запись токена (STDIN) → опц. start soul. Возвращает (started, error).
//
// Открытие сессии расходится по transport:
//   - direct: Authorize (fail-closed) → ephemeral keypair + Sign → Dial по
//     primary_ip (CA-signed host-cert verify) — переиспускает push-инфраструктуру
//     (newEphemeralEd25519/authMethodsFromSign) ровно как SshDispatcher.SendApply,
//     ephemeral-приватник не покидает Keeper.
//   - teleport: retry-Dial по SID (node-name) — без Authorize/Sign/ephemeral;
//     транспорт+auth+host-verify внутри Teleport-Dialer через identity-file.
//
// Хвост (запись токена + start soul) общий для обоих режимов.
func (m *Module) deliverHost(ctx context.Context, prov SshProviderHost, h hostInput, user string, port int, script string, startSoul bool, joinWait time.Duration) (started bool, err error) {
	var sess push.Session
	if m.teleport() {
		sess, err = m.dialTeleport(ctx, h, user, port, joinWait)
	} else {
		sess, err = m.dialDirect(ctx, prov, h, user, port)
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = sess.Close() }()

	// ★ Токен — в STDIN (script делает `cat > token_path`), НЕ в argv.
	if _, rerr := sess.Run(ctx, script, []byte(h.token)); rerr != nil {
		return false, fmt.Errorf("write token: %w", rerr)
	}

	if startSoul {
		if _, rerr := sess.Run(ctx, startSoulCmd, nil); rerr != nil {
			return false, fmt.Errorf("start soul: %w", rerr)
		}
		started = true
	}
	return started, nil
}

// dialDirect — generic-режим: Authorize → ephemeral keypair + Sign → push.Dial
// по primary_ip (CA-signed host-cert verify). Без изменений относительно
// первоначального A1-flow.
func (m *Module) dialDirect(ctx context.Context, prov SshProviderHost, h hostInput, user string, port int) (push.Session, error) {
	// Authorize — fail-closed: deny прекращает доставку до connect-а.
	authReply, err := prov.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: h.primaryIP, User: user})
	if err != nil {
		return nil, fmt.Errorf("authorize %s@%s: %w", user, h.primaryIP, err)
	}
	if !authReply.GetAllowed() {
		return nil, fmt.Errorf("authorize denied for %s@%s: %s", user, h.primaryIP, authReply.GetReason())
	}

	// Ephemeral keypair: Keeper-side ed25519-пара per-host. Pubkey уезжает в
	// SignRequest для CA-провайдеров; приватник НИКОГДА не покидает Keeper.
	ephSigner, ephPub, err := push.NewEphemeralEd25519()
	if err != nil {
		return nil, fmt.Errorf("ephemeral keypair: %w", err)
	}
	signReply, err := prov.Sign(ctx, &pluginv1.SignRequest{Host: h.primaryIP, User: user, PublicKey: ephPub})
	if err != nil {
		return nil, fmt.Errorf("sign %s@%s: %w", user, h.primaryIP, err)
	}
	auth, err := push.AuthMethodsFromSign(signReply, ephSigner)
	if err != nil {
		return nil, fmt.Errorf("ssh auth: %w", err)
	}

	sess, err := m.Dial(ctx, push.DialConfig{
		Host:            h.primaryIP,
		Port:            port,
		User:            user,
		Auth:            auth,
		HostAuthorities: m.HostCAs,
		ProxyJump:       signReply.GetProxyJump(),
	})
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return sess, nil
}

// dialTeleport — by-name режим: Dial по SID (node-name) через Teleport-Dialer,
// обёрнутый retry-with-backoff до joinWait. Свежая VM появляется в Teleport
// только через ~3-5мин после создания, поэтому первые попытки DialHost вернут
// 'offline or does not exist' — это ожидаемо, не valid повод валить шаг сразу.
//
// ★ DialConfig несёт ТОЛЬКО Host=SID/Port/User/Timeout: Auth/HostAuthorities/
// ProxyJump в teleport-режиме игнорируются Teleport-Dialer-ом (auth+host-verify
// из identity-file). Authorize/Sign/ephemeral здесь не вызываются вовсе.
func (m *Module) dialTeleport(ctx context.Context, h hostInput, user string, port int, joinWait time.Duration) (push.Session, error) {
	cfg := push.DialConfig{
		Host: h.sid, // ★ node-name = SID, НЕ primary_ip
		Port: port,
		User: user,
	}
	sess, err := m.dialWithJoinRetry(ctx, cfg, joinWait)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return sess, nil
}

// dialWithJoinRetry повторяет m.Dial(cfg) с фиксированным backoff + jitter, пока
// не получит сессию или не истечёт joinWait (либо ctx). По deadline возвращает
// последнюю ошибку Dial — caller переведёт шаг в failed (B1-strict, error_locked).
//
// Первая попытка — немедленно (без ожидания): если нода уже online (re-run /
// долгий provision), доставка не платит лишний интервал.
func (m *Module) dialWithJoinRetry(ctx context.Context, cfg push.DialConfig, joinWait time.Duration) (push.Session, error) {
	base, jitter := m.retryBackoff()
	deadline := time.Now().Add(joinWait)
	var lastErr error
	for attempt := 0; ; attempt++ {
		sess, err := m.Dial(ctx, cfg)
		if err == nil {
			return sess, nil
		}
		lastErr = err

		// Контекст отменён (прогон прерван) — выходим немедленно с ctx-ошибкой.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("teleport join wait cancelled after %d attempt(s): %w", attempt+1, ctx.Err())
		}
		// Бюджет ожидания исчерпан — VM так и не появилась в Teleport.
		wait := base + time.Duration(rand.Int63n(int64(jitter)+1))
		if time.Now().Add(wait).After(deadline) {
			return nil, fmt.Errorf("node not reachable via Teleport within join_wait_timeout (%s, %d attempt(s)): %w", joinWait, attempt+1, lastErr)
		}

		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("teleport join wait cancelled after %d attempt(s): %w", attempt+1, ctx.Err())
		case <-t.C:
		}
	}
}

func (m *Module) providerNames() []string {
	out := make([]string, 0, len(m.Providers))
	for k := range m.Providers {
		out = append(out, k)
	}
	return out
}

// parseHosts извлекает список {sid, primary_ip, bootstrap_token} из param
// `hosts`. На практике приходит CEL-выражением `${ register.<provision>.hosts }`
// (выход core.cloud.created). Пустой список / отсутствие обязательных полей —
// ошибка (нечего доставлять / некуда / нечего записывать).
func parseHosts(params *structpb.Struct) ([]hostInput, error) {
	lv, err := util.ListParam(params, "hosts")
	if err != nil {
		return nil, err
	}
	if len(lv) == 0 {
		return nil, fmt.Errorf("param %q: empty list (no hosts to deliver to)", "hosts")
	}
	out := make([]hostInput, 0, len(lv))
	for i, item := range lv {
		sv, ok := item.Kind.(*structpb.Value_StructValue)
		if !ok {
			return nil, fmt.Errorf("param %q[%d]: expected object, got %T", "hosts", i, item.Kind)
		}
		h, herr := hostFromStruct(sv.StructValue, i)
		if herr != nil {
			return nil, herr
		}
		out = append(out, h)
	}
	return out, nil
}

func hostFromStruct(s *structpb.Struct, idx int) (hostInput, error) {
	sid, err := util.StringParam(s, "sid")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	ip, err := util.StringParam(s, "primary_ip")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	tok, err := util.StringParam(s, "bootstrap_token")
	if err != nil {
		return hostInput{}, fmt.Errorf("param %q[%d].%w", "hosts", idx, err)
	}
	return hostInput{sid: sid, primaryIP: ip, token: tok}, nil
}

// maskErr маскирует возможный leak секретов (vault-ref / токен) в тексте ошибки
// перед выдачей в failed-event. Тот же substring-фильтр shared/audit, что чистит
// register-output (`token`-фрагмент + vault-ref). Ключ `_` несекретный.
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
