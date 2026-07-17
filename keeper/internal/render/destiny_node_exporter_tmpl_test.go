package render

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// nodeExporterTemplatesDir is the node-exporter destiny's .tmpl directory,
// relative to this package (keeper/internal/render). The test lives here
// because this package owns the integration with Soul's text/template engine
// (shared/tmpl) and already hosts TestRenderToSoulExecute_* — the shared
// .tmpl render path.
const nodeExporterTemplatesDir = "../../../examples/destiny/node-exporter/templates"

// TestNodeExporterTemplates_ParseAndRender guards the invariant that every
// node-exporter destiny .tmpl actually PARSES and RENDERS under strict
// text/template.
//
// Why: this destiny's L0 trial asserts the task PLAN and only runs the CEL
// phase — it never executes the text/template phase on the templates. A
// literal `{{ }}` in a template comment (which Go text/template strict
// parses as an empty action → "missing value for command" on Parse) slipped
// past tests this way and hard-failed core.file.rendered on the host. This
// test runs EVERY .tmpl through the same engine as Soul (shared/tmpl.Engine,
// strict missingkey=error), with the §3.2 context root
// {vars,self,role,essence}. A Parse/Execute failure catches a "template
// doesn't parse/render" regression at the unit-test level, before E2E.
func TestNodeExporterTemplates_ParseAndRender(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(nodeExporterTemplatesDir, "*.tmpl"))
	if err != nil {
		t.Fatalf("glob .tmpl: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no .tmpl found in %q - path to destiny is broken", nodeExporterTemplatesDir)
	}
	sort.Strings(files)

	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}

	// core.file.rendered's text/template context root (templating.md §3.2,
	// Option B): {vars, input, self, role, essence}. vars are destiny locals
	// from vars.yml (bin_dir/bin_path), read by the template as
	// `.vars.<file_var>` DIRECTLY (no params.vars passthrough); input is the
	// pass's operator input (user/group/listen/textfile_dir + daemon prod
	// params), read by the template as `.input.<name>` directly (Option B,
	// ADR-010 §3.2 amendment). Templates without input/vars (scripts, timers)
	// simply don't read the extra keys.
	root := nodeExporterRenderVars

	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			out, err := engine.Render(string(body), root)
			if err != nil {
				// This is exactly where the literal `{{ }}` in a comment blocker
				// used to fail (ErrParse: "missing value for command").
				t.Fatalf("text/template render %s failed: %v", name, err)
			}
			// The render shouldn't leave unclosed action markers — a sign that
			// something wasn't substituted/was swallowed.
			if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
				t.Errorf("render %s still has `{{`/`}}` - action was not performed:\n%s", name, out)
			}
		})
	}
}

// nodeExporterRenderVars is the render root for content assertions (Option
// B, templating.md §3.2): {vars, input, self, role, essence}. vars are
// destiny locals from vars.yml (bin_dir/bin_path), read by the template as
// `.vars.<file_var>` DIRECTLY; input is the pass's operator input
// (user/group/listen/textfile_dir + daemon prod params), read by the
// template as `.input.<name>`. Shared by both tests (ParseAndRender uses the
// same set).
var nodeExporterRenderVars = map[string]any{
	// vars is a superset of file-vars and values that destiny STEPS lift into
	// params.vars: the main unit (service.yml, Option B) doesn't lift
	// params.vars at all — it reads file-var `.vars.bin_path` directly;
	// collector steps (collectors.yml, OUT of Option B's scope) still pass
	// user/textfile_dir/bin_dir through their own params.vars, so
	// collector-.tmpl read `.vars.user`/`.vars.textfile_dir`. The synthetic
	// root supplies both layers.
	"vars": map[string]any{
		"bin_dir":      "/usr/local/bin",
		"bin_path":     "/usr/local/bin/node_exporter",
		"user":         "node_exporter",
		"textfile_dir": "/var/lib/node_exporter",
	},
	"input": map[string]any{
		"user":         "node_exporter",
		"group":        "node_exporter",
		"listen":       "127.0.0.1:9100",
		"textfile_dir": "/var/lib/node_exporter",
		// Daemon prod params (node_exporter.service.tmpl §comment): defaults
		// under which optional flags are omitted, while log.*/
		// web.telemetry-path are always substituted.
		"gomaxprocs":              int64(0),
		"disabled_collectors":     []any{},
		"enabled_collectors":      []any{},
		"collector_options":       map[string]any{},
		"log_level":               "info",
		"log_format":              "logfmt",
		"web_telemetry_path":      "/metrics",
		"fs_mount_points_exclude": "",
		"netdev_device_exclude":   "",
	},
	"self": map[string]any{
		"os":      map[string]any{"family": "debian"},
		"network": map[string]any{"primary_ip": "10.0.0.1"},
	},
	"role":    "",
	"essence": map[string]any{},
}

