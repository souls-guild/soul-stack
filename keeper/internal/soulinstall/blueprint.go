// Package soulinstall is canonical install blueprint for deploying soul agent
// on a fresh VM. Single source of truth for two delivery paths:
//
//   - cloud-init userdata (B-flat, [ADR-017(h)](../../../docs/adr/0017-keeper-side-core.md)):
//     [RenderCloudInitYAML] prints cloud-config YAML, provider puts it into VM
//     metadata during Create. Used by `core.cloud.created`.
//   - full install over SSH (Teleport, [ADR-063 amendment](../../../docs/adr/0063-bootstrap-token-delivery.md)):
//     [RenderInstallScript] returns sequence of SSH commands for platforms
//     without cloud-init userdata (e.g. a namespace with `ci_user_data` disabled).
//     Secrets (CA, soul.yml) go through STDIN, not argv. Foundation for now:
//     called in Slice 2 (install mode `core.bootstrap.delivered`).
//
// Blueprint describes ONE same install result: same files by same paths with
// same modes (constants below), same soul.yml and systemd unit. True single
// source: soul.yml/unit contents are produced by SoulConfigYAML/SystemdUnit, and
// cloud-init template renders them through {{ .SoulConfigYAMLIndented }} /
// {{ .SystemdUnitIndented }} (not text copy); both renderers physically take the
// same material, making drift impossible. Only intentional difference between
// paths is keeper-ca.pem mode: 0600 in SSH install (stricter floor) vs 0644 in
// cloud-init (CA is public); see KeeperCAMode.
//
// Per-VM bootstrap token is NOT carried by blueprint (in either renderer):
// userdata is logged by provider (security floor), token is a separate scenario
// step (see ADR-017(h) B-flat). RenderInstallScript does NOT include token write
// and `systemctl start`; delivered mode adds that in Slice 2.
package soulinstall

import (
	"embed"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"text/template"
)

//go:embed templates/cloud-init.tmpl
var templatesFS embed.FS

// parsedTemplate is cloud-init template from embed.FS parsed once. Parse time is
// checked by happy-path test; error here is code bug (template byte-compiled
// from go:embed), not runtime issue.
var parsedTemplate = template.Must(
	template.New("cloud-init.tmpl").ParseFS(templatesFS, "templates/cloud-init.tmpl"),
)

// Canonical paths and modes of install result are single source for both
// renderers. RenderInstallScript builds install commands directly from these
// constants; cloud-init.tmpl keeps paths in write_files/runcmd, while file bodies
// (soul.yml/unit) are rendered from SoulConfigYAML/SystemdUnit (see
// RenderCloudInitYAML).
const (
	// TLSDir is directory for soul agent TLS material on VM.
	TLSDir = "/etc/soul/tls"
	// KeeperCAPath is Keeper CA PEM (pin Bootstrap channel souls<->keeper mTLS).
	KeeperCAPath = "/etc/soul/tls/keeper-ca.pem"
	// SoulConfigPath is minimal soul agent config (keeper.endpoints + ca).
	SoulConfigPath = "/etc/soul/soul.yml"
	// SoulServicePath is systemd unit of soul agent.
	SoulServicePath = "/etc/systemd/system/soul.service"
	// SoulBinaryPath is soul binary install path.
	SoulBinaryPath = "/usr/local/bin/soul"
	// SeedCertPath is cert.pem of active SoulSeed on VM: <paths.seed from
	// SoulConfigYAML>/current/cert.pem (layout soul/internal/seed, symlink
	// `current` + CertFile). File existence means token already redeemed:
	// idempotency guard for `soul init` in core.bootstrap.delivered (token is
	// single-use).
	// Sync-guard: TestSeedCertPath_SyncWithSoulSeedLayout.
	SeedCertPath = "/var/lib/soul-stack/seed/current/cert.pem"
	// SelfOnboardTokensPath is FQDN->token map file on VM (self-onboard Variant T).
	// cloud-init chooses its token by hostname and runs `soul init`. Mode 0600
	// (carries secrets, test stand). See Blueprint.SelfOnboardTokens.
	SelfOnboardTokensPath = "/etc/soul/self-onboard-tokens"

	// KeeperCAMode is keeper-ca.pem mode for SSH install (0600; in cloud-init same
	// file is written by write_files with 0644; neither needs more privacy: CA is
	// public, but SSH install sets stricter floor).
	KeeperCAMode = "0600"
	// SoulBinaryMode is soul binary mode (executable).
	SoulBinaryMode = "0755"
)

