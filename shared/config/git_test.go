package config

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/audit"
	"github.com/souls-guild/soul-stack/shared/diag"
)

// keeperBaseWithGit — a minimally-valid keeper.yml with an arbitrary `git:` block
// body for validateGit tests.
func keeperBaseWithGit(gitBlock string) []byte {
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
` + gitBlock)
}

func TestGit_Valid(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      key_ref: vault:secret/keeper/git#ssh_key
      key_file: /etc/keeper/git/id_ed25519
      known_hosts_ref: vault:secret/keeper/git#known_hosts
      token_ref: vault:secret/keeper/git#token
    - host: code.internal
      token_file: /etc/keeper/git/token
`)
	cfg, _, diags, err := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if err != nil {
		t.Fatalf("io error: %v", err)
	}
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for a valid git block")
	}
	if cfg.Git == nil {
		t.Fatal("cfg.Git is nil after parsing")
	}
	if len(cfg.Git.Credentials) != 2 {
		t.Fatalf("Credentials len = %d, want 2", len(cfg.Git.Credentials))
	}
	// Both key sources at once are legal: the priority is resolved at load
	// time, not by forbidding one of them in the schema.
	if cfg.Git.Credentials[0].KeyRef == "" || cfg.Git.Credentials[0].KeyFile == "" {
		t.Errorf("Credentials[0] lost a key source: %+v", cfg.Git.Credentials[0])
	}
	// An https-only entry is a complete entry: the ssh half is what needs
	// known_hosts, and it is absent here.
	if cfg.Git.Credentials[1].TokenFile != "/etc/keeper/git/token" {
		t.Errorf("Credentials[1].TokenFile = %q", cfg.Git.Credentials[1].TokenFile)
	}
}

// TestGit_AbsentBlockOK — the feature is additive: a keeper.yml that never
// mentions `git:` must stay valid, and cfg.Git nil is what tells every consumer
// to keep the pre-NIM-898 resolution.
func TestGit_AbsentBlockOK(t *testing.T) {
	cfg, _, diags, _ := LoadKeeperFromBytes("keeper.yml", keeperBaseWithGit(""), ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors without a git block")
	}
	if cfg.Git != nil {
		t.Errorf("cfg.Git = %+v, want nil", cfg.Git)
	}
}

// TestGit_SSHKeyWithoutKnownHostsRejected — the acceptance criterion and the
// symmetry with push's ban on InsecureIgnoreHostKey: an ssh credential that
// pins no host keys must not load. Without this, go-git would fall back to
// searching the keeper's own ~/.ssh/known_hosts — trust nobody wrote down.
func TestGit_SSHKeyWithoutKnownHostsRejected(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      key_ref: vault:secret/keeper/git#ssh_key
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "git_known_hosts_required") {
		dump(t, diags)
		t.Fatalf("expected git_known_hosts_required for an ssh key with no known_hosts source")
	}
}

// TestGit_TokenOnlyNeedsNoKnownHosts marks the narrowness of the rule above:
// host verification over https is TLS's job, so an https-only entry must not be
// dragged into needing known_hosts.
func TestGit_TokenOnlyNeedsNoKnownHosts(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      token_ref: vault:secret/keeper/git#token
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for a token-only entry")
	}
}

// TestGit_KnownHostsFileSatisfiesTheRequirement — either source counts; the
// requirement is on the FACT of host pinning, not on where it comes from.
func TestGit_KnownHostsFileSatisfiesTheRequirement(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      key_file: /etc/keeper/git/id_ed25519
      known_hosts_file: /etc/keeper/git/known_hosts
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if diag.HasErrors(diags) {
		dump(t, diags)
		t.Fatalf("expected 0 errors for a file-sourced ssh entry")
	}
}

// TestGit_PlaintextKeyRejected — the `*_ref` fields name a Vault location;
// inline key material in keeper.yml is a security-policy violation, symmetric
// with push.host_ca_refs[].ref and sigil.signing_key_ref.
func TestGit_PlaintextKeyRejected(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      key_ref: "-----BEGIN OPENSSH PRIVATE KEY-----"
      known_hosts_ref: vault:secret/keeper/git#known_hosts
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "vault_ref_invalid", "$.git.credentials[0].key_ref") {
		dump(t, diags)
		t.Fatalf("expected vault_ref_invalid for an inline key_ref")
	}
}

