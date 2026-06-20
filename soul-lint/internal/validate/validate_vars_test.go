package validate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDestinyDir кладёт destiny.yml + tasks/main.yml (+опц. vars.yml) в каталог
// и возвращает путь к destiny.yml.
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

// TestValidateDestiny_VarsCollisionWarn — имя, объявленное и в vars.yml, и в
// task-level vars: одной задачи → warn vars_collision (Вариант A); exit-code OK
// (warning не ошибка).
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

// TestValidateDestiny_NoCollision_Clean — непересекающиеся имена → OK без warn.
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

// TestValidateDestiny_NoVarsYml_Clean — без vars.yml кросс-проверка пропускается
// (vars.yml опционален); манифест валиден → OK.
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
