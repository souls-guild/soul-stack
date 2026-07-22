package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// redisTemplatesDir is the redis destiny's .tmpl directory, relative to this
// package. Lives here for the same reason as destiny_node_exporter_tmpl_test.go:
// this package owns the text/template engine integration (shared/tmpl) and the
// .tmpl render path. The destiny's L0 trial asserts the PLAN and runs only the
// CEL phase, never text/template, so a users.acl line-order regression stays
// invisible at L0.
const redisTemplatesDir = "../../../examples/destiny/redis/templates"

// renderRedisTmpl renders one redis destiny .tmpl through the same
// shared/tmpl.Engine as Soul (strict, missingkey=error). A Parse/Execute
// failure fails the test.
func renderRedisTmpl(t *testing.T, name string, root map[string]any) string {
	t.Helper()
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(redisTemplatesDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	out, err := engine.Render(string(body), root)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return out
}

// TestRedisUsersAcl_DeterministicOrder is a direct regress-guard for a
// tiling-critical users.acl nondeterminism bug (QA 2026-06-22). Root cause: ACL
// rendered from a user LIST, and Go text/template range over a list preserves
// source order — which for a collection built by CEL `.map(...)` over a map
// inherits Go's nondeterministic map iteration. Result: users.acl lines differ
// between runs → false change in core.file.rendered → spurious Redis restart
// (cascading on a rolling-restart soul-set).
//
// Fix: users.acl.tmpl ranges over a MAP (Go sorts keys). This test renders the
// REAL template with names that violate insertion order (zeta/alpha/mike) and
// proves (a) lines come out in sorted-name order, (b) output is stable across N
// runs. Reverting to a list render fails both assertions.
//
// default_admin redesign (2026-06-30): the template no longer renders a literal
// default line from .vars.password (requirepass removed) — it prints `user
// default off` when the operator did not declare `default` in .vars.users (safe
// default: with an aclfile set but no default line, Redis would otherwise fall
// back to built-in `default on nopass`). .vars.password is no longer passed.
// Here .vars.users has no `default` key, so we expect `user default off` as the
// first line, followed by the sorted map range.
func TestRedisUsersAcl_DeterministicOrder(t *testing.T) {
	// vars.users is a MAP name→{perms,state,password}, as assembled by scenario
	// (merge(list(map)) over .map(...)). Names are deliberately out of insertion
	// and Go-map-iteration order. The system default_admin arrives through the
	// same map (ranged like everyone else) — modeled here alongside
	// operator-extra zeta/alpha/mike.
	root := map[string]any{
		"vars": map[string]any{
			"users": map[string]any{
				"zeta":          map[string]any{"perms": "~* +@all", "state": "on", "password": "zeta-pass"},
				"alpha":         map[string]any{"perms": "~app:* +@read", "state": "on", "password": "alpha-pass"},
				"mike":          map[string]any{"perms": "~m:* +@write", "state": "off", "password": "mike-pass"},
				"default_admin": map[string]any{"perms": "~* &* +@all", "state": "on", "password": "admin-pass"},
			},
		},
	}

	const runs = 16
	var first string
	for i := 0; i < runs; i++ {
		out := renderRedisTmpl(t, "users.acl.tmpl", root)

		// default off comes first (literal before the range — default isn't in
		// the map), then map users in sorted order: alpha < default_admin < mike < zeta.
		lines := nonEmptyLines(out)
		if len(lines) != 5 {
			t.Fatalf("expected 5 user lines (default off + 4 map), got %d:\n%s", len(lines), out)
		}
		gotNames := []string{userName(lines[0]), userName(lines[1]), userName(lines[2]), userName(lines[3]), userName(lines[4])}
		wantNames := []string{"default", "alpha", "default_admin", "mike", "zeta"}
		for j := range wantNames {
			if gotNames[j] != wantNames[j] {
				t.Fatalf("user order = %v, want %v (default off-literal + range over map -> key sort)\n%s", gotNames, wantNames, out)
			}
		}

		// Stability across runs (determinism): every run must be identical.
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("run %d produced a DIFFERENT output than run 0 - nondeterminism:\n--- run 0 ---\n%s\n--- run %d ---\n%s", i, first, i, out)
		}
	}

	// Password is written as a HASH (#<sha256>) — plaintext never hits the file.
	if strings.Contains(first, "zeta-pass") || strings.Contains(first, "alpha-pass") ||
		strings.Contains(first, "mike-pass") || strings.Contains(first, "admin-pass") {
		t.Fatalf("plaintext password leaked into users.acl:\n%s", first)
	}
	if !strings.Contains(first, "#") {
		t.Fatalf("users.acl has no password hash (#<sha256>):\n%s", first)
	}
	// Built-in default is OFF (`user default off`): redis no longer uses it
	// (default_admin took the role), but without an explicit off line, an
	// aclfile with no default entry would fall back to built-in `on nopass` —
	// open passwordless access under protected-mode.
	if !strings.Contains(first, "user default off") {
		t.Fatalf("the built-in default should render `user default off` (it is not in .vars.users):\n%s", first)
	}
	// default_admin is the full-access system user (~* &* +@all), carries #<hash>.
	if !strings.Contains(first, "user default_admin on #") || !strings.Contains(first, "~* &* +@all") {
		t.Fatalf("default_admin should carry #<hash> and full access ~* &* +@all:\n%s", first)
	}
}

