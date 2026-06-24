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
	// Полный пример из examples/destiny/redis/destiny.yml должен
	// валидироваться напрямую с 0 diagnostics — это контракт регрессии.
	runExpect(t, "../../../examples/destiny/redis/destiny.yml", KindDestiny, false, ExitOK, nil)
}

func TestGolden_ServiceExample(t *testing.T) {
	runExpect(t, "../../testdata/service-golden/redis-ha.yml", KindService, false, ExitOK, nil)
}

func TestGolden_ServiceExample_FromRepo(t *testing.T) {
	// Полный пример из examples/service/redis/service.yml (консолидированный
	// redis) должен валидироваться напрямую с 0 diagnostics — контракт регрессии.
	runExpect(t, "../../../examples/service/redis/service.yml", KindService, false, ExitOK, nil)
}

func TestGolden_ScenarioExample(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/redis-create.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioConditionalInclude — условный include со СТАТИЧЕСКИМ when:
// (input.*) ВАЛИДЕН (conditional-include group-drop, ADR-009 amendment). Проходит
// линт (ExitOK): include-when статичен → не падает include_when_dynamic_unsupported;
// include-цель офлайн не резолвится → лишь HINT, exit OK. Реверс (статический
// include-when ошибочно зарезан) → ExitHasErrors → тест падает.
func TestGolden_ScenarioConditionalInclude(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/conditional-include.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioAssertPrecondition — assert-задача (ADR-009 amendment
// 2026-06-23): валидная форма (that[] CEL-bool со soulprint.hosts, message-строка)
// проходит линт (ExitOK). Реверс (assert ошибочно зарезан) → ExitHasErrors → тест
// падает.
func TestGolden_ScenarioAssertPrecondition(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/assert-precondition.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioIncarnationStateRead — `incarnation.state.<path>` (read-only
// снимок incarnation.state в scenario render, ADR-009/010 Вариант A) ВАЛИДЕН в
// предикате и apply.input. Контракт: каноническая форма НЕ ловится
// state_naked_reference (его режет только голый `state.*` без префикса). Реверс
// (incarnation.state ошибочно зарезан) → ExitHasErrors → тест падает.
func TestGolden_ScenarioIncarnationStateRead(t *testing.T) {
	runExpect(t, "../../testdata/scenario-golden/incarnation-state-read.yml", KindScenario, false, ExitOK, nil)
}

// TestGolden_ScenarioSerialStaged — serial: + staged (N>1 Passage) ВАЛИДЕН (ADR-056
// §S4 amend, S-2D1): рестрикт serial_staged_unsupported снят, 2D serial×passage
// реализован. Сценарий проходит линт с ExitOK и passage_plan HINT (структура
// Passage), БЕЗ ошибки serial_staged_unsupported. Реверс (рестрикт вернулся) →
// ExitHasErrors → этот тест падает.
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

func TestGolden_ManifestSoulModule(t *testing.T) {
	runExpect(t, "../../testdata/manifest-golden/soul-module.yaml", KindManifest, false, ExitOK, nil)
}

func TestGolden_ManifestCloudDriver(t *testing.T) {
	runExpect(t, "../../testdata/manifest-golden/cloud-driver.yaml", KindManifest, false, ExitOK, nil)
}

func TestGolden_ManifestSSHProvider(t *testing.T) {
	runExpect(t, "../../testdata/manifest-golden/ssh-provider.yaml", KindManifest, false, ExitOK, nil)
}

// TestGolden_ManifestExamplesFromRepo — каждый из трёх примеров в examples/module/
// должен валидироваться напрямую с 0 errors. Контракт регрессии: если ADR-020
// или docs/keeper/plugins.md меняется, examples/ обновляются, и тест либо
// упадёт (если новый pattern не учли в валидаторе), либо пройдёт.
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

// TestBroken_ManifestFixtures — симметрия с другими TestBroken_*: для каждой
// .yaml-фикстуры обязателен .expected.json с полным множеством кодов.
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
			// Subset-семантика (как в scenario-broken): фикстура утверждает,
			// что конкретный code должен подняться; побочные коды (warning-
			// уровень про missing description, type_mismatch от decoder-а)
			// допустимы. Полный набор кодов тестируется в shared/plugin.
			runExpectJSONSubset(t, p, KindManifest, ExitHasErrors, exp.Codes)
		})
	}
}

// TestBroken_ScenarioFixtures — симметрия с TestBroken_DestinyFixtures по scenario-broken/.
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

// TestBroken_ServiceFixtures — симметрия с TestBroken_DestinyFixtures по service-broken/.
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

// Negative-фикстуры — table-driven по `*.yml` в testdata/broken/.
// Для каждой `.yml` обязателен companion `.expected.json` вида
// `{"codes": ["<code1>", ...]}` — полный набор diagnostic-кодов, которые
// должна вернуть проверка. Сравниваем как множества: ожидаемые ⊆ полученные
// И полученные ⊆ ожидаемые (любое расхождение → fail). Это ловит регрессии,
// добавляющие посторонние коды.
func TestBroken_Fixtures(t *testing.T) {
	matches, err := filepath.Glob("../../testdata/broken/*.yml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	// keeper-partial-decode.yml — уже покрыт TestLoadKeeperFromBytes_PartialDecode,
	// дублировать e2e не нужно (см. delegation).
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

// TestBroken_DestinyFixtures — симметрия с TestBroken_Fixtures по destiny-broken/.
// Каждая .yml сопровождается .expected.json с полным набором кодов.
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
	// Минимум одна JSON-строка с code=kid_invalid_format.
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

// UTF-8 BOM (EF BB BF) перед валидным конфигом не должна ломать ни auto-detect,
// ни парсер — это стандартное поведение YAML 1.2 (silent strip).
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

// runExpect — общий helper для негативных и позитивных кейсов.
// `expectErrorOutput` — флаг «ожидаем непустой stdout с диагностикой».
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

// runExpectJSONSet — JSON-Lines режим, сравнивает множество diagnostic-кодов
// в потоке с ожидаемым множеством. Любое расхождение (потерянный или лишний
// код) → fail. Так ловятся регрессии, добавляющие посторонние diagnostic-и.
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

// runExpectJSONSubset — слабее runExpectJSONSet: ожидает, что **каждый** код из
// `expectCodes` присутствует в потоке (subset), не запрещая дополнительные.
// Используется для scenario-broken фикстур, где побочные коды (например,
// task_discriminator_missing вместе с register_on_block_invalid) допустимы:
// фикстура утверждает «этот конкретный код должен подняться», но не
// фиксирует полный набор — это сделают unit-тесты в shared/config.
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
