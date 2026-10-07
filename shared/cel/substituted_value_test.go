package cel

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// The Keeper substitutes values into a predicate as literals (predicate-rendering.md
// §5). A literal is data: `vault(`, `now(` and `generate_secret(` inside one must not
// fail the predicate, in any literal form, beside a comment or a backtick name.
func TestGuard_LiteralContentIsData(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	vars := Vars{Register: map[string]any{"self": map[string]any{"stdout": "x"}}}
	for _, expr := range []string{
		`register.self.stdout != dyn("vault('secret/x')")`,
		`register.self.stdout != 'now()'`,
		`register.self.stdout != "generate_secret(32)"`,
		`register.self.stdout != "a\" vault( b"`,
		`register.self.stdout != """a "vault(" b"""`,
		`register.self.stdout != r'now() '`,
		`register.self.stdout != b"generate_secret(" ? true : true`,
		`register.self.stdout != ` + strconv.Quote(`p"w vault('x') now() generate_secret(1)`),
		"register.self.stdout != 'y' // vault(\n",
		"{'a//b': 1}.`a//b` == 1 && register.self.stdout != 'now()'",
	} {
		ok, err := e.EvalPredicate(expr, vars)
		if err != nil || !ok {
			t.Errorf("%q: got (%v, %v), want (true, nil) — the guard read a literal as a call", expr, ok, err)
		}
	}
}

// The same calls as code are still refused: global, member, after a leading dot,
// inside a comprehension, and after text a text scan would misjudge.
func TestGuard_CallsAreStillRefused(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	for _, tc := range []struct{ expr, feature string }{
		{`now() > timestamp("2020-01-01T00:00:00Z")`, "now()"},
		{`register.self.now() == 1`, "now()"},
		{`.now() > timestamp("2020-01-01T00:00:00Z")`, "now()"},
		{`[1].exists(i, now() > timestamp("2020-01-01T00:00:00Z"))`, "now()"},
		{`vault('secret/x') == "y"`, "vault(...)"},
		{`generate_secret(32) == "y"`, generateSecretFeature},
		{`r"a\" == vault('x') || "b" == register.self.stdout`, "vault(...)"},
		{"{'a//b': 1}.`a//b` == 1 && vault('x') == 'y'", "vault(...)"},
		{"true // it's\n && vault('x') == 'y'", "vault(...)"},
	} {
		_, err := e.EvalPredicate(tc.expr, Vars{})
		var ue *ErrUnsupported
		if !errors.As(err, &ue) || ue.Feature != tc.feature {
			t.Errorf("%q: got %T %v, want *ErrUnsupported %s", tc.expr, err, err, tc.feature)
		}
	}
}

// A `.where("…")` predicate is a literal to the parser but code once rewritten, so a
// call in it is refused — and a literal in it is still data.
func TestGuard_WherePredicateCallsCount(t *testing.T) {
	e := newEngine(t)
	_, err := e.EvalExpression(`soulprint.hosts.where("now() > timestamp('2020-01-01T00:00:00Z')").size()`, scenarioVars())
	var ue *ErrUnsupported
	if !errors.As(err, &ue) || ue.Feature != "now()" {
		t.Fatalf("now() inside a .where predicate: got %T %v, want *ErrUnsupported now()", err, err)
	}
	out, err := e.EvalExpression(`soulprint.hosts.where("role == 'now()'").size()`, scenarioVars())
	if err != nil {
		t.Fatalf("a literal inside a .where predicate was read as a call: %v", err)
	}
	if out.Value() != int64(0) {
		t.Fatalf("size = %v, want 0", out.Value())
	}
}

// Text the parser refuses keeps today's refusal, matched over the raw text.
func TestGuard_UnparseableTextKeepsTheTextGuard(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	for _, tc := range []struct{ expr, feature string }{
		{`now() ===`, "now()"},
		{`vault('secret/x') ===`, "vault(...)"},
		{`generate_secret(1) ===`, generateSecretFeature},
	} {
		_, err = e.EvalPredicate(tc.expr, Vars{})
		var ue *ErrUnsupported
		if !errors.As(err, &ue) || ue.Feature != tc.feature {
			t.Errorf("%q: got %T %v, want *ErrUnsupported %s", tc.expr, err, err, tc.feature)
		}
	}
}

