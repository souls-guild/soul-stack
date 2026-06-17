package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	vaultapi "github.com/hashicorp/vault/api"
	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
)

// paramsEnv — env-var, через который keeper.push передаёт vault-провайдеру его
// params (JSON по schema.json) при fork-е плагина. Симметрично soul-ssh-static:
// SshProvider-контракт (Sign/Authorize) не несёт per-request параметров
// провайдера, поэтому config приезжает на старте процесса, как и путь к сокету.
const paramsEnv = "SOUL_SSH_VAULT_PARAMS"

// authMethodToken / authMethodAppRole — поддерживаемые в MVP методы. Kubernetes
// и AWS IAM — расширение без правки proto/manifest (добавить case в authClient).
const (
	authMethodToken   = "token"
	authMethodAppRole = "approle"
)

// params — конфигурация vault-провайдера, разобранная из paramsEnv.
type params struct {
	// VaultAddr — базовый URL Vault API (https://vault.example.com:8200).
	VaultAddr string `json:"vault_addr"`
	// VaultMount — SSH CA mount path (без leading/trailing slash). По умолчанию "ssh".
	VaultMount string `json:"vault_mount"`
	// Role — Vault SSH role name.
	Role string `json:"role"`
	// AuthMethod — token / approle (см. константы). По умолчанию token.
	AuthMethod string `json:"auth_method"`
	// Token — Vault token для AuthMethod=token. SENSITIVE.
	Token string `json:"token"`
	// AppRole — креденшалы для AuthMethod=approle.
	AppRole appRoleCreds `json:"approle"`
	// ValidPrincipals — allowlist principal-ов; пустой = принимать `req.User`
	// без локальной фильтрации (отдаётся Vault role на проверку).
	ValidPrincipals []string `json:"valid_principals"`
	// Deny — deny-list пар (host, user); пустой = allow-all (dev/test).
	Deny []denyRule `json:"deny"`
}

type appRoleCreds struct {
	RoleID   string `json:"role_id"`
	SecretID string `json:"secret_id"`
	// Mount — AppRole mount path (default "approle").
	Mount string `json:"mount"`
}

// denyRule — одна запись deny-list. Пустое поле = wildcard по этому измерению.
// Семантика идентична soul-ssh-static (одинаковая модель отказа для тиража).
type denyRule struct {
	Host string `json:"host"`
	User string `json:"user"`
}

func (r denyRule) matches(host, user string) bool {
	return (r.Host == "" || r.Host == host) && (r.User == "" || r.User == user)
}

// loadParams читает и валидирует params из env. fail-closed: невалидный JSON,
// отсутствие vault_addr/role, неподдерживаемый auth_method, нехватка
// auth-credentials → ошибка, плагин не стартует.
func loadParams() (params, error) {
	raw := os.Getenv(paramsEnv)
	if raw == "" {
		return params{}, fmt.Errorf("env %s пуст: vault-провайдеру нужны params", paramsEnv)
	}
	var p params
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return params{}, fmt.Errorf("разбор %s: %w", paramsEnv, err)
	}
	if p.VaultAddr == "" {
		return params{}, errors.New("vault_addr обязателен")
	}
	if p.Role == "" {
		return params{}, errors.New("role обязателен (имя Vault SSH role)")
	}
	if p.VaultMount == "" {
		p.VaultMount = "ssh"
	}
	if p.AuthMethod == "" {
		p.AuthMethod = authMethodToken
	}
	switch p.AuthMethod {
	case authMethodToken:
		if p.Token == "" {
			return params{}, errors.New("auth_method=token требует поле token")
		}
	case authMethodAppRole:
		if p.AppRole.RoleID == "" || p.AppRole.SecretID == "" {
			return params{}, errors.New("auth_method=approle требует approle.role_id и approle.secret_id")
		}
		if p.AppRole.Mount == "" {
			p.AppRole.Mount = "approle"
		}
	default:
		return params{}, fmt.Errorf("неподдерживаемый auth_method=%q (ожидался token|approle)", p.AuthMethod)
	}
	return p, nil
}

// VaultProvider — SshProvider поверх Vault SSH CA mount-а.
//
// Sign: ходит в Vault (vault_addr, ssh/sign/<role>), подписывает
// `req.PublicKey` (ephemeral pubkey от Keeper-а), возвращает только
// certificate (private_key=""). Authorize: deny-list, по умолчанию allow-all.
type VaultProvider struct {
	sshprovider.BaseProvider
	cfg params
	// newClient — фабрика Vault-клиента (внедряется для unit-тестов через
	// httptest-сервер). nil → defaultClient.
	newClient func(p params) (vaultClient, error)
}

