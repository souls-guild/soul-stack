package tmpl

import "testing"

// TestUsesRootField — детектор обращения шаблона к корневому полю `.<field>`
// ([ADR-010] §3.2, условная инъекция render_context.input, Вариант B). Должен
// ловить РЕАЛЬНЫЕ action-обращения (`{{ .input.X }}`/`range`/`if`/`with`/
// вложенные шаблоны) и НЕ реагировать на упоминание `.input` в литеральном
// тексте/комментариях тела шаблона (redis.conf.tmpl: `# ... apply.input ...`).
func TestUsesRootField(t *testing.T) {
	e := newEngine(t)

	cases := []struct {
		name  string
		field string
		body  string
		want  bool
	}{
		{"direct field", "input", `User={{ .input.user }}`, true},
		{"bare field", "input", `{{ .input }}`, true},
		{"in range", "input", `{{ range .input.collectors }}--c={{ . }}{{ end }}`, true},
		{"in if", "input", `{{ if .input.tls }}x{{ end }}`, true},
		{"in with", "input", `{{ with .input.opts }}{{ .a }}{{ end }}`, true},
		{"in pipeline arg", "input", `{{ default "x" .input.listen }}`, true},
		{"nested define", "input", `{{ define "t" }}{{ .input.x }}{{ end }}{{ template "t" . }}`, true},

		{"only vars", "input", `socket {{ .vars.socket }}`, false},
		{"self only", "input", `family {{ .self.os.family }}`, false},
		// ★КЛЮЧЕВОЙ кейс: упоминание `.input` в литеральном тексте/комментарии
		// (как в redis.conf.tmpl/sentinel.conf.tmpl) НЕ должно считаться
		// обращением — это TextNode, не FieldNode.
		{"input in comment text", "input", "# .vars.config: apply.input резолвится host-инвариантно\nbind 0.0.0.0", false},
		{"input substring in literal", "input", `# describes .input.textfile_dir contract`, false},
		{"empty template", "input", `bind 0.0.0.0`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.UsesRootField(tc.body, tc.field)
			if err != nil {
				t.Fatalf("UsesRootField: %v", err)
			}
			if got != tc.want {
				t.Errorf("UsesRootField(%q) = %v, want %v", tc.field, got, tc.want)
			}
		})
	}
}

// TestUsesRootField_ParseError — битый шаблон → ErrParse (caller падает так же,
// как при Render, расхождения поведения нет).
func TestUsesRootField_ParseError(t *testing.T) {
	e := newEngine(t)
	if _, err := e.UsesRootField(`{{ .input.x `, "input"); err == nil {
		t.Fatalf("ожидалась ошибка парсинга битого шаблона")
	}
}

// TestRootFieldSubKeys — сбор подключей `.<field>.<subkey>`, реально читаемых
// шаблоном (AST). Основа точечной инъекции file-vars в render_context.vars:
// инъектим РОВНО те file-vars, чей ключ шаблон читает. Должен ловить subkey в
// action/range/if/with/pipeline/вложенном define и игнорировать упоминание в
// литеральном тексте/комментарии и голое `.<field>` без подключа.
func TestRootFieldSubKeys(t *testing.T) {
	e := newEngine(t)

	cases := []struct {
		name  string
		field string
		body  string
		want  map[string]bool
	}{
		{"single subkey", "vars", `ExecStart={{ .vars.bin_path }}`, map[string]bool{"bin_path": true}},
		{"multiple subkeys", "vars", `{{ .vars.a }}{{ .vars.b }}`, map[string]bool{"a": true, "b": true}},
		{"subkey in range", "vars", `{{ range .vars.loadmodules }}{{ . }}{{ end }}`, map[string]bool{"loadmodules": true}},
		{"subkey in if", "vars", `{{ if .vars.config }}x{{ end }}`, map[string]bool{"config": true}},
		{"subkey in pipeline arg", "vars", `{{ default "x" .vars.password }}`, map[string]bool{"password": true}},
		{"nested define", "vars", `{{ define "t" }}{{ .vars.users }}{{ end }}{{ template "t" . }}`, map[string]bool{"users": true}},
		{"deep chain takes second ident", "vars", `{{ .vars.config.maxmemory }}`, map[string]bool{"config": true}},

		// Голое `.vars` без подключа — подключей нет.
		{"bare field no subkey", "vars", `{{ .vars }}`, map[string]bool{}},
		// Другое поле игнорируется.
		{"other field ignored", "vars", `{{ .input.user }}{{ .self.os.family }}`, map[string]bool{}},
		// Упоминание в комментарии/тексте — TextNode, не обращение.
		{"in comment text", "vars", "# .vars.bin_path резолвится из vars.yml\nbind 0.0.0.0", map[string]bool{}},
		{"empty template", "vars", `bind 0.0.0.0`, map[string]bool{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.RootFieldSubKeys(tc.body, tc.field)
			if err != nil {
				t.Fatalf("RootFieldSubKeys: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("RootFieldSubKeys(%q) = %v, want %v", tc.field, got, tc.want)
			}
			for k := range tc.want {
				if !got[k] {
					t.Errorf("RootFieldSubKeys(%q): отсутствует ключ %q (got %v)", tc.field, k, got)
				}
			}
		})
	}
}

// TestRootFieldSubKeys_ParseError — битый шаблон → ErrParse (симметрично
// UsesRootField).
func TestRootFieldSubKeys_ParseError(t *testing.T) {
	e := newEngine(t)
	if _, err := e.RootFieldSubKeys(`{{ .vars.x `, "vars"); err == nil {
		t.Fatalf("ожидалась ошибка парсинга битого шаблона")
	}
}
