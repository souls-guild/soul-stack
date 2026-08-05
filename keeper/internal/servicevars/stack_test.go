package servicevars

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func resolveWith(t *testing.T, dir string, inc IncarnationContext) map[string]any {
	t.Helper()
	got, err := NewResolver(nil).Resolve(ResolveInput{ServiceDir: dir, Incarnation: inc})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return got
}

func resolveErr(t *testing.T, dir string, inc IncarnationContext) error {
	t.Helper()
	_, err := NewResolver(nil).Resolve(ResolveInput{ServiceDir: dir, Incarnation: inc})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	return err
}

// TestStack_DeclaresTheOrder — with a `_stack.yaml` the ORDER is the stack's,
// not the directory's. The two files are named so that lexical order would give
// the opposite answer: without the stack, `zz-last.yaml` would win.
func TestStack_DeclaresTheOrder(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "winner: base\n",
		"zz-last.yaml": "winner: lexical\n",
		"_stack.yaml": `stack:
  - file: zz-last.yaml
  - file: 00-base.yaml
`,
	})

	if got := resolveWith(t, dir, IncarnationContext{})["winner"]; got != "base" {
		t.Fatalf("the stack must decide the order, got %#v (lexical order would give %q)", got, "lexical")
	}
}

// TestStack_LexicalFallbackWhenAbsent — the same two files with no stack resolve
// the other way. Paired with the test above this pins that the branch is real
// and not a coincidence of the fixture.
func TestStack_LexicalFallbackWhenAbsent(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "winner: base\n",
		"zz-last.yaml": "winner: lexical\n",
	})

	if got := resolveWith(t, dir, IncarnationContext{})["winner"]; got != "lexical" {
		t.Fatalf("without a stack the order is lexical, got %#v", got)
	}
}

// TestStack_WhenGatesAStep — a `when:` predicate over the vars ACCUMULATED so
// far. This is the reason `_stack.yaml` exists rather than a sorted directory:
// a later step branches on what an earlier one loaded.
func TestStack_WhenGatesAStep(t *testing.T) {
	layers := map[string]string{
		"00-base.yaml":  "mode: cache\n",
		"10-cache.yaml": "maxmemory: 512mb\n",
		"10-store.yaml": "maxmemory: 0\n",
		"_stack.yaml": `stack:
  - file: 00-base.yaml
  - file: 10-cache.yaml
    when: vars.mode == 'cache'
  - file: 10-store.yaml
    when: vars.mode == 'store'
`,
	}
	got := resolveWith(t, writeLayers(t, layers), IncarnationContext{})
	if got["maxmemory"] != "512mb" {
		t.Fatalf("the cache branch must apply and the store branch must not: %#v", got)
	}

	layers["00-base.yaml"] = "mode: store\n"
	got = resolveWith(t, writeLayers(t, layers), IncarnationContext{})
	if got["maxmemory"] != uint64(0) {
		t.Fatalf("flipping the accumulated value must flip the branch: %#v", got)
	}
}

// TestStack_ForeachOverIncarnationCovens — the replacement for the deleted
// hard-wired `coven/<label>.yaml` overlay. The axis is the INCARNATION's own
// labels selecting overlays of ITS OWN config (ADR-0082): the step iterates
// `incarnation.covens`, and nothing here reads a host's `souls.coven` — a member
// is served this config because it belongs, not because it carries the label
// (NIM-281).
func TestStack_ForeachOverIncarnationCovens(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":     "tier: base\ninterval: 45s\n",
		"coven/cache.yaml": "interval: 15s\nfrom_cache: 1\n",
		"coven/prod.yaml":  "tier: gold\n",
		"_stack.yaml": `stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    file: "coven/${ coven }.yaml"
    optional: true
`,
	})

	got := resolveWith(t, dir, IncarnationContext{Covens: []string{"cache", "prod"}})
	if got["interval"] != "15s" || got["from_cache"] != uint64(1) || got["tier"] != "gold" {
		t.Fatalf("both coven overlays must apply: %#v", got)
	}

	// An untagged incarnation gets the base and nothing else — an empty list
	// iterates zero times rather than failing.
	bare := resolveWith(t, dir, IncarnationContext{})
	if bare["interval"] != "45s" || bare["tier"] != "base" {
		t.Fatalf("no covens must leave the base untouched: %#v", bare)
	}
	if _, leaked := bare["from_cache"]; leaked {
		t.Fatalf("an overlay applied to an incarnation that carries no such label: %#v", bare)
	}
}

