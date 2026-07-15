package validate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDestinyDir writes destiny.yml + tasks/main.yml (+ optional vars.yml)
// into a directory and returns the path to destiny.yml.
func writeDestinyDir(t *testing.T, name, varsYml, tasksYml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "tasks"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "destiny.yml"), []byte("name: "+name+"\n"), 0o644); err != nil {
		t.Fatalf("write destiny.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tasks", "main.yml"), []byte(tasksYml), 0o644); err != nil {
		t.Fatalf("write tasks/main.yml: %v", err)
	}
	if varsYml != "" {
		if err := os.WriteFile(filepath.Join(dir, "vars.yml"), []byte(varsYml), 0o644); err != nil {
			t.Fatalf("write vars.yml: %v", err)
		}
	}
	return filepath.Join(dir, "destiny.yml")
}

// TestValidateDestiny_VarsCollisionWarn — a name declared in both vars.yml
// and a task-level vars: of one task → warn vars_collision (Variant A);
// exit code OK (a warning is not an error).
func TestValidateDestiny_VarsCollisionWarn(t *testing.T) {
	tasks := `- name: t
  module: core.exec.run
  vars:
    unit: redis-staging
  params:
    cmd: "echo ${ vars.unit }"
`
	path := writeDestinyDir(t, "redis", "unit: redis-server\nconf: /etc/redis\n", tasks)

	var out, errOut bytes.Buffer
	code := Run(Options{Path: path, Kind: KindDestiny}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK (warning не ошибка)\nstdout: %s", code, out.String())
	}
	s := out.String()
	if !strings.Contains(s, "vars_collision") {
		t.Errorf("ожидался warn vars_collision в выводе, got: %s", s)
	}
	if !strings.Contains(s, "unit") {
		t.Errorf("ожидалось имя 'unit' в warn, got: %s", s)
	}
}

// TestValidateDestiny_NoCollision_Clean — non-overlapping names → OK, no warn.
func TestValidateDestiny_NoCollision_Clean(t *testing.T) {
	tasks := `- name: t
  module: core.exec.run
  vars:
    extra: flag
  params:
    cmd: "echo ${ vars.unit } ${ vars.extra }"
`
	path := writeDestinyDir(t, "redis", "unit: redis-server\n", tasks)

	var out, errOut bytes.Buffer
	code := Run(Options{Path: path, Kind: KindDestiny}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK", code)
	}
	if strings.Contains(out.String(), "vars_collision") {
		t.Errorf("ложный warn vars_collision на непересекающихся именах: %s", out.String())
	}
}

// TestValidateDestiny_NoVarsYml_Clean — without vars.yml, the cross-check is
// skipped (vars.yml is optional); the manifest is valid → OK.
func TestValidateDestiny_NoVarsYml_Clean(t *testing.T) {
	tasks := `- name: t
  module: core.exec.run
  vars:
    unit: x
  params:
    cmd: "echo ${ vars.unit }"
`
	path := writeDestinyDir(t, "redis", "", tasks)

	var out, errOut bytes.Buffer
	code := Run(Options{Path: path, Kind: KindDestiny}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit = %d, want ExitOK", code)
	}
	if strings.Contains(out.String(), "vars_collision") {
		t.Errorf("warn vars_collision без vars.yml — не должно быть: %s", out.String())
	}
}