// TestRedisConf_ClusterAnnounceIP_PerHost is a direct regress-guard for a
// tiling bug with host-invariant cluster-announce-ip (QA 2026-06-22). Root
// cause: announce-ip was threaded through apply.input.config, which resolves
// host-INVARIANTLY (first host by SID, resolveApplyInput targeted[0]), so every
// node announced the first node's IP — cluster-bus broken behind NAT/in cloud.
//
// Fix: cluster-announce-ip moved out of apply.input.config and rendered in
// redis.conf.tmpl from `{{ .self.network.primary_ip }}` (render_context.self is
// PER-HOST, symmetric with bind), gated on `cluster-enabled`. This test renders
// the REAL redis.conf.tmpl with a DIFFERENT .self per host and proves each gets
// its OWN primary_ip: host A → IP A, host B → IP B. Reverting announce-ip to the
// config map fails the test (config map is host-invariant, both hosts would get
// one IP).
func TestRedisConf_ClusterAnnounceIP_PerHost(t *testing.T) {
	// HOST-INVARIANT cluster-config (what apply.input.config delivers: same for
	// all hosts). announce-ip is NOT here — it comes per-host from .self.
	clusterConfig := map[string]any{
		"cluster-enabled":      "yes",
		"cluster-config-file":  "nodes.conf",
		"cluster-node-timeout": "5000",
		"maxmemory":            "256mb",
	}

	type hostCase struct {
		sid string
		ip  string
	}
	hosts := []hostCase{
		{sid: "node-a.example.com", ip: "10.0.0.1"},
		{sid: "node-b.example.com", ip: "10.0.0.2"},
	}

	for _, h := range hosts {
		root := map[string]any{
			// render_context.self is PER-HOST: under real dispatch each host renders
			// its .tmpl with its own self; here we model that by passing host h's self.
			"self": map[string]any{
				"network": map[string]any{"primary_ip": h.ip},
			},
			"vars": map[string]any{
				"password": "s3cr3t-redis-pass",
				"config":   clusterConfig,
				"data_dir": "/var/lib/redis",
				"conf_dir": "/etc/redis",
				"port":     6379,
				"run_dir":  "/var/run/redis",
				"log_dir":  "/var/log/redis",
			},
		}
		out := renderRedisTmpl(t, "redis.conf.tmpl", root)

		wantAnnounce := "cluster-announce-ip " + h.ip
		if !strings.Contains(out, wantAnnounce) {
			t.Fatalf("host %s: missing its own announce-ip line %q:\n%s", h.sid, wantAnnounce, out)
		}
		// This host's announce line must not carry another host's IP (the
		// host-invariant bug would show up exactly this way — one fixed IP for all).
		for _, other := range hosts {
			if other.ip == h.ip {
				continue
			}
			if strings.Contains(out, "cluster-announce-ip "+other.ip) {
				t.Fatalf("host %s announces a FOREIGN IP %s - announce-ip is not host-invariant (bug is back):\n%s", h.sid, other.ip, out)
			}
		}
		// bind uses the same per-host .self.network.primary_ip — symmetry preserved.
		if !strings.Contains(out, "bind "+h.ip+" 127.0.0.1") {
			t.Fatalf("host %s: bind not on its own primary_ip %s:\n%s", h.sid, h.ip, out)
		}
	}
}

// TestRedisConf_ClusterAnnounceIP_StandaloneOmitsLine proves that outside
// cluster mode (config without cluster-enabled), redis.conf has no
// cluster-announce-ip line — the `{{ if (index .vars.config "cluster-enabled") }}`
// gate suppresses it, i.e. the per-host announce-ip fix didn't leak a cluster
// directive into standalone renders.
func TestRedisConf_ClusterAnnounceIP_StandaloneOmitsLine(t *testing.T) {
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.7"}},
		"vars": map[string]any{
			"password": "s3cr3t-redis-pass",
			"data_dir": "/var/lib/redis",
			"conf_dir": "/etc/redis",
			"port":     6379,
			"run_dir":  "/var/run/redis",
			"log_dir":  "/var/log/redis",
			"config": map[string]any{
				"maxmemory":  "256mb",
				"appendonly": "no",
				"save":       "900 1 300 10 60 10000",
			},
		},
	}
	out := renderRedisTmpl(t, "redis.conf.tmpl", root)
	if strings.Contains(out, "cluster-announce-ip") {
		t.Fatalf("standalone (without cluster-enabled): cluster-announce-ip should not be present:\n%s", out)
	}
	// bind still renders from .self (per-host, mode-agnostic).
	if !strings.Contains(out, "bind 10.0.0.7 127.0.0.1") {
		t.Fatalf("standalone: bind not on primary_ip:\n%s", out)
	}
}