// SoulBinaryCA values define which trust store curl uses when downloading soul
// binary. Empty value is treated as SoulBinaryCAKeeper (back-compat
// secure-default). It relaxes ONLY certificate verification of artifact host;
// Bootstrap channel (souls<->keeper mTLS) and binary SHA256 verification are
// unaffected.
const (
	// SoulBinaryCAKeeper pins Keeper PEM CA (`curl --cacert keeper-ca.pem`).
	SoulBinaryCAKeeper = "keeper"
	// SoulBinaryCASystem uses OS trust bundle (`curl` without `--cacert`); for
	// artifact hosts with public CA (for example Nexus behind GlobalSign).
	SoulBinaryCASystem = "system"
)

// Blueprint is resolved parameters of install result (cloud-init or SSH). Built
// from [shared/config.KeeperCloudInit] on each render call (hot-reload friendly).
// Source of fields is one keeper.yml::cloud_init block shared by both delivery
// paths (config reuse, DRY).
type Blueprint struct {
	// BootstrapEndpoint is `host:port` of Keeper LB (Bootstrap RPC listener).
	// host goes to soul.yml keeper.endpoints[0].host, port to bootstrap_port.
	BootstrapEndpoint string

	// EventStreamPort is TCP port of EventStream phase (mTLS listener) on same
	// host; goes to soul.yml event_stream_port. 0 -> bootstrap_endpoint port
	// (back-compat: single-port LB). See 6th wall of ADR-063: both ports from one
	// endpoint made soul dial EventStream on Bootstrap port.
	EventStreamPort int

	// KeeperCAPem is PEM-encoded Keeper CA (contents of `ca` field from Vault KV).
	// Written to VM at [KeeperCAPath]; then curl --cacert uses it when downloading
	// soul binary (in SoulBinaryCAKeeper mode).
	KeeperCAPem string

	// SoulBinaryURL is HTTPS URL for downloading `soul` binary. Plain http is
	// rejected (security: only over TLS, regardless of SoulBinaryCA).
	SoulBinaryURL string

	// SoulBinaryCA is trust store for curl when downloading binary:
	// SoulBinaryCAKeeper (default/empty) -> `--cacert keeper-ca.pem`;
	// SoulBinaryCASystem -> system bundle (without `--cacert`, for public CAs).
	// Relaxes ONLY artifact host certificate verification; Bootstrap channel and
	// binary SHA256 verification are unaffected.
	SoulBinaryCA string

	// SoulVersion is optional string label (for diagnostics). Goes to cloud-init
	// as comment. Binary signature verification is deferred (ADR-017(h) amendment).
	SoulVersion string

	// SelfOnboardTokens is map FQDN->plain bootstrap token for self-onboard
	// "Variant T" (ADR-017(h) amendment). Non-empty -> cloud-init bakes these
	// tokens into userdata (shared blob) and adds `soul init` phase (token selected
	// by VM hostname), between binary install and `soul run`. VM onboards itself
	// in one cloud-init cycle, without claim callback and without keeper.push.
	//
	// SECURITY (test stand): in this mode plain tokens land in userdata, which
	// cloud provider stores in plaintext metadata. This is a deliberate compromise
	// of self-onboard test stand (removes security guard `bootstrap_token`; see
	// RenderCloudInitYAML). TODO(prod): return late-binding claim (Variant C) or
	// per-VM userdata (individual blob per VM instead of shared map).
	//
	// Empty/nil -> regular B-flat render (no tokens in userdata, guard active).
	SelfOnboardTokens map[string]string
}

