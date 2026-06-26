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