// TestStack_ForeachOrderIsTheListOrder — the covens are applied in the order the
// incarnation declares them, so a collision between two overlays resolves
// predictably from the row alone.
func TestStack_ForeachOrderIsTheListOrder(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":   "tier: base\n",
		"coven/aaa.yaml": "tier: aaa\n",
		"coven/zzz.yaml": "tier: zzz\n",
		"_stack.yaml": `stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    file: "coven/${ coven }.yaml"
`,
	})

	if got := resolveWith(t, dir, IncarnationContext{Covens: []string{"aaa", "zzz"}})["tier"]; got != "zzz" {
		t.Fatalf("last declared coven must win, got %#v", got)
	}
	if got := resolveWith(t, dir, IncarnationContext{Covens: []string{"zzz", "aaa"}})["tier"]; got != "aaa" {
		t.Fatalf("reversing the declaration must reverse the winner, got %#v", got)
	}
}

// TestStack_InlineSeesAccumulatedVars — an inline step computing from what the
// files already loaded. String values interpolate; a whole-cell expression
// yields a native type.
func TestStack_InlineSeesAccumulatedVars(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "conf_dir: /etc/redis\nshards: 3\n",
		"_stack.yaml": `stack:
  - file: 00-base.yaml
  - inline:
      acl_path: "${ vars.conf_dir }/users.acl"
      replicas: "${ int(vars.shards) * 2 }"
      literal: 7
`,
	})

	got := resolveWith(t, dir, IncarnationContext{})
	if got["acl_path"] != "/etc/redis/users.acl" {
		t.Errorf("inline must interpolate over accumulated vars: %#v", got["acl_path"])
	}
	if got["replicas"] != int64(6) {
		t.Errorf("a whole-cell expression must yield a native type: %#v (%T)", got["replicas"], got["replicas"])
	}
	if got["literal"] != uint64(7) {
		t.Errorf("a non-string inline value passes through: %#v", got["literal"])
	}
}

// TestStack_StrategyReplaceVsDeep — the knob redis needed and could not express.
// `install_package` documents that an override replaces the WHOLE map; a deep
// merge leaves the base's gpg_key_url attached to an overridden repo_uri — a
// mirror URL from one place and its signing key from another.
func TestStack_StrategyReplaceVsDeep(t *testing.T) {
	layers := map[string]string{
		"00-base.yaml":   "install_package:\n  repo_uri: https://upstream/deb\n  gpg_key_url: https://upstream/key.asc\n",
		"10-mirror.yaml": "install_package:\n  repo_uri: https://mirror.internal/deb\n",
	}

	deep := resolveWith(t, writeLayers(t, mergeStrings(layers, map[string]string{
		"_stack.yaml": "stack:\n  - file: 00-base.yaml\n  - file: 10-mirror.yaml\n",
	})), IncarnationContext{})
	wantDeep := map[string]any{
		"repo_uri":    "https://mirror.internal/deb",
		"gpg_key_url": "https://upstream/key.asc",
	}
	if !reflect.DeepEqual(deep["install_package"], wantDeep) {
		t.Errorf("deep (default) must merge per key:\n got=%#v\nwant=%#v", deep["install_package"], wantDeep)
	}

	replace := resolveWith(t, writeLayers(t, mergeStrings(layers, map[string]string{
		"_stack.yaml": "stack:\n  - file: 00-base.yaml\n  - file: 10-mirror.yaml\n    strategy: replace\n",
	})), IncarnationContext{})
	wantReplace := map[string]any{"repo_uri": "https://mirror.internal/deb"}
	if !reflect.DeepEqual(replace["install_package"], wantReplace) {
		t.Errorf("replace must drop the base's remaining keys:\n got=%#v\nwant=%#v", replace["install_package"], wantReplace)
	}
}

