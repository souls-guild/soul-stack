// TLS-коннект плагина community.mongo. Модель безопасности (default secure):
// при tls=true плагин по умолчанию ПРОВЕРЯЕТ серверный сертификат (RootCAs из
// переданного PEM CA). Client-cert (mTLS) — опционален. Отключить проверку можно
// ТОЛЬКО явным tls_skip_verify=true (по умолчанию false).
//
// PEM приходит ЦЕЛИКОМ в params (scenario резолвит из Vault в render-фазе),
// плагин свой Vault-доступ не тянет (capability — network_outbound). PEM-поля
// (tls_ca/tls_cert/tls_key) помечены secret в manifest и маскируются выходным
// слоем по имени ключа — в события/логи/ошибки не попадают.
//
// ★ PILOT: MongoDB-сервис поднимается в plain-режиме (net.tls.mode disabled).
// Параметры коннекта тут объявлены для симметрии с community.redis и forward-
// compat (mongo TLS на порту 27017 через net.tls.mode — отдельный слайс); в
// pilot-сценарии они не задаются (tls=false → plaintext).
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
// отсутствует/false → enabled=false, коннект plaintext. PEM-строки держатся
// отдельно от всего, что попадает в события (как password — ИБ-инвариант ADR-010).
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
// не включён (caller строит plaintext-коннект). Ошибка — только на битом PEM.
// Чистая функция (без I/O) → L0 проверяет результат напрямую.
func buildTLSConfig(p tlsParams) (*tls.Config, error) {
	if !p.enabled {
		return nil, nil
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: p.skipVerify, //nolint:gosec // ЯВНЫЙ opt-out оператора (tls_skip_verify), default false — проверка включена
	}

	if p.caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(p.caPEM)) {
			return nil, fmt.Errorf("tls_ca: не удалось распарсить PEM CA-сертификата")
		}
		cfg.RootCAs = pool
	}

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
