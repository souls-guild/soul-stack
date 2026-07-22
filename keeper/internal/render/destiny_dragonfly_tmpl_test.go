package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// dragonflyTemplatesDir is the dragonfly destiny .tmpl directory relative to
// this package. The destiny's L0 trial asserts the PLAN and runs only the CEL
// phase — it does NOT execute the text/template phase (flagfile/unit
// rendering). These L1 tests cover the text/template phase: they prove the
// flagfile carries CORRECT DF flags (--flag=value, absl), not redis.conf
// directives. A flagfile form regression (hyphen instead of underscore,
// redis-style `port 6379` instead of `--port=6379`) is invisible at L0 —
// caught here.
const dragonflyTemplatesDir = "../../../examples/destiny/dragonfly/templates"

// renderDragonflyTmpl renders one dragonfly destiny .tmpl through the same
// shared/tmpl.Engine as Soul (strict, missingkey=error). A Parse/Execute
// failure fails the test.
func renderDragonflyTmpl(t *testing.T, name string, root map[string]any) string {
	t.Helper()
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dragonflyTemplatesDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	out, err := engine.Render(string(body), root)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return out
}

// TestDragonflyFlagfile_AbslFlagForm guards the FORM of the DragonFly
// flagfile (absl flags --flag=value, NOT redis.conf). Root risk: DragonFly
// reads a flagfile (key=value with a double hyphen and `=`), NOT redis.conf
// syntax (`directive value`). A "rendered redis.conf instead of flagfile"
// regression (no `--`, no `=`, hyphen in the flag name) would leave DF unable
// to parse the config → start failure on the host, invisible at L0.
//
// The test renders the REAL dragonfly.flags.tmpl with the context the
// scenario assembles (vars + .self) and proves: (a) base directives in absl
// form --<flag>=<value>; (b) host layout (--dir/--aclfile/--pidfile/--log_dir)
// derives from the passed directories; (c) merged config ranges into
// --<key>=<value> with DF flags (underscore); (d) bind comes from
// .self.network.primary_ip (per-host).
func TestDragonflyFlagfile_AbslFlagForm(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"conf_dir": "/etc/dragonfly",
			"data_dir": "/var/lib/dragonfly",
			"port":     6379,
			"run_dir":  "/var/run/dragonfly",
			"log_dir":  "/var/log/dragonfly",
			// merged config from the scenario — DF flags (underscore form).
			// TLS block here proves the bool flag tls renders as --tls=true
			// (valid for absl).
			// ★ default_admin REDESIGN: masteruser/masterauth are ORDINARY
			// config keys (scenario sets them via vault()-in-cell); the
			// flagfile ranges over them like any directive.
			// ★ maxmemory_policy is NOT set: DF has no such flag (absl FATAL
			// on unknown).
			// ★ tls_port is NOT set: DF has no tls_port — TLS goes on the
			// main --port (TestDragonflyFlagfile_TLSOnMainPort gates the
			// absence of --tls_port).
			"config": map[string]any{
				"maxmemory":     "256mb",
				"maxclients":    10000,
				"tls":           "true",
				"tls_cert_file": "/etc/dragonfly/tls/dragonfly.crt",
				"masteruser":    "default_admin",
				"masterauth":    "df-admin-secret",
			},
		},
		"self": map[string]any{
			"network": map[string]any{"primary_ip": "10.0.0.1"},
		},
	}

	out := renderDragonflyFlagfileNormalized(t, root)

	// (a) base directives — absl form --<flag>=<value>, host layout from
	// directories.
	mustContainLine(t, out, "--bind=10.0.0.1")
	mustContainLine(t, out, "--port=6379")
	// unixsocket is a local listener (DF with --bind=primary_ip does NOT
	// listen on 127.0.0.1; local plugin calls go through the socket). Path
	// derives from run_dir.
	mustContainLine(t, out, "--unixsocket=/var/run/dragonfly/dragonfly.sock")
	mustContainLine(t, out, "--dir=/var/lib/dragonfly")
	mustContainLine(t, out, "--dbfilename=dump")
	mustContainLine(t, out, "--aclfile=/etc/dragonfly/users.acl")
	mustContainLine(t, out, "--pidfile=/var/run/dragonfly/dragonfly.pid")
	mustContainLine(t, out, "--log_dir=/var/log/dragonfly")

	// ★ default_admin REDESIGN: requirepass is REMOVED from the flagfile —
	// auth is under the ACL user default_admin (users.acl). Guards against a
	// regression reintroducing requirepass (which would reopen the master
	// password instead of the ACL model).
	if strings.Contains(out, "--requirepass") {
		t.Fatalf("flagfile contains --requirepass: the default_admin redesign removed it (authentication is under the default_admin ACL). Render:\n%s", out)
	}

	// (c) merged config — --<key>=<value> with DF flags (underscore). bool
	// tls=true. masteruser/masterauth (replica→master AUTH under
	// default_admin) are ordinary config keys.
	mustContainLine(t, out, "--maxmemory=256mb")
	mustContainLine(t, out, "--maxclients=10000")
	mustContainLine(t, out, "--tls=true")
	mustContainLine(t, out, "--tls_cert_file=/etc/dragonfly/tls/dragonfly.crt")
	mustContainLine(t, out, "--masteruser=default_admin")
	mustContainLine(t, out, "--masterauth=df-admin-secret")

	// NOT redis.conf: no line lacking a leading `--` (except blanks) — every
	// non-empty line must be an absl flag. Catches a "redis.conf syntax"
	// regression.
	for _, line := range strings.Split(out, "\n") {
		ln := strings.TrimSpace(line)
		if ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, "--") {
			t.Fatalf("flagfile contains a non-absl line (expected --flag=value): %q", ln)
		}
	}
}