// renderNodeExporterTmpl renders one node-exporter destiny .tmpl through the
// same shared/tmpl.Engine as Soul (strict, missingkey=error), with the vars
// context above. A Parse/Execute failure means a test failure (broken
// template).
func renderNodeExporterTmpl(t *testing.T, name string) string {
	t.Helper()
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(nodeExporterTemplatesDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	out, err := engine.Render(string(body), nodeExporterRenderVars)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return out
}

// TestNodeExporterTemplates_HardeningContent guards the CONTENT of rendered
// systemd units: checks that directives critical for functionality and
// security are actually present in the output. ParseAndRender above only
// catches "template doesn't parse" — a regression that removed a hardening
// line (e.g. RestrictAddressFamilies, without which node_exporter won't
// listen on TCP, or User=, leaving the daemon running as root) would pass
// silently. This test renders each unit and asserts a set of required
// substrings; removing any of them fails the test.
//
// The substrings aren't a full file diff (too brittle) but point invariants:
// what the unit needs to be functionally/securely correct. The set's source
// is the spec's §collectors and the §3 prod convention (hardening),
// documented in the .tmpl comments.
func TestNodeExporterTemplates_HardeningContent(t *testing.T) {
	// Each case is one .tmpl plus the substrings required in its render.
	cases := []struct {
		file string
		want []string
	}{
		{
			// Main daemon: without RestrictAddressFamilies=AF_INET[6]
			// node_exporter won't listen on TCP; User=node_exporter (NOT root);
			// NoNewPrivileges; listen-address and textfile.directory flags in
			// ExecStart.
			file: "node_exporter.service.tmpl",
			want: []string{
				"RestrictAddressFamilies=AF_INET AF_INET6",
				"User=node_exporter",
				"NoNewPrivileges=yes",
				"--web.listen-address=",
				"--collector.textfile.directory=",
				// web.telemetry-path always renders (default is non-empty /metrics).
				"--web.telemetry-path=/metrics",
			},
		},
		{
			// smartmon .service: has a Condition (won't start on a VM), runs
			// privileged (User=root, PrivateDevices=no, non-empty
			// DeviceAllow/CapabilityBoundingSet, PrivateNetwork=yes,
			// ReadWritePaths with the textfile directory).
			file: "node-exporter-smartmon.service.tmpl",
			want: []string{
				"ConditionVirtualization=no",
				"User=root",
				"PrivateDevices=no",
				"DeviceAllow=block-* r",
				"CapabilityBoundingSet=CAP_SYS_RAWIO CAP_SYS_ADMIN",
				"PrivateNetwork=yes",
				"ReadWritePaths=/var/lib/node_exporter",
			},
		},
		{
			// smartmon .timer carries the same Condition as .service.
			file: "node-exporter-smartmon.timer.tmpl",
			want: []string{"ConditionVirtualization=no"},
		},
		{
			// nvme .service: Condition on /dev/nvme*, privileged sandbox.
			file: "node-exporter-nvme.service.tmpl",
			want: []string{
				"ConditionPathExistsGlob=/dev/nvme[0-9]*",
				"User=root",
				"PrivateDevices=no",
				"DeviceAllow=char-nvme rw",
				"CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SYS_RAWIO",
				"PrivateNetwork=yes",
				"ReadWritePaths=/var/lib/node_exporter",
			},
		},
		{
			file: "node-exporter-nvme.timer.tmpl",
			want: []string{"ConditionPathExistsGlob=/dev/nvme[0-9]*"},
		},
		{
			// ipmi .service: Condition on /dev/ipmi0, privileged sandbox.
			// ProtectKernelModules=no — the ipmi driver loads lazily (spec).
			file: "node-exporter-ipmitool.service.tmpl",
			want: []string{
				"ConditionPathExists=/dev/ipmi0",
				"User=root",
				"PrivateDevices=no",
				"DeviceAllow=char-ipmidev rw",
				"CapabilityBoundingSet=CAP_SYS_RAWIO CAP_DAC_OVERRIDE",
				"PrivateNetwork=yes",
				"ReadWritePaths=/var/lib/node_exporter",
				"ProtectKernelModules=no",
			},
		},
		{
			file: "node-exporter-ipmitool.timer.tmpl",
			want: []string{"ConditionPathExists=/dev/ipmi0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			out := renderNodeExporterTmpl(t, tc.file)
			for _, sub := range tc.want {
				if !strings.Contains(out, sub) {
					t.Errorf("render %s missing required directive %q\n--- render ---\n%s", tc.file, sub, out)
				}
			}
		})
	}
}

// renderNodeExporterServiceWithVars renders the main
// node_exporter.service.tmpl with part of .input overridden (layered on the
// default set): needed by content tests that vary
// collector_options/web_telemetry_path. Option B: these fields are operator
// input, the template reads them as `.input.<name>`, so overrides merge into
// the input submap. Base set is nodeExporterRenderVars.
func renderNodeExporterServiceWithVars(t *testing.T, overrides map[string]any) string {
	t.Helper()
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	base := nodeExporterRenderVars["input"].(map[string]any)
	input := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		input[k] = v
	}
	for k, v := range overrides {
		input[k] = v
	}
	root := map[string]any{
		"vars":    nodeExporterRenderVars["vars"],
		"input":   input,
		"self":    nodeExporterRenderVars["self"],
		"role":    nodeExporterRenderVars["role"],
		"essence": nodeExporterRenderVars["essence"],
	}
	body, err := os.ReadFile(filepath.Join(nodeExporterTemplatesDir, "node_exporter.service.tmpl"))
	if err != nil {
		t.Fatalf("read node_exporter.service.tmpl: %v", err)
	}
	out, err := engine.Render(string(body), root)
	if err != nil {
		t.Fatalf("render node_exporter.service.tmpl: %v", err)
	}
	return out
}