// normalize trims the edges and nothing else, so the engine compiles the program the
// text spells. Each case is one a whitespace-collapsing normalize changed: a literal
// after an escaped quote, the newline that ends a comment, a backtick name.
func TestNormalize_CompilesTheTextAsWritten(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	stdout := "a\"  b"
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{`register.self.stdout == ` + strconv.Quote(stdout), true},
		{"true // first term\n   && false", false},
		{"{'a  b': 1}.`a  b` == 1", true},
		{"  register.self.stdout != 'x'\n", true},
	} {
		got, err := e.EvalPredicate(tc.expr, Vars{Register: map[string]any{"self": map[string]any{"stdout": stdout}}})
		if err != nil || got != tc.want {
			t.Errorf("%q: got (%v, %v), want (%v, nil)", tc.expr, got, err, tc.want)
		}
	}
	if got := normalize("\t a  &&\n b \n"); got != "a  &&\n b" {
		t.Errorf("normalize = %q, want the edges trimmed and the inside untouched", got)
	}
}

// RedactedReason is what the Soul prints for a predicate error: the class, and none
// of the input — not the expression, and not cel-go's detail, which quotes the
// source on a compile error and the key on a lookup that misses.
func TestRedactedReason_CarriesNoInput(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	const secret = "S3CR3T-nim929"
	for _, tc := range []struct {
		expr, want string
	}{
		{`{"a": 1}[dyn("` + secret + `")] == 1`, "failed to evaluate"},
		{`"` + secret + `" ===`, "does not compile"},
		{`now() == "` + secret + `"`, "uses an unsupported construct: now()"},
		{`"` + secret + `"`, "did not return a bool"},
	} {
		_, err := e.EvalPredicate(tc.expr, Vars{})
		if err == nil || !strings.Contains(err.Error(), secret) {
			t.Fatalf("%q: the error must carry the secret for this check to mean anything, got %v", tc.expr, err)
		}
		var ee *ErrEval
		if tc.want == "failed to evaluate" && errors.As(err, &ee) && !strings.Contains(ee.Err.Error(), secret) {
			t.Fatalf("%q: cel-go's own detail must carry the secret too, or a reason that forwards it goes unseen: %v", tc.expr, ee.Err)
		}
		if got := RedactedReason(err); got != tc.want {
			t.Errorf("%q: RedactedReason = %q, want %q", tc.expr, got, tc.want)
		}
	}
}

// A rendered predicate is unique per host and run and holds its substituted values,
// so an Engine built WithoutCompileCache keeps none of it once evaluated.
func TestWithoutCompileCache_KeepsNothing(t *testing.T) {
	e, err := NewFlowControl(WithoutCompileCache())
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	vars := Vars{Register: map[string]any{"self": map[string]any{"stdout": "x"}}}
	for i := 0; i < 3; i++ {
		ok, err := e.EvalPredicate(`register.self.stdout != dyn("secret-`+strconv.Itoa(i)+`")`, vars)
		if err != nil || !ok {
			t.Fatalf("eval %d: got (%v, %v)", i, ok, err)
		}
	}
	if n := e.CachedPrograms(); n != 0 {
		t.Fatalf("CachedPrograms = %d, want 0", n)
	}
	cached, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	if _, err := cached.EvalPredicate(`register.self.stdout != "x"`, vars); err != nil {
		t.Fatal(err)
	}
	if cached.CachedPrograms() != 1 {
		t.Fatalf("a default Engine cached %d programs, want 1 — the count does not observe the cache", cached.CachedPrograms())
	}
}

// RegisterNames reads the parse tree: a `register.<name>` inside a literal — a value
// the Keeper substituted — names nothing, and text a quote-pairing regex misreads is
// read as the parser reads it.
func TestRegisterNames_ReadsTheTree(t *testing.T) {
	e, err := NewFlowControl()
	if err != nil {
		t.Fatalf("NewFlowControl: %v", err)
	}
	for _, tc := range []struct {
		expr string
		want []string
	}{
		{`register.self.x == dyn("a\" register.web b")`, []string{}},
		{`register.self.x == dyn('''a ' register.web ''')`, []string{}},
		{"register.self.x == 'y' // register.web\n", []string{}},
		{`register.probe.ok && register.hosts.db.x == 1 && .register.web.ok`, []string{"db", "probe", "web"}},
		{`register.hosts.size() > 0 && register.self.ok`, []string{}},
	} {
		got, ok := e.RegisterNames(tc.expr)
		if !ok || strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%q: got (%v, %v), want (%v, true)", tc.expr, got, ok, tc.want)
		}
	}
	if _, ok := e.RegisterNames(`register.web ===`); ok {
		t.Error("unparseable text reported ok")
	}
}
