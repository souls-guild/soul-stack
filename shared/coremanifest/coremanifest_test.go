package coremanifest

import "testing"

// expectedModules — полный набор core-манифестов после тиража H2. Ключ — имя
// верхнего уровня (Namespace+"."+Name), значение — ожидаемые states. Стережёт
// «забыли добавить файл в coreFiles» и регресс набора states.
//
// Keeper-side core (`core.soul`/`core.cloud`/`core.vault`/`core.choir`) заведены
// тем же механизмом; имена state выровнены на фактический dispatch coremod-ов
// keeper-стороны (StateCreated/StateDestroyed, StateRead, present/absent).
var expectedModules = map[string][]string{
	"core.exec":     {"run"},
	"core.file":     {"present", "absent", "rendered", "directory"},
	"core.pkg":      {"installed", "latest", "absent"},
	"core.service":  {"running", "stopped", "restarted", "enabled"},
	"core.user":     {"present", "absent"},
	"core.group":    {"present", "absent"},
	"core.cmd":      {"shell"},
	"core.cron":     {"present", "absent"},
	"core.mount":    {"present", "absent", "mounted", "unmounted"},
	"core.git":      {"cloned", "pulled"},
	"core.archive":  {"extracted"},
	"core.sysctl":   {"present", "applied"},
	"core.url":      {"fetched"},
	"core.line":     {"present", "absent"},
	"core.repo":     {"present", "absent"},
	"core.firewall": {"present", "absent"},
	"core.http":     {"probe"},
	"core.noop":     {"run"},                             // no-op/barrier-якорь (ADR-015)
	"core.soul":     {"registered"},                      // keeper-side (on: keeper)
	"core.cloud":    {"created", "destroyed", "resized"}, // keeper-side (ADR-017; resized — авто-расширение VM)
	"core.vault":    {"kv-read", "kv-present"},           // keeper-side (ADR-017): kv-read (явное чтение) + kv-present (generate-if-absent)
	"core.choir":    {"present", "absent"},               // keeper-side (ADR-044)
}

// TestDefault_EmbedManifestsParse — все embed-манифесты парсятся и валидны
// (mustBuild не паникует), реестр содержит ровно ожидаемый набор core-модулей с
// их states.
func TestDefault_EmbedManifestsParse(t *testing.T) {
	reg := Default()
	if got, want := len(reg.Names()), len(expectedModules); got != want {
		t.Errorf("в реестре %d модулей, ожидалось %d: %v", got, want, reg.Names())
	}
	for name, states := range expectedModules {
		m, ok := reg.Lookup(name)
		if !ok {
			t.Errorf("реестр не содержит %q", name)
			continue
		}
		for _, s := range states {
			if _, ok := m.Spec.States[s]; !ok {
				t.Errorf("%s: нет state %q", name, s)
			}
		}
		if len(m.Spec.States) != len(states) {
			t.Errorf("%s: states = %d, ожидалось %d", name, len(m.Spec.States), len(states))
		}
	}
	if _, ok := reg.Lookup("core.nope"); ok {
		t.Error("Lookup несуществующего модуля вернул ok=true")
	}
}

// TestDefault_RequiredParamsPresent — каждое required-поле каждого state имеет
// непустой type (manifest-валидатор это проверяет, но дублируем как
// дрейф-стража: required без type — баг манифеста).
func TestDefault_RequiredParamsPresent(t *testing.T) {
	reg := Default()
	for name := range expectedModules {
		m, _ := reg.Lookup(name)
		for state, def := range m.Spec.States {
			for pname, p := range def.Input {
				if p.Required && p.Type == "" {
					t.Errorf("%s.%s: required param %q без type", name, state, pname)
				}
			}
		}
	}
}

// TestNames_Deterministic — R2: Names() возвращает отсортированный порядок.
func TestNames_Deterministic(t *testing.T) {
	names := Default().Names()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("Names() не отсортирован: %q после %q", names[i], names[i-1])
		}
	}
}

// TestState_ExecRun — exec.run несёт required cmd и опциональные args/creates/…
func TestState_ExecRun(t *testing.T) {
	def, ok := Default().State("core.exec", "run")
	if !ok {
		t.Fatal("core.exec.run не найден")
	}
	cmd, ok := def.Input["cmd"]
	if !ok || !cmd.Required {
		t.Errorf("cmd должен быть required: %+v", cmd)
	}
	if cmd.Type != "string" {
		t.Errorf("cmd.Type = %q, want string", cmd.Type)
	}
	if args, ok := def.Input["args"]; !ok || args.Required {
		t.Errorf("args должен быть опциональным list: %+v", args)
	}
	// `command` — частая опечатка вместо cmd; в схеме его быть не должно.
	if _, ok := def.Input["command"]; ok {
		t.Error("в схеме core.exec.run не должно быть param 'command'")
	}
}

// TestState_FileStates — present/absent/rendered с правильными required-полями.
func TestState_FileStates(t *testing.T) {
	cases := []struct {
		state    string
		required []string
	}{
		{"present", []string{"path"}},
		{"absent", []string{"path"}},
		{"rendered", []string{"path", "template"}},
	}
	for _, tc := range cases {
		def, ok := Default().State("core.file", tc.state)
		if !ok {
			t.Fatalf("core.file.%s не найден", tc.state)
		}
		for _, name := range tc.required {
			p, ok := def.Input[name]
			if !ok || !p.Required {
				t.Errorf("core.file.%s: %q должен быть required, got %+v", tc.state, name, p)
			}
		}
	}
}

// TestState_UnknownState — отсутствующий state даёт ok=false.
func TestState_UnknownState(t *testing.T) {
	if _, ok := Default().State("core.exec", "runn"); ok {
		t.Error("несуществующий state вернул ok=true")
	}
}
