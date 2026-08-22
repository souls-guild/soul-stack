package config

import (
	"reflect"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestPathAddressesOwnNamespace(t *testing.T) {
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"plain own path", "secret/redis/prod/redis_users/app", true},
		{"vault marker", "vault:secret/redis/prod/redis_users/app", true},
		{"field selector stripped", "vault:secret/redis/prod/redis_users/app#password", true},
		{"leading slash tolerated", "/secret/redis/prod/x", true},
		{"mount agnostic", "kv/redis/prod/x", true},
		{"concatenation prefix", "secret/redis/", true},
		{"bare mount and service", "secret/redis", true},

		{"other service", "secret/postgres/prod/x", false},
		{"keeper namespace", "secret/keeper/jwt-signing-key", false},
		{"shared services bucket", "secret/services/redis/tls#cert", false},
		{"whole-segment, not prefix", "secret/redis-evil/prod/x", false},
		{"whole-segment, not suffix", "secret/evil-redis/prod/x", false},
		{"single segment", "redis", false},
		{"middle fragment of a concatenation", "/users/", false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PathAddressesOwnNamespace(tc.path, "redis"); got != tc.want {
				t.Fatalf("PathAddressesOwnNamespace(%q, redis) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// An empty service name checks nothing rather than everything — a push or unit-eval
// caller that has no service identity must not have every path rejected.
func TestPathAddressesOwnNamespaceNoService(t *testing.T) {
	if PathAddressesOwnNamespace("secret/redis/prod/x", "") {
		t.Fatal("an empty service name matched a path")
	}
	if d := ScanOwnNamespaceVault("main.yml", "", &ScenarioManifest{}, []Task{{
		Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "${ vault('secret/redis/x#p') }"}},
	}}); len(d) != 0 {
		t.Fatalf("scan with no service reported %d diagnostics", len(d))
	}
}

// The four spellings ADR-0083 §7 names, each in the form the corpus actually
// uses, plus the two shapes the fence must NOT touch.
func TestScanOwnNamespaceVaultSpellings(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want string // YAMLPath of the expected diagnostic, "" = none
	}{
		{
			name: "cel macro, concatenated path",
			task: Task{Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{
				"cmd": "redis-cli AUTH ${ vault('secret/redis/' + incarnation.name + '/users/' + u.name + '#password') }",
			}}},
			want: "$.tasks[0].params.cmd",
		},
		{
			name: "vault ref in params",
			task: Task{Module: &ModuleTask{Module: "core.file.present", Params: map[string]any{
				"content": "vault:secret/redis/prod/redis_users/app#password",
			}}},
			want: "$.tasks[0].params.content",
		},
		{
			name: "kv-read path",
			task: Task{Module: &ModuleTask{Module: "core.vault.kv-read", Params: map[string]any{
				"path": "secret/redis/prod/redis_users/app",
			}}},
			want: "$.tasks[0].params.path",
		},
		{
			name: "kv-present targets, a CEL list",
			task: Task{Module: &ModuleTask{Module: "core.vault.kv-present", Params: map[string]any{
				"targets": "${ input.users.map(u, {'path': 'secret/redis/' + incarnation.name + '/users/' + u.name, 'field': 'password'}) }",
			}}},
			want: "$.tasks[0].params.targets",
		},
		{
			name: "flow control is fenced too",
			task: Task{
				When:   "${ vault('secret/redis/prod/x#password') } != ''",
				Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			want: "$.tasks[0].when",
		},
		{
			name: "apply input",
			task: Task{Apply: &ApplyTask{Destiny: "redis-deploy", Input: map[string]any{
				"password": "${ vault('secret/redis/' + incarnation.name + '/users/admin#password') }",
			}}},
			want: "$.tasks[0].apply.input.password",
		},
		{
			name: "loop items",
			task: Task{
				Loop:   &LoopSpec{Items: "${ [vault('secret/redis/prod/x#password')] }", As: "p"},
				Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}},
			},
			want: "$.tasks[0].loop.items",
		},
		{
			name: "cross-namespace read survives",
			task: Task{Module: &ModuleTask{Module: "core.file.rendered", Params: map[string]any{
				"ca": "${ vault('secret/services/shared/tls#ca') }",
			}}},
			want: "",
		},
		{
			name: "a filesystem path carrying the service name is not a vault path",
			task: Task{Module: &ModuleTask{Module: "core.file.present", Params: map[string]any{
				"path": "/opt/redis/redis.conf",
			}}},
			want: "",
		},
		{
			name: "a vault path built entirely from variables is out of static reach",
			task: Task{Module: &ModuleTask{Module: "core.file.present", Params: map[string]any{
				"content": "${ vault(vars.some_ref) }",
			}}},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanOwnNamespaceVault("scenario/deploy/main.yml", "redis", nil, []Task{tc.task})
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want clean, got %d: %+v", len(got), got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 diagnostic, got %d: %+v", len(got), got)
			}
			if got[0].YAMLPath != tc.want {
				t.Fatalf("YAMLPath = %q, want %q", got[0].YAMLPath, tc.want)
			}
			if got[0].Code != VaultOwnNamespaceCode {
				t.Fatalf("Code = %q, want %q", got[0].Code, VaultOwnNamespaceCode)
			}
			if got[0].Level != diag.LevelError {
				t.Fatalf("Level = %v, want error", got[0].Level)
			}
			if got[0].File != "scenario/deploy/main.yml" {
				t.Fatalf("File = %q", got[0].File)
			}
		})
	}
}