// selfOnboard reports self-onboard mode (Blueprint carries per-VM tokens).
func (b Blueprint) selfOnboard() bool { return len(b.SelfOnboardTokens) > 0 }

// Validate checks that Blueprint is filled enough for rendering. Returns first
// found error with clear message (no need to raise several errors: config phase
// already caught format issues, here it is runtime precondition).
func (b Blueprint) Validate() error {
	if b.BootstrapEndpoint == "" {
		return errors.New("cloud_init.bootstrap_endpoint is empty")
	}
	if _, _, err := net.SplitHostPort(b.BootstrapEndpoint); err != nil {
		return fmt.Errorf("cloud_init.bootstrap_endpoint %q is not host:port: %w", b.BootstrapEndpoint, err)
	}
	if !strings.Contains(b.KeeperCAPem, "BEGIN CERTIFICATE") {
		return errors.New("cloud_init.tls_ca is not a PEM-encoded certificate")
	}
	if b.SoulBinaryURL == "" {
		return errors.New("cloud_init.soul_binary_url is empty")
	}
	if !strings.HasPrefix(b.SoulBinaryURL, "https://") {
		return fmt.Errorf("cloud_init.soul_binary_url %q must be https (CA over TLS only, plain http rejected)", b.SoulBinaryURL)
	}
	if b.EventStreamPort < 0 || b.EventStreamPort > 65535 {
		return fmt.Errorf("cloud_init.event_stream_port %d must be in 1..65535 (0 = bootstrap_endpoint port)", b.EventStreamPort)
	}
	switch b.SoulBinaryCA {
	case "", SoulBinaryCAKeeper, SoulBinaryCASystem:
	default:
		return fmt.Errorf("cloud_init.soul_binary_ca %q must be %q or %q (empty defaults to %q)",
			b.SoulBinaryCA, SoulBinaryCAKeeper, SoulBinaryCASystem, SoulBinaryCAKeeper)
	}
	return nil
}

// useSystemCA means curl without `--cacert` (system trust store). It affects
// ONLY binary download; keeper-ca.pem is still written to VM, and Bootstrap
// channel (souls<->keeper mTLS) is always pinned to Keeper CA.
func (b Blueprint) useSystemCA() bool {
	return b.SoulBinaryCA == SoulBinaryCASystem
}

// hostPort parses BootstrapEndpoint into host + validated TCP port. Called after
// Validate (format already checked), but re-checks port range.
func (b Blueprint) hostPort() (string, int, error) {
	host, portStr, _ := net.SplitHostPort(b.BootstrapEndpoint)
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return "", 0, fmt.Errorf("cloud_init.bootstrap_endpoint %q: port not valid: %v", b.BootstrapEndpoint, err)
	}
	return host, port, nil
}

// soulEndpoint is host + pair of ports for soul.yml. EventStreamPort==0 ->
// fallback to bootstrap port (back-compat: single-port LB).
func (b Blueprint) soulEndpoint() (host string, eventPort, bootstrapPort int, err error) {
	host, bootstrapPort, err = b.hostPort()
	if err != nil {
		return "", 0, 0, err
	}
	eventPort = b.EventStreamPort
	if eventPort == 0 {
		eventPort = bootstrapPort
	}
	return host, eventPort, bootstrapPort, nil
}

