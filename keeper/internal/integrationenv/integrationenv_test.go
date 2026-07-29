package integrationenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NO build tag on this file, deliberately. These guards protect the rule that
// decides whether an integration suite may skip itself, so they must run in the
// docker-less gate (`make check`) — a guard that only runs under
// `-tags=integration` would be protected by the very thing it protects.

// TestRequireDocker_DefaultsToRequiring pins the inversion NIM-238 is about: an
// absent variable means REQUIRE, not skip. The old helper returned true only
// when SOUL_STACK_INTEGRATION_REQUIRE_DOCKER was set, so forgetting it printed a
// green result for a suite that ran nothing.
func TestRequireDocker_DefaultsToRequiring(t *testing.T) {
	t.Setenv(SkipEnv, "")
	t.Setenv(RequireEnv, "")
	if !RequireDocker() {
		t.Fatal("RequireDocker() = false with no variables set — an absent variable must never mean 'skip quietly'")
	}
}

// TestRequireDocker_SkipIsExplicit — the escape hatch works, and it is the ONLY
// one. A skip is a claim that nothing was verified, so it has to be said out
// loud; any non-empty value counts, because the failure mode we care about is
// silence, not a mistyped "false".
func TestRequireDocker_SkipIsExplicit(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "please"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(SkipEnv, v)
			if RequireDocker() {
				t.Errorf("RequireDocker() = true with %s=%q — the opt-out must work", SkipEnv, v)
			}
		})
	}
}

// TestRequireDocker_LegacyVarCannotTurnRequirementOff — the old opt-in variable
// stays honoured (make test-integration and CI both set it), but it no longer
// GRANTS the requirement, so setting it to something false-looking must not
// silently restore the old trap.
func TestRequireDocker_LegacyVarCannotTurnRequirementOff(t *testing.T) {
	t.Setenv(SkipEnv, "")
	for _, v := range []string{"", "0", "false", "no"} {
		t.Setenv(RequireEnv, v)
		if !RequireDocker() {
			t.Errorf("RequireDocker() = false with %s=%q — only %s may relax the requirement", RequireEnv, v, SkipEnv)
		}
	}
}

// TestNoPackageReadsTheDockerEnvItself is the drift guard, and the reason this
// package exists at all rather than 35 copies of one `if`.
//
// Those copies were byte-identical in behaviour and all wrong in the same
// direction; nothing stopped a 36th from appearing, and nothing announced it
// when one did. The rule is now: exactly one place reads these variables. A
// package that starts reading them again is re-forking the decision, so the
// gate says so by name instead of waiting for a suite to go quietly green.
func TestNoPackageReadsTheDockerEnvItself(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve keeper module root: %v", err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve own dir: %v", err)
	}

	var offenders []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip this package (it is the one legitimate reader) and anything
			// that is not our source (vendor/build artefacts).
			if path == self || info.Name() == "bin" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// READS, not mentions. Dozens of suites carry the run command in a doc
		// comment (`SOUL_STACK_INTEGRATION_REQUIRE_DOCKER=1 go test -tags=…`), and
		// flagging those would make the guard noise that gets silenced. What is
		// forbidden is code that asks the environment this question itself.
		src := string(b)
		for _, name := range []string{SkipEnv, RequireEnv} {
			if strings.Contains(src, `os.Getenv("`+name+`"`) || strings.Contains(src, "os.Getenv(`"+name+"`") {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel)
				break
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", root, walkErr)
	}
	if len(offenders) > 0 {
		t.Errorf("these files read the integration docker variables directly instead of calling integrationenv.RequireDocker(): %v\n"+
			"That is how the decision forked into ~35 copies that all defaulted to a silent skip (NIM-238). "+
			"Call the shared helper, or change the policy here where it is one edit.", offenders)
	}
}