// TestRedisUsersAcl_EmptyMapKeepsDefault proves an empty users map still
// produces exactly the default line (not an empty file) — default renders from
// vars.password outside the map. An empty aclfile would mean built-in default
// with no password, breaking external connections (cluster-init/REPLICAOF)
// under protected-mode; this guard catches a regression to an empty file.
func TestRedisUsersAcl_EmptyMapKeepsDefault(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{"password": "main-requirepass", "users": map[string]any{}},
	}
	out := renderRedisTmpl(t, "users.acl.tmpl", root)
	lines := nonEmptyLines(out)
	if len(lines) != 1 {
		t.Fatalf("empty users -> expected 1 line (default), got %d:\n%s", len(lines), out)
	}
	if userName(lines[0]) != "default" {
		t.Fatalf("the only line should be the default user, got %q", lines[0])
	}
	if strings.Contains(out, "main-requirepass") {
		t.Fatalf("plaintext master secret leaked into the default line:\n%s", out)
	}
}

// TestSentinelConf_AnnounceIP_PerHost is a direct regress-guard for a tiling
// bug with host-invariant sentinel announce-ip (mirrors cluster-announce-ip).
// Root cause of the potential bug: threading announce-ip through apply.input
// (host-INVARIANT resolve, first host by SID) would make every sentinel
// announce the first node's IP — gossip broken behind NAT/in cloud.
//
// Fix: `sentinel announce-ip` renders in sentinel.conf.tmpl from
// `{{ .self.network.primary_ip }}` (render_context.self is PER-HOST, symmetric
// with bind/cluster-announce-ip). This test renders the REAL sentinel.conf.tmpl
// with a DIFFERENT .self per host: each must announce its OWN primary_ip.
// monitor.ip (master), by contrast, is HOST-INVARIANT — same for both.
func TestSentinelConf_AnnounceIP_PerHost(t *testing.T) {
	// HOST-INVARIANT monitor vars (same for all hosts, as from apply.input).
	monitorVars := map[string]any{
		"master_name":     "mymaster",
		"master_ip":       "10.0.0.1", // master address (one per cluster)
		"master_port":     "6379",
		"quorum":          "2",
		"auth_user":       "",
		"auth_pass":       "",
		"data_dir":        "/var/lib/redis",
		"conf_dir":        "/etc/redis",
		"port":            26379,
		"run_dir":         "/var/run/redis",
		"log_dir":         "/var/log/redis",
		"sentinel_config": map[string]any{},
	}

	type hostCase struct {
		sid string
		ip  string
	}
	hosts := []hostCase{
		{sid: "node-a.example.com", ip: "10.0.0.5"},
		{sid: "node-b.example.com", ip: "10.0.0.6"},
	}

	for _, h := range hosts {
		vars := map[string]any{}
		for k, v := range monitorVars {
			vars[k] = v
		}
		root := map[string]any{
			"self": map[string]any{"network": map[string]any{"primary_ip": h.ip}},
			"vars": vars,
		}
		out := renderRedisTmpl(t, "sentinel.conf.tmpl", root)

		// announce-ip is this host's OWN primary_ip.
		if !strings.Contains(out, "sentinel announce-ip "+h.ip) {
			t.Fatalf("host %s: missing its own announce-ip line %q:\n%s", h.sid, h.ip, out)
		}
		for _, other := range hosts {
			if other.ip != h.ip && strings.Contains(out, "sentinel announce-ip "+other.ip) {
				t.Fatalf("host %s announces a FOREIGN IP %s - announce-ip is not host-invariant (bug):\n%s", h.sid, other.ip, out)
			}
		}
		// monitor.ip (master) is HOST-INVARIANT: same for both hosts.
		if !strings.Contains(out, "sentinel monitor mymaster 10.0.0.1 6379 2") {
			t.Fatalf("host %s: missing sentinel monitor master 10.0.0.1:\n%s", h.sid, out)
		}
	}
}

// TestSentinelUnit_ConfDirInReadWritePaths is a direct regress-guard for
// "redis-sentinel fails to start under systemd hardening" (LIVE 2026-06-28).
// Root cause: redis-sentinel REWRITES its own sentinel.conf at runtime
// (persisting topology — master/replicas/epoch/known-sentinels), but the unit
// had ProtectSystem=strict + ReadWritePaths WITHOUT conf_dir → /etc read-only →
// sentinel failed on start ("sentinel.conf is not writable: Read-only file
// system", exit 1, restart loop). Fix: conf_dir added to ReadWritePaths in
// redis-sentinel.service.tmpl. This test renders the REAL unit and proves the
// ReadWritePaths line CONTAINS conf_dir (the actual value, not a hardcoded
// /etc/redis — checked against a non-standard conf_dir). Reverting the template
// to ReadWritePaths without conf_dir fails the test.
func TestSentinelUnit_ConfDirInReadWritePaths(t *testing.T) {
	const confDir = "/opt/redis-conf" // non-standard conf_dir — must land in RW verbatim
	root := map[string]any{
		"vars": map[string]any{
			"sentinel_bin":  "/usr/bin",
			"cli_bin":       "/usr/bin",
			"conf_dir":      confDir,
			"sentinel_port": 26379,
			"redis_user":    "redis",
			"redis_group":   "redis",
			"data_dir":      "/var/lib/redis",
			"log_dir":       "/var/log/redis",
			"run_dir":       "/var/run/redis",
		},
	}
	out := renderRedisTmpl(t, "redis-sentinel.service.tmpl", root)

	rwLine := readWritePathsLine(t, out)
	if !strings.Contains(rwLine, confDir) {
		t.Fatalf("ReadWritePaths of the sentinel unit does not contain conf_dir %q (sentinel will not be able to rewrite sentinel.conf):\n%s", confDir, rwLine)
	}
	// Remaining required write dirs are still present (didn't crowd each other out).
	for _, want := range []string{"/var/lib/redis", "/var/log/redis", "/var/run/redis"} {
		if !strings.Contains(rwLine, want) {
			t.Fatalf("ReadWritePaths of the sentinel unit does not contain %q:\n%s", want, rwLine)
		}
	}
}