// A nested block: the fence is not a top-level-only scan, and the address it
// reports has to name the nesting so an operator can find the line.
func TestScanOwnNamespaceVaultBlock(t *testing.T) {
	got := ScanOwnNamespaceVault("main.yml", "redis", nil, []Task{{
		Block: &BlockTask{Block: []Task{
			{Name: "noise", Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}},
			{Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{
				"cmd": "${ vault('secret/redis/prod/redis_users/app#password') }",
			}}},
		}},
	}})
	if len(got) != 1 || got[0].YAMLPath != "$.tasks[0].block[1].params.cmd" {
		t.Fatalf("got %+v", got)
	}
}

// Scenario-level surfaces: vars / compute / validate are authored CEL too.
func TestScanOwnNamespaceVaultManifestLevel(t *testing.T) {
	m := &ScenarioManifest{
		Vars:     map[string]any{"pw": "${ vault('secret/redis/prod/x#password') }"},
		Compute:  ComputeBlock{{Name: "c", Value: "${ vault('secret/redis/prod/y#password') }"}},
		Validate: []ValidateRule{{That: "vault('secret/redis/prod/z#password') != ''", Message: "m"}},
	}
	got := ScanOwnNamespaceVault("main.yml", "redis", m, nil)
	want := []string{"$.vars.pw", "$.compute[0]", "$.validate[0].that"}
	var have []string
	for _, d := range got {
		have = append(have, d.YAMLPath)
	}
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("addresses = %v, want %v", have, want)
	}
}

// Identical state must produce byte-identical output: the walk sorts map keys
// rather than ranging a Go map.
func TestScanOwnNamespaceVaultDeterministic(t *testing.T) {
	task := Task{Module: &ModuleTask{Module: "core.vault.kv-read", Params: map[string]any{
		"zeta":  "secret/redis/prod/z",
		"alpha": "secret/redis/prod/a",
		"mid":   "secret/redis/prod/m",
	}}}
	var first []string
	for i := 0; i < 20; i++ {
		var run []string
		for _, d := range ScanOwnNamespaceVault("main.yml", "redis", nil, []Task{task}) {
			run = append(run, d.YAMLPath)
		}
		if i == 0 {
			first = run
			continue
		}
		if !reflect.DeepEqual(run, first) {
			t.Fatalf("run %d = %v, first = %v", i, run, first)
		}
	}
	if len(first) != 3 {
		t.Fatalf("want 3 diagnostics, got %v", first)
	}
}