// TestStack_FileDeclaresItsOwnStrategy — the file is where the intent lives: a
// `10-mirror.yaml` KNOWS it is an override, and in lexical mode there is no step
// to say so for it. Both forms, and the precedence between them.
func TestStack_FileDeclaresItsOwnStrategy(t *testing.T) {
	base := "install_package:\n  repo_uri: upstream\n  gpg: up-key\nother:\n  a: 1\n"

	t.Run("whole file, lexical mode", func(t *testing.T) {
		dir := writeLayers(t, map[string]string{
			"00-base.yaml":   base,
			"10-mirror.yaml": "_strategy: replace\ninstall_package:\n  repo_uri: mirror\n",
		})
		got := resolveWith(t, dir, IncarnationContext{})
		if want := map[string]any{"repo_uri": "mirror"}; !reflect.DeepEqual(got["install_package"], want) {
			t.Fatalf("install_package = %#v, want %#v", got["install_package"], want)
		}
		if _, leaked := got[strategyKey]; leaked {
			t.Fatalf("the reserved key leaked into the result: %#v", got)
		}
		if got["other"] == nil {
			t.Fatalf("a key the override does not carry must survive: %#v", got)
		}
	})

	t.Run("per key beats whole file", func(t *testing.T) {
		dir := writeLayers(t, map[string]string{
			"00-base.yaml":   base,
			"10-mirror.yaml": "_strategy:\n  install_package: replace\ninstall_package:\n  repo_uri: mirror\nother:\n  b: 2\n",
		})
		got := resolveWith(t, dir, IncarnationContext{})
		if want := map[string]any{"repo_uri": "mirror"}; !reflect.DeepEqual(got["install_package"], want) {
			t.Fatalf("the named key must replace: %#v", got["install_package"])
		}
		if want := map[string]any{"a": uint64(1), "b": uint64(2)}; !reflect.DeepEqual(got["other"], want) {
			t.Fatalf("an unnamed key must still deep-merge: %#v", got["other"])
		}
	})

	t.Run("file overrides the step", func(t *testing.T) {
		dir := writeLayers(t, map[string]string{
			"00-base.yaml":   base,
			"10-mirror.yaml": "_strategy: replace\ninstall_package:\n  repo_uri: mirror\n",
			"_stack.yaml":    "stack:\n  - file: 00-base.yaml\n  - file: 10-mirror.yaml\n    strategy: deep\n",
		})
		got := resolveWith(t, dir, IncarnationContext{})
		if want := map[string]any{"repo_uri": "mirror"}; !reflect.DeepEqual(got["install_package"], want) {
			t.Fatalf("the file's declaration must win over the step's: %#v", got["install_package"])
		}
	})
}

// TestStack_InvalidFileStrategyRefuses — `_strategy: shallow` silently
// deep-merging is the fail-open this package's error type exists to prevent, and
// a stray `_strategy` left in the map would surface later as a var nobody
// declared.
func TestStack_InvalidFileStrategyRefuses(t *testing.T) {
	for name, decl := range map[string]string{
		"unknown scalar":  "_strategy: shallow\n",
		"unknown per key": "_strategy:\n  install_package: shallow\n",
		"wrong type":      "_strategy: 7\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeLayers(t, map[string]string{
				"00-base.yaml":   "install_package:\n  repo_uri: upstream\n",
				"10-mirror.yaml": decl + "install_package:\n  repo_uri: mirror\n",
			})
			if err := resolveErr(t, dir, IncarnationContext{}); !errors.Is(err, ErrStackStepInvalid) {
				t.Fatalf("want ErrStackStepInvalid, got %v", err)
			}
		})
	}
}

