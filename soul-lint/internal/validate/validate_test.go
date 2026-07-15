package validate

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/diag"
)

func TestGolden_KeeperExample(t *testing.T) {
	runExpect(t, "../../testdata/golden/keeper.yml", KindConfig, false, ExitOK, nil)
}

func TestGolden_SoulExample(t *testing.T) {
	runExpect(t, "../../testdata/golden/soul.yml", KindConfig, false, ExitOK, nil)
}

func TestGolden_KeeperJSON_EmptyStdout(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(Options{Path: "../../testdata/golden/keeper.yml", JSON: true, Kind: KindConfig}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit code: got %d, want %d (stderr=%q)", code, ExitOK, errOut.String())
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout for golden+json, got %q", out.String())
	}
}

func TestGolden_DestinyExample(t *testing.T) {
	runExpect(t, "../../testdata/destiny-golden/redis.yml", KindDestiny, false, ExitOK, nil)
}

func TestGolden_DestinyExample_FromRepo(t *testing.T) {
	// The full example from examples/destiny/redis/destiny.yml must validate
	// directly with 0 diagnostics — a regression contract.
	runExpect(t, "../../../examples/destiny/redis/destiny.yml", KindDestiny, false, ExitOK, nil)
}

func TestGolden_ServiceExample(t *testing.T) {
	runExpect(t, "../../testdata/service-golden/redis-ha.yml", KindService, false, ExitOK, nil)
}

func TestGolden_ServiceExample_FromRepo(t *testing.T) {
	// The full example from examples/service/redis/service.yml (the consolidated
	// redis) must validate directly with 0 diagnostics — a regression contract.
	runExpect(t, "../../../examples/service/redis/service.yml", KindService, false, ExitOK, nil)
}