// TestGit_RejectedRefValueNeverReachesTheMessage — a keeper diagnostic travels
// into the append-only `audit_log` (NIM-505), so the value that failed the
// vault-ref check must not be quoted back. The fixture is the mistake that
// makes this matter: a token pasted where its vault-ref belongs.
func TestGit_RejectedRefValueNeverReachesTheMessage(t *testing.T) {
	const secret = "s3cr3t-token-pasted-in-the-wrong-field"
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      token_ref: "` + secret + `"
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	d := codeAt(diags, "vault_ref_invalid", "$.git.credentials[0].token_ref")
	if d == nil {
		dump(t, diags)
		t.Fatalf("expected vault_ref_invalid for a plaintext token_ref")
	}
	if strings.Contains(d.Message, secret) || strings.Contains(d.Hint, secret) {
		t.Fatalf("the rejected value leaked into the diagnostic: %q / %q", d.Message, d.Hint)
	}
	if !strings.Contains(d.Message, audit.MaskedValue) {
		t.Errorf("message does not carry the mask token: %q", d.Message)
	}
}

func TestGit_PlaintextKnownHostsRefRejected(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      key_ref: vault:secret/keeper/git#ssh_key
      known_hosts_ref: "gitlab.example.com ssh-ed25519 AAAA..."
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "vault_ref_invalid", "$.git.credentials[0].known_hosts_ref") {
		dump(t, diags)
		t.Fatalf("expected vault_ref_invalid for an inline known_hosts_ref")
	}
}

func TestGit_RelativeFilePathRejected(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      key_file: etc/keeper/git/id_ed25519
      known_hosts_file: /etc/keeper/git/known_hosts
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCodeAt(diags, "path_not_absolute", "$.git.credentials[0].key_file") {
		dump(t, diags)
		t.Fatalf("expected path_not_absolute for a relative key_file")
	}
}

func TestGit_MissingHost(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - token_ref: vault:secret/keeper/git#token
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	// Anchored on the entry, not on `.host`: the key is absent, so a path
	// naming it would resolve to nothing and ship line 0 — see
	// TestGit_DiagnosticsCarryALocation.
	if !hasCodeAt(diags, "missing_required_field", "$.git.credentials[0]") {
		dump(t, diags)
		t.Fatalf("expected missing_required_field for credentials[0].host")
	}
}

// TestGit_HostMustBeBare — the lookup compares against url.Hostname(), which
// carries no scheme, no userinfo, no port and no path. An entry spelled with
// any of them would match nothing and look configured, so it is refused here.
func TestGit_HostMustBeBare(t *testing.T) {
	for _, host := range []string{
		"https://gitlab.example.com",
		"git@gitlab.example.com",
		"gitlab.example.com:2222",
		"gitlab.example.com/org/repo.git",
		"gitlab example.com",
	} {
		t.Run(host, func(t *testing.T) {
			src := keeperBaseWithGit(`git:
  credentials:
    - host: "` + host + `"
      token_ref: vault:secret/keeper/git#token
`)
			_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
			if !hasCodeAt(diags, "git_credential_host_invalid", "$.git.credentials[0].host") {
				dump(t, diags)
				t.Fatalf("expected git_credential_host_invalid for host %q", host)
			}
		})
	}
}

// TestGit_HostFormsAccepted — the rule rejects values that are not a bare
// host, and nothing else. Case is accepted because the lookup lowercases both
// sides; underscores because internal zones serve them and `url.Hostname()`
// returns them unchanged; IP literals because a remote may be addressed by one
// and there would otherwise be no way to give it a credential at all.
func TestGit_HostFormsAccepted(t *testing.T) {
	for _, host := range []string{
		"gitlab.example.com",
		"GitLab.Example.com",
		"git_internal.corp",
		"localhost",
		"10.1.2.3",
		"fd00::1",
		"[fd00::1]",
	} {
		t.Run(host, func(t *testing.T) {
			src := keeperBaseWithGit(`git:
  credentials:
    - host: "` + host + `"
      token_ref: vault:secret/keeper/git#token
`)
			_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
			if diag.HasErrors(diags) {
				dump(t, diags)
				t.Fatalf("host %q was rejected", host)
			}
		})
	}
}

