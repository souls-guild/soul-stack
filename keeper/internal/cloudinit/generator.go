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
//
// Сам install-blueprint (write_files + runcmd, пути/права) вынесен в shared
// [keeper/internal/soulinstall] (ADR-063 amendment 2026-06-30): этот пакет —
// config-резолвер (Vault) + тонкая обёртка над `soulinstall.RenderCloudInitYAML`.
// Внешний контракт (Config/Resolver/GenerateUserdata) сохранён.
package cloudinit

import (
	"context"
	"errors"
	"fmt"

	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"
	keepervault "github.com/souls-guild/soul-stack/keeper/internal/vault"
	"github.com/souls-guild/soul-stack/shared/config"
)

// Значения SoulBinaryCA — какой trust-store использует curl при скачивании
// soul-бинаря. Re-export из soulinstall (единый словарь для обоих рендереров);
// сохраняются как публичный API пакета для существующих вызовов/тестов.
const (
	// SoulBinaryCAKeeper — pin на PEM-CA Keeper-а (`curl --cacert keeper-ca.pem`).
	SoulBinaryCAKeeper = soulinstall.SoulBinaryCAKeeper
	// SoulBinaryCASystem — OS-trust bundle (`curl` без `--cacert`); для artifact-
	// хостов с публичным CA (например, Nexus за GlobalSign).
	SoulBinaryCASystem = soulinstall.SoulBinaryCASystem
)

// Config — резолвленные параметры рендера userdata. Создаётся из
// [shared/config.KeeperCloudInit] + [keepervault.Client] на каждом
// GenerateUserdata-вызове (hot-reload-friendly: каждый apply подхватывает
// текущий config.Store snapshot).
type Config struct {
	// BootstrapEndpoint — `host:port` LB Keeper-а (Bootstrap-RPC listener).
	// host идёт в soul.yml keeper.endpoints[0].host, port — в bootstrap_port.
	BootstrapEndpoint string

	// EventStreamPort — TCP-порт EventStream-фазы (mTLS) того же host-а;
	// soul.yml event_stream_port. 0 → порт bootstrap_endpoint (back-compat,
	// single-port LB). 6-я стена ADR-063.
	EventStreamPort int

	// TLSCAPem — PEM-encoded CA Keeper-а (содержимое `ca`-поля из Vault KV).
	// Запекается в userdata под `write_files: /etc/soul/tls/keeper-ca.pem`,
	// затем curl --cacert использует его при скачивании soul-бинаря.
	TLSCAPem string

	// SoulBinaryURL — HTTPS URL для скачивания `soul`-бинаря. Plain http
	// отвергается (security: только над TLS, независимо от SoulBinaryCA).
	SoulBinaryURL string

	// SoulBinaryCA — trust-store для curl при скачивании бинаря:
	// SoulBinaryCAKeeper (default/пусто) → `--cacert keeper-ca.pem`;
	// SoulBinaryCASystem → системный bundle (без `--cacert`, для публичных CA).
	// Ослабляет ТОЛЬКО верификацию cert artifact-хоста; Bootstrap-канал и
	// SHA256-verify бинаря не затрагиваются.
	SoulBinaryCA string

	// SoulVersion — опц. строка, попадает в userdata как комментарий-метка
	// (для диагностики). Sig-verify бинаря отложен (ADR-017(h) amendment).
	SoulVersion string
}

// Blueprint собирает [soulinstall.Blueprint] из Config — ЕДИНСТВЕННАЯ точка
// маппинга cloudinit.Config → shared blueprint. Экспортирована, чтобы install-
// путь (core.bootstrap.delivered) собирал blueprint тем же маппером, а не
// дублировал список полей: новое поле Blueprint не потеряется молча в install.
func (c Config) Blueprint() soulinstall.Blueprint {
	return soulinstall.Blueprint{
		BootstrapEndpoint: c.BootstrapEndpoint,
		EventStreamPort:   c.EventStreamPort,
		KeeperCAPem:       c.TLSCAPem,
		SoulBinaryURL:     c.SoulBinaryURL,
		SoulBinaryCA:      c.SoulBinaryCA,
		SoulVersion:       c.SoulVersion,
	}
}

// Validate проверяет, что Config заполнен достаточно для рендера. Делегирует
// в [soulinstall.Blueprint.Validate] (единый набор проверок для обоих путей).
func (c Config) Validate() error {
	return c.Blueprint().Validate()
}

// GenerateUserdata рендерит cloud-config YAML. Идемпотентна: на тех же входах
// даёт байт-идентичный вывод. Тонкая обёртка над
// [soulinstall.RenderCloudInitYAML] (install-blueprint вынесен в shared).
//
// Безопасность: вывод проверяется на отсутствие подстроки `bootstrap_token` /
// `vault:` (security floor) — внутри soulinstall-рендера.
func GenerateUserdata(cfg Config) (string, error) {
	return soulinstall.RenderCloudInitYAML(cfg.Blueprint())
}

// GenerateUserdataSelfOnboard рендерит cloud-config YAML для self-onboard
// «Вариант T» (ADR-017(h) amendment): userdata несёт map FQDN→plain-token и фазу
// `soul init` (токен по hostname). Keeper предсказывает FQDN каждой VM ДО create
// и передаёт сюда токены. Токены попадают в userdata (тест-стенд) — security-guard
// `bootstrap_token` снят внутри soulinstall для этого режима (см. Blueprint.SelfOnboardTokens).
//
// tokens пуст → ошибка (self-onboard без токенов бессмыслен; caller обязан
// передать непустой map). vault-ref-floor остаётся активен и здесь.
func GenerateUserdataSelfOnboard(cfg Config, tokens map[string]string) (string, error) {
	if len(tokens) == 0 {
		return "", errors.New("cloud_init: self-onboard requires non-empty FQDN→token map")
	}
	bp := cfg.Blueprint()
	bp.SelfOnboardTokens = tokens
	return soulinstall.RenderCloudInitYAML(bp)
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
		EventStreamPort:   cfg.EventStreamPort,
		TLSCAPem:          caPem,
		SoulBinaryURL:     cfg.SoulBinaryURL,
		SoulBinaryCA:      cfg.SoulBinaryCA,
		SoulVersion:       cfg.SoulVersion,
	}, nil
}
