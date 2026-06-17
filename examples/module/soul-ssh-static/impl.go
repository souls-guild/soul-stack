package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"github.com/souls-guild/soul-stack/sdk/sshprovider"
	"golang.org/x/crypto/ssh"
)

// paramsEnv — env-var, через который keeper.push передаёт static-провайдеру его
// params (JSON по schema.json) при fork-е плагина. Параллель с
// handshake.SocketEnv: SshProvider-контракт (Sign/Authorize) не несёт per-request
// параметров провайдера, поэтому config приезжает на старте процесса, как и
// путь к сокету. Это convention static-провайдера (имя env скоупится этим
// бинарём), НЕ generic-механизм доставки params для всех плагинов — последнее
// затронуло бы handshake/proto и решается отдельным ADR.
const paramsEnv = "SOUL_SSH_STATIC_PARAMS"

// params — конфигурация static-провайдера, разобранная из paramsEnv.
type params struct {
	// KeyPath — путь к приватному SSH-ключу на keeper-host-е (взаимоисключающе
	// с VaultRef; oneOf в schema.json).
	KeyPath string `json:"key_path"`
	// VaultRef — ссылка на Vault KV-секрет с ключом. В пилоте резолв из Vault
	// НЕ реализован (keeper.push резолвит секрет и подставляет key_path —
	// A-flow, параллель с cloud credentials); поле здесь для полноты schema и
	// fail-closed-ветки.
	VaultRef string `json:"vault_ref"`
	// Deny — deny-list пар (host, user). Пустой = allow-all (dev/test default).
	Deny []denyRule `json:"deny"`
}

// denyRule — одна запись deny-list. Пустое поле = wildcard по этому измерению
// (host:"" → любой host; user:"" → любой user). Запись из двух пустых полей
// денит всё — это явный «закрыть провайдер».
type denyRule struct {
	Host string `json:"host"`
	User string `json:"user"`
}

func (r denyRule) matches(host, user string) bool {
	return (r.Host == "" || r.Host == host) && (r.User == "" || r.User == user)
}

// StaticProvider — SshProvider поверх долгоживущего static-ключа на keeper-host-е
// (ADR-016 dev/test и инсталляции без Vault). Sign возвращает готовую пару
// (private_key из файла, certificate=""); public_key из запроса игнорируется
// (static не подписывает ключ клиента). Authorize — deny-list, по умолчанию
// allow-all.
type StaticProvider struct {
	sshprovider.BaseProvider
	cfg params
}

// loadParams читает и валидирует params из env. fail-closed: невалидный JSON,
// отсутствие источника ключа или vault_ref (не реализован в пилоте) → ошибка,
// плагин не стартует.
func loadParams() (params, error) {
	raw := os.Getenv(paramsEnv)
	if raw == "" {
		return params{}, fmt.Errorf("env %s пуст: static-провайдеру нужны params", paramsEnv)
	}
	var p params
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return params{}, fmt.Errorf("разбор %s: %w", paramsEnv, err)
	}
	if p.VaultRef != "" && p.KeyPath == "" {
		// A-flow: резолв Vault KV → key_path делает keeper.push до запуска
		// плагина (как cloud credentials). Прямой vault-резолв в плагине —
		// вне пилота.
		return params{}, errors.New("vault_ref резолвится keeper.push в key_path до запуска плагина (vault-резолв в плагине — вне пилота)")
	}
	if p.KeyPath == "" {
		return params{}, errors.New("не задан источник ключа: нужен key_path (или vault_ref, резолвимый в key_path)")
	}
	return p, nil
}

// Sign читает приватный ключ из cfg.KeyPath, проверяет его парсимость
// (fail-closed: keeper.push дальше делает ssh.ParsePrivateKey — лучше упасть
// здесь с понятной причиной) и возвращает готовую пару. certificate="" —
// static-провайдер ничего не подписывает.
func (s *StaticProvider) Sign(_ context.Context, _ *pluginv1.SignRequest) (*pluginv1.SignReply, error) {
	keyPEM, err := os.ReadFile(s.cfg.KeyPath)
	if err != nil {
		return nil, sshprovider.SignError(sshprovider.SignFailReadKey, fmt.Errorf("чтение %s: %w", s.cfg.KeyPath, err))
	}
	if _, perr := ssh.ParsePrivateKey(keyPEM); perr != nil {
		return nil, sshprovider.SignError(sshprovider.SignFailReadKey, fmt.Errorf("разбор ключа %s: %w", s.cfg.KeyPath, perr))
	}
	return &pluginv1.SignReply{
		Certificate: "",
		PrivateKey:  string(keyPEM),
		// Static-ключ долгоживущий: 0 = «без срока refresh» (keeper.push не
		// планирует ротацию для static, в отличие от CA-провайдеров).
		TtlSeconds: 0,
	}, nil
}

// Authorize — deny-list. Пустой deny → allow-all (dev/test). Совпадение с любым
// правилом → deny с reason из словаря SDK (для агрегации причин на Keeper-е).
func (s *StaticProvider) Authorize(_ context.Context, req *pluginv1.AuthorizeRequest) (*pluginv1.AuthorizeReply, error) {
	for _, rule := range s.cfg.Deny {
		if rule.matches(req.GetHost(), req.GetUser()) {
			return &pluginv1.AuthorizeReply{
				Allowed: false,
				Reason:  sshprovider.DenyMessage(sshprovider.DenyExplicitDeny, req.GetUser()+"@"+req.GetHost()),
			}, nil
		}
	}
	return &pluginv1.AuthorizeReply{Allowed: true}, nil
}
