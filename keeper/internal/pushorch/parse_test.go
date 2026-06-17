package pushorch

import (
	"errors"
	"testing"
)

func TestParseDestinyRef_Valid(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantRef  string
	}{
		{"redis-base@v1.4.0", "redis-base", "v1.4.0"},
		{"redis-base@main", "redis-base", "main"},
		{"a@b", "a", "b"},
		{"single@feature/branch-name", "single", "feature/branch-name"},
		// Множественное `@` берёт ПОСЛЕДНИЙ как разделитель (git-ref может
		// содержать `@`, имя destiny — нет).
		{"redis-base@feature@odd", "redis-base", "feature@odd"},
		{"my-svc-2@1.2.3-rc1", "my-svc-2", "1.2.3-rc1"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			name, ref, err := ParseDestinyRef(tc.in)
			if err != nil {
				t.Fatalf("ParseDestinyRef(%q) unexpected error: %v", tc.in, err)
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if ref != tc.wantRef {
				t.Errorf("ref = %q, want %q", ref, tc.wantRef)
			}
		})
	}
}

func TestParseDestinyRef_Invalid(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"no_at", "redis-base"},
		{"empty_name", "@v1.4.0"},
		{"empty_ref", "redis-base@"},
		{"leading_space", " redis-base@v1.4.0"},
		{"trailing_space", "redis-base@v1.4.0 "},
		// kebab-case: `_` запрещён в name (regex reDestinyName).
		{"underscore_name", "redis_base@v1.4.0"},
		// kebab-case: leading dash в name запрещён.
		{"leading_dash_name", "-redis@v1.4.0"},
		// kebab-case: trailing dash в name запрещён.
		{"trailing_dash_name", "redis-@v1.4.0"},
		// kebab-case: double-dash в name запрещён.
		{"double_dash_name", "red--is@v1.4.0"},
		// kebab-case: uppercase в name запрещён.
		{"upper_name", "Redis@v1.4.0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseDestinyRef(tc.in)
			if err == nil {
				t.Fatalf("ParseDestinyRef(%q): expected error, got nil", tc.in)
			}
			if !errors.Is(err, ErrInvalidDestinyRef) {
				t.Errorf("error = %v, want ErrInvalidDestinyRef-wrapped", err)
			}
		})
	}
}