// TestStack_StrategyReplaceIsPerStepNotGlobal — `replace` replaces only the
// top-level keys the step itself carries; everything else the stack accumulated
// survives.
func TestStack_StrategyReplaceIsPerStepNotGlobal(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":   "keep: true\ninstall_package:\n  repo_uri: up\n  gpg: k\n",
		"10-mirror.yaml": "install_package:\n  repo_uri: mirror\n",
		"_stack.yaml":    "stack:\n  - file: 00-base.yaml\n  - file: 10-mirror.yaml\n    strategy: replace\n",
	})

	got := resolveWith(t, dir, IncarnationContext{})
	if got["keep"] != true {
		t.Errorf("replace must not touch keys the step does not carry: %#v", got)
	}
}

// TestStack_OptionalTolerates_MissingIsLoudByDefault — a composed path is
// exactly where a typo hides, so an absent file is an ERROR unless the author
// says otherwise.
func TestStack_OptionalTolerates_MissingIsLoudByDefault(t *testing.T) {
	base := map[string]string{"00-base.yaml": "key: base\n"}

	optional := writeLayers(t, mergeStrings(base, map[string]string{
		"_stack.yaml": "stack:\n  - file: 00-base.yaml\n  - file: nope.yaml\n    optional: true\n",
	}))
	if got := resolveWith(t, optional, IncarnationContext{})["key"]; got != "base" {
		t.Fatalf("optional: true must skip a missing file, got %#v", got)
	}

	required := writeLayers(t, mergeStrings(base, map[string]string{
		"_stack.yaml": "stack:\n  - file: 00-base.yaml\n  - file: nope.yaml\n",
	}))
	if err := resolveErr(t, required, IncarnationContext{}); err == nil {
		t.Fatal("a missing file without optional: must fail")
	}
}

// TestStack_InvalidStepsRefuse — every shape the validator rejects, each with
// its own fixture. Fail-closed: a stack that cannot be read as written must not
// resolve to a plausible-looking subset of itself.
func TestStack_InvalidStepsRefuse(t *testing.T) {
	cases := map[string]string{
		"neither file nor inline": "stack:\n  - when: \"true\"\n",
		"both file and inline":    "stack:\n  - file: 00-base.yaml\n    inline: {a: 1}\n",
		"foreach without as":      "stack:\n  - foreach: \"${ incarnation.covens }\"\n    file: x.yaml\n",
		"as without foreach":      "stack:\n  - as: coven\n    file: x.yaml\n",
		"as shadows vars":         "stack:\n  - foreach: \"${ incarnation.covens }\"\n    as: vars\n    file: x.yaml\n",
		"as shadows incarnation":  "stack:\n  - foreach: \"${ incarnation.covens }\"\n    as: incarnation\n    file: x.yaml\n",
		"optional on inline":      "stack:\n  - inline: {a: 1}\n    optional: true\n",
		"unknown strategy":        "stack:\n  - file: 00-base.yaml\n    strategy: shallow\n",
	}
	for name, stack := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeLayers(t, map[string]string{"00-base.yaml": "key: base\n", "_stack.yaml": stack})
			err := resolveErr(t, dir, IncarnationContext{Covens: []string{"c"}})
			if !errors.Is(err, ErrStackStepInvalid) {
				t.Fatalf("want ErrStackStepInvalid, got %v", err)
			}
		})
	}
}