// The post-render half. A `path:` written as `${ vars.p }` carries no segment the
// authoring-time scan can compare; the dispatcher holds the resolved value.
func TestScanRenderedVaultParams(t *testing.T) {
	cases := []struct {
		name    string
		module  string
		params  map[string]any
		flagged bool
	}{
		{"kv-read own namespace", "core.vault.kv-read",
			map[string]any{"path": "secret/redis/prod/redis_users/app"}, true},
		{"kv-read sibling incarnation", "core.vault.kv-read",
			map[string]any{"path": "secret/redis/staging/redis_users/app"}, true},
		{"kv-present nested target", "core.vault.kv-present",
			map[string]any{"targets": []any{map[string]any{"path": "secret/redis/prod/system_acl_users/admin"}}}, true},
		{"kv-read other service", "core.vault.kv-read",
			map[string]any{"path": "secret/services/shared/tls"}, false},
		{"kv-read prefix lookalike", "core.vault.kv-read",
			map[string]any{"path": "secret/redis-evil/prod/users/app"}, false},
		{"not a vault module", "core.file.present",
			map[string]any{"path": "/opt/redis/redis.conf", "content": "secret/redis/prod/redis_users/app"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanRenderedVaultParams(tc.module, "redis", tc.params)
			if tc.flagged != (len(got) > 0) {
				t.Fatalf("flagged = %v, want %v (%+v)", len(got) > 0, tc.flagged, got)
			}
			if tc.flagged && got[0].Code != VaultOwnNamespaceCode {
				t.Fatalf("Code = %q, want %q", got[0].Code, VaultOwnNamespaceCode)
			}
		})
	}
}

// No service identity fences nothing, as everywhere else in this pair.
func TestScanRenderedVaultParamsNoService(t *testing.T) {
	got := ScanRenderedVaultParams("core.vault.kv-read", "",
		map[string]any{"path": "secret/redis/prod/redis_users/app"})
	if len(got) != 0 {
		t.Fatalf("the fence fired without a service identity: %+v", got)
	}
}

// The fence decides on the path the Vault client will resolve, not on the one that was
// typed. Two rewrites happen downstream — repeated slashes collapse, and the mount may
// be left off entirely (a form ReadKV documents) — and each one used to carry an
// author-written path straight into the namespace the platform mints into.
func TestPathAddressesOwnNamespace_SurvivesClientNormalisation(t *testing.T) {
	const service = "redis"
	fenced := []string{
		"secret/redis/prod/redis_users/alice",
		"secret//redis/prod/redis_users/alice",  // normalizeLogical collapses the empty segment
		"//secret//redis//prod/redis_users",     // and any number of them
		"secret/./redis/prod/redis_users/alice", // `.` is a no-op segment to every normalizer
		"redis/prod/redis_users/alice",          // relativeKVPath substitutes the mount
		"/redis/prod/admin_password",            // leading slash trimmed by both
		"vault:secret//redis/prod/admin#value",  // marker and selector stripped first
	}
	for _, p := range fenced {
		if !PathAddressesOwnNamespace(p, service) {
			t.Errorf("PathAddressesOwnNamespace(%q) = false, want it fenced -- it resolves to the derived secret", p)
		}
	}
	open := []string{
		"secret/shared/tls-ca",
		"secret/other-service/prod/admin_password",
		"secret/redis-evil/prod/admin", // whole-segment comparison, not a prefix
		"shared/tls-ca",                // mount-relative, outside the namespace
		"secret",                       // too short to address anything
	}
	for _, p := range open {
		if PathAddressesOwnNamespace(p, service) {
			t.Errorf("PathAddressesOwnNamespace(%q) = true, want it left open -- the fence is on one prefix, not on Vault", p)
		}
	}
}