// TestNodeExporterService_CollectorOptionsDeterministic — ★spec double-check:
// the nested range over map<string,map<string,string>> collector_options in
// ExecStart is DETERMINISTIC (Go text/template's range over a map visits
// keys in SORTED order — an invariant users.acl.tmpl/redis also rely on).
// The test seeds collectors and options in a deliberately non-lexicographic
// insertion order and requires the render to emit flags strictly ascending
// by collector name, and within a collector, ascending by option key. The
// run repeats to rule out a coincidental match with one hash-table
// iteration order.
func TestNodeExporterService_CollectorOptionsDeterministic(t *testing.T) {
	// Insertion order is deliberately scrambled (systemd before cpu; inside
	// systemd, unit-allowlist before enable-restart). Expected render is sorted.
	collectorOptions := map[string]any{
		"systemd": map[string]any{
			"unit-include":            ".+\\.service",
			"enable-restarts-metrics": "true",
		},
		"cpu": map[string]any{
			"info": "true",
		},
	}

	// Expected flag order: collectors cpu<systemd, and within systemd, keys
	// enable-restarts-metrics<unit-include (lexicographic).
	wantOrder := []string{
		"--collector.cpu.info=true",
		"--collector.systemd.enable-restarts-metrics=true",
		"--collector.systemd.unit-include=.+\\.service",
	}

	// Multiple runs: ranging over different map instances shouldn't change
	// the order (if it depended on hash-table iteration, it would diverge).
	for run := 0; run < 8; run++ {
		out := renderNodeExporterServiceWithVars(t, map[string]any{"collector_options": collectorOptions})

		prev := -1
		for _, flag := range wantOrder {
			idx := strings.Index(out, flag)
			if idx < 0 {
				t.Fatalf("run %d: flag %q missing in ExecStart\n--- render ---\n%s", run, flag, out)
			}
			if idx <= prev {
				t.Fatalf("run %d: flag %q is not in ascending order (idx=%d, prev=%d) - range order is NOT deterministic\n--- render ---\n%s", run, flag, idx, prev, out)
			}
			prev = idx
		}

		// collector_options flags sit AFTER the enabled/disabled collectors
		// and BEFORE gomaxprocs (block position in ExecStart, spec §1c).
		// gomaxprocs=0 means no flag, so we check relative to --log.format
		// (the preceding unconditional flag) and gomaxprocs's absence.
		logIdx := strings.Index(out, "--log.format=")
		firstOptIdx := strings.Index(out, "--collector.cpu.info=")
		if firstOptIdx < logIdx {
			t.Fatalf("run %d: collector_options rendered before --log.format - block position violated\n--- render ---\n%s", run, out)
		}
	}
}

// TestNodeExporterService_ExecStartBinPath — ★regression guard for "removed
// the last vars passthrough": ExecStart takes the binary path from the
// file-var `.vars.bin_path` (vars.yml), not the retired `.vars.exporter_bin`
// passthrough. The path must stay the same (/usr/local/bin/node_exporter) —
// ExecStart's behavior didn't change, only the delivery channel (file-var
// directly).
func TestNodeExporterService_ExecStartBinPath(t *testing.T) {
	out := renderNodeExporterTmpl(t, "node_exporter.service.tmpl")
	if !strings.Contains(out, "ExecStart=/usr/local/bin/node_exporter ") {
		t.Errorf("ExecStart does not take the binary path from .vars.bin_path:\n%s", out)
	}
}

// TestNodeExporterService_TelemetryPathOverride — web_telemetry_path != /metrics
// renders in ExecStart as --web.telemetry-path=<override> (unconditional flag).
func TestNodeExporterService_TelemetryPathOverride(t *testing.T) {
	out := renderNodeExporterServiceWithVars(t, map[string]any{"web_telemetry_path": "/node/metrics"})
	if !strings.Contains(out, "--web.telemetry-path=/node/metrics") {
		t.Errorf("web_telemetry_path override not rendered\n--- render ---\n%s", out)
	}
	if strings.Contains(out, "--web.telemetry-path=/metrics") {
		t.Errorf("default --web.telemetry-path=/metrics remained despite override\n--- render ---\n%s", out)
	}
}
