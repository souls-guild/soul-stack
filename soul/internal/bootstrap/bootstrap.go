// Package bootstrap — Soul-side онбординг по [ADR-012(b)].
//
// `soul init` (entrypoint в cmd/soul) последовательно:
//
//  1. Определяет SID (явный --sid или os.Hostname).
//  2. Генерирует RSA-key + PKCS#10 CSR (CN = SID).
//  3. Подключается к одному из Keeper Bootstrap endpoint-ов
//     (server-only TLS, `keeper.tls.ca` из soul.yml).
//  4. Вызывает unary Bootstrap RPC с (sid, plain-token, csr_pem).
//  5. Раскладывает (cert.pem, key.pem, ca.pem) в `paths.seed` через seed.Write.
//
// Приватный ключ никогда не покидает хост (ADR-012(b)); CSR несёт только
// public key, token хешится server-side.
//
// [ADR-012(b)]: docs/adr/0012-keeper-soul-grpc.md#adr-012-контракт-keepersoul-grpc-один-eventstream-с-oneof-keeper-side-рендер-forward-compat-only-add
package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/tlsx"
	"github.com/souls-guild/soul-stack/soul/internal/seed"
)

// Размер RSA-ключа Soul-стороны. 2048 — индустриальный минимум, совместим с
// большинством Vault PKI-ролями; смена в одну точку при ужесточении политики.
const rsaKeySize = 2048

