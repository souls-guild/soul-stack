// Package wiretime moves every instant an answer carries to UTC before it is
// encoded, so how a REST or MCP answer spells a time does not depend on the
// zone the Keeper process runs in (NIM-1160).
//
// The zone comes in from more than one side: pgx scans timestamptz into
// time.Local, time.Now is in time.Local, and a time a client sent in a typed
// field keeps the client's offset when it is echoed back. encoding/json writes
// whatever location a time.Time carries. Normalizing at the encoder covers all
// of them without knowing which field came from where.
package wiretime

import (
	"reflect"
	"sync"
	"time"
)

var timeType = reflect.TypeFor[time.Time]()

// UTC returns v with every time.Time that encoding/json would write with an
// offset moved to UTC; the instant is unchanged. v is never written to: only the
// parts holding such a time are copied, so a body built from a cached or shared
// value leaves that value as it was, and a body with nothing to move — any
// answer of a Keeper running in UTC — comes back as it went in. So does a value
// that leads back into itself, which encoding/json refuses anyway.
//
// Not reached: a time in an unexported or json:"-" field that a custom
// MarshalJSON reads, a time embedded under an unexported name or behind an
// embedded pointer to an unexported struct type, a time used as a map key, and
// a time already encoded into a json.RawMessage or a string.
func UTC(v any) any {
	if v == nil {
		return nil
	}
	rv := reflect.ValueOf(v)
	w := &walk{}
	if !w.stale(rv) || w.cyclic {
		return v
	}
	cp := reflect.New(rv.Type()).Elem()
	cp.Set(rv)
	w.fix(cp)
	if w.cyclic {
		return v
	}
	return cp.Interface()
}

// walk is one UTC call. path holds the pointers, slices and maps being walked
// through; meeting one of them again is a cycle, which ends the walk.
type walk struct {
	path   []visit
	cyclic bool
}

// visit identifies a pointer, slice or map the way encoding/json does when it
// looks for a cycle: a pointer by its type and address, a slice by its address
// and length, a map by its address. A struct and its first field, or a slice
// and its prefix, share an address and are still told apart.
type visit struct {
	t reflect.Type
	p uintptr
	n int
}

func visitOf(v reflect.Value) visit {
	switch v.Kind() {
	case reflect.Pointer:
		return visit{t: v.Type(), p: v.Pointer()}
	case reflect.Slice:
		return visit{p: v.Pointer(), n: v.Len()}
	default:
		return visit{p: v.Pointer(), n: -1}
	}
}

func (w *walk) enter(k visit) bool {
	for _, on := range w.path {
		if on == k {
			w.cyclic = true
			return false
		}
	}
	w.path = append(w.path, k)
	return true
}

func (w *walk) leave() {
	w.path = w.path[:len(w.path)-1]
}

// stale reports whether encoding/json would write a time under v with an
// offset; a zero offset is already written as Z.
func (w *walk) stale(v reflect.Value) bool {
	if w.cyclic {
		return false
	}
	t := v.Type()
	if t == timeType {
		if !v.CanInterface() {
			return false
		}
		_, off := v.Interface().(time.Time).Zone()
		return off != 0
	}
	if !mayHoldTime(t) {
		return false
	}
	switch t.Kind() {
	case reflect.Interface:
		return !v.IsNil() && w.stale(v.Elem())
	case reflect.Pointer:
		if v.IsNil() || !w.enter(visitOf(v)) {
			return false
		}
		defer w.leave()
		return w.stale(v.Elem())
	case reflect.Struct:
		for i := range t.NumField() {
			if encoded(t.Field(i)) && w.stale(v.Field(i)) {
				return true
			}
		}
	case reflect.Slice:
		if v.Len() == 0 || !w.enter(visitOf(v)) {
			return false
		}
		defer w.leave()
		return w.anyStale(v)
	case reflect.Array:
		return w.anyStale(v)
	case reflect.Map:
		if v.Len() == 0 || !w.enter(visitOf(v)) {
			return false
		}
		defer w.leave()
		for it := v.MapRange(); it.Next(); {
			if w.stale(it.Value()) {
				return true
			}
		}
	}
	return false
}

func (w *walk) anyStale(v reflect.Value) bool {
	for i := range v.Len() {
		if w.stale(v.Index(i)) {
			return true
		}
	}
	return false
}

// fix moves the stale times under v to UTC. v is addressable and belongs to
// the copy; whatever it shares by reference — a pointee, a slice's backing
// array, a map, an interface's value — is copied before it is written to, and
// only when it holds a stale time.
func (w *walk) fix(v reflect.Value) {
	if w.cyclic {
		return
	}
	t := v.Type()
	if t == timeType {
		if v.CanSet() && w.stale(v) {
			v.Set(reflect.ValueOf(v.Interface().(time.Time).UTC()))
		}
		return
	}
	if !mayHoldTime(t) || (t.Kind() != reflect.Struct && !v.CanSet()) {
		return
	}
	switch t.Kind() {
	case reflect.Interface:
		if v.IsNil() || !w.stale(v.Elem()) {
			return
		}
		c := reflect.New(v.Elem().Type()).Elem()
		c.Set(v.Elem())
		w.fix(c)
		v.Set(c)
	case reflect.Pointer, reflect.Slice, reflect.Map:
		if !w.stale(v) || !w.enter(visitOf(v)) {
			return
		}
		defer w.leave()
		v.Set(w.fixedCopy(v))
	case reflect.Struct:
		// An embedded unexported struct cannot be set as a whole, but encoding/json
		// writes its exported fields, and reflect lets those be set one level down.
		for i := range t.NumField() {
			if encoded(t.Field(i)) {
				w.fix(v.Field(i))
			}
		}
	case reflect.Array:
		for i := range v.Len() {
			w.fix(v.Index(i))
		}
	}
}

// fixedCopy copies what the pointer, slice or map v holds and fixes the copy.
func (w *walk) fixedCopy(v reflect.Value) reflect.Value {
	t := v.Type()
	switch t.Kind() {
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		p.Elem().Set(v.Elem())
		w.fix(p.Elem())
		return p
	case reflect.Slice:
		c := reflect.MakeSlice(t, v.Len(), v.Len())
		reflect.Copy(c, v)
		for i := range c.Len() {
			w.fix(c.Index(i))
		}
		return c
	default:
		c := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			e := reflect.New(t.Elem()).Elem()
			e.Set(it.Value())
			w.fix(e)
			c.SetMapIndex(it.Key(), e)
		}
		return c
	}
}

// encoded reports whether encoding/json writes the field, or the fields it
// promotes.
func encoded(f reflect.StructField) bool {
	return (f.IsExported() || f.Anonymous) && f.Tag.Get("json") != "-"
}

var holds sync.Map // reflect.Type → bool

// mayHoldTime reports whether a value of type t can reach a time.Time the way
// encoding/json walks it. An interface can hold anything, so it always may.
func mayHoldTime(t reflect.Type) bool {
	if r, ok := holds.Load(t); ok {
		return r.(bool)
	}
	r := scan(t, map[reflect.Type]bool{})
	holds.Store(t, r)
	return r
}

func scan(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == timeType {
		return true
	}
	if seen[t] {
		// A recursive type: the visit already in progress scans the remaining fields.
		return false
	}
	seen[t] = true
	switch t.Kind() {
	case reflect.Interface:
		return true
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return scan(t.Elem(), seen)
	case reflect.Struct:
		for i := range t.NumField() {
			if f := t.Field(i); encoded(f) && scan(f.Type, seen) {
				return true
			}
		}
	}
	return false
}
