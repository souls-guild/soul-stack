// Package tlsx — общие TLS-helpers для Keeper-/Soul-/lint-бинарей.
//
// MVP-scope:
//   - [LoadServerOnlyTLS] (M2.1.b.1) — server-only TLS для Bootstrap-RPC
//     Keeper-а ([ADR-012(b)]). У Soul-а до онбординга ещё нет SoulSeed-cert.
//   - [LoadMutualTLS] (M2.2) — mTLS для EventStream-listener: серверный
//     cert + key + CA-bundle для валидации входящих SoulSeed клиентских
//     сертификатов (`RequireAndVerifyClientCert`).
//
// Post-MVP:
//   - LoadClientTLS для Soul-стороны (клиентский cert+key+server CA) —
//     M2.3.
//
// Package называется tlsx, чтобы не конфликтовать со stdlib `crypto/tls`
// в импортах caller-ов.
//
// [ADR-012(b)]: docs/adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add
package tlsx

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

// ServerConfig — параметры загрузки серверного TLS-конфига из файлов.
//
// Поле `CAPath` оставлено для будущего расширения (M2.2 mTLS); в
// [LoadServerOnlyTLS] оно игнорируется — наличие поля не превращает
// конфиг в mTLS-режим.
type ServerConfig struct {
	// CertPath — PEM-encoded x509-сертификат сервера (полная цепочка
	// допустима — `tls.LoadX509KeyPair` читает все PEM-блоки).
	CertPath string
	// KeyPath — приватный ключ к CertPath (PEM).
	KeyPath string
	// CAPath — резерв под mTLS (M2.2). В LoadServerOnlyTLS игнорируется.
	CAPath string
}

