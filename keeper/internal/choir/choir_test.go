package choir

import (
	"strings"
	"testing"
)

func TestValidChoirName(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"redis_primary", true},
		{"redis-replica", true},
		{"workers", true},
		{"a", true},
		{"a1", true},
		{"frontends-01", true},
		{"", false},
		{"Redis", false},     // uppercase
		{"1node", false},     // ведущая цифра
		{"-leading", false},  // ведущий дефис
		{"_leading", false},  // ведущий underscore
		{"has space", false}, // пробел
		{"dot.name", false},  // точка вне формата
	}
	for _, tc := range cases {
		if got := ValidChoirName(tc.name); got != tc.ok {
			t.Errorf("ValidChoirName(%q) = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

func TestValidateSizeBounds(t *testing.T) {
	i := func(n int) *int { return &n }
	cases := []struct {
		label   string
		min     *int
		max     *int
		wantErr bool
	}{
		{"both nil", nil, nil, false},
		{"min only", i(1), nil, false},
		{"max only", nil, i(5), false},
		{"min <= max", i(2), i(5), false},
		{"min == max", i(3), i(3), false},
		{"min > max", i(5), i(2), true},
		{"min zero", i(0), nil, true},
		{"max zero", nil, i(0), true},
		{"min negative", i(-1), nil, true},
	}
	for _, tc := range cases {
		err := validateSizeBounds(tc.min, tc.max)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: validateSizeBounds err=%v, wantErr=%v", tc.label, err, tc.wantErr)
		}
	}
}

func TestErrNotMembers_Error(t *testing.T) {
	e := &ErrNotMembers{Incarnation: "service-redis", Missing: []string{"a", "b"}}
	got := e.Error()
	if got == "" {
		t.Fatal("ErrNotMembers.Error() empty")
	}
	// Должно нести имя инкарнации и список SID-ов.
	for _, frag := range []string{"service-redis", "a", "b"} {
		if !strings.Contains(got, frag) {
			t.Errorf("Error() %q missing %q", got, frag)
		}
	}
}
