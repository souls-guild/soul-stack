package pushorch

import (
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// `POST /v1/push/apply` and the `keeper.push.apply` MCP tool carry a `transport`
// field since NIM-880. It is refused at the boundary rather than accepted into a
// run that fails asynchronously: the caller gets a 422 it can correlate, instead
// of an apply_id it has to poll to learn its body was wrong.
//
// The decoder is the SAME one the rendered plan goes through, so a body this
// accepts cannot be refused later for its shape.
func TestValidateTransport(t *testing.T) {
	tests := []struct {
		name      string
		transport any
		wantErr   bool
	}{
		{"omitted - the registry decides", nil, false},
		{"scalar ssh", config.TransportSSH, false},
		{
			"ssh with params",
			map[string]any{config.TransportSSH: map[string]any{
				config.TransportParamSSHProvider: "vault-bastion",
				config.TransportParamUser:        "deploy",
				// JSON gives a float64 here, which is the shape this endpoint
				// actually receives.
				config.TransportParamPort: float64(2222),
			}},
			false,
		},
		{"agent - this endpoint IS the ssh transport", config.TransportAgent, true},
		{"unregistered name", "teleport", true},
		{"two keys - no defined order of preference", map[string]any{"ssh": nil, "agent": nil}, true},
		{"unknown param", map[string]any{config.TransportSSH: map[string]any{"host": "10.0.0.1"}}, true},
		{"port that is not an integer", map[string]any{config.TransportSSH: map[string]any{config.TransportParamPort: "2222"}}, true},
		{"not a transport shape at all", []any{"ssh"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTransport(tt.transport)
			if tt.wantErr && err == nil {
				t.Fatalf("validateTransport(%v) = nil, want a refusal", tt.transport)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateTransport(%v) = %v, want nil", tt.transport, err)
			}
			// The sentinel is what the HTTP handler and the MCP tool map to 422;
			// a bare error would come back as a 500 and read as the Keeper's fault.
			if tt.wantErr && !errors.Is(err, ErrInvalidTransport) {
				t.Errorf("err = %v, want it to wrap %v", err, ErrInvalidTransport)
			}
		})
	}
}

// There must be no address parameter — a task that could name one would be a
// task that bootstraps a bare VM, which ADR-0088 explicitly is not. Asserted
// here rather than only in the DSL tests because this is the OTHER door into
// the same decoder, and an address added for the API alone would open it.
func TestValidateTransport_NoAddressParameter(t *testing.T) {
	for _, key := range []string{"host", "address", "addr", "ip"} {
		if err := validateTransport(map[string]any{
			config.TransportSSH: map[string]any{key: "10.0.0.1"},
		}); err == nil {
			t.Errorf("transport.ssh.%s was accepted - the key must carry no address", key)
		}
	}
}