// TestRedisServerHardening_ConfDirInReadWritePaths is a direct regress-guard
// of the same class for redis-server (audit of the sentinel defect,
// 2026-06-28). The update_config operational scenario runs CONFIG REWRITE
// (community.redis.config rewrite:true) — redis-server rewrites redis.conf to
// persist applied directives. The hardening drop-in had ProtectSystem=strict +
// ReadWritePaths WITHOUT conf_dir, so the first directive that actually
// changed would hit CONFIG REWRITE against a read-only /etc (same bug class as
// sentinel, but surfacing on an operational run, not create). Fix: conf_dir
// added to ReadWritePaths in hardening.conf.tmpl (wired via a var in
// tasks/server.yml). This test renders the REAL drop-in and proves conf_dir is
// present in ReadWritePaths. Reverting the template without conf_dir fails the
// test.
func TestRedisServerHardening_ConfDirInReadWritePaths(t *testing.T) {
	const confDir = "/opt/redis-conf" // non-standard conf_dir — must land in RW verbatim
	root := map[string]any{
		"vars": map[string]any{
			"data_dir": "/var/lib/redis",
			"run_dir":  "/var/run/redis",
			"log_dir":  "/var/log/redis",
			"conf_dir": confDir,
		},
	}
	out := renderRedisTmpl(t, "hardening.conf.tmpl", root)

	rwLine := readWritePathsLine(t, out)
	if !strings.Contains(rwLine, confDir) {
		t.Fatalf("ReadWritePaths of the hardening drop-in does not contain conf_dir %q (CONFIG REWRITE on update_config will hit read-only /etc):\n%s", confDir, rwLine)
	}
	for _, want := range []string{"/var/lib/redis", "/var/run/redis", "/var/log/redis"} {
		if !strings.Contains(rwLine, want) {
			t.Fatalf("ReadWritePaths of the hardening drop-in does not contain %q:\n%s", want, rwLine)
		}
	}
}

// TestSentinelConf_DirectivesDeterministicOrder proves sentinel_config startup
// directives range over a MAP in SORTED order (determinism — no false
// change/restart), and that an empty auth_pass omits the auth-pass line (gate).
// Direct guard on directive determinism (mirrors the users.acl order guard).
// Masking is NOT checked here (that's the Soul/Keeper output layer, not the
// render phase) — auth-pass is written as-is (needed for sentinel's AUTH to master).
func TestSentinelConf_DirectivesDeterministicOrder(t *testing.T) {
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.5"}},
		"vars": map[string]any{
			"master_name": "mymaster",
			"master_ip":   "10.0.0.1",
			"master_port": "6379",
			"quorum":      "2",
			"auth_user":   "sentinel",
			"auth_pass":   "",
			"data_dir":    "/var/lib/redis",
			"conf_dir":    "/etc/redis",
			"port":        26379,
			"run_dir":     "/var/run/redis",
			"log_dir":     "/var/log/redis",
			// Deliberately unsorted: ranging over the MAP must sort the keys.
			"sentinel_config": map[string]any{
				"sentinel down-after-milliseconds mymaster": "12000",
				"loglevel":                           "notice",
				"sentinel failover-timeout mymaster": "70000",
			},
		},
	}
	const runs = 12
	var first string
	for i := 0; i < runs; i++ {
		out := renderRedisTmpl(t, "sentinel.conf.tmpl", root)
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("run %d produced a DIFFERENT output (directive nondeterminism):\n--- 0 ---\n%s\n--- %d ---\n%s", i, first, i, out)
		}
	}
	// loglevel < sentinel down... < sentinel failover... — sorted-key order.
	iLog := strings.Index(first, "loglevel notice")
	iDown := strings.Index(first, "down-after-milliseconds mymaster 12000")
	iFail := strings.Index(first, "failover-timeout mymaster 70000")
	if iLog < 0 || iDown < 0 || iFail < 0 {
		t.Fatalf("expected directives missing:\n%s", first)
	}
	if !(iLog < iDown && iDown < iFail) {
		t.Fatalf("directives not in sorted order (loglevel<down<failover):\n%s", first)
	}
	// Empty auth-pass → no auth-pass line.
	if strings.Contains(first, "sentinel auth-pass") {
		t.Fatalf("empty auth_pass: there should be no auth-pass line:\n%s", first)
	}
}