// TestStack_ForeachOverANonListRefuses — `foreach: "${ incarnation.name }"` is a
// mistake worth an error, not a loop of one.
func TestStack_ForeachOverANonListRefuses(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "key: base\n",
		"_stack.yaml":  "stack:\n  - foreach: \"${ incarnation.name }\"\n    as: x\n    file: \"${ x }.yaml\"\n",
	})

	err := resolveErr(t, dir, IncarnationContext{Name: "redis-prod"})
	if !errors.Is(err, ErrStackStepInvalid) {
		t.Fatalf("want ErrStackStepInvalid, got %v", err)
	}
}

// TestStack_WhenMustBeBool — a non-bool predicate is an error, not a truthiness
// guess. A stack step that silently "matched" a string would be the same class
// of quiet wrong answer this whole change is removing.
func TestStack_WhenMustBeBool(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "mode: cache\n",
		"_stack.yaml":  "stack:\n  - file: 00-base.yaml\n  - file: 00-base.yaml\n    when: vars.mode\n",
	})

	if err := resolveErr(t, dir, IncarnationContext{}); !errors.Is(err, ErrStackStepInvalid) {
		t.Fatalf("want ErrStackStepInvalid, got %v", err)
	}
}

// TestStack_UndeclaredRootsAreCompileErrors — "refused outright" has to be
// literal. A root left DECLARED but empty would let `has(soulprint.self.os)`
// compile, evaluate false and silently skip the step — the fail-open this
// package's error type exists to prevent. Non-declaration turns the same
// expression into an error naming the root.
func TestStack_UndeclaredRootsAreCompileErrors(t *testing.T) {
	for name, expr := range map[string]string{
		"soulprint via has": `has(soulprint.self.os)`,
		"soulprint direct":  `soulprint.self.os.family == 'debian'`,
		"input":             `input.mode == 'x'`,
		"register":          `register.probe.stdout == 'x'`,
		"compute":           `compute.x == 1`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeLayers(t, map[string]string{
				"00-base.yaml":  "key: base\n",
				"10-extra.yaml": "extra: 1\n",
				"_stack.yaml":   "stack:\n  - file: 00-base.yaml\n  - file: 10-extra.yaml\n    when: \"" + expr + "\"\n",
			})
			err := resolveErr(t, dir, IncarnationContext{})
			if !strings.Contains(err.Error(), "undeclared reference") {
				t.Fatalf("want an undeclared-reference compile error, got %v", err)
			}
		})
	}
}

// TestStack_WhenGatesEachForeachIteration — the combination the whole
// coven-overlay replacement rests on, and the one neither the `when:` test nor
// the `foreach:` tests covered: a predicate reading the `as:` binding, deciding
// per iteration.
func TestStack_WhenGatesEachForeachIteration(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":    "tier: base\n",
		"coven/keep.yaml": "from_keep: 1\n",
		"coven/skip.yaml": "from_skip: 1\n",
		"_stack.yaml": `stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    when: coven != 'skip'
    file: "coven/${ coven }.yaml"
`,
	})

	got := resolveWith(t, dir, IncarnationContext{Covens: []string{"keep", "skip"}})
	if got["from_keep"] != uint64(1) {
		t.Errorf("the kept iteration must apply: %#v", got)
	}
	if _, leaked := got["from_skip"]; leaked {
		t.Errorf("the gated-out iteration must not apply: %#v", got)
	}
}

// TestStack_InlineReadsTheForeachBinding — an inline step inside a fan-out,
// composing a value from the element rather than loading a file for it.
func TestStack_InlineReadsTheForeachBinding(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "prefix: svc\n",
		"_stack.yaml": `stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    inline:
      last_coven: "${ vars.prefix }-${ coven }"
`,
	})

	got := resolveWith(t, dir, IncarnationContext{Covens: []string{"aaa", "zzz"}})
	if got["last_coven"] != "svc-zzz" {
		t.Fatalf("last_coven = %#v, want svc-zzz (last iteration wins)", got["last_coven"])
	}
}

