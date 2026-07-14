package config

import (
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

// keeperBaseWithPush — a minimally-valid keeper.yml with an arbitrary `push:` block body
// for validatePush tests.
func keeperBaseWithPush(pushBlock string) []byte {
	return []byte(`kid: keeper-eu-west-01
listen:
  grpc:
    bootstrap:    { addr: "0.0.0.0:9442", tls: { cert: /c, key: /k } }
    event_stream: { addr: "0.0.0.0:8443", tls: { cert: /c, key: /k, ca: /a } }
  openapi: { addr: "0.0.0.0:8080" }
  mcp:     { addr: "0.0.0.0:8081" }
  metrics: { addr: "0.0.0.0:9090" }
postgres:
  dsn_ref: vault:secret/keeper/postgres
  pool: { min: 1, max: 5 }
redis:
  addr: "r:6379"
  password_ref: vault:secret/keeper/redis
vault:
  addr: "https://v:8200"
  auth: { method: token }
  pki_mount: pki/x
auth:
  jwt:
    signing_key_ref: vault:secret/keeper/jwt-signing-key
` + pushBlock)
}

func TestPush_Valid(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_ref: vault:secret/keeper/ssh-host-ca
  targets:
    - sid: soul-a.example.com
      ssh_port: 2222
      ssh_user: deploy
      soul_path: /opt/soul/bin/soul
    - sid: soul-b.example.com
  providers:
    - name: vault-bastion
      params:
        vault_addr: https://vault.internal:8200
        role: ssh-bastion-role
`)
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for valid push block")
	}
	if cfg.Push == nil {
		t.Fatal("cfg.Push is nil after parsing")
	}
	if cfg.Push.HostCARef != "vault:secret/keeper/ssh-host-ca" {
		t.Errorf("HostCARef = %q", cfg.Push.HostCARef)
	}
	if len(cfg.Push.Targets) != 2 {
		t.Fatalf("Targets len = %d, want 2", len(cfg.Push.Targets))
	}
	if cfg.Push.Targets[0].SSHPort != 2222 {
		t.Errorf("Targets[0].SSHPort = %d", cfg.Push.Targets[0].SSHPort)
	}
	if len(cfg.Push.Providers) != 1 || cfg.Push.Providers[0].Name != "vault-bastion" {
		t.Errorf("Providers = %+v", cfg.Push.Providers)
	}
	if cfg.Push.Providers[0].Params["role"] != "ssh-bastion-role" {
		t.Errorf("Params.role = %v", cfg.Push.Providers[0].Params["role"])
	}
}

func TestPush_HostCARefPlaintextRejected(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_ref: "ssh-ed25519 AAAA... inline-pem"
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "vault_ref_invalid", "$.push.host_ca_ref") {
		dump(t, diags)
		t.Fatalf("expected vault_ref_invalid for plaintext host_ca_ref")
	}
}

func TestPush_EmptyBlockOK(t *testing.T) {
	// push: empty object — allowed, the push orchestrator simply won't start.
	src := keeperBaseWithPush(`push: {}
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for empty push block")
	}
}

func TestPush_TargetMissingSID(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_ref: vault:secret/keeper/ssh-host-ca
  targets:
    - ssh_port: 22
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.push.targets[0].sid") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for targets[0].sid")
	}
}

func TestPush_DuplicateTargetSID(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_ref: vault:secret/keeper/ssh-host-ca
  targets:
    - { sid: dup.example.com }
    - { sid: dup.example.com }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "duplicate_push_target_sid") {
		dump(t, diags)
		t.Fatalf("expected duplicate_push_target_sid")
	}
}

func TestPush_ProviderMissingName(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_ref: vault:secret/keeper/ssh-host-ca
  providers:
    - params: { foo: bar }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.push.providers[0].name") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for providers[0].name")
	}
}

func TestPush_DuplicateProviderName(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_ref: vault:secret/keeper/ssh-host-ca
  providers:
    - { name: vault-bastion, params: { a: 1 } }
    - { name: vault-bastion, params: { b: 2 } }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "duplicate_push_provider_name") {
		dump(t, diags)
		t.Fatalf("expected duplicate_push_provider_name")
	}
}

func TestPush_BadSSHPort(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_ref: vault:secret/keeper/ssh-host-ca
  targets:
    - sid: bad.example.com
      ssh_port: 70000
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "port_out_of_range") {
		dump(t, diags)
		t.Fatalf("expected port_out_of_range for ssh_port=70000")
	}
}

