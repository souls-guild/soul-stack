package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// genCertPEM возвращает (certPEM, keyPEM) самоподписанного ECDSA-сертификата —
// материал для L0 client-cert / CA проверок (валидный для x509.AppendCertsFromPEM
// и tls.X509KeyPair). Без сети/файлов.
func genCertPEM(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "redis-l0"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// --- buildTLSConfig (чистая функция) ---

func TestBuildTLSConfig_DisabledReturnsNil(t *testing.T) {
	cfg, err := buildTLSConfig(tlsParams{enabled: false, caPEM: "ignored"})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg != nil {
		t.Fatal("tls выключен → *tls.Config обязан быть nil (plaintext-коннект)")
	}
}

func TestBuildTLSConfig_CALoadedVerifyOn(t *testing.T) {
	caPEM, _ := genCertPEM(t)
	cfg, err := buildTLSConfig(tlsParams{enabled: true, caPEM: caPEM})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("tls включён → *tls.Config обязан быть не-nil")
	}
	if cfg.RootCAs == nil {
		t.Fatal("CA PEM передан → RootCAs обязан быть загружен")
	}
	if cfg.InsecureSkipVerify {
		t.Fatal("default — ПРОВЕРЯТЬ сертификат (skip_verify не задан → false)")
	}
}

func TestBuildTLSConfig_SkipVerifyPropagated(t *testing.T) {
	cfg, err := buildTLSConfig(tlsParams{enabled: true, skipVerify: true})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if !cfg.InsecureSkipVerify {
		t.Fatal("tls_skip_verify=true → InsecureSkipVerify обязан быть проброшен")
	}
}

func TestBuildTLSConfig_ClientCertWhenPresent(t *testing.T) {
	caPEM, _ := genCertPEM(t)
	certPEM, keyPEM := genCertPEM(t)
	cfg, err := buildTLSConfig(tlsParams{enabled: true, caPEM: caPEM, certPEM: certPEM, keyPEM: keyPEM})
	if err != nil {
		t.Fatalf("buildTLSConfig: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("client-cert (cert+key) → ровно 1 Certificate, got %d (mTLS)", len(cfg.Certificates))
	}
}

func TestBuildTLSConfig_BadCAErrors(t *testing.T) {
	_, err := buildTLSConfig(tlsParams{enabled: true, caPEM: "-----BEGIN CERTIFICATE-----\nnot-pem\n-----END CERTIFICATE-----"})
	if err == nil {
		t.Fatal("битый CA PEM → ошибка")
	}
}

func TestBuildTLSConfig_CertWithoutKeyErrors(t *testing.T) {
	certPEM, _ := genCertPEM(t)
	_, err := buildTLSConfig(tlsParams{enabled: true, certPEM: certPEM})
	if err == nil {
		t.Fatal("tls_cert без tls_key → ошибка (mTLS пара только вместе)")
	}
}

// --- parseTLS + проброс в connConfig через connect-инъекцию ---

// TestApply_TLSParamsReachConnect — все state-пути (command/config/replica/
// sentinel) читают TLS из params через parseConnConfig → tls попадает в
// connConfig, который видит connect-инъекция. cluster коннектится по nodes-map,
// проверен отдельно (TestApplyCluster_TLSReachesNodeConnect).
func TestApply_TLSParamsReachConnect(t *testing.T) {
	caPEM, _ := genCertPEM(t)
	certPEM, keyPEM := genCertPEM(t)

	var captured connConfig
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			captured = cfg
			return &fakeConn{}, nil
		},
	}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":            "127.0.0.1:7379",
			"password":        secretPass,
			"args":            []any{"PING"},
			"tls":             true,
			"tls_ca":          caPEM,
			"tls_cert":        certPEM,
			"tls_key":         keyPEM,
			"tls_skip_verify": false,
		}),
	}, &applyStream{})

	if !captured.tls.enabled {
		t.Fatal("tls=true не доехал до connConfig.tls.enabled")
	}
	if captured.tls.caPEM != caPEM {
		t.Error("tls_ca не доехал до connConfig.tls.caPEM")
	}
	if captured.tls.certPEM != certPEM || captured.tls.keyPEM != keyPEM {
		t.Error("client-cert (tls_cert/tls_key) не доехал до connConfig.tls")
	}
	if captured.tls.skipVerify {
		t.Error("tls_skip_verify=false должен пробросить false (default secure)")
	}

	// Из тех же params строим итоговый *tls.Config — он корректен (CA + client-cert).
	cfg, err := buildTLSConfig(captured.tls)
	if err != nil {
		t.Fatalf("buildTLSConfig из доехавших params: %v", err)
	}
	if cfg.RootCAs == nil || len(cfg.Certificates) != 1 || cfg.InsecureSkipVerify {
		t.Fatalf("tls.Config неполон: RootCAs=%v certs=%d skip=%v", cfg.RootCAs != nil, len(cfg.Certificates), cfg.InsecureSkipVerify)
	}
}