// TestSentinelConf_AuthRendered is a positive guard on the auth block: with
// non-empty auth_user/auth_pass, both `sentinel auth-user`/`sentinel
// auth-pass` directives render with the monitored master's name (symmetric
// with the empty case in DirectivesDeterministicOrder, where both lines are
// absent). Catches a regression of the `{{- if .vars.auth_X }}` gate or a lost
// master_name in the auth lines. Sentinel's AUTH to master is required under
// requirepass — without these lines, failover breaks silently.
func TestSentinelConf_AuthRendered(t *testing.T) {
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.5"}},
		"vars": map[string]any{
			"master_name":     "mymaster",
			"master_ip":       "10.0.0.1",
			"master_port":     "6379",
			"quorum":          "2",
			"auth_user":       "sentinel-user",
			"auth_pass":       "s3cr3t-sentinel-pass",
			"data_dir":        "/var/lib/redis",
			"conf_dir":        "/etc/redis",
			"port":            26379,
			"run_dir":         "/var/run/redis",
			"log_dir":         "/var/log/redis",
			"sentinel_config": map[string]any{},
		},
	}
	out := renderRedisTmpl(t, "sentinel.conf.tmpl", root)

	// auth-user carries the monitored master's name.
	if !strings.Contains(out, "sentinel auth-user mymaster sentinel-user") {
		t.Fatalf("missing auth-user line with master_name:\n%s", out)
	}
	// auth-pass carries the monitored master's name.
	if !strings.Contains(out, "sentinel auth-pass mymaster s3cr3t-sentinel-pass") {
		t.Fatalf("missing auth-pass line with master_name:\n%s", out)
	}
}

// TestRedisConf_MasterAuthPersisted is a direct regress-guard for a live
// defect: "sentinel restart fails: replica master_link_status:DOWN, masterauth
// empty" (2026-06-30). Root cause: after the default_admin redesign
// (requirepass removed → ACL default_admin), replica→master replication runs
// under an ACL user (masterauth+masteruser). The community.redis.replica
// plugin sets them via CONFIG SET at runtime, but CONFIG SET does NOT persist
// to redis.conf without CONFIG REWRITE — so on restart (sentinel restart wave
// 2) redis-server comes up WITHOUT masterauth → replica fails to authenticate
// to master → link DOWN → replica-synced timeout. Fix: the sentinel
// deploy-body writes masteruser/masterauth as ORDINARY directives in the
// apply.input config map → redis.conf.tmpl prints them via the config range
// (like any directive) → STATICALLY in the file → survives restart. The
// directives are harmless on the current master (Redis only applies them once
// a node becomes a replica — failover-safe: an old master turned replica
// already carries the credentials).
//
// This test renders the REAL redis.conf.tmpl with masteruser/masterauth config
// KEYS and proves: (a) both directives are present with the given values; (b)
// the password is NOT hashed (masterauth is plaintext in redis.conf, like
// auth_pass in sentinel.conf — AUTH needs the raw secret, the file is
// protected by mode 0640). Reverting scenario to render without masterauth in
// config fails the paired L0 guard (sentinel-create-1master-2replica).
func TestRedisConf_MasterAuthPersisted(t *testing.T) {
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.2"}},
		"vars": map[string]any{
			// masteruser/masterauth are ORDINARY config directives (the sentinel
			// deploy-body places them here via vault()-in-cell). The template ranges
			// over config and prints them as-is.
			"config": map[string]any{
				"maxmemory":  "256mb",
				"masteruser": "default_admin",
				"masterauth": "default-admin-secret-pass",
			},
			"data_dir": "/var/lib/redis",
			"conf_dir": "/etc/redis",
			"port":     6379,
			"run_dir":  "/var/run/redis",
			"log_dir":  "/var/log/redis",
		},
	}
	out := renderRedisTmpl(t, "redis.conf.tmpl", root)

	// (a) masteruser default_admin + masterauth <pass> are present as DIRECTIVES
	// (line starts with the directive name — not a substring match inside a
	// template comment, where the word masterauth also appears).
	if !hasDirectiveLine(out, "masteruser default_admin") {
		t.Fatalf("missing masteruser default_admin directive (replica will not pick the ACL user on the master after a restart):\n%s", out)
	}
	if !hasDirectiveLine(out, "masterauth default-admin-secret-pass") {
		t.Fatalf("missing masterauth <pass> directive (replica will not authenticate to the master on restart -> link DOWN):\n%s", out)
	}
}

// The /etc/sysctl.d/30-redis.conf drop-in is no longer rendered by a template:
// host-tuning extras moved to core.sysctl.applied (the module builds a
// deterministic drop-in from a sorted-keys map, ADR-015 amend).
// redis.sysctl.conf.tmpl was removed; sorted-determinism is now covered by that
// module's unit test (soul/internal/coremod/sysctl/applied_test.go).