// TestStack_StrategyReplaceInsideForeach — `replace` is per STEP, so inside a
// fan-out it applies on every iteration: the last coven's map stands alone
// rather than accumulating the earlier ones key by key.
func TestStack_StrategyReplaceInsideForeach(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":   "pkg:\n  repo: upstream\n  gpg: up-key\n",
		"coven/aaa.yaml": "pkg:\n  repo: aaa-mirror\n",
		"coven/zzz.yaml": "pkg:\n  repo: zzz-mirror\n",
		"_stack.yaml": `stack:
  - file: 00-base.yaml
  - foreach: "${ incarnation.covens }"
    as: coven
    file: "coven/${ coven }.yaml"
    strategy: replace
`,
	})

	got := resolveWith(t, dir, IncarnationContext{Covens: []string{"aaa", "zzz"}})
	want := map[string]any{"repo": "zzz-mirror"}
	if !reflect.DeepEqual(got["pkg"], want) {
		t.Fatalf("pkg = %#v, want %#v (replace drops both the base and the earlier iteration)", got["pkg"], want)
	}
}

// TestStack_FileNotListedIsIgnored — the rule pipeline.go states as deliberate:
// a stack declares the order, and the two modes are never combined. A layer file
// the stack does not name contributes nothing.
func TestStack_FileNotListedIsIgnored(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":     "key: base\n",
		"99-unlisted.yaml": "key: unlisted\nfrom_unlisted: 1\n",
		"_stack.yaml":      "stack:\n  - file: 00-base.yaml\n",
	})

	got := resolveWith(t, dir, IncarnationContext{})
	if got["key"] != "base" {
		t.Errorf("an unlisted file must not apply: %#v", got["key"])
	}
	if _, leaked := got["from_unlisted"]; leaked {
		t.Errorf("an unlisted file leaked into the result: %#v", got)
	}
}

// TestStack_ExistingEmptyFileIsNotReportedMissing — an author who leaves a layer
// file empty on purpose must not be told it does not exist, and must certainly
// not be advised to add `optional: true`, which would then silently skip a file
// they placed deliberately.
func TestStack_ExistingEmptyFileIsNotReportedMissing(t *testing.T) {
	dir := writeLayers(t, map[string]string{
		"00-base.yaml":  "key: base\n",
		"10-empty.yaml": "",
		"20-note.yaml":  "# comments only, no keys\n",
		"_stack.yaml":   "stack:\n  - file: 00-base.yaml\n  - file: 10-empty.yaml\n  - file: 20-note.yaml\n",
	})

	got := resolveWith(t, dir, IncarnationContext{})
	if got["key"] != "base" {
		t.Fatalf("got=%#v", got)
	}
}

