package soulinstall_test

import (
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/soulinstall"

	"gopkg.in/yaml.v3"
)

const testCAPem = `-----BEGIN CERTIFICATE-----
MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAcXamPlexamplePEMblock
ForUnitTestsOnlyNotARealCertificateThisIsJustForRenderCheckLength
PaddingPaddingPaddingPaddingPaddingPaddingPaddingPaddingPadding==
-----END CERTIFICATE-----`

func validBlueprint() soulinstall.Blueprint {
	return soulinstall.Blueprint{
		BootstrapEndpoint: "lb.keeper.example:9442",
		KeeperCAPem:       testCAPem,
		SoulBinaryURL:     "https://artifacts.example/soul/v1.0.0/soul",
		SoulVersion:       "v1.0.0",
	}
}

// TestRenderInstallScript verifies SSH step order of full install (directories
// -> keeper-ca.pem -> soul.yml -> soul.service -> curl binary) and
// ARGV-LEAK-GUARD: PEM CA goes through .Stdin, not printed in .Cmd (cat > path,
// not echo PEM).
func TestRenderInstallScript(t *testing.T) {
	steps, err := soulinstall.RenderInstallScript(validBlueprint())
	if err != nil {
		t.Fatalf("RenderInstallScript: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("RenderInstallScript returned no steps")
	}

	// Order: each marker must first appear after previous one. Marker is searched
	// in .Cmd (Stdin carries content, not write path).
	order := []struct {
		name   string
		marker string
	}{
		{"install-d dirs", "install -d"},
		{"keeper-ca.pem", soulinstall.KeeperCAPath},
		{"soul.yml", soulinstall.SoulConfigPath},
		{"soul.service", soulinstall.SoulServicePath},
		{"curl binary", "curl"},
	}
	prev := -1
	for _, want := range order {
		idx := firstStepCmdContains(steps, want.marker)
		if idx < 0 {
			t.Fatalf("step %q (marker %q) not found in install script", want.name, want.marker)
		}
		if idx <= prev {
			t.Errorf("step %q at index %d out of order (must be > %d)", want.name, idx, prev)
		}
		prev = idx
	}

	// curl binary is last by order among markers; chmod 0755 goes after.
	if !stepCmdContainsAny(steps, soulinstall.SoulBinaryPath) {
		t.Errorf("install script does not reference soul binary path %q", soulinstall.SoulBinaryPath)
	}

	// ARGV-LEAK-GUARD: CA body (PEM) travels in step .Stdin, no .Cmd carries PEM.
	caStdinSeen := false
	for i, s := range steps {
		if strings.Contains(s.Cmd, "BEGIN CERTIFICATE") || strings.Contains(s.Cmd, "END CERTIFICATE") {
			t.Errorf("step %d leaks PEM body into argv (.Cmd): %q", i, s.Cmd)
		}
		if strings.Contains(s.Cmd, "echo") && strings.Contains(s.Cmd, soulinstall.KeeperCAPath) {
			t.Errorf("step %d uses echo to write CA (argv leak): %q", i, s.Cmd)
		}
		if strings.Contains(string(s.Stdin), "BEGIN CERTIFICATE") {
			caStdinSeen = true
			// CA-write step must write to keeper-ca.pem through redirect, not echo.
			if !strings.Contains(s.Cmd, soulinstall.KeeperCAPath) {
				t.Errorf("CA-stdin step %d does not redirect into keeper-ca.pem: %q", i, s.Cmd)
			}
		}
	}
	if !caStdinSeen {
		t.Error("CA PEM body never delivered via .Stdin — ARGV-LEAK-GUARD cannot hold")
	}
}

// TestRenderInstallScript_CAmode: curl step downloading binary pins Keeper CA
// (--cacert) in keeper/empty modes and goes without --cacert in system mode.
func TestRenderInstallScript_CAmode(t *testing.T) {
	t.Run("system → no --cacert", func(t *testing.T) {
		bp := validBlueprint()
		bp.SoulBinaryCA = soulinstall.SoulBinaryCASystem
		steps, err := soulinstall.RenderInstallScript(bp)
		if err != nil {
			t.Fatalf("RenderInstallScript: %v", err)
		}
		curl := binaryCurlStep(t, steps)
		if strings.Contains(curl, "--cacert") {
			t.Errorf("system mode must not pin --cacert, got: %q", curl)
		}
	})

	for _, ca := range []string{"", soulinstall.SoulBinaryCAKeeper} {
		t.Run("keeper/empty → --cacert "+ca, func(t *testing.T) {
			bp := validBlueprint()
			bp.SoulBinaryCA = ca
			steps, err := soulinstall.RenderInstallScript(bp)
			if err != nil {
				t.Fatalf("RenderInstallScript: %v", err)
			}
			curl := binaryCurlStep(t, steps)
			if !strings.Contains(curl, "--cacert") || !strings.Contains(curl, soulinstall.KeeperCAPath) {
				t.Errorf("keeper mode must pin --cacert on keeper-ca.pem, got: %q", curl)
			}
		})
	}
}

// TestRenderCloudInitYAML_Stable verifies byte stability of cloud-init after
// extracting blueprint: key userdata elements are present and https floor rejects
// http.
func TestRenderCloudInitYAML_Stable(t *testing.T) {
	out, err := soulinstall.RenderCloudInitYAML(validBlueprint())
	if err != nil {
		t.Fatalf("RenderCloudInitYAML: %v", err)
	}
	if !strings.HasPrefix(out, "#cloud-config") {
		t.Errorf("output does not start with #cloud-config header")
	}
	for _, want := range []string{
		"write_files:",
		soulinstall.KeeperCAPath,
		"-----BEGIN CERTIFICATE-----",
		soulinstall.SoulConfigPath,
		soulinstall.SoulServicePath,
		"runcmd:",
		"curl",
		"https://artifacts.example/soul/v1.0.0/soul",
		"--cacert /etc/soul/tls/keeper-ca.pem",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cloud-init missing key element %q", want)
		}
	}

	// Valid YAML with write_files + runcmd top-level (header ignored).
	var v map[string]any
	if err := yaml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("rendered userdata is not valid YAML: %v", err)
	}
	if _, ok := v["write_files"]; !ok {
		t.Errorf("rendered YAML has no top-level write_files key")
	}
	if _, ok := v["runcmd"]; !ok {
		t.Errorf("rendered YAML has no top-level runcmd key")
	}

	// https floor: plain http is rejected at render (security, independent of CA).
	bp := validBlueprint()
	bp.SoulBinaryURL = "http://artifacts.example/soul"
	if _, err := soulinstall.RenderCloudInitYAML(bp); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http URL must be rejected with https-floor error, got %v", err)
	}
}

