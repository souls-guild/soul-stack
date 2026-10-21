package cel

import (
	"reflect"
	"sort"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/common/types/traits"
)

// orderedAdapter is cel-go's type adapter, except that a Go map becomes a CEL map
// iterating its keys in sorted order ([orderedMap]). cel-go walks a Go map in Go's
// order, which is random, so a comprehension over one — `input.m.all(k, …)`,
// `vars.m.exists(…)` — failed on a different element, with a different text, from
// run to run, and `.map(…)` built its list in a different order. A list is rebuilt
// over this adapter too, so a map inside one is reached the same way.
//
// Every Engine's env adapts through it, which covers every root of the activation,
// and so does whatever this package hands to CEL itself ([orderedTypes]). A map
// literal is built by cel-go and is not covered. Neither is a map the Keeper
// substitutes into a flow-control predicate it renders for the Soul (`changed_when:`,
// `failed_when:`, `retry.until:`, a grouped child's `when:`): it travels as a literal
// ([predicateRenderer.mapLiteral]), and on the Soul that literal iterates in random
// order (NIM-1026).
type orderedAdapter struct{ types.Adapter }

// orderedMaps wraps the env's own adapter in [orderedAdapter].
func orderedMaps(e *cel.Env) (*cel.Env, error) {
	return cel.CustomTypeAdapter(orderedAdapter{e.CELTypeAdapter()})(e)
}

// orderedTypes adapts the values this package builds outside an env: the lazy `vars`,
// vault(), merge().
var orderedTypes types.Adapter = orderedAdapter{types.DefaultTypeAdapter}

func (a orderedAdapter) NativeToValue(value any) ref.Val {
	switch v := value.(type) {
	case ref.Val:
		return v
	case map[string]any:
		return orderedMap{types.NewStringInterfaceMap(a, v)}
	case map[string]string:
		return orderedMap{types.NewStringStringMap(a, v)}
	case map[ref.Val]ref.Val:
		return orderedMap{types.NewRefValMap(a, v)}
	case []any:
		return types.NewDynamicList(a, v)
	case []byte, []string, []ref.Val:
		return a.Adapter.NativeToValue(value)
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Map:
		return orderedMap{types.NewDynamicMap(a, value)}
	case reflect.Slice, reflect.Array:
		if rv.Type().Elem().Kind() != reflect.Uint8 {
			return types.NewDynamicList(a, value)
		}
	}
	return a.Adapter.NativeToValue(value)
}

// orderedMap is a CEL map whose Iterator yields its keys sorted ([keyLess]). Only the
// iteration sorts: a field access costs what it did.
type orderedMap struct{ traits.Mapper }

func (m orderedMap) Iterator() traits.Iterator {
	var keys []ref.Val
	for it := m.Mapper.Iterator(); it.HasNext() == types.True; {
		keys = append(keys, it.Next())
	}
	sort.Slice(keys, func(i, j int) bool { return keyLess(keys[i], keys[j]) })
	return types.NewRefValList(types.DefaultTypeAdapter, keys).Iterator()
}
