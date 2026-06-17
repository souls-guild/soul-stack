package grpc

import (
	"context"
	"crypto/x509"
	"errors"

	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"

	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
)

// Sentinel-ошибки извлечения peer-cert. Маппятся в gRPC-status в
// interceptor-е [seedAuthStreamInterceptor].
var (
	// ErrPeerNotInContext — gRPC не положил peer в context-е. Происходит
	// только при некорректной интеграции (тесты, кастомные dialer-ы);
	// production-gRPC-stack всегда заполняет peer.
	ErrPeerNotInContext = errors.New("grpc: peer not in context")
	// ErrPeerNotTLS — peer установил соединение, но не через TLS. Не
	// должен случаться при mTLS-listener-е, страховка от misconfig.
	ErrPeerNotTLS = errors.New("grpc: peer is not TLS")
	// ErrPeerNoCert — TLS-handshake прошёл, но клиент не предъявил
	// сертификат. При `RequireAndVerifyClientCert` Go-runtime отвергает
	// такой handshake раньше, но проверка дёшевая.
	ErrPeerNoCert = errors.New("grpc: peer presented no client certificate")
)

// peerCert извлекает leaf-сертификат клиента из gRPC peer-context-а.
// Возвращает [ErrPeerNotInContext] / [ErrPeerNotTLS] / [ErrPeerNoCert] —
// caller маппит в нужный gRPC-status.
func peerCert(ctx context.Context) (*x509.Certificate, error) {
	p, ok := grpcpeer.FromContext(ctx)
	if !ok || p == nil {
		return nil, ErrPeerNotInContext
	}
	ti, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return nil, ErrPeerNotTLS
	}
	if len(ti.State.PeerCertificates) == 0 {
		return nil, ErrPeerNoCert
	}
	return ti.State.PeerCertificates[0], nil
}

// peerFingerprint — convenience: leaf-cert → SHA-256 fingerprint
// SubjectPublicKeyInfo. Совпадает с тем, что Keeper писал в `soul_seeds`
// при онбординге через [soulseed.FingerprintFromCert].
func peerFingerprint(ctx context.Context) (string, error) {
	cert, err := peerCert(ctx)
	if err != nil {
		return "", err
	}
	return soulseed.FingerprintFromCert(cert), nil
}
