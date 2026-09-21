package cmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestTargetFlagsParsingCSV(t *testing.T) {
	tf := targetFlags{
		SIDs:  "host1, host2 ,,host3",
		Coven: "prod-eu,dc1",
	}
	got, err := tf.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !reflect.DeepEqual(got.SIDs, []string{"host1", "host2", "host3"}) {
		t.Errorf("SIDs: got %v", got.SIDs)
	}
	if !reflect.DeepEqual(got.Coven, []string{"prod-eu", "dc1"}) {
		t.Errorf("Coven: got %v", got.Coven)
	}
	if got.Where != "" {
		t.Errorf("Where should be empty: %q", got.Where)
	}
}

func TestTargetFlagsTranslateGlob(t *testing.T) {
	tf := targetFlags{Glob: "web-*"}
	got, _ := tf.resolve()
	want := `sid.glob("web-*")`
	if got.Where != want {
		t.Errorf("Glob→CEL: got %q want %q", got.Where, want)
	}
}

func TestTargetFlagsTranslateRegex(t *testing.T) {
	tf := targetFlags{Regex: `host-[0-9]+`}
	got, _ := tf.resolve()
	want := `sid.matches("host-[0-9]+")`
	if got.Where != want {
		t.Errorf("Regex→CEL: got %q want %q", got.Where, want)
	}
}

func TestTargetFlagsAndMerge(t *testing.T) {
	tf := targetFlags{
		Glob:  "web-*",
		Regex: `host-[0-9]+`,
		Where: `soulprint.self.os.family == "debian"`,
	}
	got, _ := tf.resolve()
	want := `sid.glob("web-*") && sid.matches("host-[0-9]+") && (soulprint.self.os.family == "debian")`
	if got.Where != want {
		t.Errorf("AND-merge:\n got  %s\n want %s", got.Where, want)
	}
}

func TestTargetFlagsQuoteEscape(t *testing.T) {
	tf := targetFlags{Glob: `web-"prod"`}
	got, _ := tf.resolve()
	if !strings.Contains(got.Where, `sid.glob("web-\"prod\"")`) {
		t.Errorf("quote-escape: %q", got.Where)
	}
}

func TestTargetFlagsEmptyHasAny(t *testing.T) {
	r, _ := (targetFlags{}).resolve()
	if r.hasAny() {
		t.Error("empty target should have hasAny()==false")
	}
	r2, _ := (targetFlags{SIDs: "host1"}).resolve()
	if !r2.hasAny() {
		t.Error("SIDs-only target should have hasAny()==true")
	}
}

func TestTargetFlagsRequire(t *testing.T) {
	r, _ := (targetFlags{}).resolve()
	if err := r.require(); err == nil {
		t.Error("require() should fail on empty target")
	}
	r2, _ := (targetFlags{Where: "x"}).resolve()
	if err := r2.require(); err != nil {
		t.Errorf("require() should not fail: %v", err)
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b , c ", []string{"a", "b", "c"}},
		{",,", nil},
		{"a,,b", []string{"a", "b"}},
	}
	for _, tc := range cases {
		got := splitCSV(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCSV(%q): got %v want %v", tc.in, got, tc.want)
		}
	}
}
