package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
)

// paramsEnv — env-var, через который keeper.push передаёт teleport-провайдеру
// его params (JSON по schema.json) при fork-е плагина. Симметрично
// soul-ssh-vault / soul-ssh-static: SshProvider-контракт (Sign/Authorize) не
// несёт per-request параметров провайдера, поэтому config приезжает на старте
// процесса, как и путь к сокету.
const paramsEnv = "SOUL_SSH_TELEPORT_PARAMS"

// params — конфигурация teleport-провайдера, разобранная из paramsEnv.
type params struct {
	// ProxyAddr — endpoint Teleport-proxy (`host:port`). Едет в
	// SignReply.proxy_jump для последующего использования keeper.push
	// (dispatcher proxy_jump support — S3).
	ProxyAddr string `json:"proxy_addr"`
	// ClusterName — имя Teleport-кластера (опц., для multi-cluster trust).
	ClusterName string `json:"cluster_name"`
	// IdentityFile — путь к Teleport identity-file (creds-flow B).
	IdentityFile string `json:"identity_file"`
	// TbotSocket — путь к Unix-сокету tbot-демона (альтернатива
	// IdentityFile). Один из двух источников обязателен.
	TbotSocket string `json:"tbot_socket"`
	// Roles — список Teleport-ролей, под которыми запрашивается cert.
	// Пустой = роли по умолчанию identity/bot.
	Roles []string `json:"roles"`
	// ValidPrincipals — allowlist principal-ов поверх Teleport-роли;
	// пустой = принимать любой req.User.
	ValidPrincipals []string `json:"valid_principals"`
	// Deny — deny-list пар (host, user); пустой = allow-all (dev/test).
	Deny []denyRule `json:"deny"`
}

// denyRule — одна запись deny-list. Пустое поле = wildcard по этому измерению.
// Семантика идентична soul-ssh-vault / soul-ssh-static.
type denyRule struct {
	Host string `json:"host"`
	User string `json:"user"`
}

func (r denyRule) matches(host, user string) bool {
	return (r.Host == "" || r.Host == host) && (r.User == "" || r.User == user)
}

// loadParams читает и валидирует params из env. fail-closed: невалидный JSON,
// отсутствие proxy_addr, отсутствие источника credentials (identity_file и
// tbot_socket оба пусты) → ошибка, плагин не стартует.
func loadParams() (params, error) {
	raw := os.Getenv(paramsEnv)
	if raw == "" {
		return params{}, fmt.Errorf("env %s пуст: teleport-провайдеру нужны params", paramsEnv)
	}
	var p params
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return params{}, fmt.Errorf("разбор %s: %w", paramsEnv, err)
	}
	if p.ProxyAddr == "" {
		return params{}, errors.New("proxy_addr обязателен (Teleport proxy host:port)")
	}
	if p.IdentityFile == "" && p.TbotSocket == "" {
		return params{}, errors.New("нужен источник credentials: identity_file или tbot_socket")
	}
	if p.IdentityFile != "" && p.TbotSocket != "" {
		return params{}, errors.New("identity_file и tbot_socket взаимоисключающи (oneOf)")
	}
	return p, nil
}

// TeleportProvider — SshProvider поверх Teleport Auth API.
//
// Sign: аутентифицируется в Teleport через identity_file/tbot, вызывает
// GenerateUserCerts на req.PublicKey (Keeper-ephemeral), возвращает только
// certificate (private_key="") + proxy_jump=<proxy_addr>. Authorize: deny-list,
// по умолчанию allow-all.
type TeleportProvider struct {
	sshprovider.BaseProvider
	cfg params
	// newClient — фабрика Teleport-клиента (внедряется для unit-тестов).
	// nil → defaultClient.
	newClient func(ctx context.Context, p params) (teleportClient, error)
}

// teleportClient — узкий интерфейс над Teleport Auth API: только то, чем
// пользуется TeleportProvider. Держит unit-тесты простыми (mock без полного
// SDK) и сужает поверхность зависимости — production-реализация в одном месте.
type teleportClient interface {
	// GenerateUserSSHCert вызывает Teleport `GenerateUserCerts` с заданным
	// public_key и принципалом. Возвращает PEM/openssh-encoded SSH user
	// certificate (поле Cert ответа SSH). Не возвращает private_key —
	// Keeper-ephemeral mode: приватник остаётся у Keeper-а.
	GenerateUserSSHCert(ctx context.Context, pubkey, principal string, roles []string) (cert string, err error)
	// Close освобождает gRPC-коннект в Teleport (best-effort).
	Close() error
}

// Sign — Keeper-ephemeral режим (PM-decision SSH key-ownership):
//
//   - req.PublicKey должен быть непустым OpenSSH authorized_keys-encoded
//     pubkey (ephemeral, который Keeper сгенерил per-session). Пустой →
//     fail-closed (Teleport CA не подписывает «пустой» ключ).
//   - принципал — req.User (Linux-пользователь на push-хосте); если в
//     params задан valid_principals — req.User должен в нём быть (локальный
//     allowlist поверх Teleport-роли).
//   - reply.private_key = "" — приватник остаётся у Keeper-а.
//   - reply.proxy_jump = cfg.ProxyAddr — для последующего использования
//     keeper.push (на момент пилота dispatcher это поле игнорирует, см.
//     docstring пакета и docs/keeper/plugins.md).
func (t *TeleportProvider) Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	if req.GetPublicKey() == "" {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			errors.New("public_key пуст: Teleport работает только в Keeper-ephemeral режиме"))
	}
	if len(t.cfg.ValidPrincipals) > 0 && !contains(t.cfg.ValidPrincipals, req.GetUser()) {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			fmt.Errorf("user %q не в valid_principals", req.GetUser()))
	}

	newClient := t.newClient
	if newClient == nil {
		newClient = defaultClient
	}
	client, err := newClient(ctx, t.cfg)
	if err != nil {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			fmt.Errorf("teleport auth: %w", err))
	}
	defer func() { _ = client.Close() }()

	signed, err := client.GenerateUserSSHCert(ctx, req.GetPublicKey(), req.GetUser(), t.cfg.Roles)
	if err != nil {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			fmt.Errorf("teleport GenerateUserCerts: %w", err))
	}
	if signed == "" {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			errors.New("teleport вернул пустой ssh-cert"))
	}

	return &pluginv1.SignReply{
		Certificate: signed,
		PrivateKey:  "",
		// Teleport-cert TTL приходит от Auth-роли; keeper.push сейчас не
		// планирует refresh (одноразовый cert на сессию), 0 = «без refresh».
		TtlSeconds: 0,
		ProxyJump:  t.cfg.ProxyAddr,
	}, nil
}

// Authorize — deny-list. Пустой deny → allow-all (dev/test). Симметрично
// soul-ssh-vault / soul-ssh-static: тираж SshProvider использует общий
// словарь reason-кодов из sdk/sshprovider для агрегации причин отказа в audit
// Keeper-а.
func (t *TeleportProvider) Authorize(_ context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	for _, rule := range t.cfg.Deny {
		if rule.matches(req.GetHost(), req.GetUser()) {
			return &pluginv1.AuthorizeReply{
				Allowed: false,
				Reason:  sshprovider.DenyMessage(sshprovider.DenyExplicitDeny, req.GetUser()+"@"+req.GetHost()),
			}, nil
		}
	}
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
