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
// Provider, HostCAs и Dial обязательны (nil даёт явную ошибку при Apply либо
// не-регистрацию в Registry). Audit опционален (nil → запись пропускается).
type Module struct {
	// Provider — резолв SSH-провайдера по имени param `ssh_provider`. MVP —
	// single-provider: модуль держит карту, собранную wire-up-ом из
	// дискаверенных SshProvider-плагинов (по manifest.Name).
	Providers map[string]SshProviderHost

	// HostCAs — multi-CA-набор для verify host-cert целевых VM (тот же
	// push.LoadHostCAs, что у SshDispatcher). Непустой; пустой → Apply вернёт
	// явную ошибку (CA-signed host-cert verify обязателен, fail-closed).
	HostCAs []push.NamedHostKeyAuthority

	// Dial — открытие SSH-сессии. Прод — push.Dial; тест — мок-Dialer.
	Dial push.Dialer

	// Audit — audit-writer (`bootstrap.delivered`). nil → запись пропускается.
	Audit AuditWriter
}

// hostInput — одна VM из param `hosts` (= register.<provision>.hosts от
// core.cloud.created): SID, IP для коннекта и plain bootstrap-токен.
type hostInput struct {
	sid       string
	primaryIP string
	token     string
}

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

	// Конфигурационные предусловия — явная ошибка вместо невнятного nil-panic.
	if m.Dial == nil {
		return util.SendFailed(stream, "bootstrap delivered: dialer not configured (wire push.Dial in main)")
	}
	if len(m.HostCAs) == 0 {
		return util.SendFailed(stream, "bootstrap delivered: host CAs not configured (set keeper.yml::push.host_ca_refs[] — CA-signed host-cert verify обязателен)")
	}
	prov, ok := m.Providers[providerName]
	if !ok || prov == nil {
		return util.SendFailed(stream, fmt.Sprintf("bootstrap delivered: ssh_provider %q not registered (known: %v)", providerName, m.providerNames()))
	}

	script := fmt.Sprintf(deliverScriptFmt, tokenPath, tokenPath)

	results := make([]any, 0, len(hosts))
	sids := make([]any, 0, len(hosts))
	for _, h := range hosts {
		started, err := m.deliverHost(ctx, prov, h, sshUser, int(sshPort), script, startSoul)
		if err != nil {
			// B1-strict: ошибка любого хоста валит весь шаг. maskErr страхует
			// от утечки vault-ref/токена в текст ошибки (failed-event уходит в
			// status_details — наблюдаемый канал).
			return util.SendFailed(stream, fmt.Sprintf("deliver token to %q (%s): %s", h.sid, h.primaryIP, maskErr(err)))
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

// deliverHost обрабатывает один хост: Authorize → ephemeral keypair + Sign →
// Dial → запись токена (STDIN) → опц. start soul. Возвращает (started, error).
//
// Переиспользует push-инфраструктуру (newEphemeralEd25519/authMethodsFromSign/
// Dial/Session) ровно как SshDispatcher.SendApply — тот же CA-host-cert-verify
// путь, ephemeral-приватник не покидает Keeper.
func (m *Module) deliverHost(ctx context.Context, prov SshProviderHost, h hostInput, user string, port int, script string, startSoul bool) (started bool, err error) {
	// Authorize — fail-closed: deny прекращает доставку до connect-а.
	authReply, err := prov.Authorize(ctx, &pluginv1.AuthorizeRequest{Host: h.primaryIP, User: user})
	if err != nil {
		return false, fmt.Errorf("authorize %s@%s: %w", user, h.primaryIP, err)
	}
	if !authReply.GetAllowed() {
		return false, fmt.Errorf("authorize denied for %s@%s: %s", user, h.primaryIP, authReply.GetReason())
	}

	// Ephemeral keypair: Keeper-side ed25519-пара per-host. Pubkey уезжает в
	// SignRequest для CA-провайдеров; приватник НИКОГДА не покидает Keeper.
	ephSigner, ephPub, err := push.NewEphemeralEd25519()
	if err != nil {
		return false, fmt.Errorf("ephemeral keypair: %w", err)
	}
	signReply, err := prov.Sign(ctx, &pluginv1.SignRequest{Host: h.primaryIP, User: user, PublicKey: ephPub})
	if err != nil {
		return false, fmt.Errorf("sign %s@%s: %w", user, h.primaryIP, err)
	}
	auth, err := push.AuthMethodsFromSign(signReply, ephSigner)
	if err != nil {
		return false, fmt.Errorf("ssh auth: %w", err)
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
		return false, fmt.Errorf("connect: %w", err)
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