// TestPush_HostCARefs_Valid — S7-3 multi-CA: a non-empty host_ca_refs[] without the
// singular host_ca_ref passes validation.
func TestPush_HostCARefs_Valid(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_refs:
    - { ref: vault:secret/keeper/ssh-host-ca-prod, name: prod }
    - { ref: vault:secret/keeper/ssh-host-ca-stage, name: stage }
`)
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for valid host_ca_refs[]")
	}
	if cfg.Push == nil || len(cfg.Push.HostCARefs) != 2 {
		t.Fatalf("HostCARefs len = %d, want 2", len(cfg.Push.HostCARefs))
	}
	if cfg.Push.HostCARefs[0].Name != "prod" || cfg.Push.HostCARefs[1].Name != "stage" {
		t.Errorf("HostCARefs names = %q,%q", cfg.Push.HostCARefs[0].Name, cfg.Push.HostCARefs[1].Name)
	}
}

// TestPush_HostCARefs_MutuallyExclusiveWithSingular — S7-3: the simultaneous presence of
// singular `host_ca_ref` and `host_ca_refs[]` is rejected. The daemon auto-adapts the
// singular into a singleton only when `host_ca_refs[]` is empty.
func TestPush_HostCARefs_MutuallyExclusiveWithSingular(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_ref: vault:secret/keeper/ssh-host-ca
  host_ca_refs:
    - { ref: vault:secret/keeper/ssh-host-ca-prod, name: prod }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "mutually_exclusive_keys") {
		dump(t, diags)
		t.Fatalf("expected mutually_exclusive_keys for both singular and plural host_ca_*")
	}
}

// TestPush_HostCARefs_PlaintextRejected — S7-3: every element in `host_ca_refs[]` must be
// a vault ref; a plaintext PEM is rejected (symmetric with the singular).
func TestPush_HostCARefs_PlaintextRejected(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_refs:
    - { ref: "ssh-ed25519 AAAA... inline-pem", name: prod }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "vault_ref_invalid", "$.push.host_ca_refs[0].ref") {
		dump(t, diags)
		t.Fatalf("expected vault_ref_invalid for plaintext host_ca_refs[0].ref")
	}
}

// TestPush_HostCARefs_DuplicateName — S7-3: names in the set must be unique (lookup by
// name in logs/metrics without ambiguity).
func TestPush_HostCARefs_DuplicateName(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_refs:
    - { ref: vault:secret/keeper/ssh-host-ca-1, name: prod }
    - { ref: vault:secret/keeper/ssh-host-ca-2, name: prod }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "duplicate_push_host_ca_name") {
		dump(t, diags)
		t.Fatalf("expected duplicate_push_host_ca_name")
	}
}

// TestPush_HostCARefs_MissingName — S7-3: name is a required field (used as the label
// value in keeper_push_host_ca_used_total{ca_name=...}).
func TestPush_HostCARefs_MissingName(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_refs:
    - { ref: vault:secret/keeper/ssh-host-ca }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.push.host_ca_refs[0].name") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for host_ca_refs[0].name")
	}
}

// TestPush_HostCARefs_NameInvalidFormat — S7-3: a name in snake_case or with uppercase
// letters fails kebab-case validation.
func TestPush_HostCARefs_NameInvalidFormat(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_refs:
    - { ref: vault:secret/keeper/ssh-host-ca, name: Bad_Name }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "name_invalid_format", "$.push.host_ca_refs[0].name") {
		dump(t, diags)
		t.Fatalf("expected name_invalid_format for non-kebab host_ca_refs[0].name")
	}
}

// TestPush_HostCARefs_MissingRef — S7-3: ref is required, like the singular.
func TestPush_HostCARefs_MissingRef(t *testing.T) {
	src := keeperBaseWithPush(`push:
  host_ca_refs:
    - { name: prod }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "missing_required_field", "$.push.host_ca_refs[0].ref") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for host_ca_refs[0].ref")
	}
}

// TestPush_Transport — table-driven guard for validatePushTransport (ADR-063 amendment
// "Teleport by-name transport"). Covers enum validation of `transport` and the
// requirement that the `teleport` block's fields be non-empty when `transport: teleport`.
// Each case states only the acceptance criterion (no errors / a specific diag by
// code+path); the rest of keeper.yml is a minimally-valid base.
func TestPush_Transport(t *testing.T) {
	const validTeleport = `
  teleport:
    proxy_addr: proxy.example.com:443
    identity_file: /etc/soul/teleport-identity
    cluster: prod`

	cases := []struct {
		name      string
		pushBlock string
		// wantCode/wantPath — the expected diag; empty wantCode → no errors expected.
		wantCode string
		wantPath string
	}{
		{
			name:      "empty transport defaults to direct (ok)",
			pushBlock: "push: {}\n",
		},
		{
			name: "transport direct (ok)",
			pushBlock: `push:
  transport: direct
`,
		},
		{
			name: "transport teleport with all three creds (ok)",
			pushBlock: `push:
  transport: teleport` + validTeleport + "\n",
		},
		{
			name: "invalid transport enum",
			pushBlock: `push:
  transport: bastion
`,
			wantCode: "invalid_enum_value",
			wantPath: "$.push.transport",
		},
		{
			name: "teleport without teleport block",
			pushBlock: `push:
  transport: teleport
`,
			wantCode: "missing_required_field",
			wantPath: "$.push.teleport",
		},
		{
			name: "teleport with empty proxy_addr",
			pushBlock: `push:
  transport: teleport
  teleport:
    identity_file: /etc/soul/teleport-identity
    cluster: prod
`,
			wantCode: "missing_required_field",
			wantPath: "$.push.teleport.proxy_addr",
		},
		{
			name: "teleport with empty identity_file",
			pushBlock: `push:
  transport: teleport
  teleport:
    proxy_addr: proxy.example.com:443
    cluster: prod
`,
			wantCode: "missing_required_field",
			wantPath: "$.push.teleport.identity_file",
		},
		{
			name: "teleport with empty cluster",
			pushBlock: `push:
  transport: teleport
  teleport:
    proxy_addr: proxy.example.com:443
    identity_file: /etc/soul/teleport-identity
`,
			wantCode: "missing_required_field",
			wantPath: "$.push.teleport.cluster",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := keeperBaseWithPush(tc.pushBlock)
			cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
			if err != nil {
				t.Fatalf("io error: %v", err)
			}
			if tc.wantCode == "" {
				if diag.HasErrors(diags) {
					dump(t, diags)
					t.Fatalf("expected 0 errors")
				}
				if cfg.Push == nil {
					t.Fatal("cfg.Push is nil after parsing valid push block")
				}
				return
			}
			if !hasCodeAt(diags, tc.wantCode, tc.wantPath) {
				dump(t, diags)
				t.Fatalf("expected %s at %s", tc.wantCode, tc.wantPath)
			}
		})
	}
}
