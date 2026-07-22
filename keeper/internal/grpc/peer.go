package grpc

import (
	"context"
	"crypto/x509"
	"errors"

	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"

	"github.com/souls-guild/soul-stack/keeper/internal/soulseed"
)

// Sentinel errors for peer-cert extraction. Mapped to gRPC status in the
// [seedAuthStreamInterceptor] interceptor.
var (
	// ErrPeerNotInContext — gRPC didn't put a peer in the context. Happens
	// only with incorrect integration (tests, custom dialers); the
	// production gRPC stack always populates the peer.
	ErrPeerNotInContext = errors.New("grpc: peer not in context")
	// ErrPeerNotTLS — the peer connected, but not over TLS. Shouldn't
	// happen with an mTLS listener; a safeguard against misconfiguration.
	ErrPeerNotTLS = errors.New("grpc: peer is not TLS")
	// ErrPeerNoCert — the TLS handshake succeeded, but the client didn't
	// present a certificate. With `RequireAndVerifyClientCert` the Go
	// runtime rejects such a handshake earlier, but the check is cheap.
	ErrPeerNoCert = errors.New("grpc: peer presented no client certificate")
)

// peerCert extracts the client's leaf certificate from the gRPC peer
// context. Returns [ErrPeerNotInContext] / [ErrPeerNotTLS] / [ErrPeerNoCert]
// — the caller maps it to the appropriate gRPC status.
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

// peerFingerprint — convenience: leaf-cert → SHA-256 fingerprint of
// SubjectPublicKeyInfo. Matches what Keeper wrote to `soul_seeds` during
// onboarding via [soulseed.FingerprintFromCert].
func peerFingerprint(ctx context.Context) (string, error) {
	cert, err := peerCert(ctx)
	if err != nil {
		return "", err
	}
	return soulseed.FingerprintFromCert(cert), nil
}
