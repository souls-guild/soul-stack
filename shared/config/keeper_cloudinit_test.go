package config

import (
	"testing"

	"github.com/goccy/go-yaml"
)

// Guard for KeeperCloudInit yaml tags: event_stream_port (6th wall, ADR-063) must
// parse from keeper.yml — a tag typo would silently leave both soul.yml ports
// equal to the bootstrap port.
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