// TestRedisConf_Loadmodule_NoTrailingSpace is a direct guard on loadmodule
// directive cleanliness (Redis modules, Redis < 8). Root cause of the
// potential bug: if loadmodule were a config-map KEY, the template range
// `{{$key}} {{$value}}` with an empty value would print `loadmodule /path.so `
// — a trailing space. Any instability in that trailing space → false
// core.file.rendered change → spurious Redis restart.
//
// Fix: loadmodule moved to its own template section driven by the
// .vars.loadmodules list (`loadmodule {{ . }}` — no trailing value). This test
// renders the REAL redis.conf.tmpl and proves: (a) loadmodule lines have no
// trailing space; (b) line order matches list order (deterministic across
// runs); (c) paths are rendered in full.
func TestRedisConf_Loadmodule_NoTrailingSpace(t *testing.T) {
	loadmodules := []any{
		"/var/lib/redis/modules/redisearch.so",
		"/var/lib/redis/modules/rejson.so",
	}
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.1"}},
		"vars": map[string]any{
			"password":    "s3cr3t-redis-pass",
			"config":      map[string]any{"maxmemory": "256mb"},
			"loadmodules": loadmodules,
			"data_dir":    "/var/lib/redis",
			"conf_dir":    "/etc/redis",
			"port":        6379,
			"run_dir":     "/var/run/redis",
			"log_dir":     "/var/log/redis",
		},
	}

	const runs = 12
	var first string
	for i := 0; i < runs; i++ {
		out := renderRedisTmpl(t, "redis.conf.tmpl", root)
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("run %d produced a DIFFERENT output (loadmodule nondeterminism):\n--- 0 ---\n%s\n--- %d ---\n%s", i, first, i, out)
		}
	}

	var modLines []string
	for _, ln := range strings.Split(first, "\n") {
		if strings.HasPrefix(ln, "loadmodule") {
			modLines = append(modLines, ln)
		}
	}
	if len(modLines) != 2 {
		t.Fatalf("expected 2 loadmodule lines, got %d:\n%s", len(modLines), first)
	}
	// (a) No trailing space — the line ends exactly at .so.
	for _, ln := range modLines {
		if ln != strings.TrimRight(ln, " ") {
			t.Fatalf("loadmodule line with trailing whitespace: %q", ln)
		}
		if !strings.HasSuffix(ln, ".so") {
			t.Fatalf("loadmodule line does not end with .so: %q", ln)
		}
	}
	// (b) Line order matches list order (list determinism, not map iteration).
	want := []string{
		"loadmodule /var/lib/redis/modules/redisearch.so",
		"loadmodule /var/lib/redis/modules/rejson.so",
	}
	for i := range want {
		if modLines[i] != want[i] {
			t.Fatalf("line %d = %q, want %q (list order)", i, modLines[i], want[i])
		}
	}
}

// TestRedisConf_Loadmodule_EmptyAndAbsent proves the loadmodule section is
// absent from redis.conf in both cases: the loadmodules key set to an empty
// list (Redis 8+: scenario passes []) AND the key missing from .vars entirely
// (`index .vars "loadmodules"` on a missing key returns nil without error in
// strict mode, symmetric with the cluster-enabled gate). Direct guard on the
// version-gate branch (8+ → no loadmodule) and on the back-compat render
// without modules-vars.
func TestRedisConf_Loadmodule_EmptyAndAbsent(t *testing.T) {
	base := func(loadmodules any) map[string]any {
		vars := map[string]any{
			"password": "s3cr3t-redis-pass",
			"config":   map[string]any{"maxmemory": "256mb"},
			"data_dir": "/var/lib/redis",
			"conf_dir": "/etc/redis",
			"port":     6379,
			"run_dir":  "/var/run/redis",
			"log_dir":  "/var/log/redis",
		}
		if loadmodules != nil {
			vars["loadmodules"] = loadmodules
		}
		return map[string]any{
			"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.1"}},
			"vars": vars,
		}
	}

	cases := map[string]any{
		"empty list (Redis 8+ gate)": []any{},
		"absent key (no modules)":    nil,
	}
	for name, lm := range cases {
		out := renderRedisTmpl(t, "redis.conf.tmpl", base(lm))
		if strings.Contains(out, "loadmodule") {
			t.Fatalf("%s: loadmodule directive should not be present:\n%s", name, out)
		}
	}
}

// TestSentinelConf_AclfileSecondFile is a guard on the SECOND aclfile
// (system-ACL-users d2): sentinel.conf must set aclfile =
// ${conf_dir}/sentinel-users.acl (SEPARATE from redis-server's users.acl). The
// path follows conf_dir (directive B). Fails if sentinel lacks its own aclfile
// or hardcodes /etc/redis when conf_dir is overridden.
func TestSentinelConf_AclfileSecondFile(t *testing.T) {
	root := map[string]any{
		"self": map[string]any{"network": map[string]any{"primary_ip": "10.0.0.5"}},
		"vars": map[string]any{
			"master_name":     "master",
			"master_ip":       "10.0.0.1",
			"master_port":     "6379",
			"quorum":          "2",
			"auth_user":       "",
			"auth_pass":       "",
			"data_dir":        "/var/lib/redis",
			"conf_dir":        "/opt/redis-conf", // non-standard conf_dir — aclfile must follow it
			"port":            26379,
			"run_dir":         "/var/run/redis",
			"log_dir":         "/var/log/redis",
			"sentinel_config": map[string]any{},
		},
	}
	out := renderRedisTmpl(t, "sentinel.conf.tmpl", root)
	if !strings.Contains(out, "aclfile /opt/redis-conf/sentinel-users.acl") {
		t.Fatalf("sentinel.conf: missing aclfile under conf_dir (sentinel-users.acl):\n%s", out)
	}
	// redis-server's users.acl must not be mentioned in sentinel.conf (separate file).
	if strings.Contains(out, "/users.acl") {
		t.Fatalf("sentinel.conf references the redis-server users.acl - it should reference its own sentinel-users.acl:\n%s", out)
	}
}