func TestGolden_ScenarioExample(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/redis-create.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioConditionalInclude — a conditional include with a
// STATIC when: (input.*) is VALID (conditional-include group-drop, ADR-009
// amendment). Passes lint (ExitOK): a static include-when doesn't trigger
// include_when_dynamic_unsupported; an offline-unresolvable include target
// is just a HINT, exit OK. Reverse (a static include-when wrongly rejected)
// → ExitHasErrors → this test fails.
func TestGolden_ScenarioConditionalInclude(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/conditional-include.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioAssertPrecondition — an assert task (ADR-009 amendment
// 2026-06-23): a valid form (that[] CEL-bool with soulprint.hosts, a message
// string) passes lint (ExitOK). Reverse (assert wrongly rejected) →
// ExitHasErrors → this test fails.
func TestGolden_ScenarioAssertPrecondition(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/assert-precondition.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioValidatePrecondition — the top-level validate: block
// (ADR-009 amendment 2026-06-23, DSL wave 2): declarative input invariants
// [{that, message}] pass lint (ExitOK). Guards against a regression where
// soul-lint would raise unknown_key on "validate" (field
// ScenarioManifest.Validate + reflect-walker stop-point
// validateRuleSliceType + AST validator validateValidateBlock). Reverse
// (validate: wrongly rejected as unknown_key) → ExitHasErrors → this test
// fails.
func TestGolden_ScenarioValidatePrecondition(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/validate-precondition.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioIncarnationStateRead — `incarnation.state.<path>` (a
// read-only snapshot of incarnation.state in scenario render, ADR-009/010
// Variant A) is VALID in a predicate and in apply.input. Contract: the
// canonical form is NOT caught by state_naked_reference (that only rejects
// a bare `state.*` without the prefix). Reverse (incarnation.state wrongly
// rejected) → ExitHasErrors → this test fails.
func TestGolden_ScenarioIncarnationStateRead(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/incarnation-state-read.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioSerialStaged — serial: + staged (N>1 Passage) is VALID
// (ADR-056 §S4 amend, S-2D1): the serial_staged_unsupported restriction was
// lifted, 2D serial×passage is implemented. The scenario passes lint with
// ExitOK and a passage_plan HINT (Passage structure), WITHOUT the
// serial_staged_unsupported error. Reverse (the restriction came back) →
// ExitHasErrors → this test fails.
func TestGolden_ScenarioSerialStaged(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(Options{Path: "../../testdata/scenario-golden/serial-staged.yml", JSON: true, Kind: KindScenario}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit code: got %d, want %d (serial+staged теперь валиден)\nstdout: %s\nstderr: %s", code, ExitOK, out.String(), errOut.String())
	}
	codes := map[string]bool{}
	dec := json.NewDecoder(&out)
	for {
		var d diag.Diagnostic
		if err := dec.Decode(&d); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("json decode: %v", err)
		}
		codes[d.Code] = true
	}
	if !codes["passage_plan"] {
		t.Errorf("ожидался passage_plan HINT (staged-структура), got: %v", sortedKeys(codes))
	}
	if codes["serial_staged_unsupported"] {
		t.Fatalf("★ serial_staged_unsupported поднялся — рестрикт вернулся (ADR-056 §S4 amend нарушен)")
	}
}

// TestGolden_ScenarioVaultStaged — the vault-secrets-generated passage axis
// (ADR-056 amendment, Variant A) is VISIBLE to the offline linter. The
// core.vault.kv-present emitter pushes a vault()-reading task into the next
// Passage; the scenario is self-contained (emitter and consumer in the same
// file — otherwise a read task across an include boundary isn't visible
// offline). Contract: lint passes with ExitOK and emits passage_plan with a
// staged structure (>1 Passage). Reverse meaning (symmetric with
// serial-staged): if the vault axis breaks, kv-present stops splitting
// vault()-read, the plan collapses to single-passage, the passage_plan
// message becomes "single-passage", and the check for a staged-run message
// goes red. This is the offline analog of the trial guard tests
// (redis_create_from_souls_secrets_passage_test.go), which catch the same
// regression on the keeper-side plan.
func TestGolden_ScenarioVaultStaged(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(Options{Path: "../../testdata/scenario-golden/vault-staged.yml", JSON: true, Kind: KindScenario}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit code: got %d, want %d (vault-staged должен проходить линт)\nstdout: %s\nstderr: %s", code, ExitOK, out.String(), errOut.String())
	}
	staged := false
	dec := json.NewDecoder(&out)
	for {
		var d diag.Diagnostic
		if err := dec.Decode(&d); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("json decode: %v", err)
		}
		// A passage_plan with a staged structure confirms the vault axis split the
		// plan into >1 Passage. A single-passage message (plan collapsed) will NOT
		// set staged.
		if d.Code == "passage_plan" && strings.Contains(d.Message, "staged-прогон") {
			staged = true
		}
	}
	if !staged {
		t.Fatalf("★ vault-ось НЕ расщепила план: ожидался passage_plan со staged-прогон (>1 Passage), но его нет — kv-present-эмиттер перестал уводить vault()-read в следующий Passage (ADR-056 amendment нарушен)")
	}
}

func TestGolden_ManifestSoulModule(t *testing.T) {
	runExpect(t, "../../testdata/manifest-golden/soul-module.yaml", KindManifest, false, ExitOK, nil)
}

func TestGolden_ManifestCloudDriver(t *testing.T) {
	runExpect(t, "../../testdata/manifest-golden/cloud-driver.yaml", KindManifest, false, ExitOK, nil)
}

func TestGolden_ManifestSSHProvider(t *testing.T) {
	runExpect(t, "../../testdata/manifest-golden/ssh-provider.yaml", KindManifest, false, ExitOK, nil)
}

// TestGolden_ManifestExamplesFromRepo — each of the three examples in
// examples/module/ must validate directly with 0 errors. Regression
// contract: if ADR-020 or docs/keeper/plugins.md changes and examples/ are
// updated, this test will either fail (if the validator doesn't account for
// the new pattern) or pass.
func TestGolden_ManifestExamplesFromRepo(t *testing.T) {
	cases := []string{
		"../../../examples/module/soul-mod-redis-failover/manifest.yaml",
		"../../../examples/module/soul-cloud-aws/manifest.yaml",
		"../../../examples/module/soul-ssh-vault/manifest.yaml",
	}
	for _, p := range cases {
		p := p
		t.Run(filepath.Base(filepath.Dir(p)), func(t *testing.T) {
			runExpect(t, p, KindManifest, false, ExitOK, nil)
		})
	}
}

// TestBroken_ManifestFixtures — symmetric with the other TestBroken_*: every
// .yaml fixture requires a companion .expected.json with the full set of codes.
func TestBroken_ManifestFixtures(t *testing.T) {
	matches, err := filepath.Glob("../../testdata/manifest-broken/*.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no manifest-broken fixtures found")
	}
	for _, p := range matches {
		p := p
		base := filepath.Base(p)
		t.Run(base, func(t *testing.T) {
			expPath := strings.TrimSuffix(p, ".yaml") + ".expected.json"
			raw, rerr := os.ReadFile(expPath)
			if rerr != nil {
				t.Fatalf("missing companion %s: %v", expPath, rerr)
			}
			var exp struct {
				Codes []string `json:"codes"`
			}
			if jerr := json.Unmarshal(raw, &exp); jerr != nil {
				t.Fatalf("decode %s: %v", expPath, jerr)
			}
			// Subset semantics (as in scenario-broken): the fixture asserts that
			// a specific code must be raised; incidental codes (warning-level
			// about missing description, decoder type_mismatch) are allowed.
			// The full code set is tested in shared/plugin.
			runExpectJSONSubset(t, p, KindManifest, ExitHasErrors, exp.Codes)
		})
	}
}

// TestBroken_ScenarioFixtures — symmetric with TestBroken_DestinyFixtures for scenario-broken/.
func TestBroken_ScenarioFixtures(t *testing.T) {
	matches, err := filepath.Glob("../../testdata/scenario-broken/*.yml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no scenario-broken fixtures found")
	}
	for _, p := range matches {
		p := p
		base := filepath.Base(p)
		t.Run(base, func(t *testing.T) {
			expPath := strings.TrimSuffix(p, ".yml") + ".expected.json"
			raw, rerr := os.ReadFile(expPath)
			if rerr != nil {
				t.Fatalf("missing companion %s: %v", expPath, rerr)
			}
			var exp struct {
				Codes []string `json:"codes"`
			}
			if jerr := json.Unmarshal(raw, &exp); jerr != nil {
				t.Fatalf("decode %s: %v", expPath, jerr)
			}
			runExpectJSONSubset(t, p, KindScenario, ExitHasErrors, exp.Codes)
		})
	}
}

// TestBroken_ServiceFixtures — symmetric with TestBroken_DestinyFixtures for service-broken/.
func TestBroken_ServiceFixtures(t *testing.T) {
	matches, err := filepath.Glob("../../testdata/service-broken/*.yml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no service-broken fixtures found")
	}
	for _, p := range matches {
		p := p
		base := filepath.Base(p)
		t.Run(base, func(t *testing.T) {
			expPath := strings.TrimSuffix(p, ".yml") + ".expected.json"
			raw, rerr := os.ReadFile(expPath)
			if rerr != nil {
				t.Fatalf("missing companion %s: %v", expPath, rerr)
			}
			var exp struct {
				Codes []string `json:"codes"`
			}
			if jerr := json.Unmarshal(raw, &exp); jerr != nil {
				t.Fatalf("decode %s: %v", expPath, jerr)
			}
			runExpectJSONSet(t, p, KindService, ExitHasErrors, exp.Codes)
		})
	}
}

// Negative fixtures — table-driven over `*.yml` in testdata/broken/. Every
// `.yml` requires a companion `.expected.json` of the form
// `{"codes": ["<code1>", ...]}` — the full set of diagnostic codes the
// check must return. Compared as sets: expected ⊆ got AND got ⊆ expected
// (any mismatch → fail). This catches regressions that add stray codes.
func TestBroken_Fixtures(t *testing.T) {
	matches, err := filepath.Glob("../../testdata/broken/*.yml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// keeper-partial-decode.yml is already covered by
	// TestLoadKeeperFromBytes_PartialDecode; no need to duplicate the e2e case
	// (see delegation).
	skip := map[string]bool{"keeper-partial-decode.yml": true}
	for _, p := range matches {
		p := p
		base := filepath.Base(p)
		if skip[base] {
			continue
		}
		t.Run(base, func(t *testing.T) {
			expPath := strings.TrimSuffix(p, ".yml") + ".expected.json"
			raw, rerr := os.ReadFile(expPath)
			if rerr != nil {
				t.Fatalf("missing companion %s: %v", expPath, rerr)
			}
			var exp struct {
				Codes []string `json:"codes"`
			}
			if jerr := json.Unmarshal(raw, &exp); jerr != nil {
				t.Fatalf("decode %s: %v", expPath, jerr)
			}
			runExpectJSONSet(t, p, KindConfig, ExitHasErrors, exp.Codes)
		})
	}
}

// TestBroken_DestinyFixtures — symmetric with TestBroken_Fixtures for
// destiny-broken/. Every .yml is accompanied by an .expected.json with the
// full set of codes.
func TestBroken_DestinyFixtures(t *testing.T) {
	matches, err := filepath.Glob("../../testdata/destiny-broken/*.yml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) == 0 {
		t.Fatal("no destiny-broken fixtures found")
	}
	for _, p := range matches {
		p := p
		base := filepath.Base(p)
		t.Run(base, func(t *testing.T) {
			expPath := strings.TrimSuffix(p, ".yml") + ".expected.json"
			raw, rerr := os.ReadFile(expPath)
			if rerr != nil {
				t.Fatalf("missing companion %s: %v", expPath, rerr)
			}
			var exp struct {
				Codes []string `json:"codes"`
			}
			if jerr := json.Unmarshal(raw, &exp); jerr != nil {
				t.Fatalf("decode %s: %v", expPath, jerr)
			}
			runExpectJSONSet(t, p, KindDestiny, ExitHasErrors, exp.Codes)
		})
	}
}

func TestBroken_JSONLines(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(Options{
		Path: "../../testdata/broken/keeper-bad-kid.yml",
		JSON: true,
		Kind: KindConfig,
	}, &out, &errOut)
	if code != ExitHasErrors {
		t.Fatalf("exit code: got %d, want %d", code, ExitHasErrors)
	}
	// At least one JSON line with code=kid_invalid_format.
	dec := json.NewDecoder(&out)
	foundCode := false
	for {
		var d diag.Diagnostic
		if err := dec.Decode(&d); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("json decode: %v", err)
		}
		if d.Code == "kid_invalid_format" {
			foundCode = true
		}
	}
	if !foundCode {
		t.Fatalf("expected kid_invalid_format in JSON stream, errOut=%q", errOut.String())
	}
}

// A UTF-8 BOM (EF BB BF) before a valid config must not break either
// auto-detect or the parser — this is standard YAML 1.2 behavior (silent
// strip).
func TestBOM_KeeperGoldenStillOK(t *testing.T) {
	src, err := os.ReadFile("../../testdata/golden/keeper.yml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	bom := append([]byte{0xEF, 0xBB, 0xBF}, src...)
	tmp := t.TempDir()
	p := filepath.Join(tmp, "keeper-bom.yml")
	if err := os.WriteFile(p, bom, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out, errOut bytes.Buffer
	code := Run(Options{Path: p, Kind: KindConfig}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit: got %d want %d\nstdout: %s\nstderr: %s", code, ExitOK, out.String(), errOut.String())
	}
}

func TestIOFatal(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run(Options{Path: "/no/such/path.yml", Kind: KindConfig}, &out, &errOut)
	if code != ExitIOFatal {
		t.Fatalf("exit code: got %d, want %d", code, ExitIOFatal)
	}
}

// runExpect is a shared helper for negative and positive cases.
// `expectErrorOutput` is a flag meaning "expect non-empty stdout with a diagnostic".
func runExpect(t *testing.T, path string, kind Kind, expectErrorOutput bool, wantCode int, requireCodes []string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Options{Path: path, Kind: kind}, &out, &errOut)
	if code != wantCode {
		t.Fatalf("exit code: got %d, want %d\nstdout: %s\nstderr: %s", code, wantCode, out.String(), errOut.String())
	}
	if expectErrorOutput && out.Len() == 0 {
		t.Fatalf("expected non-empty stdout with diagnostics, got empty")
	}
	for _, c := range requireCodes {
		if !strings.Contains(out.String(), c) {
			t.Fatalf("expected diagnostic code %q in stdout, got: %s", c, out.String())
		}
	}
}

// runExpectJSONSet — JSON-Lines mode; compares the set of diagnostic codes
// in the stream against the expected set. Any mismatch (missing or extra
// code) → fail. This catches regressions that add stray diagnostics.
func runExpectJSONSet(t *testing.T, path string, kind Kind, wantCode int, expectCodes []string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Options{Path: path, JSON: true, Kind: kind}, &out, &errOut)
	if code != wantCode {
		t.Fatalf("exit code: got %d, want %d\nstdout: %s\nstderr: %s", code, wantCode, out.String(), errOut.String())
	}
	gotSet := map[string]bool{}
	dec := json.NewDecoder(&out)
	for {
		var d diag.Diagnostic
		if err := dec.Decode(&d); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("json decode: %v", err)
		}
		gotSet[d.Code] = true
	}
	wantSet := map[string]bool{}
	for _, c := range expectCodes {
		wantSet[c] = true
	}
	missing := setDiff(wantSet, gotSet)
	extra := setDiff(gotSet, wantSet)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("diagnostic-code set mismatch:\n  want: %v\n  got:  %v\n  missing: %v\n  extra: %v",
			sortedKeys(wantSet), sortedKeys(gotSet), missing, extra)
	}
}