// `state_changes:` is the surface where a fenced path is not merely read but
// COMMITTED into incarnation.state — the second copy [ADR-0083] exists to remove,
// written into the place the ADR calls the source of truth. Every cell of both
// forms is scanned, `foreach.do` included.
func TestScanOwnNamespaceVault_StateChanges(t *testing.T) {
	const own = "${ vault('secret/redis/prod/redis_users/app#password') }"
	cases := map[string]*StateChanges{
		"set value": {IsList: true, Ops: []StateChange{
			{Verb: VerbSet, Field: "admin_password", Value: own},
		}},
		"add key": {IsList: true, Ops: []StateChange{
			{Verb: VerbAdd, Field: "redis_users", Key: own, Value: map[string]any{"acl": "+@all"}},
		}},
		"modify match": {IsList: true, Ops: []StateChange{
			{Verb: VerbModify, Field: "redis_users", Match: own + " == elem.pw"},
		}},
		"modify patch": {IsList: true, Ops: []StateChange{
			{Verb: VerbModify, Field: "redis_users", Match: "true", Patch: map[string]any{"pw": own}},
		}},
		"foreach in": {IsList: true, Ops: []StateChange{
			{Verb: VerbForeach, In: own, As: "u"},
		}},
		"foreach do": {IsList: true, Ops: []StateChange{
			{Verb: VerbForeach, In: "${ input.users }", As: "u", Do: []StateChange{
				{Verb: VerbSet, Field: "admin_password", Value: own},
			}},
		}},
		"legacy sets": {Sets: map[string]string{"admin_password": own}},
	}
	for name, sc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &ScenarioManifest{Name: "deploy", StateChanges: sc}
			got := ScanOwnNamespaceVault("deploy.yml", "redis", m, nil)
			if len(got) != 1 {
				t.Fatalf("diagnostics = %+v, want exactly one %s", got, VaultOwnNamespaceCode)
			}
			if got[0].Code != VaultOwnNamespaceCode {
				t.Fatalf("code = %s, want %s", got[0].Code, VaultOwnNamespaceCode)
			}
		})
	}
}

// The negative twin: a cross-namespace read in state_changes is left alone.
func TestScanOwnNamespaceVault_StateChangesCrossNamespaceOpen(t *testing.T) {
	m := &ScenarioManifest{Name: "deploy", StateChanges: &StateChanges{IsList: true, Ops: []StateChange{
		{Verb: VerbSet, Field: "ca", Value: "${ vault('secret/services/shared/tls#ca') }"},
	}}}
	if got := ScanOwnNamespaceVault("deploy.yml", "redis", m, nil); len(got) != 0 {
		t.Fatalf("the fence fired on a cross-namespace path: %+v", got)
	}
}

// TestScanOwnNamespaceVault_WhitespaceBeforeParen — CEL's lexer skips whitespace between
// an identifier and its argument list, so `vault ('…')` reads Vault exactly like
// `vault('…')`. A detector that required them adjacent let one space carry a path
// through the [ADR-0083] §7 load-time scan untouched.
func TestScanOwnNamespaceVault_WhitespaceBeforeParen(t *testing.T) {
	for _, expr := range []string{
		"${ vault ('secret/redis/prod/redis_users/app#password') }",
		"${ vault\t('secret/redis/prod/redis_users/app#password') }",
	} {
		tasks := []Task{{
			Name:   "leak",
			Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": expr}},
		}}
		diags := ScanOwnNamespaceVault("scenario/deploy.yml", "redis", nil, tasks)
		if len(diags) != 1 || diags[0].Code != "vault_path_in_own_namespace" {
			t.Errorf("expr %q: diags = %+v, want one vault_path_in_own_namespace", expr, diags)
		}
	}

	// The identifier boundary still holds: `myvault(` / `obj.vault(` are not the builtin.
	for _, expr := range []string{
		"${ myvault('secret/redis/prod/redis_users/app#password') }",
		"${ obj.vault('secret/redis/prod/redis_users/app#password') }",
	} {
		tasks := []Task{{
			Name:   "not the builtin",
			Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": expr}},
		}}
		if diags := ScanOwnNamespaceVault("scenario/deploy.yml", "redis", nil, tasks); len(diags) != 0 {
			t.Errorf("expr %q: diags = %+v, want none", expr, diags)
		}
	}
}

