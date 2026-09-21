package tmpl

import "testing"

// TestUsesRootField — detector of a template accessing the root field `.<field>`
// ([ADR-010] §3.2, conditional injection of render_context.input, Variant B). Must
// catch REAL action accesses (`{{ .input.X }}`/`range`/`if`/`with`/nested
// templates) and NOT react to a mention of `.input` in literal text/comments of
// the template body (redis.conf.tmpl: `# ... apply.input ...`).
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
		// ★KEY case: a mention of `.input` in literal text/a comment (as in
		// redis.conf.tmpl/sentinel.conf.tmpl) must NOT be counted as an access —
		// it's a TextNode, not a FieldNode.
		{"input in comment text", "input", "# .vars.config: apply.input resolves host-invariantly\nbind 0.0.0.0", false},
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

// TestUsesRootField_ParseError — a broken template → ErrParse (the caller fails
// the same way as on Render, no behavior divergence).
func TestUsesRootField_ParseError(t *testing.T) {
	e := newEngine(t)
	if _, err := e.UsesRootField(`{{ .input.x `, "input"); err == nil {
		t.Fatalf("expected a parse error for the broken template")
	}
}

// TestRootFieldSubKeys — collects subkeys `.<field>.<subkey>` actually read by the
// template (AST). The basis for pinpoint injection of file-vars into
// render_context.vars: we inject EXACTLY the file-vars whose key the template
// reads. Must catch a subkey in action/range/if/with/pipeline/nested define and
// ignore a mention in literal text/a comment and a bare `.<field>` without a subkey.
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

		// A bare `.vars` without a subkey — no subkeys.
		{"bare field no subkey", "vars", `{{ .vars }}`, map[string]bool{}},
		// Another field is ignored.
		{"other field ignored", "vars", `{{ .input.user }}{{ .self.os.family }}`, map[string]bool{}},
		// A mention in a comment/text — TextNode, not an access.
		{"in comment text", "vars", "# .vars.bin_path resolves from vars.yml\nbind 0.0.0.0", map[string]bool{}},
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
					t.Errorf("RootFieldSubKeys(%q): key %q is missing (got %v)", tc.field, k, got)
				}
			}
		})
	}
}

// TestRootFieldSubKeys_ParseError — a broken template → ErrParse (symmetric to
// UsesRootField).
func TestRootFieldSubKeys_ParseError(t *testing.T) {
	e := newEngine(t)
	if _, err := e.RootFieldSubKeys(`{{ .vars.x `, "vars"); err == nil {
		t.Fatalf("expected a parse error for the broken template")
	}
}
