package applyrun

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func ids(s string) []int {
	out := make([]int, len(s))
	for i, r := range s {
		out[i] = int(r)
	}
	return out
}

func applyEdits(a, b []int, edits []lineEdit) []int {
	var out []int
	pos := 0
	for _, e := range edits {
		out = append(out, a[pos:e.s]...)
		out = append(out, b[e.hs:e.he]...)
		pos = e.e
	}
	return append(out, a[pos:]...)
}

// The script is the shortest one, and the runs it reports turn a into b.
func TestDiffLines_ShortestScriptRebuildsTheTarget(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		check bool
		want  []lineEdit
	}{
		{"abc", "abc", true, nil},
		{"", "", true, nil},
		{"abc", "axc", true, []lineEdit{{1, 2, 1, 2}}},
		{"abc", "abxc", true, []lineEdit{{2, 2, 2, 3}}},
		{"abc", "ac", true, []lineEdit{{1, 2, 1, 1}}},
		{"", "ab", true, []lineEdit{{0, 0, 0, 2}}},
		{"ab", "", true, []lineEdit{{0, 2, 0, 0}}},
		{"abcabba", "cbabac", false, nil},
	} {
		budget := diffBudget
		a, b := ids(tc.a), ids(tc.b)
		got, ok := diffLines(a, b, maxDiffEdits, &budget)
		if !ok {
			t.Fatalf("%q→%q: gave up", tc.a, tc.b)
		}
		if tc.check && !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q→%q: edits = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if r := applyEdits(a, b, got); !slices.Equal(r, b) && !(len(r) == 0 && len(b) == 0) {
			t.Errorf("%q→%q: rebuilt %v", tc.a, tc.b, r)
		}
	}
	// Myers' own example: the shortest script is 5 edits.
	budget := diffBudget
	got, _ := diffLines(ids("abcabba"), ids("cbabac"), maxDiffEdits, &budget)
	edits := 0
	for _, e := range got {
		edits += (e.e - e.s) + (e.he - e.hs)
	}
	if edits != 5 {
		t.Errorf("abcabba→cbabac: %d edits in %v, want 5", edits, got)
	}
}

// ★ The bounds: past maxEdits, or past the shared budget, the diff gives up
// rather than spending what a hostile text would make it spend.
// Mutations: drop the budget check → the 3-step budget finishes; drop the edit
// bound → a host 600 edits away is diffed on an unlimited budget.
func TestDiffLines_GivesUpPastItsBounds(t *testing.T) {
	// Each changed line is two edits: 300 changes are 600, 200 are 400.
	spread := func(changes int) ([]int, []int) {
		a, b := make([]int, 3000), make([]int, 3000)
		for i := range a {
			a[i], b[i] = i, i
		}
		for c := 0; c < changes; c++ {
			b[c*10] = -1 - c
		}
		return a, b
	}
	unlimited := 1 << 40
	a300, b300 := spread(300)
	if _, ok := diffLines(a300, b300, maxDiffEdits, &unlimited); ok {
		t.Error("a host 600 edits from the base was diffed past maxDiffEdits")
	}
	a200, b200 := spread(200)
	if _, ok := diffLines(a200, b200, maxDiffEdits, &unlimited); !ok {
		t.Error("a host 400 edits from the base gave up within maxDiffEdits")
	}

	a, b := make([]int, 4000), make([]int, 4000)
	for i := range a {
		a[i], b[i] = i, i
		if i%2 == 1 {
			b[i] = -i
		}
	}
	budget := diffBudget
	if _, ok := diffLines(a, b, maxDiffEdits, &budget); ok {
		t.Error("a text differing on every other line diffed within maxDiffEdits")
	}
	small := 1000
	if _, ok := diffLines(ids("abcdefgh"), ids("hgfedcba"), maxDiffEdits, &small); !ok {
		t.Error("a short diff gave up on a 1000-step budget")
	}
	tiny := 3
	if _, ok := diffLines(ids("abcdefgh"), ids("hgfedcba"), maxDiffEdits, &tiny); ok {
		t.Error("a diff finished on a 3-step budget")
	}
}

// Random texts of a few hundred lines: the runs always rebuild the target.
func TestDiffLines_RebuildsLongRandomTexts(t *testing.T) {
	rng := rand.New(rand.NewSource(986))
	for iter := 0; iter < 200; iter++ {
		a := make([]int, 200+rng.Intn(400))
		for i := range a {
			a[i] = rng.Intn(20)
		}
		b := slices.Clone(a)
		for e := rng.Intn(30); e > 0; e-- {
			pos := rng.Intn(len(b) + 1)
			switch rng.Intn(3) {
			case 0:
				b = slices.Insert(b, pos, rng.Intn(20))
			case 1:
				if pos < len(b) {
					b = slices.Delete(b, pos, pos+1)
				}
			default:
				if pos < len(b) {
					b[pos] = 100 + rng.Intn(20)
				}
			}
		}
		budget := diffBudget
		edits, ok := diffLines(a, b, maxDiffEdits, &budget)
		if !ok {
			t.Fatalf("iter %d: gave up", iter)
		}
		if r := applyEdits(a, b, edits); !slices.Equal(r, b) {
			t.Fatalf("iter %d: rebuilt text differs", iter)
		}
	}
}

// A config whose hosts differ on every other line is compared whole, not diffed
// line by line on every poll.
func TestGroupParamsByHost_HostileTextIsAWholeValue(t *testing.T) {
	in := map[string]map[string]any{}
	for h := 0; h < 40; h++ {
		var b strings.Builder
		for i := 0; i < 4999; i++ {
			if i%2 == 0 {
				fmt.Fprintf(&b, "shared %d\n", i)
			} else {
				fmt.Fprintf(&b, "host %d line %d\n", h, i)
			}
		}
		in[fmt.Sprintf("h%02d", h)] = map[string]any{"t": b.String() + "end"}
	}
	got, err := GroupParamsByHost([]byte(`{"t":"<per-host>"}`), hostRows(t, 0, in))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Texts) != 0 || len(got.Cells) != 1 || got.Cells[0].Line != 0 || len(got.Cells[0].Values) != 40 {
		t.Errorf("cells = %d, texts = %d; want one whole-value cell with 40 values", len(got.Cells), len(got.Texts))
	}
}

// The budget is charged for every line compared and every trace slot allocated:
// abc→axc takes three rounds (3+5+7 trace slots) and seven comparison steps.
// Mutation: drop the trace-slot charge → 7.
func TestDiffLines_ChargesComparisonsAndTraceSlots(t *testing.T) {
	budget := 1000
	if _, ok := diffLines(ids("abc"), ids("axc"), maxDiffEdits, &budget); !ok {
		t.Fatal("gave up")
	}
	if spent := 1000 - budget; spent != 22 {
		t.Errorf("spent %d steps, want 22 (15 trace slots + 7 comparisons)", spent)
	}
}
