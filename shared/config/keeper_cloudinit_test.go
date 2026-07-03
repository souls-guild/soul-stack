package config

import (
	"testing"

	"github.com/goccy/go-yaml"
)

// Guard yaml-тегов KeeperCloudInit: event_stream_port (6-я стена ADR-063) обязан
// парситься из keeper.yml — опечатка в теге тихо оставила бы оба порта soul.yml
// равными bootstrap-порту.
func TestKeeperCloudInit_EventStreamPortTag(t *testing.T) {
	src := []byte(`bootstrap_endpoint: "lb:9442"
event_stream_port: 9443
tls_ca_ref: "vault:secret/keeper/ca"
soul_binary_url: "https://a.example/soul"
`)
	var c KeeperCloudInit
	if err := yaml.Unmarshal(src, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.EventStreamPort != 9443 {
		t.Errorf("EventStreamPort = %d, want 9443", c.EventStreamPort)
	}
	if c.BootstrapEndpoint != "lb:9442" {
		t.Errorf("BootstrapEndpoint = %q, want lb:9442", c.BootstrapEndpoint)
	}
}