// TestSentinelUsersAcl_DeterministicOrder proves sentinel-users.acl.tmpl (the
// SECOND aclfile, system-ACL-users d2) renders the sentinel daemon's system
// user map in SORTED order (determinism — no false change/restart of the
// sentinel daemon) and hashes the password (sha256, not plaintext). Same
// invariant as users.acl. Includes the default user (for the sentinel daemon
// it lives in the aclfile, unlike redis-server's requirepass).
func TestSentinelUsersAcl_DeterministicOrder(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"users": map[string]any{
				"sentinel":   map[string]any{"perms": "+auth +sentinel|master", "state": "on", "password": "sentinel-pass"},
				"default":    map[string]any{"perms": "allchannels allkeys +@all", "state": "on", "password": "default-pass"},
				"monitoring": map[string]any{"perms": "-@all +info", "state": "on", "password": "mon-pass"},
				"haproxy":    map[string]any{"perms": "-@all +ping", "state": "on", "password": "haproxy-pass"},
			},
		},
	}
	const runs = 12
	var first string
	for i := 0; i < runs; i++ {
		out := renderRedisTmpl(t, "sentinel-users.acl.tmpl", root)
		lines := nonEmptyLines(out)
		if len(lines) != 4 {
			t.Fatalf("expected 4 user lines, got %d:\n%s", len(lines), out)
		}
		// Sorted: default < haproxy < monitoring < sentinel.
		gotNames := []string{userName(lines[0]), userName(lines[1]), userName(lines[2]), userName(lines[3])}
		want := []string{"default", "haproxy", "monitoring", "sentinel"}
		for j := range want {
			if gotNames[j] != want[j] {
				t.Fatalf("line order not sorted: got %v want %v\n%s", gotNames, want, out)
			}
		}
		if i == 0 {
			first = out
		} else if out != first {
			t.Fatalf("run %d produced a DIFFERENT output (nondeterminism):\n%s", i, out)
		}
	}
	// Password is a HASH (#<sha256hex>) — plaintext must not leak.
	if strings.Contains(first, "default-pass") || strings.Contains(first, "sentinel-pass") {
		t.Fatalf("plaintext password in sentinel-users.acl (should be a sha256 hash):\n%s", first)
	}
	if !strings.Contains(first, "#") {
		t.Fatalf("missing sha256 password hash (#<hash>):\n%s", first)
	}
}

// TestSentinelUsersAcl_EmptyMapValid proves an empty map yields a valid
// aclfile. default_admin redesign (2026-06-30): the template renders `user
// default off` when .vars.users has no default key (safe default: with an
// aclfile set but no default line, the sentinel daemon would otherwise fall
// back to built-in `default on nopass` — passwordless access to :26379). An
// empty map has no default a fortiori, so the only line is `user default off`
// (system users only arrive via a non-empty map).
func TestSentinelUsersAcl_EmptyMapValid(t *testing.T) {
	root := map[string]any{"vars": map[string]any{"users": map[string]any{}}}
	out := renderRedisTmpl(t, "sentinel-users.acl.tmpl", root)
	lines := nonEmptyLines(out)
	if len(lines) != 1 || lines[0] != "user default off" {
		t.Fatalf("an empty map should produce exactly `user default off` (safe default), got:\n%s", out)
	}
}

// nonEmptyLines returns the render's non-blank lines (drops blanks left by
// {{- -}} trimming around template comments).
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, strings.TrimSpace(ln))
		}
	}
	return out
}

// readWritePathsLine returns the `ReadWritePaths=...` line from a rendered
// systemd unit/drop-in. Fails the test if absent (ProtectSystem=strict without
// ReadWritePaths means all of / is read-only — a production incident).
func readWritePathsLine(t *testing.T, out string) string {
	t.Helper()
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "ReadWritePaths=") {
			return ln
		}
	}
	t.Fatalf("the render has no ReadWritePaths= line:\n%s", out)
	return ""
}

// hasDirectiveLine reports whether the render has a redis.conf directive LINE
// with the given prefix (starts with it after TrimSpace). Distinguishes a real
// directive (`masterauth <pass>`) from the same word inside a template comment
// (`# … replication masterauth …`), which strings.Contains would match falsely.
func hasDirectiveLine(out, prefix string) bool {
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), prefix) {
			return true
		}
	}
	return false
}

// hasExactLine — reports whether the render has a line that, after TrimSpace, EXACTLY equals want.
// For directives with an empty value (CapabilityBoundingSet=), where a prefix-based
// hasDirectiveLine would miss a regression to a non-empty value.
func hasExactLine(out, want string) bool {
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == want {
			return true
		}
	}
	return false
}

