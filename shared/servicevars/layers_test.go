package servicevars

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/cel"
)

// TestLayers_LateBindingAcrossFiles — point 1 and point 5 inside the service
// rung: a definition reads `vars.<other>` through the whole view, so the value it
// gets is the top one, whichever file defines it and in whichever order the
// files sort. Declaration order never matters.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: evaluate each file while
// it is assembled — in lexical(), Materialize a view of the layers so far plus
// this one and store the values as its Defs — and "definition below the
// override" reads /etc/redis.
func TestLayers_LateBindingAcrossFiles(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"definition below the override": {
			"00-base.yaml": "conf_dir: /etc/redis\nacl_file: \"${ vars.conf_dir }/users.acl\"\n",
			"10-wb.yaml":   "conf_dir: /opt/redis\n",
		},
		"definition above the value": {
			"00-base.yaml": "conf_dir: /opt/redis\n",
			"10-acl.yaml":  "acl_file: \"${ vars.conf_dir }/users.acl\"\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := view(t, snapshot(files), IncarnationContext{})
			if got := lookup(t, v, "acl_file"); got != "/opt/redis/users.acl" {
				t.Fatalf("acl_file = %#v, want /opt/redis/users.acl", got)
			}
		})
	}
}

// TestLayers_SelfReferenceReadsTheFileBelow — point 9: inside the service rung
// "below" is file order, so `conf_dir: "${ vars.conf_dir }/wb"` in a later file
// reads the earlier file's conf_dir, and with nothing below it is a cycle rather
// than an absent name.
func TestLayers_SelfReferenceReadsTheFileBelow(t *testing.T) {
	v := view(t, snapshot(map[string]string{
		"00-base.yaml": "conf_dir: /etc/redis\n",
		"10-wb.yaml":   "conf_dir: \"${ vars.conf_dir }/wb\"\n",
	}), IncarnationContext{})
	if got := lookup(t, v, "conf_dir"); got != "/etc/redis/wb" {
		t.Fatalf("conf_dir = %#v, want /etc/redis/wb", got)
	}

	alone := view(t, snapshot(map[string]string{
		"00-base.yaml": "conf_dir: \"${ vars.conf_dir }/wb\"\n",
	}), IncarnationContext{})
	if _, _, err := alone.Lookup("conf_dir"); !errors.Is(err, cel.ErrVarCycle) {
		t.Fatalf("a self-reference with nothing below must be var_cycle, got %v", err)
	}
}

// TestLayers_DefinitionsReadTheIncarnation — a service definition's environment
// is the incarnation and nothing else, in lexical mode as in a stack.
func TestLayers_DefinitionsReadTheIncarnation(t *testing.T) {
	v := view(t, snapshot(map[string]string{
		"00-base.yaml": "owner: \"${ incarnation.id }\"\ncovens: \"${ incarnation.covens }\"\n",
	}), IncarnationContext{ID: "redis-prod", Covens: []string{"cache"}})
	got, err := v.Materialize()
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	want := map[string]any{"owner": "redis-prod", "covens": []any{"cache"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// TestLayers_InvalidFileNameIsRefusedByName — a layer file whose name is not
// valid UTF-8 is not a valid fs path. The lexical scan is the one way to reach
// such a name — a `_stack.yaml` `file:` cannot spell one, the YAML decoder
// replaces the byte — and it refuses the file by name. Without the check one FS
// answered a bare "invalid argument" and another, this test's, reported the path
// missing, so the layer was skipped without a word.
func TestLayers_InvalidFileNameIsRefusedByName(t *testing.T) {
	_, err := Layers(snapshot(map[string]string{"00-base.yaml": "a: 1\n", "\xff-bad.yaml": "b: 2\n"}), IncarnationContext{}, nil)
	if err == nil {
		t.Fatal("a layer file with an invalid name must be refused, not skipped")
	}
	if !strings.Contains(err.Error(), `"vars/\xff-bad.yaml"`) || !strings.Contains(err.Error(), "not a valid path") {
		t.Fatalf("the error must name the file and say why: %v", err)
	}
}

// TestLayers_StrategyErrorNamesTheSmallestKey — of two malformed per-key
// `_strategy` entries the error names the same one every time, the smallest key:
// the same file must give the same error text on every run.
//
// HOW TO BREAK IT ON PURPOSE, in the form of real code: in takeLayerStrategy range
// the per-key map directly instead of over its sorted keys — this goes red within
// the repeats.
func TestLayers_StrategyErrorNamesTheSmallestKey(t *testing.T) {
	fsys := snapshot(map[string]string{"00-base.yaml": "_strategy: {b: bad2, a: bad1, c: bad3}\na: 1\nb: 2\nc: 3\n"})
	for range 50 {
		_, err := Layers(fsys, IncarnationContext{}, nil)
		if err == nil || !strings.Contains(err.Error(), "_strategy.a: bad1 is not deep or replace") {
			t.Fatalf("want the error on _strategy.a, got %v", err)
		}
	}
}

// TestLayers_ShareOneEngine — every assembly, stack parse and single layer uses
// the one package engine rather than building a CEL environment per call.
func TestLayers_ShareOneEngine(t *testing.T) {
	a, err := Layers(snapshot(map[string]string{"00-base.yaml": "a: 1\n"}), IncarnationContext{}, nil)
	if err != nil {
		t.Fatalf("Layers: %v", err)
	}
	b, err := LexicalLayers(snapshot(map[string]string{"00-base.yaml": "a: 1\n"}), IncarnationContext{}, nil)
	if err != nil {
		t.Fatalf("LexicalLayers: %v", err)
	}
	c, err := NewLayer("fixtures.vars", map[string]any{"a": 1}, IncarnationContext{})
	if err != nil {
		t.Fatalf("NewLayer: %v", err)
	}
	if a[0].Engine == nil || a[0].Engine != b[0].Engine || a[0].Engine != c.Engine {
		t.Fatalf("engines differ across calls: %p %p %p", a[0].Engine, b[0].Engine, c.Engine)
	}
}

// TestNewLayer_IsOneServiceLayer — the L0 harness's `fixtures.vars` stands for the
// whole rung: its definitions, nested ones included, are evaluated on request
// against the case's incarnation.
func TestNewLayer_IsOneServiceLayer(t *testing.T) {
	layer, err := NewLayer("fixtures.vars", map[string]any{
		"owner":  "${ incarnation.id }-x",
		"pkg":    map[string]any{"repo": "${ vars.mirror }/deb"},
		"mirror": "https://m.internal",
	}, IncarnationContext{ID: "case-1"})
	if err != nil {
		t.Fatalf("NewLayer: %v", err)
	}
	v := cel.NewVarView(nil, layer)
	if got := lookup(t, v, "owner"); got != "case-1-x" {
		t.Errorf("owner = %#v", got)
	}
	if got := lookup(t, v, "pkg"); !reflect.DeepEqual(got, map[string]any{"repo": "https://m.internal/deb"}) {
		t.Errorf("pkg = %#v", got)
	}
}