// TestRenderCloudInitYAML_SelfOnboard verifies self-onboard "Variant T"
// (ADR-017(h)): Blueprint carries map FQDN->token, cloud-init bakes them and
// adds `soul init` phase (token by hostname) between binary install and
// `soul run`.
func TestRenderCloudInitYAML_SelfOnboard(t *testing.T) {
	bp := validBlueprint()
	bp.SelfOnboardTokens = map[string]string{
		"redis-0.ns.vm.example": "TOKEN-AAA",
		"redis-1.ns.vm.example": "TOKEN-BBB",
	}
	out, err := soulinstall.RenderCloudInitYAML(bp)
	if err != nil {
		t.Fatalf("RenderCloudInitYAML self-onboard: %v", err)
	}

	// Tokens and FQDNs are baked in (map delivered to VM).
	for fqdn, tok := range bp.SelfOnboardTokens {
		if !strings.Contains(out, fqdn) {
			t.Errorf("self-onboard userdata missing FQDN %q", fqdn)
		}
		if !strings.Contains(out, tok) {
			t.Errorf("self-onboard userdata missing token for %q", fqdn)
		}
	}

	// soul init phase is present and token is selected by hostname (not hardcoded).
	if !strings.Contains(out, "soul init") {
		t.Error("self-onboard userdata has no `soul init` phase")
	}
	if !strings.Contains(out, "hostname") {
		t.Error("self-onboard must select token by hostname (no hostname reference found)")
	}
	// init BEFORE `soul run`/systemd start: collect indexes and verify order.
	initIdx := strings.Index(out, "soul init")
	startIdx := strings.Index(out, "systemctl start soul")
	if initIdx < 0 || startIdx < 0 {
		t.Fatalf("both soul-init and systemctl-start must be present (init=%d start=%d)", initIdx, startIdx)
	}
	if initIdx > startIdx {
		t.Errorf("`soul init` (%d) must run BEFORE `systemctl start soul` (%d)", initIdx, startIdx)
	}

	// Token is NOT in `soul init` argv (env SOUL_BOOTSTRAP_TOKEN, not
	// --token=<plain>): argv is visible in ps/journald on VM. Verify no
	// `--token=TOKEN-`.
	if strings.Contains(out, "--token=TOKEN-") {
		t.Errorf("self-onboard leaks token into `soul init --token=` argv (use env SOUL_BOOTSTRAP_TOKEN)")
	}

	// Valid YAML.
	var v map[string]any
	if err := yaml.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("self-onboard userdata is not valid YAML: %v", err)
	}
}

// TestRenderCloudInitYAML_SecurityGuard_BlocksTokenWithoutSelfOnboard verifies
// security floor is preserved without self-onboard: substring bootstrap_token in
// userdata (for example accidentally leaked through Blueprint) fails render.
// Guard is removed ONLY in self-onboard mode (where tokens in userdata are
// intentional, test stand).
func TestRenderCloudInitYAML_SecurityGuard_BlocksTokenWithoutSelfOnboard(t *testing.T) {
	bp := validBlueprint()
	// Leaked token in a field that lands in userdata (SoulVersion goes as comment).
	bp.SoulVersion = "v1 bootstrap_token=leak"
	if _, err := soulinstall.RenderCloudInitYAML(bp); err == nil {
		t.Fatal("security guard must reject bootstrap_token substring when NOT self-onboard")
	}
}

