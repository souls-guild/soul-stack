// Package cloudinit — рендер cloud-init userdata для VM, создаваемых
// `core.cloud.provisioned` (ADR-017(h) amendment 2026-05-27, B-flat).
//
// Userdata содержит ТОЛЬКО soul-инициализацию: установка `soul`-бинаря через
// pinned-CA HTTPS-curl, конфиг `soul.yml` с `keeper.endpoints` (host:port LB),
// embedded PEM CA Keeper-а и systemd-unit `soul.service`. Per-VM bootstrap-
// токен НЕ запекается в userdata: cloud-provider API хранит userdata в plaintext
// metadata, доступной процессам VM (security floor). Per-VM-токен выписывается
// в `applyCreated` после Create и кладётся в register-output задачи; доставка
// на VM — отдельный шаг scenario (типично `keeper.push` через SSH-провайдер).
//
// CA Keeper-а резолвится из Vault по `tls_ca_ref` (вызов `ReadKV` поля `ca`).
// CA — публичный материал, но единый источник правды в Vault нужен для
// ротации без правок keeper.yml.
package cloudinit

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"text/template"

	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

//go:embed templates/cloud-init.tmpl
var templatesFS embed.FS

// parsedTemplate — один раз распарсенный template из embed.FS. parse-time
// проверяется тестом TestGenerateUserdata_HappyPath; ошибка тут — bug в коде
// (template byte-compiled из go:embed), а не runtime-issue.
var parsedTemplate = template.Must(
	template.New("cloud-init.tmpl").ParseFS(templatesFS, "templates/cloud-init.tmpl"),
)

// Config — резолвленные параметры рендера userdata. Создаётся из
// [shared/config.KeeperCloudInit] + [keepervault.Client] на каждом
// GenerateUserdata-вызове (hot-reload-friendly: каждый apply подхватывает
// текущий config.Store snapshot).
type Config struct {
	// BootstrapEndpoint — `host:port` LB Keeper-а (Bootstrap-RPC listener).
	// Тот же host:port используется в userdata-шаблоне как event_stream_port
	// (за LB-ом обе фазы ведут на разные backend-listener-ы; Soul ещё
	// SoulSeed-cert-а не имеет — обращается только к Bootstrap, EventStream
	// добавится после онбординга).
	BootstrapEndpoint string

	// TLSCAPem — PEM-encoded CA Keeper-а (содержимое `ca`-поля из Vault KV).
	// Запекается в userdata под `write_files: /etc/soul/tls/keeper-ca.pem`,
	// затем curl --cacert использует его при скачивании soul-бинаря.
	TLSCAPem string

	// SoulBinaryURL — HTTPS URL для скачивания `soul`-бинаря. Plain http
	// отвергается (security: pinned-CA только над TLS).
	SoulBinaryURL string

	// SoulVersion — опц. строка, попадает в userdata как комментарий-метка
	// (для диагностики). Sig-verify бинаря отложен (ADR-017(h) amendment).
	SoulVersion string
}

// Validate проверяет, что Config заполнен достаточно для рендера. Возвращает
// первую найденную ошибку с понятным сообщением (несколько ошибок поднимать
// не нужно: config-фаза уже отловила format-issues, тут — runtime-precondition).
func (c Config) Validate() error {
	if c.BootstrapEndpoint == "" {
		return errors.New("cloud_init.bootstrap_endpoint is empty")
	}
	if _, _, err := net.SplitHostPort(c.BootstrapEndpoint); err != nil {
		return fmt.Errorf("cloud_init.bootstrap_endpoint %q is not host:port: %w", c.BootstrapEndpoint, err)
	}
	if !strings.Contains(c.TLSCAPem, "BEGIN CERTIFICATE") {
		return errors.New("cloud_init.tls_ca is not a PEM-encoded certificate")
	}
	if c.SoulBinaryURL == "" {
		return errors.New("cloud_init.soul_binary_url is empty")
	}
	if !strings.HasPrefix(c.SoulBinaryURL, "https://") {
		return fmt.Errorf("cloud_init.soul_binary_url %q must be https (pinned-CA over TLS only)", c.SoulBinaryURL)
	}
	return nil
}