// vaultClient — узкий интерфейс над *vaultapi.Client: только то, чем пользуется
// VaultProvider. Узкая поверхность держит unit-тесты простыми (мок без всего
// SDK), не плодя ради этого фабрику ради фабрики — реализация ровно одна.
type vaultClient interface {
	// SSHSign вызывает `<mount>/sign/<role>` с заданными data. Возвращает
	// signed-key (поле `signed_key` в Vault response) либо typed-error.
	SSHSign(ctx context.Context, mount, role string, data map[string]any) (signedKey string, err error)
}

// Sign — Keeper-ephemeral режим (PM-decision SSH key-ownership):
//
//   - req.PublicKey должен быть непустым OpenSSH authorized_keys (ephemeral
//     pubkey, который Keeper сгенерил per-session). Пустой → fail-closed (для
//     Vault SSH CA нет смысла без pubkey).
//   - принципал — req.User (Linux-пользователь на push-хосте); если в params
//     задан valid_principals — req.User должен в нём быть (локальный allowlist
//     поверх Vault role policy).
//   - reply.private_key = "" — приватник остаётся у Keeper-а.
func (v *VaultProvider) Sign(ctx context.Context, req *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	if req.GetPublicKey() == "" {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			errors.New("public_key пуст: Vault SSH CA работает только в Keeper-ephemeral режиме"))
	}
	if len(v.cfg.ValidPrincipals) > 0 && !contains(v.cfg.ValidPrincipals, req.GetUser()) {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			fmt.Errorf("user %q не в valid_principals", req.GetUser()))
	}

	newClient := v.newClient
	if newClient == nil {
		newClient = defaultClient
	}
	client, err := newClient(v.cfg)
	if err != nil {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			fmt.Errorf("vault auth: %w", err))
	}

	signed, err := client.SSHSign(ctx, v.cfg.VaultMount, v.cfg.Role, map[string]any{
		"public_key":       req.GetPublicKey(),
		"valid_principals": req.GetUser(),
		"cert_type":        "user",
	})
	if err != nil {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			fmt.Errorf("vault ssh/sign/%s: %w", v.cfg.Role, err))
	}
	if signed == "" {
		return nil, sshprovider.SignError(sshprovider.SignFailIssue,
			errors.New("vault вернул пустой signed_key"))
	}

	return &pluginv1.SignReply{
		Certificate: signed,
		PrivateKey:  "",
		// Vault SSH CA TTL приходит в response.lease_duration, но keeper.push
		// его пока не использует (одноразовый сертификат на сессию; refresh
		// делается следующим SendApply). 0 = «без планируемого refresh».
		TtlSeconds: 0,
	}, nil
}

// Authorize — deny-list. Пустой deny → allow-all (dev/test). Симметрично
// soul-ssh-static: тираж SshProvider использует общий словарь reason-кодов из
// sdk/sshprovider для агрегации причин отказа в audit Keeper-а.
func (v *VaultProvider) Authorize(_ context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	for _, rule := range v.cfg.Deny {
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

// --- vault client production-реализация ---

// realVaultClient — обёртка над *vaultapi.Client, реализующая узкий vaultClient.
type realVaultClient struct {
	c *vaultapi.Client
}

func (r *realVaultClient) SSHSign(ctx context.Context, mount, role string, data map[string]any) (string, error) {
	path := fmt.Sprintf("%s/sign/%s", mount, role)
	secret, err := r.c.Logical().WriteWithContext(ctx, path, data)
	if err != nil {
		return "", err
	}
	if secret == nil || secret.Data == nil {
		return "", errors.New("vault: пустой response от ssh/sign")
	}
	v, ok := secret.Data["signed_key"]
	if !ok {
		return "", errors.New("vault: в response нет поля signed_key")
	}
	signed, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("vault: signed_key не string (%T)", v)
	}
	return signed, nil
}

// defaultClient собирает production-vaultClient: создаёт vaultapi.Client,
// выполняет аутентификацию по cfg.AuthMethod.
func defaultClient(p params) (vaultClient, error) {
	cfg := vaultapi.DefaultConfig()
	cfg.Address = p.VaultAddr
	c, err := vaultapi.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("vault client: %w", err)
	}
	switch p.AuthMethod {
	case authMethodToken:
		c.SetToken(p.Token)
	case authMethodAppRole:
		secret, err := c.Logical().Write(
			fmt.Sprintf("auth/%s/login", p.AppRole.Mount),
			map[string]any{"role_id": p.AppRole.RoleID, "secret_id": p.AppRole.SecretID},
		)
		if err != nil {
			return nil, fmt.Errorf("approle login: %w", err)
		}
		if secret == nil || secret.Auth == nil || secret.Auth.ClientToken == "" {
			return nil, errors.New("approle login: пустой ClientToken")
		}
		c.SetToken(secret.Auth.ClientToken)
	default:
		return nil, fmt.Errorf("неподдерживаемый auth_method=%q", p.AuthMethod)
	}
	return &realVaultClient{c: c}, nil
}