// runExpectJSONSubset — weaker than runExpectJSONSet: expects that
// **every** code in `expectCodes` is present in the stream (subset),
// without forbidding extra ones. Used for scenario-broken fixtures, where
// incidental codes (e.g. task_discriminator_missing alongside
// register_on_block_invalid) are allowed: the fixture only asserts "this
// specific code must be raised", not the full set — that's covered by
// unit tests in shared/config.
func runExpectJSONSubset(t *testing.T, path string, kind Kind, wantCode int, expectCodes []string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(Options{Path: path, JSON: true, Kind: kind}, &out, &errOut)
	if code != wantCode {
		t.Fatalf("exit code: got %d, want %d\nstdout: %s\nstderr: %s", code, wantCode, out.String(), errOut.String())
	}
	gotSet := map[string]bool{}
	dec := json.NewDecoder(&out)
	for {
		var d diag.Diagnostic
		if err := dec.Decode(&d); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("json decode: %v", err)
		}
		gotSet[d.Code] = true
	}
	var missing []string
	for _, c := range expectCodes {
		if !gotSet[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("expected diagnostic codes %v not found in output. got: %v", missing, sortedKeys(gotSet))
	}
}

func setDiff(a, b map[string]bool) []string {
	var out []string
	for k := range a {
		if !b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