// RenderCloudInitYAML renders cloud-config YAML from cloud-init template.
// Idempotent: same inputs produce byte-identical output.
//
// Security: output is checked for absence of substrings `bootstrap_token` /
// `vault:`; guard against accidental secret leak from template (for example if a
// Blueprint field contains such substring). This is invariant for all channels
// receiving userdata (cloud-provider metadata, audit payload, OTel).
func RenderCloudInitYAML(bp Blueprint) (string, error) {
	if err := bp.Validate(); err != nil {
		return "", err
	}
	host, eventPort, bootstrapPort, err := bp.soulEndpoint()
	if err != nil {
		return "", err
	}

	// Body of soul.yml and systemd unit is rendered FROM the same Go functions
	// (SoulConfigYAML/SystemdUnit) used by RenderInstallScript: single source of
	// truth without text duplicate in template. Indent 6 spaces puts block under
	// YAML key `content: |` (like indentBlock for CA PEM).
	view := struct {
		TLSCAPemIndented          string
		SoulConfigYAMLIndented    string
		SystemdUnitIndented       string
		SoulBinaryURL             string
		UseSystemCA               bool
		SoulVersion               string
		SelfOnboard               bool
		SelfOnboardTokensIndented string
	}{
		TLSCAPemIndented:       indentBlock(bp.KeeperCAPem, "      "),
		SoulConfigYAMLIndented: indentBlock(SoulConfigYAML(host, eventPort, bootstrapPort), "      "),
		SystemdUnitIndented:    indentBlock(SystemdUnit(), "      "),
		SoulBinaryURL:          bp.SoulBinaryURL,
		UseSystemCA:            bp.useSystemCA(),
		SoulVersion:            bp.SoulVersion,
		SelfOnboard:            bp.selfOnboard(),
	}
	if bp.selfOnboard() {
		view.SelfOnboardTokensIndented = indentBlock(selfOnboardTokensFile(bp.SelfOnboardTokens), "      ")
	}

	var sb strings.Builder
	if err := parsedTemplate.Execute(&sb, view); err != nil {
		return "", fmt.Errorf("cloud-init render: %w", err)
	}
	out := sb.String()

	// Security floor: userdata must NOT carry vault-ref (secrets are resolved
	// BEFORE render). Always enforced, including self-onboard (userdata may
	// legitimately carry bootstrap tokens, but not vault paths).
	if strings.Contains(out, "vault:") {
		return "", errors.New("cloud-init render: output contains 'vault:' substring — vault-refs must be resolved before render")
	}
	// Security floor `bootstrap_token`: userdata must NOT carry tokens (B-flat).
	// Disabled in self-onboard mode (Variant T, test stand): per-VM tokens in
	// userdata are intentional there (VM onboards in one cloud-init cycle).
	// Outside self-onboard, guard remains active as before (accidental leak guard).
	// TODO(prod): restore guard for prod (late-binding claim / per-VM userdata).
	if !bp.selfOnboard() && strings.Contains(out, "bootstrap_token") {
		return "", errors.New("cloud-init render: output contains 'bootstrap_token' substring — userdata must not carry per-VM tokens (B-flat invariant)")
	}
	return out, nil
}

// InstallStep is one SSH command of install sequence. Cmd is string for
// `session.Run` (lands in process argv on VM); Stdin is data fed to process via
// stdin (nil means no stdin). Secrets MUST go through Stdin, NOT Cmd: argv is
// visible in `ps`/audit.log/journald on the VM itself (ADR-063 §A1).
type InstallStep struct {
	Cmd   string
	Stdin []byte
}

// RenderInstallScript returns sequence of SSH commands installing the same
// install result as [RenderCloudInitYAML], for platforms without cloud-init
// userdata (full install over SSH, ADR-063 amendment). Order:
//
//  1. install -d directories (TLS dir 0600 + soul state directories).
//  2. cat > keeper-ca.pem (Stdin=CA PEM) + chmod 0600.
//  3. cat > soul.yml (Stdin=soul.yml content).
//  4. cat > soul.service (Stdin=systemd-unit).
//  5. curl soul binary (--cacert keeper-ca.pem in keeper mode, absent in system)
//     + chmod 0755.
//
// Does NOT include token write and `systemctl start`: delivered mode adds this
// (Slice 2). CA and soul.yml go through Stdin (not argv): secret-write floor.
// Not called by anyone yet: foundation for Slice 2.
func RenderInstallScript(bp Blueprint) ([]InstallStep, error) {
	if err := bp.Validate(); err != nil {
		return nil, err
	}
	host, eventPort, bootstrapPort, err := bp.soulEndpoint()
	if err != nil {
		return nil, err
	}

	steps := []InstallStep{
		{Cmd: fmt.Sprintf(
			"install -d -m %s %s && install -d -m 0755 /etc/soul /var/lib/soul-stack /var/lib/soul-stack/modules /var/lib/soul-stack/seed",
			KeeperCAMode, TLSDir)},
		{Cmd: fmt.Sprintf("umask 077 && cat > %s && chmod %s %s", KeeperCAPath, KeeperCAMode, KeeperCAPath),
			Stdin: []byte(bp.KeeperCAPem)},
		{Cmd: fmt.Sprintf("cat > %s", SoulConfigPath),
			Stdin: []byte(SoulConfigYAML(host, eventPort, bootstrapPort))},
		{Cmd: fmt.Sprintf("cat > %s", SoulServicePath),
			Stdin: []byte(SystemdUnit())},
		{Cmd: binaryCurlCmd(bp.SoulBinaryURL, bp.useSystemCA())},
		{Cmd: fmt.Sprintf("chmod %s %s", SoulBinaryMode, SoulBinaryPath)},
	}
	return steps, nil
}

