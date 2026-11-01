package push

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TeleportIdentityError is the Keeper's own Teleport identity being unusable: its
// TLS certificate has expired, or the Teleport proxy refused it.
//
// It is a type for the reason [HostKeyError] is: a join wait has to tell it apart
// from "the node has not joined yet" without reading text. The proxy judges the
// identity before it looks at the node, so every dial through it is refused alike
// whatever host it names, and only reissuing the identity helps — waiting for the
// host turns the refusal into a join timeout (NIM-1171).
type TeleportIdentityError struct {
	// Path is the identity file.
	Path string
	// NotAfter is when the identity's TLS certificate expires.
	NotAfter time.Time
	// Err is the proxy's refusal. Nil when the expiry was caught before any dial.
	Err error
}

func (e *TeleportIdentityError) Error() string {
	expiry := e.NotAfter.UTC().Format(time.RFC3339)
	if e.Err == nil {
		return fmt.Sprintf("Keeper's own Teleport identity %q expired at %s; the proxy was not contacted, reissue the identity", e.Path, expiry)
	}
	return fmt.Sprintf("Teleport proxy refused Keeper's own identity %q (TLS certificate valid until %s): %v", e.Path, expiry, e.Err)
}

func (e *TeleportIdentityError) Unwrap() error { return e.Err }

// IsTeleportIdentityError reports whether err is, or wraps, a [TeleportIdentityError].
func IsTeleportIdentityError(err error) bool {
	var ie *TeleportIdentityError
	return errors.As(err, &ie)
}

// presentedCertificate is the client certificate tlsConfig hands the proxy, read in
// crypto/tls's order: GetClientCertificate wins over Certificates, and it is where
// the Teleport client puts the identity's certificate (configureTLS in api/client).
func presentedCertificate(tlsConfig *tls.Config) (*x509.Certificate, error) {
	var cert *tls.Certificate
	switch {
	case tlsConfig.GetClientCertificate != nil:
		c, err := tlsConfig.GetClientCertificate(&tls.CertificateRequestInfo{})
		if err != nil {
			return nil, err
		}
		cert = c
	case len(tlsConfig.Certificates) > 0:
		cert = &tlsConfig.Certificates[0]
	}
	if cert == nil || len(cert.Certificate) == 0 {
		return nil, errors.New("no TLS client certificate")
	}
	if cert.Leaf != nil {
		return cert.Leaf, nil
	}
	return x509.ParseCertificate(cert.Certificate[0])
}

// remoteAlertOp is the [net.OpError] Op crypto/tls files an alert received from
// the peer under.
const remoteAlertOp = "remote error"

// proxyRefusalAlerts are the alerts a TLS server sends about the certificate the
// client presented (RFC 8446 §6.2). Coming from the proxy, each is a verdict on the
// Keeper's identity and none depends on the node being dialed. crypto/tls keeps
// the named constants unexported.
var proxyRefusalAlerts = []tls.AlertError{
	42,  // bad_certificate
	43,  // unsupported_certificate
	44,  // certificate_revoked
	45,  // certificate_expired
	46,  // certificate_unknown
	48,  // unknown_ca
	49,  // access_denied
	116, // certificate_required
}

// accessDenied is the marker method of trace.AccessDeniedError, which the Teleport
// client makes of a PermissionDenied the proxy answers a stream's Recv with.
// Matched by method so the keeper module does not take on gravitational/trace for
// one predicate.
type accessDenied interface{ IsAccessDeniedError() bool }

// isProxyRefusal reports whether err is the Teleport proxy refusing the Keeper's
// own identity rather than failing to reach the node.
//
// ★ A TLS alert is matched by text because no type of it reaches here: gRPC
// reports a failed transport as a status built from the error's text
// (codes.Unavailable, "connection error: desc = …"). The text is rebuilt from the
// same stdlib values crypto/tls formats it with rather than written out by hand.
// A proxy error quoting such an alert from a hop of its own matches too, and is
// no more curable by waiting.
func isProxyRefusal(err error) bool {
	if err == nil {
		return false
	}
	var denied accessDenied
	if errors.As(err, &denied) && denied.IsAccessDeniedError() {
		return true
	}
	// A status passes the stream-open interceptor unconverted, so a denial there is
	// still a code and not a trace type.
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied:
		return true
	}
	msg := err.Error()
	return slices.ContainsFunc(proxyRefusalAlerts, func(a tls.AlertError) bool {
		return strings.Contains(msg, (&net.OpError{Op: remoteAlertOp, Err: a}).Error())
	})
}
