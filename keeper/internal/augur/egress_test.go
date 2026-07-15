package augur

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/souls-guild/soul-stack/shared/netguard"
)

// The SSRF-guard logic (IP classifier, guardedDialContext, redirect-downgrade,
// https-only) lives and is tested in shared/netguard. What's here is only the
// augur wrapper validateEndpoint and wiring the SSRF guard into the broker
// client.

func TestValidateEndpoint(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"https public host", "https://prom.example.com:9090", false},
		{"http denied", "http://prom.example.com:9090", true},
		{"http metadata denied", "http://169.254.169.254/latest", true},
		{"https literal metadata IP", "https://169.254.169.254/", true},
		{"https loopback literal", "https://127.0.0.1:9090", true},
		{"https rfc1918 literal", "https://10.1.2.3:9090", true},
		{"file scheme denied", "file:///etc/passwd", true},
		{"no host", "https://", true},
		{"newline smuggle", "https://\nhttp://evil", true},
		{"https public literal IP", "https://8.8.8.8:9090", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateEndpoint(c.url)
			if (err != nil) != c.wantErr {
				t.Errorf("validateEndpoint(%q) err=%v, wantErr=%v", c.url, err, c.wantErr)
			}
		})
	}
}

// blockResolver — a resolver that returns a fixed set of IPs for any host.
type blockResolver struct{ addrs []string }

func (r blockResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	out := make([]net.IPAddr, len(r.addrs))
	for i, a := range r.addrs {
		out[i] = net.IPAddr{IP: net.ParseIP(a)}
	}
	return out, nil
}

var _ netguard.Resolver = blockResolver{}

// TestNewEgressClient_GuardWiring — the broker client carries an SSRF guard
// at the dial phase (DialContext is set), redirect-downgrade protection, and
// the overall request timeout.
func TestNewEgressClient_GuardWiring(t *testing.T) {
	c := newEgressClient(blockResolver{addrs: []string{"8.8.8.8"}})

	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport не *http.Transport: %T", c.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("DialContext не выставлен — SSRF-guard отключён")
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect не выставлен — downgrade-защита отключена")
	}
	if c.Timeout != egressRequestTimeout {
		t.Errorf("client.Timeout = %v, want %v", c.Timeout, egressRequestTimeout)
	}

	// The guard actually works: a host resolving to metadata never reaches dial.
	bad := newEgressClient(blockResolver{addrs: []string{"169.254.169.254"}})
	badTr := bad.Transport.(*http.Transport)
	if _, err := badTr.DialContext(context.Background(), "tcp", "evil.example:443"); err == nil {
		t.Fatal("dial в metadata через резолв не заблокирован")
	}
}