// Every surface [vaultScan.scanTasks] visits, one case each — including the four the
// spellings table already exercises, so the table is a complete census rather than a
// list of leftovers. The scan is a hand-written walk with one line per field: a field
// added to Task and not wired in, or a line dropped in an edit, silently opens a hole
// that nothing else in the suite would notice. Each case puts the same path in exactly
// one position and asserts the diagnostic lands at that address, so a deleted line
// fails the one case that names it.
func TestScanOwnNamespaceVault_EveryTaskSurface(t *testing.T) {
	// The expression form, for the keys whose whole value is CEL, and the
	// interpolated form for the string contexts. Same path either way.
	const bare = "vault('secret/redis/prod/redis_users/app#password') != ''"
	const interp = "${ vault('secret/redis/prod/redis_users/app#password') }"
	noop := &ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": "true"}}

	cases := []struct {
		name string
		task Task
		want string
	}{
		{"when", Task{When: bare, Module: noop}, "$.tasks[0].when"},
		{"where", Task{Where: bare, Module: noop}, "$.tasks[0].where"},
		{"changed_when", Task{ChangedWhen: bare, Module: noop}, "$.tasks[0].changed_when"},
		{"failed_when", Task{FailedWhen: bare, Module: noop}, "$.tasks[0].failed_when"},
		{"on", Task{On: []any{interp}, Module: noop}, "$.tasks[0].on[0]"},
		{"vars", Task{Vars: map[string]any{"pw": interp}, Module: noop}, "$.tasks[0].vars.pw"},
		{"output", Task{Output: map[string]any{"pw": interp}, Module: noop}, "$.tasks[0].output.pw"},
		{"loop.items", Task{Loop: &LoopSpec{Items: "${ [vault('secret/redis/prod/redis_users/app#password')] }", As: "p"}, Module: noop}, "$.tasks[0].loop.items"},
		{"loop.when", Task{Loop: &LoopSpec{Items: "${ [1] }", As: "p", When: bare}, Module: noop}, "$.tasks[0].loop.when"},
		{"retry.until", Task{Retry: &RetrySpec{Count: 3, Until: bare}, Module: noop}, "$.tasks[0].retry.until"},
		// Index 1, not 0: the address is built per element, and a hardcoded [0]
		// would report the wrong line to the operator who has to find it.
		{"assert.that", Task{Assert: &AssertSpec{That: []string{"true", bare}, Message: "m"}}, "$.tasks[0].assert.that[1]"},
		{"params", Task{Module: &ModuleTask{Module: "core.exec.run", Params: map[string]any{"cmd": interp}}}, "$.tasks[0].params.cmd"},
		{"apply.input", Task{Apply: &ApplyTask{Destiny: "d", Input: map[string]any{"pw": interp}}}, "$.tasks[0].apply.input.pw"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanOwnNamespaceVault("main.yml", "redis", nil, []Task{tc.task})
			if len(got) != 1 {
				t.Fatalf("want 1 diagnostic at %s, got %d: %+v", tc.want, len(got), got)
			}
			if got[0].YAMLPath != tc.want {
				t.Fatalf("YAMLPath = %q, want %q", got[0].YAMLPath, tc.want)
			}
			if got[0].Code != VaultOwnNamespaceCode {
				t.Fatalf("Code = %q, want %q", got[0].Code, VaultOwnNamespaceCode)
			}
		})
	}
}