func TestGit_DuplicateHost(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - { host: gitlab.example.com, token_ref: "vault:secret/keeper/a#token" }
    - { host: gitlab.example.com, token_ref: "vault:secret/keeper/b#token" }
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "duplicate_git_credential_host") {
		dump(t, diags)
		t.Fatalf("expected duplicate_git_credential_host")
	}
}

// TestGit_DuplicateHostNormalizes — the duplicate check has to agree with the
// lookup key, which lowercases DNS names and normalizes an IP literal through
// net.ParseIP. Two entries that are one host under that normalization are a
// duplicate here, or they become one entry with a silent winner at runtime.
func TestGit_DuplicateHostNormalizes(t *testing.T) {
	cases := map[string][2]string{
		"case":          {"gitlab.example.com", "GitLab.Example.COM"},
		"ipv6 spelling": {"fd00::1", "fd00:0:0:0:0:0:0:1"},
		"ipv6 brackets": {"[fd00::1]", "fd00::1"},
	}
	for name, pair := range cases {
		t.Run(name, func(t *testing.T) {
			src := keeperBaseWithGit(`git:
  credentials:
    - { host: "` + pair[0] + `", token_ref: "vault:secret/keeper/a#token" }
    - { host: "` + pair[1] + `", token_ref: "vault:secret/keeper/b#token" }
`)
			_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
			if !hasCode(diags, "duplicate_git_credential_host") {
				dump(t, diags)
				t.Fatalf("expected duplicate_git_credential_host for %q and %q", pair[0], pair[1])
			}
		})
	}
}

// TestGit_DiagnosticsCarryALocation — a keeper diagnostic travels to the
// operator and into the audit trail; one anchored on a key the document does
// not contain resolves to nothing and ships with line 0, pointing at the top of
// the file instead of the entry that is wrong.
func TestGit_DiagnosticsCarryALocation(t *testing.T) {
	cases := map[string]string{
		"missing_required_field": `git:
  credentials:
    - token_ref: vault:secret/keeper/git#token
`,
		"git_credential_empty": `git:
  credentials:
    - host: gitlab.example.com
`,
		"git_known_hosts_required": `git:
  credentials:
    - host: gitlab.example.com
      key_file: /etc/keeper/git/id_ed25519
`,
		"git_known_hosts_without_key": `git:
  credentials:
    - host: gitlab.example.com
      known_hosts_file: /etc/keeper/git/known_hosts
      token_file: /etc/keeper/git/token
`,
	}
	for code, block := range cases {
		t.Run(code, func(t *testing.T) {
			_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", keeperBaseWithGit(block), ValidateOptions{})
			d := findDiagCode(diags, code)
			if d == nil {
				dump(t, diags)
				t.Fatalf("expected %s", code)
			}
			if d.Line == 0 {
				t.Errorf("%s has no location: %+v", code, *d)
			}
		})
	}
}

// TestGit_KnownHostsWithoutKeyRejected — known_hosts is read only alongside a
// key. On its own it is never opened, so a key_* line lost from a template
// would otherwise produce no diagnostic and no error, just every ssh clone on
// that host quietly using the agent.
func TestGit_KnownHostsWithoutKeyRejected(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      known_hosts_file: /etc/keeper/git/known_hosts
      token_file: /etc/keeper/git/token
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "git_known_hosts_without_key") {
		dump(t, diags)
		t.Fatalf("expected git_known_hosts_without_key")
	}
}

// TestGit_EmptyEntryRejected — an entry naming a host and configuring no
// credential reads as "this host is handled" and is not: the lookup would find
// nothing for either scheme and fall through to the agent.
func TestGit_EmptyEntryRejected(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "git_credential_empty") {
		dump(t, diags)
		t.Fatalf("expected git_credential_empty for an entry with no credential")
	}
}

func TestGit_UnknownKeyRejected(t *testing.T) {
	src := keeperBaseWithGit(`git:
  credentials:
    - host: gitlab.example.com
      token_ref: vault:secret/keeper/git#token
      passphrase: hunter2
`)
	_, _, diags, _ := LoadKeeperFromBytes("keeper.yml", src, ValidateOptions{})
	if !hasCode(diags, "unknown_key") {
		dump(t, diags)
		t.Fatalf("expected unknown_key for git.credentials[0].passphrase")
	}
}