// TestStack_FileLeavingVarsDirRefuses — `file:` is documented relative to
// `vars/`, and securejoin clamps at the SNAPSHOT root, not at vars/. So a
// composed `../service.yml` would succeed and merge the service manifest into the
// vars namespace — inside the sandbox, and still not what the step said. Refused
// before the read, so the escape target's existence is irrelevant.
func TestStack_FileLeavingVarsDirRefuses(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.yaml"), []byte("leaked: true\n"), 0o644); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	dir := filepath.Join(outside, "snapshot")
	if err := os.MkdirAll(filepath.Join(dir, "vars"), 0o755); err != nil {
		t.Fatalf("mkdir snapshot/vars: %v", err)
	}
	for rel, body := range map[string]string{
		"00-base.yaml": "key: base\n",
		"_stack.yaml":  "stack:\n  - file: 00-base.yaml\n  - file: \"../secret.yaml\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, "vars", rel), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	got, err := NewResolver(nil).Resolve(ResolveInput{ServiceDir: dir})
	if err == nil {
		t.Fatalf("a file: leaving vars/ must be refused, got %#v", got)
	}
	if !errors.Is(err, ErrStackStepInvalid) {
		t.Fatalf("want ErrStackStepInvalid, got %v", err)
	}
}

// TestStack_NestedMarkerInInlineRefuses — only TOP-LEVEL string values of an
// `inline:` are interpolated. Passing a nested one through would put the literal
// text `${ vars.mirror }/deb` into an apt sources line, never re-evaluated
// downstream. An author who just watched the top level work has no reason to
// expect the next indent to differ, so this errors and names the key.
func TestStack_NestedMarkerInInlineRefuses(t *testing.T) {
	for name, inline := range map[string]string{
		"nested map":  "      install_package:\n        repo_uri: \"${ vars.mirror }/deb\"\n",
		"nested list": "      args: [\"--url=${ vars.mirror }\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeLayers(t, map[string]string{
				"00-base.yaml": "mirror: https://m.internal\n",
				"_stack.yaml":  "stack:\n  - file: 00-base.yaml\n  - inline:\n" + inline,
			})
			if err := resolveErr(t, dir, IncarnationContext{}); !errors.Is(err, ErrStackStepInvalid) {
				t.Fatalf("want ErrStackStepInvalid, got %v", err)
			}
		})
	}

	// The top level still interpolates — the refusal is about depth, not markers.
	dir := writeLayers(t, map[string]string{
		"00-base.yaml": "mirror: https://m.internal\n",
		"_stack.yaml":  "stack:\n  - file: 00-base.yaml\n  - inline:\n      repo: \"${ vars.mirror }/deb\"\n",
	})
	if got := resolveWith(t, dir, IncarnationContext{})["repo"]; got != "https://m.internal/deb" {
		t.Fatalf("repo = %#v", got)
	}
}

// TestStack_EmptyStackRefuses — a `_stack.yaml` that declares no steps is a
// typo, not a statement, and the outcome of tolerating it is the worst-looking
// one available: every var of the service silently gone, no error. A service
// that genuinely has no vars has no `vars/` directory.
func TestStack_EmptyStackRefuses(t *testing.T) {
	for name, body := range map[string]string{
		"explicitly empty": "stack: []\n",
		"nothing at all":   "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeLayers(t, map[string]string{
				"00-base.yaml": "key: base\n",
				"_stack.yaml":  body,
			})
			if err := resolveErr(t, dir, IncarnationContext{}); !errors.Is(err, ErrStackStepInvalid) {
				t.Fatalf("want ErrStackStepInvalid, got %v", err)
			}
		})
	}
}

// TestStack_UnknownKeysRefuse — the fail-open this file's error type exists to
// prevent, in both places it can hide.
//
// A mistyped STEP key has no symptom without strict decoding: the field never
// reaches `validate()`, which only inspects what parsed. `whn:` drops the gate
// and applies the layer unconditionally; `strateg:` deep-merges where the author
// asked for a replace. A mistyped TOP-LEVEL key is worse — `steps:` for `stack:`
// parses, resolves to nothing, and returns success.
func TestStack_UnknownKeysRefuse(t *testing.T) {
	cases := map[string]string{
		"typo in when":        "stack:\n  - file: 00-base.yaml\n  - file: 10-extra.yaml\n    whn: \"false\"\n",
		"typo in strategy":    "stack:\n  - file: 00-base.yaml\n  - file: 10-extra.yaml\n    strateg: replace\n",
		"typo in the top key": "steps:\n  - file: 00-base.yaml\n",
		"unknown step key":    "stack:\n  - file: 00-base.yaml\n    unless: \"true\"\n",
	}
	for name, stack := range cases {
		t.Run(name, func(t *testing.T) {
			dir := writeLayers(t, map[string]string{
				"00-base.yaml":  "key: base\n",
				"10-extra.yaml": "extra: 1\n",
				"_stack.yaml":   stack,
			})
			if err := resolveErr(t, dir, IncarnationContext{}); !errors.Is(err, ErrStackStepInvalid) {
				t.Fatalf("want ErrStackStepInvalid, got %v", err)
			}
		})
	}
}

func mergeStrings(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