// LoadServerOnlyTLS читает cert + key из файлов и возвращает
// `*tls.Config` с `ClientAuth = NoClientCert` (server-only TLS).
//
// Минимальная версия TLS — 1.3 (cf. requirements.md «безопасность на
// первом месте» + ADR-012). Cipher suites не задаются: TLS 1.3 в Go
// сам выбирает AEAD-only suites.
//
// Ошибки:
//   - ErrServerCertEmpty / ErrServerKeyEmpty — пустые пути.
//   - wrapped fmt.Errorf при чтении файлов (например, отсутствующий
//     путь) — caller обязан показать с контекстом конфига.
func LoadServerOnlyTLS(cfg ServerConfig) (*tls.Config, error) {
	if cfg.CertPath == "" {
		return nil, ErrServerCertEmpty
	}
	if cfg.KeyPath == "" {
		return nil, ErrServerKeyEmpty
	}

	// Pre-flight — даём более понятную ошибку, чем
	// `tls.LoadX509KeyPair` на отсутствующем файле.
	if _, err := os.Stat(cfg.CertPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat cert %q: %w", cfg.CertPath, err)
	}
	if _, err := os.Stat(cfg.KeyPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat key %q: %w", cfg.KeyPath, err)
	}

	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: load cert/key pair: %w", err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// MutualConfig — параметры загрузки mTLS-конфига для серверного listener-а
// с обязательной валидацией клиентского сертификата (`ClientAuth =
// RequireAndVerifyClientCert`).
type MutualConfig struct {
	// CertPath — серверный cert.
	CertPath string
	// KeyPath — приватный ключ к CertPath.
	KeyPath string
	// CAPath — PEM-bundle CA, по которой валидируются клиентские
	// сертификаты (для Keeper EventStream — корень SoulSeed PKI).
	CAPath string
}

// LoadMutualTLS читает cert + key + CA-bundle и возвращает `*tls.Config`
// с `ClientAuth = RequireAndVerifyClientCert` и заполненным `ClientCAs`.
//
// Поведение симметрично [LoadServerOnlyTLS]:
//   - MinVersion = TLS 1.3;
//   - cipher suites не задаются (TLS 1.3 — AEAD-only by spec);
//   - pre-flight stat по всем трём путям даёт человеко-читаемые ошибки до
//     `tls.LoadX509KeyPair`.
//
// Дополнительная аутентификация по fingerprint (lookup в `soul_seeds`)
// делается **application-side**, в gRPC-interceptor-е caller-а: TLS-уровня
// достаточно проверить, что сертификат подписан нашим CA — это гарантирует,
// что cert выпускал Vault PKI Keeper-а, но не отличает active от revoked.
func LoadMutualTLS(cfg MutualConfig) (*tls.Config, error) {
	if cfg.CertPath == "" {
		return nil, ErrServerCertEmpty
	}
	if cfg.KeyPath == "" {
		return nil, ErrServerKeyEmpty
	}
	if cfg.CAPath == "" {
		return nil, ErrServerCAEmpty
	}

	if _, err := os.Stat(cfg.CertPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat cert %q: %w", cfg.CertPath, err)
	}
	if _, err := os.Stat(cfg.KeyPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat key %q: %w", cfg.KeyPath, err)
	}
	if _, err := os.Stat(cfg.CAPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat ca %q: %w", cfg.CAPath, err)
	}

	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: load cert/key pair: %w", err)
	}

	caPEM, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: read ca %q: %w", cfg.CAPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("tlsx: ca bundle %q has no valid PEM certificates", cfg.CAPath)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// ClientConfig — параметры клиентского TLS-конфига для Soul → Keeper.
//
// Используется на Soul-стороне в двух режимах:
//   - Bootstrap-фаза (`soul init`): только CAPath, mode=ServerOnly — у Soul-а
//     ещё нет SoulSeed-сертификата;
//   - EventStream-фаза (`soul run`): полный mTLS, все три пути обязательны.
type ClientConfig struct {
	// CertPath — клиентский SoulSeed-cert (PEM). Пустой для ServerOnly-режима.
	CertPath string
	// KeyPath — приватный ключ к CertPath (PEM). Пустой для ServerOnly-режима.
	KeyPath string
	// CAPath — PEM-bundle CA, по которой клиент валидирует серверный
	// сертификат Keeper-а. Обязателен в обоих режимах.
	CAPath string
	// ServerName — ожидаемый CN/SAN серверного сертификата. Пустая
	// строка = автоматически из адреса соединения (host:port → host).
	ServerName string
}

// LoadClientTLS возвращает `*tls.Config` для клиентского dial-а в Keeper.
//
// Семантика:
//   - CertPath/KeyPath пустые → server-only TLS (для Bootstrap RPC);
//   - все три пути заданы → mTLS (для EventStream).
//
// Минимальная TLS-версия — 1.3 (cf. requirements.md «безопасность на первом
// месте»). cipher suites не задаются (TLS 1.3 — AEAD-only by spec).
//
// ServerName применяется через `tls.Config.ServerName` для верификации
// hostname-а; для cluster-конфигов с несколькими endpoint-ами по разным
// host:port caller передаёт его явно (иначе gRPC автоматически выставит
// `authority`-host-а из target-а).
func LoadClientTLS(cfg ClientConfig) (*tls.Config, error) {
	if cfg.CAPath == "" {
		return nil, ErrServerCAEmpty
	}
	if _, err := os.Stat(cfg.CAPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat ca %q: %w", cfg.CAPath, err)
	}
	caPEM, err := os.ReadFile(cfg.CAPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: read ca %q: %w", cfg.CAPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("tlsx: ca bundle %q has no valid PEM certificates", cfg.CAPath)
	}

	out := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS13,
		ServerName: cfg.ServerName,
	}

	// Bootstrap-режим: cert+key не нужны, у Soul-а их пока нет.
	if cfg.CertPath == "" && cfg.KeyPath == "" {
		return out, nil
	}
	if cfg.CertPath == "" {
		return nil, ErrServerCertEmpty
	}
	if cfg.KeyPath == "" {
		return nil, ErrServerKeyEmpty
	}
	if _, err := os.Stat(cfg.CertPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat cert %q: %w", cfg.CertPath, err)
	}
	if _, err := os.Stat(cfg.KeyPath); err != nil {
		return nil, fmt.Errorf("tlsx: stat key %q: %w", cfg.KeyPath, err)
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertPath, cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("tlsx: load cert/key pair: %w", err)
	}
	out.Certificates = []tls.Certificate{cert}
	return out, nil
}

// Sentinel-ошибки для caller-ов, желающих маппить в конкретные diag-ы /
// HTTP-status-ы. `errors.Is(err, ErrServerCertEmpty)` устойчив, не
// зависит от текста сообщения.
var (
	ErrServerCertEmpty = errors.New("tlsx: cert path is empty")
	ErrServerKeyEmpty  = errors.New("tlsx: key path is empty")
	ErrServerCAEmpty   = errors.New("tlsx: ca path is empty")
)