// sidRe — каноническая форма SID (= FQDN), синхронизирована с
// `keeper/internal/soul.SIDPattern`. Дублируется здесь, чтобы Soul-side не
// тянул internal-пакет Keeper-а.
var sidRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,253}$`)

// ValidSID — поверхностная проверка SID на Soul-стороне до round-trip-а.
// Server-side всё равно валидирует через свой regex + PG CHECK.
func ValidSID(sid string) bool { return sidRe.MatchString(sid) }

// Config — вход bootstrap.Run.
type Config struct {
	// SID — явно заданный SID; пустой → os.Hostname приведённый к lower-case.
	SID string
	// Token — однократный bootstrap-токен (plain). Никогда не логировать.
	Token string
	// SeedDir — `paths.seed` из soul.yml. Не пустой.
	SeedDir string
	// KeeperCA — путь к CA-bundle Keeper-а (`keeper.tls.ca` из soul.yml).
	// Используется для проверки серверного сертификата при server-only TLS-handshake.
	KeeperCA string
	// Endpoints — упорядоченный (по priority) список Keeper Bootstrap-адресов
	// (`host:bootstrap_port`), извлечённый из `keeper.endpoints[]` через
	// SoulKeeperEndpoint.BootstrapAddr(). Пробуются по порядку до первого
	// success; failback к bootstrap неприменим (one-shot).
	Endpoints []string
	// HandshakeTimeout — окно на один RPC. Default 10s.
	HandshakeTimeout time.Duration
	// SoulVersion — версия soul-бинаря для аудита онбординга.
	// Пустая строка допустима.
	SoulVersion string
}

// Result — итог успешного онбординга.
type Result struct {
	SID      string
	Endpoint string
	KID      string
	NotAfter time.Time
	SeedDir  string
}

// Run выполняет полный bootstrap-цикл. Идемпотентность на стороне Keeper-а
// не гарантирована: bootstrap-токен сжигается при первом успешном RPC,
// повторный вызов вернёт PermissionDenied.
func Run(ctx context.Context, cfg Config) (*Result, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("bootstrap: token is empty")
	}
	if cfg.SeedDir == "" {
		return nil, errors.New("bootstrap: seed_dir is empty (set paths.seed in soul.yml)")
	}
	if cfg.KeeperCA == "" {
		return nil, errors.New("bootstrap: keeper.tls.ca is empty in soul.yml")
	}
	if len(cfg.Endpoints) == 0 {
		return nil, errors.New("bootstrap: keeper.endpoints is empty in soul.yml")
	}

	sid := cfg.SID
	if sid == "" {
		host, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("bootstrap: detect hostname: %w", err)
		}
		sid = strings.ToLower(strings.TrimSpace(host))
	}
	if !ValidSID(sid) {
		return nil, fmt.Errorf("bootstrap: invalid sid %q (must match %s)", sid, sidRe.String())
	}

	// Генерация key+CSR — до открытия сети. CSR несёт public key + CN=SID;
	// приватник остаётся в памяти, на диск попадает только после успешного
	// RPC (вместе с выданным cert-ом). Если bootstrap упадёт, мы ничего
	// на диске не наследим.
	key, csrPEM, err := generateKeyAndCSR(sid)
	if err != nil {
		return nil, err
	}

	timeout := cfg.HandshakeTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	tlsCfg, err := tlsx.LoadClientTLS(tlsx.ClientConfig{
		CAPath: cfg.KeeperCA,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap: load client TLS: %w", err)
	}

	var (
		reply       *keeperv1.BootstrapReply
		successAddr string
		dialErrs    []string
	)
	for _, addr := range cfg.Endpoints {
		// ServerName выставляется per-endpoint: gRPC target вида host:port
		// — для SNI / hostname-verify нужен только host.
		cfgForAddr := tlsCfg.Clone()
		if h, ok := hostFromAddr(addr); ok {
			cfgForAddr.ServerName = h
		}
		r, err := dialAndBootstrap(ctx, addr, cfgForAddr, sid, cfg.Token, csrPEM, cfg.SoulVersion, timeout)
		if err == nil {
			reply = r
			successAddr = addr
			break
		}
		dialErrs = append(dialErrs, fmt.Sprintf("%s: %v", addr, err))
	}
	if reply == nil {
		return nil, fmt.Errorf("bootstrap: all endpoints failed:\n  - %s",
			strings.Join(dialErrs, "\n  - "))
	}

	// caChainPem из BootstrapReply — это CA-цепочка PKI, по которой Soul
	// будет верифицировать серверный сертификат при последующем mTLS.
	// Сохраняем именно её, а не `keeper.tls.ca` (одна цепочка покрывает
	// весь cluster, обновляется через ротацию через тот же bootstrap).
	//
	// sigil_pubkey trust-anchor подписи допусков плагинов (ADR-026, S2b/R3) —
	// опционален. Приоритет set > single (ADR-026(h)): непустой
	// sigil_pubkey_pem_set (field 6, multi-anchor для безразрывной ротации)
	// авторитетнее одиночного sigil_pubkey_pem (field 5, legacy). Оба пустых =
	// Sigil на Keeper-е не настроен → SigilPubKeyPEM остаётся nil, файл не
	// пишется, verify плагинов выключен. Персистится в той же версии seed-а, что
	// cert/key/ca — переживает рестарт (pull-режим verify в S6 без bootstrap).
	material := &seed.Material{
		CertPEM: reply.GetCertificatePem(),
		KeyPEM:  encodeRSAPrivateKeyPEM(key),
		CAPEM:   reply.GetCaChainPem(),
	}
	// Пустой trust-anchor (Sigil выключен на Keeper) → nil, а не []byte{} —
	// консистентно с seed.Load (nil = «выключен»), чтобы S6-verify не различал
	// nil/пустой набор якорей.
	if anchors := sigilAnchorsPEM(reply); len(anchors) > 0 {
		material.SigilPubKeyPEM = anchors
	}
	if err := seed.Write(cfg.SeedDir, material); err != nil {
		return nil, err
	}

	res := &Result{
		SID:      sid,
		Endpoint: successAddr,
		KID:      reply.GetKid(),
		SeedDir:  cfg.SeedDir,
	}
	if reply.GetNotAfter() != nil {
		res.NotAfter = reply.GetNotAfter().AsTime()
	}
	return res, nil
}

// dialAndBootstrap — одна попытка: gRPC-dial + Bootstrap RPC + close.
func dialAndBootstrap(ctx context.Context, addr string, tlsCfg *tls.Config, sid, token string, csrPEM []byte, soulVersion string, timeout time.Duration) (*keeperv1.BootstrapReply, error) {
	creds := credentials.NewTLS(tlsCfg)
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	rpcCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := keeperv1.NewKeeperClient(conn)
	reply, err := client.Bootstrap(rpcCtx, &keeperv1.BootstrapRequest{
		Sid:            sid,
		BootstrapToken: token,
		CsrPem:         csrPEM,
		SoulVersion:    soulVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("Bootstrap RPC: %w", err)
	}
	if len(reply.GetCertificatePem()) == 0 || len(reply.GetCaChainPem()) == 0 {
		return nil, errors.New("Bootstrap reply incomplete (missing certificate_pem or ca_chain_pem)")
	}
	return reply, nil
}

// generateKeyAndCSR создаёт RSA-key и PKCS#10 CSR с CN=sid.
func generateKeyAndCSR(sid string) (*rsa.PrivateKey, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeySize)
	if err != nil {
		return nil, nil, fmt.Errorf("bootstrap: generate rsa key: %w", err)
	}
	tmpl := x509.CertificateRequest{
		Subject:  pkix.Name{CommonName: sid},
		DNSNames: []string{sid},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &tmpl, key)
	if err != nil {
		return nil, nil, fmt.Errorf("bootstrap: create CSR: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	return key, csrPEM, nil
}

// sigilAnchorsPEM извлекает trust-anchor-ы Sigil из BootstrapReply по приоритету
// set > single (ADR-026(h)): непустой sigil_pubkey_pem_set (field 6) —
// авторитет, одиночное sigil_pubkey_pem (field 5) при этом игнорируется. Набор
// собирается конкатенацией PEM-блоков (как их хранит seed-файл sigil_pubkey.pem
// и парсит seed.ParseSigilPubKeys). Оба источника пусты → nil (Sigil выключен).
//
// Каждый элемент set-а нормализуется переводом строки на конце, чтобы
// конкатенация была корректным multi-PEM (pem.Decode требует границ блоков).
func sigilAnchorsPEM(reply *keeperv1.BootstrapReply) []byte {
	if set := reply.GetSigilPubkeyPemSet(); len(set) > 0 {
		var buf []byte
		for _, p := range set {
			if p == "" {
				continue
			}
			buf = append(buf, p...)
			if p[len(p)-1] != '\n' {
				buf = append(buf, '\n')
			}
		}
		return buf
	}
	if single := reply.GetSigilPubkeyPem(); single != "" {
		return []byte(single)
	}
	return nil
}

// encodeRSAPrivateKeyPEM возвращает PKCS#1 PEM-форму ключа.
func encodeRSAPrivateKeyPEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// hostFromAddr вырезает host из `host:port` (IPv4 / FQDN). IPv6 без скобок
// и formless-host возвращают (s, false). Этого достаточно для ServerName.
func hostFromAddr(s string) (string, bool) {
	i := strings.LastIndex(s, ":")
	if i <= 0 {
		return "", false
	}
	host := s[:i]
	if strings.Contains(host, ":") {
		// IPv6 без скобок — TLS ServerName всё равно бесполезен.
		return "", false
	}
	return host, true
}