// GenerateUserdata рендерит cloud-config YAML по template. Идемпотентна: на
// тех же входах даёт байт-идентичный вывод.
//
// Безопасность: вывод проверяется на отсутствие подстроки `bootstrap_token` /
// `vault:` — защита от случайной утечки секретов из template-а (например, если
// в Config попадёт строка с такой подстрокой). Это инвариант для всех каналов,
// куда уходит userdata (cloud-provider metadata, audit-payload, OTel-attrs).
func GenerateUserdata(cfg Config) (string, error) {
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	host, portStr, _ := net.SplitHostPort(cfg.BootstrapEndpoint)
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", fmt.Errorf("cloud_init.bootstrap_endpoint %q: port not valid: %v", cfg.BootstrapEndpoint, err)
	}

	view := struct {
		BootstrapHost    string
		BootstrapPort    int
		TLSCAPemIndented string
		SoulBinaryURL    string
		SoulVersion      string
	}{
		BootstrapHost:    host,
		BootstrapPort:    port,
		TLSCAPemIndented: indentPEM(cfg.TLSCAPem, "      "),
		SoulBinaryURL:    cfg.SoulBinaryURL,
		SoulVersion:      cfg.SoulVersion,
	}

	var sb strings.Builder
	if err := parsedTemplate.Execute(&sb, view); err != nil {
		return "", fmt.Errorf("cloud-init render: %w", err)
	}
	out := sb.String()

	// Security floor: userdata НЕ должен нести bootstrap-токен или vault-ref.
	// Token — отдельный шаг scenario (B-flat), vault-ref — резолвится тут.
	if strings.Contains(out, "bootstrap_token") {
		return "", errors.New("cloud-init render: output contains 'bootstrap_token' substring — userdata must not carry per-VM tokens (B-flat invariant)")
	}
	if strings.Contains(out, "vault:") {
		return "", errors.New("cloud-init render: output contains 'vault:' substring — vault-refs must be resolved before render")
	}
	return out, nil
}

// indentPEM сдвигает каждую строку PEM-блока на `prefix`, чтобы он лёг под
// YAML-ключ с heredoc-style `|` (cloud-config). Trailing newline сохраняется,
// чтобы между PEM и следующим YAML-ключом был корректный break.
func indentPEM(pem, prefix string) string {
	lines := strings.Split(strings.TrimRight(pem, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

// Resolver резолвит [config.KeeperCloudInit] в [Config] с подгрузкой PEM CA
// из Vault. Создаётся одним экземпляром в daemon и переиспользуется на каждый
// GenerateUserdata-вызов; собственного state не несёт (vault-client читает
// snapshot KV каждый раз — ротация CA подхватывается без рестарта).
type Resolver struct {
	Vault VaultReader
}

// VaultReader — узкое подмножество [keepervault.Client], нужное для резолва
// CA-PEM. Симметрично keeper/internal/coremod/vault.VaultReader: упрощает
// unit-тесты (fake без поднятия HTTP).
type VaultReader interface {
	ReadKV(ctx context.Context, path string) (map[string]any, error)
}

// NewResolver — wire-helper. nil vc допустим в тестовых сборках; реальный
// Resolve тогда вернёт явную ошибку.
func NewResolver(vc VaultReader) *Resolver {
	return &Resolver{Vault: vc}
}

// Resolve превращает config-блок keeper.yml в готовый к рендеру [Config]:
// разбирает vault-ref TLSCARef и читает поле `ca` из KV.
//
// Возвращает ошибку с маскированным vault-ref-ом (как cloud-resolver), чтобы
// при провале чтения путь к секрету не утекал наружу — резолв ВСЕХ vault-ref-ов
// (включая публичный CA) идёт через keeper-vault-клиент, аккуратность одинаковая.
func (r *Resolver) Resolve(ctx context.Context, cfg *config.KeeperCloudInit) (Config, error) {
	if cfg == nil {
		return Config{}, errors.New("cloud_init: keeper.yml block is missing (set keeper.cloud_init.* to use generate_userdata)")
	}
	if cfg.BootstrapEndpoint == "" {
		return Config{}, errors.New("cloud_init.bootstrap_endpoint is empty in keeper.yml")
	}
	if cfg.TLSCARef == "" {
		return Config{}, errors.New("cloud_init.tls_ca_ref is empty in keeper.yml")
	}
	if cfg.SoulBinaryURL == "" {
		return Config{}, errors.New("cloud_init.soul_binary_url is empty in keeper.yml")
	}
	if r.Vault == nil {
		return Config{}, errors.New("cloud_init: vault client is not configured (cannot resolve tls_ca_ref)")
	}

	logical, err := keepervault.ParseRef(cfg.TLSCARef)
	if err != nil {
		return Config{}, fmt.Errorf("cloud_init.tls_ca_ref: %w", err)
	}
	kv, err := r.Vault.ReadKV(ctx, logical)
	if err != nil {
		return Config{}, fmt.Errorf("cloud_init.tls_ca_ref: read vault failed")
	}
	caRaw, ok := kv["ca"]
	if !ok {
		return Config{}, fmt.Errorf("cloud_init.tls_ca_ref: vault KV at %q has no field %q", logical, "ca")
	}
	caPem, ok := caRaw.(string)
	if !ok {
		return Config{}, fmt.Errorf("cloud_init.tls_ca_ref: field %q is not a string", "ca")
	}

	return Config{
		BootstrapEndpoint: cfg.BootstrapEndpoint,
		TLSCAPem:          caPem,
		SoulBinaryURL:     cfg.SoulBinaryURL,
		SoulVersion:       cfg.SoulVersion,
	}, nil
}