// binaryCurlCmd is curl command for downloading soul binary. In keeper mode it
// pins Keeper CA (`--cacert`); in system mode uses system trust store (without
// `--cacert`). Same semantics as runcmd in cloud-init template.
func binaryCurlCmd(url string, systemCA bool) string {
	if systemCA {
		return fmt.Sprintf("curl --fail --show-error --silent --output %s %s", SoulBinaryPath, url)
	}
	return fmt.Sprintf("curl --fail --show-error --silent --cacert %s --output %s %s", KeeperCAPath, SoulBinaryPath, url)
}

// selfOnboardTokensFile serializes map FQDN->token into line format
// `<fqdn> <token>` (self-onboard Variant T). Format is grep/awk-friendly:
// cloud-init selects its line by `$(hostname -f)` as first field. FQDNs are
// sorted for byte stability of render (Go map iteration is nondeterministic).
// Tokens are base64url without spaces (bootstraptoken.Generate), so split by
// space is unambiguous.
func selfOnboardTokensFile(tokens map[string]string) string {
	fqdns := make([]string, 0, len(tokens))
	for fqdn := range tokens {
		fqdns = append(fqdns, fqdn)
	}
	sort.Strings(fqdns)
	var b strings.Builder
	for _, fqdn := range fqdns {
		b.WriteString(fqdn)
		b.WriteByte(' ')
		b.WriteString(tokens[fqdn])
		b.WriteByte('\n')
	}
	return b.String()
}

// SoulConfigYAML is contents of soul.yml (minimal soul agent config for
// bootstrap: keeper.endpoints + pin CA). Single source: this same function
// provides soul.yml body for both SSH install (RenderInstallScript) and
// cloud-init (RenderCloudInitYAML substitutes output under write_files
// /etc/soul/soul.yml). Phase ports differ: eventStreamPort is EventStream
// (mTLS), bootstrapPort is Bootstrap RPC (see 6th wall of ADR-063).
func SoulConfigYAML(host string, eventStreamPort, bootstrapPort int) string {
	return fmt.Sprintf(`# Minimal soul agent config for cloud-init bootstrap.
# Per-VM token and SoulSeed certificate will be added by next scenario step.
paths:
  modules: /var/lib/soul-stack/modules
  seed:    /var/lib/soul-stack/seed
keeper:
  endpoints:
    - host: %s
      event_stream_port: %d
      bootstrap_port: %d
  tls:
    ca: %s
`, host, eventStreamPort, bootstrapPort, KeeperCAPath)
}

// SystemdUnit is contents of soul.service. Single source: this same function
// provides unit body for both SSH install (RenderInstallScript) and cloud-init
// (RenderCloudInitYAML substitutes output under write_files soul.service).
func SystemdUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Soul Stack agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s run --config %s
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
`, SoulBinaryPath, SoulConfigPath)
}

// indentBlock shifts each line of text block by `prefix` so it lands under YAML
// key with heredoc-style `content: |` (cloud-config). Used for CA PEM, soul.yml,
// and systemd unit: all three are put into template by one mechanism. Trailing
// newline is dropped (TrimRight) so there is exactly one break between block and
// next YAML key, not an empty shifted line.
func indentBlock(block, prefix string) string {
	lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