// TestRenderCloudInitYAML_SelfOnboard_RejectsVaultRef verifies that even in
// self-onboard, vault-ref in userdata remains forbidden (secrets are resolved
// BEFORE render; only bootstrap tokens are legitimate in self-onboard userdata,
// not vault refs).
func TestRenderCloudInitYAML_SelfOnboard_RejectsVaultRef(t *testing.T) {
	bp := validBlueprint()
	bp.SelfOnboardTokens = map[string]string{"h0.example.com": "TOK"}
	bp.SoulVersion = "vault:secret/leak"
	if _, err := soulinstall.RenderCloudInitYAML(bp); err == nil {
		t.Fatal("vault-ref must be rejected even in self-onboard mode")
	}
}

// TestSoulConfigYAML_Ports verifies soul.yml carries DIFFERENT phase ports:
// event_stream_port (EventStream, mTLS) and bootstrap_port (Bootstrap RPC,
// server-only TLS). 6th wall of ADR-063: both ports used to be derived from one
// bootstrap_endpoint, so soul run dialed EventStream on Bootstrap port
// ("Unimplemented: method EventStream").
func TestSoulConfigYAML_Ports(t *testing.T) {
	out := soulinstall.SoulConfigYAML("lb.keeper.example", 9443, 9442)
	if !strings.Contains(out, "event_stream_port: 9443") {
		t.Errorf("soul.yml missing event_stream_port 9443:\n%s", out)
	}
	if !strings.Contains(out, "bootstrap_port: 9442") {
		t.Errorf("soul.yml missing bootstrap_port 9442:\n%s", out)
	}
}

// TestRender_EventStreamPort verifies Blueprint.EventStreamPort reaches
// soul.yml in BOTH renderers; 0 -> back-compat fallback to bootstrap_endpoint
// port (single-port LB); out of range -> Validate error.
func TestRender_EventStreamPort(t *testing.T) {
	withPort := validBlueprint() // bootstrap_endpoint lb.keeper.example:9442
	withPort.EventStreamPort = 9443

	t.Run("install script", func(t *testing.T) {
		steps, err := soulinstall.RenderInstallScript(withPort)
		if err != nil {
			t.Fatalf("RenderInstallScript: %v", err)
		}
		idx := firstStepCmdContains(steps, soulinstall.SoulConfigPath)
		if idx < 0 {
			t.Fatal("no soul.yml write step")
		}
		yml := string(steps[idx].Stdin)
		if !strings.Contains(yml, "event_stream_port: 9443") || !strings.Contains(yml, "bootstrap_port: 9442") {
			t.Errorf("install soul.yml ports wrong:\n%s", yml)
		}
	})

	t.Run("cloud-init", func(t *testing.T) {
		out, err := soulinstall.RenderCloudInitYAML(withPort)
		if err != nil {
			t.Fatalf("RenderCloudInitYAML: %v", err)
		}
		if !strings.Contains(out, "event_stream_port: 9443") || !strings.Contains(out, "bootstrap_port: 9442") {
			t.Errorf("cloud-init soul.yml ports wrong")
		}
	})

	t.Run("fallback 0 → bootstrap port", func(t *testing.T) {
		out, err := soulinstall.RenderCloudInitYAML(validBlueprint())
		if err != nil {
			t.Fatalf("RenderCloudInitYAML: %v", err)
		}
		if !strings.Contains(out, "event_stream_port: 9442") {
			t.Errorf("fallback must reuse bootstrap port for event_stream_port")
		}
	})

	t.Run("out of range → Validate error", func(t *testing.T) {
		bad := validBlueprint()
		bad.EventStreamPort = 70000
		if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "event_stream_port") {
			t.Fatalf("expected event_stream_port range error, got %v", err)
		}
	})
}

// firstStepCmdContains returns index of first step whose .Cmd contains sub, or -1.
func firstStepCmdContains(steps []soulinstall.InstallStep, sub string) int {
	for i, s := range steps {
		if strings.Contains(s.Cmd, sub) {
			return i
		}
	}
	return -1
}

func stepCmdContainsAny(steps []soulinstall.InstallStep, sub string) bool {
	return firstStepCmdContains(steps, sub) >= 0
}

// binaryCurlStep returns .Cmd of step downloading soul binary (curl + binary
// path). Isolates assertion from other steps.
func binaryCurlStep(t *testing.T, steps []soulinstall.InstallStep) string {
	t.Helper()
	for _, s := range steps {
		if strings.Contains(s.Cmd, "curl") && strings.Contains(s.Cmd, soulinstall.SoulBinaryPath) {
			return s.Cmd
		}
	}
	t.Fatalf("no binary-download curl step found in install script")
	return ""
}