// userName extracts the user name from a `user <name> <state> #<hash> <perms>` line.
func userName(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "user" {
		return ""
	}
	return fields[1]
}

// redisHardeningCanon — canonical systemd-hardening set (NIM-97), shared between the
// drop-in redis-server (hardening.conf.tmpl) and the sentinel unit. Each line is a
// [Service] directive required to be present in both renders. ReadWritePaths is
// checked by separate *_ConfDirInReadWritePaths tests (path-specific).
var redisHardeningCanon = []string{
	"NoNewPrivileges=yes",
	"ProtectSystem=strict",
	"ProtectHome=yes",
	"PrivateTmp=yes",
	"PrivateDevices=yes",
	"ProtectKernelTunables=yes",
	"ProtectKernelModules=yes",
	"ProtectControlGroups=yes",
	"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX",
	"RestrictNamespaces=yes",
	"RestrictRealtime=yes",
	"RestrictSUIDSGID=yes",
	"LockPersonality=yes",
	"MemoryDenyWriteExecute=yes",
	"UMask=007",
	"LimitNOFILE=65535",
}

// TestRedisServerHardening_CanonDirectives — guard on the full canonical
// hardening set in the drop-in redis-server (NIM-97). Removing any directive (MDWE —
// live-verified with redis 8.8 modules; UMask=007 — otherwise RDB is world-readable;
// LimitNOFILE — otherwise the systemd default ~1024) fails the test.
func TestRedisServerHardening_CanonDirectives(t *testing.T) {
	root := map[string]any{"vars": map[string]any{
		"data_dir": "/var/lib/redis", "run_dir": "/var/run/redis",
		"log_dir": "/var/log/redis", "conf_dir": "/etc/redis",
	}}
	out := renderRedisTmpl(t, "hardening.conf.tmpl", root)
	for _, d := range redisHardeningCanon {
		if !hasDirectiveLine(out, d) {
			t.Errorf("drop-in hardening.conf: missing directive %q\n--- render ---\n%s", d, out)
		}
	}
	// CapabilityBoundingSet MUST be EMPTY (drops all caps — stricter than the
	// redis.io-deb baseline with CAP_SYS_RESOURCE). A prefix match would miss a regression to CAP_*.
	if !hasExactLine(out, "CapabilityBoundingSet=") {
		t.Errorf("CapabilityBoundingSet must be EMPTY (drops all caps):\n%s", out)
	}
}

// TestSentinelUnit_HardeningParity — the sentinel unit carries the SAME canonical set
// as the drop-in redis-server (NIM-97 unification). RestrictAddressFamilies with AF_UNIX
// is critical: Type=notify sends sd_notify over a unix socket (live: without AF_UNIX the unit
// never reaches active).
func TestSentinelUnit_HardeningParity(t *testing.T) {
	root := map[string]any{"vars": map[string]any{
		"sentinel_bin": "/usr/bin", "cli_bin": "/usr/bin", "conf_dir": "/etc/redis",
		"sentinel_port": 26379, "redis_user": "redis", "redis_group": "redis",
		"data_dir": "/var/lib/redis", "log_dir": "/var/log/redis", "run_dir": "/var/run/redis",
	}}
	out := renderRedisTmpl(t, "redis-sentinel.service.tmpl", root)
	for _, d := range redisHardeningCanon {
		if !hasDirectiveLine(out, d) {
			t.Errorf("sentinel unit: missing directive %q (unification with drop-in)\n--- render ---\n%s", d, out)
		}
	}
	// CapabilityBoundingSet MUST be EMPTY (drops all caps — stricter than the
	// redis.io-deb baseline with CAP_SYS_RESOURCE). A prefix match would miss a regression to CAP_*.
	if !hasExactLine(out, "CapabilityBoundingSet=") {
		t.Errorf("CapabilityBoundingSet must be EMPTY (drops all caps):\n%s", out)
	}
}

// TestRedisLogrotate_Copytruncate — redis logrotate uses copytruncate, NOT the
// fragile rename+create (Redis holds the log fd, does not react to reopen → the new log
// is empty until restart; NIM-97 fix). Guard: copytruncate + required directives
// are present; no rename indicators (create/postrotate/sharedscripts); block is balanced.
func TestRedisLogrotate_Copytruncate(t *testing.T) {
	root := map[string]any{"vars": map[string]any{"log_dir": "/var/log/redis"}}
	out := renderRedisTmpl(t, "logrotate.tmpl", root)
	for _, want := range []string{"copytruncate", "daily", "missingok", "notifempty", "/var/log/redis/*.log {"} {
		if !strings.Contains(out, want) {
			t.Errorf("logrotate: missing required %q\n%s", want, out)
		}
	}
	for _, ln := range nonEmptyLines(out) {
		for _, bad := range []string{"create", "postrotate", "sharedscripts", "nocopytruncate"} {
			if strings.HasPrefix(ln, bad) {
				t.Errorf("logrotate: rename indicator %q (Redis holds the fd → empty log until restart); copytruncate required\n%s", ln, out)
			}
		}
	}
	if strings.Count(out, "{") != 1 || strings.Count(out, "}") != 1 {
		t.Errorf("logrotate: unbalanced { } block\n%s", out)
	}
}
