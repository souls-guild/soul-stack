// TLS-коннект плагина community.redis. Концепция — Ansible-роль redis
// (redis.conf TLS-директивы + defaults redis_tls_*): оператор включает TLS на
// кластере, а плагин ОБЯЗАН коннектиться по TLS — иначе в режиме only-TLS
// (`port 0`, plain закрыт) он вообще не достучится до Redis.
//
// Модель безопасности (security-memory: insecure = ЯВНЫЙ opt-out, default
// secure): при tls=true плагин по умолчанию ПРОВЕРЯЕТ серверный сертификат
// (RootCAs из переданного PEM CA). Client-cert (mTLS) — опционален. Отключить
// проверку можно ТОЛЬКО явным tls_skip_verify=true (по умолчанию false).
//
// PEM приходит ЦЕЛИКОМ в params (scenario резолвит из Vault в render-фазе и
// передаёт значение), плагин свой Vault-доступ не тянет (capability —
// network_outbound). PEM-параметры (tls_ca/tls_cert/tls_key) помечены secret в
// manifest и маскируются выходным слоем по имени ключа (shared/audit маскирует
// tls_key/tls_cert/tls_ca) — в события/логи/ошибки не попадают.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"google.golang.org/protobuf/types/known/structpb"
)

// tlsParams — сырые TLS-параметры коннекта из params, отдельно от *tls.Config:
// держим PEM-строки до построения конфига, чтобы фабрика была чистой функцией и
// тестировалась без живого сокета (L0: buildTLSConfig над фейковыми PEM).
type tlsParams struct {
	enabled    bool
	caPEM      string // PEM CA для проверки серверного сертификата (RootCAs)
	certPEM    string // PEM client-cert для mTLS (опц.; вместе с keyPEM)
	keyPEM     string // PEM client-key для mTLS (опц.; вместе с certPEM)
	skipVerify bool   // ЯВНЫЙ opt-out проверки сертификата (default false)
}

// parseTLS вытаскивает TLS-параметры из params. Все поля опциональны: tls
// отсутствует/false → enabled=false, коннект plaintext (back-compat для
// инсталляций без TLS). PEM-строки держатся отдельно от всего, что попадает в
// события (как password — ИБ-инвариант ADR-010).
func parseTLS(f map[string]*structpb.Value) tlsParams {
	return tlsParams{
		enabled:    boolOrDefault(f["tls"], false),
		caPEM:      stringOrEmpty(f["tls_ca"]),
		certPEM:    stringOrEmpty(f["tls_cert"]),
		keyPEM:     stringOrEmpty(f["tls_key"]),
		skipVerify: boolOrDefault(f["tls_skip_verify"], false),
	}
}

// buildTLSConfig строит *tls.Config из tlsParams. Возвращает nil, nil когда TLS
// не включён (caller строит plaintext-коннект). Ошибка — только на битом PEM
// (CA не распарсился / client-cert невалиден): текст ошибки НЕ содержит PEM
// (x509/tls формируют ошибки без вложения исходного материала; на всякий случай
// caller их редактирует по keyPEM).
//
// Чистая функция (без I/O) → L0 проверяет результат напрямую: RootCAs загружен,
// skip_verify проброшен, client-cert добавлен при наличии — без живого Redis.
func buildTLSConfig(p tlsParams) (*tls.Config, error) {
	if !p.enabled {
		return nil, nil
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: p.skipVerify, //nolint:gosec // ЯВНЫЙ opt-out оператора (tls_skip_verify), default false — проверка включена
	}

	// RootCAs из переданного CA PEM (проверка серверного сертификата). Без
	// skip_verify и без CA проверка пойдёт по системному пулу доверия — для
	// частного PKI это обычно фейл, поэтому CA в TLS-режиме практически
	// обязателен; пустой CA при skip_verify=false оставляем системный пул
	// (легитимно для публично-доверенного сертификата).
	if p.caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(p.caPEM)) {
			return nil, fmt.Errorf("tls_ca: не удалось распарсить PEM CA-сертификата")
		}
		cfg.RootCAs = pool
	}

	// Client-cert (mTLS) — опционально, только если переданы ОБА (cert+key).
	// Один без другого — ошибка конфигурации оператора (понятный текст без PEM).
	switch {
	case p.certPEM != "" && p.keyPEM != "":
		pair, err := tls.X509KeyPair([]byte(p.certPEM), []byte(p.keyPEM))
		if err != nil {
			return nil, fmt.Errorf("tls_cert/tls_key: невалидная client-cert пара (mTLS)")
		}
		cfg.Certificates = []tls.Certificate{pair}
	case p.certPEM != "" || p.keyPEM != "":
		return nil, fmt.Errorf("tls_cert и tls_key задаются только ВМЕСТЕ (mTLS client-cert)")
	}

	return cfg, nil
}
