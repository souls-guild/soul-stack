package trial

import (
	"fmt"
	"os"
	"path/filepath"

	yaml "github.com/goccy/go-yaml"
)

// caseFileName — canonical name of test file inside tests/<case>/.
const caseFileName = "case.yml"

// l2Markers — top-level keys marking case as level L2
// (stand execution with post-apply verification, ADR-023 post-MVP). MVP-harness
// of level L0 (render-only) does not execute them and should not fail on strict decode
// — such case recognized by soft pre-parse and skipped in recursive tree run.
var l2Markers = []string{"stand", "verify"}

// l1Markers — top-level keys marking case as level L1 (state_schema migration test,
// ADR-019/docs/migrations.md §Tests). Form of L1 case fundamentally differs from L0
// (no fixtures/assert.rendered_tasks), so it is recognized by soft pre-parse BEFORE
// strict L0 decode and goes to separate runner RunMigrationCase. Both key sections must be present.
var l1Markers = []string{"state_before", "state_after"}

// LoadCase reads and validates single `case.yml`. path accepted in two
// forms: path to file itself or path to case directory
// (`.../tests/<case>/`), inside which case.yml is sought.
//
// Strict decode ([yaml.Strict]): unknown key is error, not silent-skip.
// This cuts off cases relying on unimplemented pilot sections
// (assert.dispatch / assert.state_after) with explicit error.
func LoadCase(path string) (*Case, string, error) {
	file, err := resolveCaseFile(path)
	if err != nil {
		return nil, "", err
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, "", fmt.Errorf("trial: reading %s: %w", file, err)
	}

	var c Case
	if err := yaml.UnmarshalWithOptions(data, &c, yaml.Strict()); err != nil {
		return nil, "", fmt.Errorf("trial: parsing %s: %w", file, err)
	}
	if err := c.validate(); err != nil {
		return nil, "", fmt.Errorf("trial: %s: %w", file, err)
	}
	return &c, file, nil
}

// isL2Case — soft pre-parse of case.yml: recognizes level L2 by presence
// of top-level marker stand:/verify: BEFORE strict L0 decode. Parses only
// top-level keys into free map (lax decode), not validating their form:
// L2 sections (stand/verify/expect/…) MVP-harness does not execute, so their strict
// structure is not important here — only the fact of L2 membership matters.
//
// L0 case does not carry markers → false → then usual strict decode follows, where
// unknown-field remains error (strict-decode for L0 not weakened).
func isL2Case(file string) (bool, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("trial: reading %s: %w", file, err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return false, fmt.Errorf("trial: pre-parse %s: %w", file, err)
	}
	for _, marker := range l2Markers {
		if _, ok := top[marker]; ok {
			return true, nil
		}
	}
	return false, nil
}

// isL1Case — soft pre-parse of case file: recognizes level L1 by presence
// of top-level markers state_before:/state_after: BEFORE strict L0 decode
// (symmetric to isL2Case). Parses only top-level keys into free map,
// does not validate section form — runner RunMigrationCase does this.
//
// L1 case requires BOTH sections: single state_before without state_after (or
// vice versa) — not L1, continues and will be rejected by strict L0 decode as explicit
// form error, not silently skipped. L0 case does not carry markers → false.
func isL1Case(file string) (bool, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return false, fmt.Errorf("trial: reading %s: %w", file, err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		return false, fmt.Errorf("trial: pre-parse %s: %w", file, err)
	}
	for _, marker := range l1Markers {
		if _, ok := top[marker]; !ok {
			return false, nil
		}
	}
	return true, nil
}

// resolveCaseFile reduces input (file or directory) to case.yml path.
func resolveCaseFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("trial: %w", err)
	}
	if info.IsDir() {
		return filepath.Join(path, caseFileName), nil
	}
	return path, nil
}

// validate — structural validation of case after decode. We do not validate deeper:
// render pipeline itself will reject invalid scenario, and
// fixtures — free YAML data.
func (c *Case) validate() error {
	if c.Name == "" {
		return fmt.Errorf("name: required")
	}
	// fixtures.soulprint (single-host sugar) and fixtures.hosts (multi-host roster)
	// are mutually exclusive — both describe facts of run hosts; simultaneous
	// submission is ambiguous (one synthetic host vs N) → strict error, like unknown-key.
	if c.Fixtures.Soulprint != nil && len(c.Fixtures.Hosts) > 0 {
		return fmt.Errorf("fixtures.soulprint and fixtures.hosts are mutually exclusive: soulprint — single-host sugar, hosts — multi-host roster")
	}
	// SID uniqueness of roster: duplicate breaks determinism. RegisterByHost —
	// map by SID (harness), second host with same SID overwrites first;
	// sorting soulprint.hosts by SID makes order of duplicates unstable.
	// Strict error (like single/multi exclusion), not silent collapse.
	seenSID := make(map[string]struct{}, len(c.Fixtures.Hosts))
	for i, h := range c.Fixtures.Hosts {
		if h.SID == "" {
			return fmt.Errorf("fixtures.hosts[%d]: sid required", i)
		}
		if _, dup := seenSID[h.SID]; dup {
			return fmt.Errorf("fixtures.hosts[%d]: duplicate sid %q (sid in roster must be unique)", i, h.SID)
		}
		seenSID[h.SID] = struct{}{}
	}
	// expect_render_error (expect render-abort) ⊕ assert.* (expect plan) —
	// opposite outcomes, meaningless in one case (ADR-023 amendment).
	// Presence forms (task_present/task_absent) also expect successful plan —
	// equally mutually exclusive with abort.
	if c.ExpectRenderError != "" {
		if len(c.Assert.RenderedTasks) > 0 || len(c.Assert.TaskPresent) > 0 || len(c.Assert.TaskAbsent) > 0 ||
			c.Assert.StateChanges != nil || c.Assert.StateAfter != nil {
			return fmt.Errorf("expect_render_error and assert.* are mutually exclusive: expect_render_error expects render abort, assert.* — successful plan/result")
		}
		return nil
	}
	// L0 requires assertion of task plan in at least one form: positional
	// (rendered_tasks) OR presence (task_present/task_absent). state_changes/
	// state_after — additional sections, plan itself is not replaced by them.
	if len(c.Assert.RenderedTasks) == 0 && len(c.Assert.TaskPresent) == 0 && len(c.Assert.TaskAbsent) == 0 {
		return fmt.Errorf("assert: empty (L0 requires task plan — rendered_tasks OR task_present/task_absent; state_changes/state_after — additional sections; or set expect_render_error for fail-case)")
	}
	for i, et := range c.Assert.RenderedTasks {
		if et.Module == "" {
			return fmt.Errorf("assert.rendered_tasks[%d]: module required", i)
		}
	}
	for i, et := range c.Assert.TaskPresent {
		if et.Module == "" {
			return fmt.Errorf("assert.task_present[%d]: module required", i)
		}
	}
	for i, et := range c.Assert.TaskAbsent {
		if et.Module == "" {
			return fmt.Errorf("assert.task_absent[%d]: module required", i)
		}
	}
	return nil
}