func TestApply_TLSDefaultOffPlaintext(t *testing.T) {
	var captured connConfig
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			captured = cfg
			return &fakeConn{}, nil
		},
	}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "command",
		Params: mustStruct(t, map[string]any{"addr": "127.0.0.1:6379", "args": []any{"PING"}}),
	}, &applyStream{})

	if captured.tls.enabled {
		t.Fatal("tls не задан → enabled должен быть false (plaintext, back-compat)")
	}
	cfg, err := buildTLSConfig(captured.tls)
	if err != nil || cfg != nil {
		t.Fatalf("tls выключен → buildTLSConfig nil,nil; got cfg=%v err=%v", cfg, err)
	}
}

// TestApplyCluster_TLSReachesNodeConnect — cluster-state читает tls из params и
// прокидывает в connConfig каждой ноды (только network_outbound, своего vault
// нет). Перехватываем connect, отдаём already-formed-кластер → проба ноды видит
// TLS-параметры.
func TestApplyCluster_TLSReachesNodeConnect(t *testing.T) {
	caPEM, _ := genCertPEM(t)
	var sawTLS bool
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			if cfg.tls.enabled && cfg.tls.caPEM == caPEM {
				sawTLS = true
			}
			// already-formed: CLUSTER INFO ok + наши ноды на месте → no-op,
			// но connect к первому мастеру уже произошёл (TLS проброс доказан).
			return &clusterConn{
				info:  "cluster_state:ok\ncluster_known_nodes:1\n",
				nodes: "id0 10.0.0.1:6379@16379 myself,master - 0 0 0 connected 0-16383\n",
			}, nil
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "cluster",
		Params: mustStruct(t, map[string]any{
			"action":             "create",
			"nodes":              map[string]any{"n0.example.com": map[string]any{"addr": "10.0.0.1:6379"}},
			"replicas_per_shard": 0,
			"password":           secretPass,
			"tls":                true,
			"tls_ca":             caPEM,
		}),
	}, stream)

	if !sawTLS {
		t.Fatal("cluster: TLS-параметры (tls/tls_ca) не доехали до connConfig ноды")
	}
}

// TestTLS_NoLeakOfKeyInEvents — PEM client-key НЕ утекает в события даже на
// ошибке коннекта, чей текст содержит ключ (worst-case: драйвер положил PEM в
// ошибку). redactError вырезает; событие Failed без PEM. Симметрия с
// TestApply_ConnectFailure_DoesNotLeakPassword, но для tls_key.
func TestTLS_NoLeakOfKeyInEvents(t *testing.T) {
	_, keyPEM := genCertPEM(t)
	m := &RedisModule{
		connect: func(_ context.Context, cfg connConfig) (redisConn, error) {
			// worst-case: ошибка несёт И пароль, И PEM-ключ.
			return nil, errStr("dial failed: AUTH " + cfg.password + " key=" + cfg.tls.keyPEM)
		},
	}
	stream := &applyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State: "command",
		Params: mustStruct(t, map[string]any{
			"addr":     "127.0.0.1:7379",
			"password": secretPass,
			"args":     []any{"PING"},
			"tls":      true,
			"tls_key":  keyPEM,
		}),
	}, stream)

	for _, e := range stream.sent {
		if strings.Contains(e.GetMessage(), secretPass) {
			t.Fatalf("пароль утёк в событие: %q", e.GetMessage())
		}
		// Достаточная улика PEM-ключа — заголовок блока; полный ключ многострочен.
		if strings.Contains(e.GetMessage(), "PRIVATE KEY") {
			t.Fatalf("PEM client-key утёк в событие: %q", e.GetMessage())
		}
	}
}

// errStr — error из строки (без fmt, чтобы не тащить лишний импорт здесь).
type errStr string

func (e errStr) Error() string { return string(e) }

// мостик к structpb в этом файле уже покрыт mustStruct из impl_test.go.
var _ = structpb.NewStruct