// TestDragonflyFlagfile_TLSOnMainPort guards that DragonFly TLS goes on the
// MAIN --port, WITHOUT a separate tls_port. Root risk: DragonFly has NO
// tls_port flag (source recon: facade/dragonfly_listener.cc ABSL_FLAG(bool,
// tls, ...) switches the MAIN listener to TLS; tls_helpers.cc carries
// tls_cert_file/tls_key_file/tls_ca_cert_file; tls_port doesn't exist). A
// "brought tls_port back into config" regression → absl FATAL on an unknown
// flag → DF fails to start, invisible at L0. The test renders a TLS block
// WITHOUT tls_port and proves: --tls=true + --tls_cert_file are present,
// --unixsocket is present, and there's no --tls_port line.
func TestDragonflyFlagfile_TLSOnMainPort(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"conf_dir": "/etc/dragonfly",
			"data_dir": "/var/lib/dragonfly",
			"port":     6379,
			"run_dir":  "/var/run/dragonfly",
			"log_dir":  "/var/log/dragonfly",
			// TLS block in DF form WITHOUT tls_port (TLS on the main --port).
			"config": map[string]any{
				"tls":              "true",
				"tls_cert_file":    "/etc/dragonfly/tls/dragonfly.crt",
				"tls_key_file":     "/etc/dragonfly/tls/dragonfly.key",
				"tls_ca_cert_file": "/etc/dragonfly/tls/ca.crt",
				"tls_replication":  "true",
			},
		},
		"self": map[string]any{
			"network": map[string]any{"primary_ip": "10.0.0.1"},
		},
	}

	out := renderDragonflyFlagfileNormalized(t, root)

	mustContainLine(t, out, "--tls=true")
	mustContainLine(t, out, "--tls_cert_file=/etc/dragonfly/tls/dragonfly.crt")
	mustContainLine(t, out, "--tls_key_file=/etc/dragonfly/tls/dragonfly.key")
	mustContainLine(t, out, "--tls_ca_cert_file=/etc/dragonfly/tls/ca.crt")
	mustContainLine(t, out, "--unixsocket=/var/run/dragonfly/dragonfly.sock")

	// ★ ANTI-GUARD: no line with --tls_port. DragonFly has no such flag — its
	// presence is an absl FATAL at start. Catches a regression reintroducing
	// tls_port into df_config.
	if strings.Contains(out, "--tls_port") {
		t.Fatalf("flagfile contains --tls_port: DragonFly has no such flag (TLS goes on the main --port). Render:\n%s", out)
	}
}

// TestDragonflyFlagfile_HostLayoutOverride guards directive B (conf_dir/
// data_dir override reaches the flagfile). A "--dir/--aclfile hardcode
// /var/lib/dragonfly,/etc/dragonfly ignoring override" regression would break
// an operator's custom storage layout (snapshot writes to the wrong
// directory → write failure under systemd).
func TestDragonflyFlagfile_HostLayoutOverride(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"conf_dir": "/opt/df/conf",
			"data_dir": "/mnt/df/data",
			"port":     6379,
			"run_dir":  "/var/run/dragonfly",
			"log_dir":  "/var/log/dragonfly",
			"config":   map[string]any{},
		},
		"self": map[string]any{
			"network": map[string]any{"primary_ip": "10.0.0.5"},
		},
	}
	out := renderDragonflyFlagfileNormalized(t, root)
	mustContainLine(t, out, "--dir=/mnt/df/data")
	mustContainLine(t, out, "--aclfile=/opt/df/conf/users.acl")
}

// TestDragonflyServiceUnit_TypeSimpleAndExecStart guards the DragonFly
// (binary) unit. DragonFly runs foreground WITHOUT sd_notify → Type=simple
// (NOT notify like redis). ExecStart uses the --flagfile form from
// bin_dir/conf_dir. A Type=notify regression would hang the start (systemd
// would wait for a READY notification DF never sends).
func TestDragonflyServiceUnit_TypeSimpleAndExecStart(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"bin_dir":         "/usr/local/bin",
			"conf_dir":        "/etc/dragonfly",
			"dragonfly_user":  "dragonfly",
			"dragonfly_group": "dragonfly",
			"run_dir":         "/var/run/dragonfly",
			"data_dir":        "/var/lib/dragonfly",
			"log_dir":         "/var/log/dragonfly",
		},
	}
	out := renderDragonflyTmpl(t, "dragonfly.service.tmpl", root)
	mustContainLine(t, out, "Type=simple")
	mustContainLine(t, out, "ExecStart=/usr/local/bin/dragonfly --flagfile=/etc/dragonfly/dragonfly.conf")
	mustContainLine(t, out, "User=dragonfly")
	mustContainLine(t, out, "Group=dragonfly")
	if strings.Contains(out, "Type=notify") {
		t.Fatalf("the DragonFly unit should not be Type=notify (DF has no sd_notify): %s", out)
	}
}

// renderDragonflyFlagfileNormalized renders dragonfly.flags.tmpl and
// normalizes trailing whitespace/lines. Factored out: both flagfile tests
// read the same template.
func renderDragonflyFlagfileNormalized(t *testing.T, root map[string]any) string {
	t.Helper()
	return renderDragonflyTmpl(t, "dragonfly.flags.tmpl", root)
}

// mustContainLine fails if none of out's lines exactly match want (after
// trim). Compares by WHOLE line (not substring): `--port=6379` must not pass
// due to `--tls_port=6379` etc.
func mustContainLine(t *testing.T, out, want string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return
		}
	}
	t.Fatalf("expected line %q in the render, not found. Render:\n%s", want, out)
}
