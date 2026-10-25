package middleware

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func trusted(prefixes ...string) TrustedProxies {
	out := make(TrustedProxies, len(prefixes))
	for i, p := range prefixes {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

func TestClientAddr(t *testing.T) {
	ingress := trusted("10.0.0.0/24", "fd00::/64")
	for _, c := range []struct {
		name    string
		proxies TrustedProxies
		peer    string
		xff     []string
		want    string
	}{
		{"no trusted proxies: the header is ignored", nil, "10.0.0.5:4431", []string{"203.0.113.9"}, "10.0.0.5"},
		{"untrusted peer: the header is ignored", ingress, "198.51.100.7:4431", []string{"203.0.113.9"}, "198.51.100.7"},
		{"trusted peer: the hop it appended", ingress, "10.0.0.5:4431", []string{"203.0.113.9"}, "203.0.113.9"},
		{"a spoofed hop left of the real one is not read", ingress, "10.0.0.5:4431", []string{"1.2.3.4, 203.0.113.9"}, "203.0.113.9"},
		{"trusted hops are skipped", ingress, "10.0.0.5:4431", []string{"203.0.113.9, 10.0.0.7"}, "203.0.113.9"},
		{"several header lines read as one list", ingress, "10.0.0.5:4431", []string{"1.2.3.4", "203.0.113.9, 10.0.0.7"}, "203.0.113.9"},
		{"every hop trusted: the leftmost", ingress, "10.0.0.5:4431", []string{"10.0.0.9, 10.0.0.7"}, "10.0.0.9"},
		{"no header from a trusted peer: the peer", ingress, "10.0.0.5:4431", nil, "10.0.0.5"},
		{"garbage stops at the last vouched address", ingress, "10.0.0.5:4431", []string{"203.0.113.9, unknown, 10.0.0.7"}, "10.0.0.7"},
		{"an empty hop is garbage", ingress, "10.0.0.5:4431", []string{"203.0.113.9,,"}, "10.0.0.5"},
		{"a hop with a port", ingress, "10.0.0.5:4431", []string{"203.0.113.9:5555"}, "203.0.113.9"},
		{"a bracketed IPv6 hop", ingress, "[fd00::1]:4431", []string{"[2001:db8::7]"}, "2001:db8::7"},
		{"a bracketed IPv6 hop with a port", ingress, "[fd00::1]:4431", []string{"[2001:db8::7]:443"}, "2001:db8::7"},
		{"an IPv4-mapped peer is the IPv4 proxy", ingress, "[::ffff:10.0.0.5]:4431", []string{"203.0.113.9"}, "203.0.113.9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = c.peer
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := c.proxies.ClientAddr(r); got != c.want {
				t.Fatalf("ClientAddr = %q, want %q", got, c.want)
			}
		})
	}
}

// PeerIP and the client meta a minted token stores read the address the
// middleware resolved; without the middleware they fall back to the peer and
// never read the header themselves.
func TestResolveClientAddr_FeedsPeerIPAndClientMeta(t *testing.T) {
	var gotPeer, gotMeta string
	h := ResolveClientAddr(trusted("10.0.0.0/24"))(WithClientMeta(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPeer = PeerIP(r)
		gotMeta = ClientMetaFromContext(r.Context()).IP
	})))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.5:4431"
	r.Header.Set("X-Forwarded-For", "203.0.113.9")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if gotPeer != "203.0.113.9" || gotMeta != "203.0.113.9" {
		t.Fatalf("PeerIP=%q meta=%q, want the forwarded client", gotPeer, gotMeta)
	}

	bare := httptest.NewRequest(http.MethodGet, "/", nil)
	bare.RemoteAddr = "10.0.0.5:4431"
	bare.Header.Set("X-Forwarded-For", "203.0.113.9")
	if got := PeerIP(bare); got != "10.0.0.5" {
		t.Fatalf("PeerIP without the middleware = %q, want the peer", got)
	}
}
