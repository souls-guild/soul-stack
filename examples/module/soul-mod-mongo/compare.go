// Comparing a DECLARED document with a LIVE one — the thing every idempotent
// action in this artifact does, and the thing that decides whether `changed=false`
// is a fact or a lie.
//
// The two sides arrive in different shapes and neither is canonical. A declared
// value comes from `structpb` through [valueToNative] as `map[string]any` /
// `[]any` / int64 / float64 / string / bool; a live one comes off the wire as
// `bson.D` / `bson.A` / int32 / int64 / double. Comparing them directly reports a
// difference on every apply, and a step that always says it changed something is
// one an operator stops reading.
//
// [canonicalValue] renders either shape into one string, and two rules in it are
// deliberate:
//
//   - MAP KEYS ARE SORTED. A validator predicate means the same thing whichever
//     order its keys came back in, so ordering them would report a change that is
//     not one. ARRAY ORDER IS KEPT, because there it is meaning — `$and: [a, b]`
//     and an index key are both order-bearing.
//   - NUMBERS COMPARE BY VALUE, not by width. `1` written in YAML reaches here as
//     int64 and comes back from mongod as int32; treating those as different would
//     make every apply a change. The int/double distinction that mongo does care
//     about is preserved on the WRITE path ([valueToNative]) — this is the read
//     path, and its only job is to answer whether anything moved.
package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// canonicalValue renders a declared or live value into the one string form the two
// can be compared in. Unknown types fall back to %v, which is a comparison that may
// be too strict but is never too loose — the direction to fail in.
func canonicalValue(v any) string {
	var b strings.Builder
	writeCanonical(&b, v)
	return b.String()
}

func writeCanonical(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")

	case bson.D:
		pairs := make([][2]string, 0, len(t))
		for _, e := range t {
			var vb strings.Builder
			writeCanonical(&vb, e.Value)
			pairs = append(pairs, [2]string{e.Key, vb.String()})
		}
		writeSortedPairs(b, pairs)

	case bson.M:
		pairs := make([][2]string, 0, len(t))
		for k, val := range t {
			var vb strings.Builder
			writeCanonical(&vb, val)
			pairs = append(pairs, [2]string{k, vb.String()})
		}
		writeSortedPairs(b, pairs)

	case map[string]any:
		pairs := make([][2]string, 0, len(t))
		for k, val := range t {
			var vb strings.Builder
			writeCanonical(&vb, val)
			pairs = append(pairs, [2]string{k, vb.String()})
		}
		writeSortedPairs(b, pairs)

	case bson.A:
		writeCanonicalSlice(b, t)
	case []any:
		writeCanonicalSlice(b, t)

	case string:
		b.WriteString(strconv.Quote(t))
	case bool:
		b.WriteString(strconv.FormatBool(t))

	case int, int32, int64, float32, float64:
		b.WriteString(strconv.FormatFloat(asFloat(t), 'g', -1, 64))

	case primitive.ObjectID:
		b.WriteString(t.Hex())

	default:
		fmt.Fprintf(b, "%v", t)
	}
}

func writeCanonicalSlice(b *strings.Builder, items []any) {
	b.WriteByte('[')
	for i, e := range items {
		if i > 0 {
			b.WriteByte(',')
		}
		writeCanonical(b, e)
	}
	b.WriteByte(']')
}

// writeSortedPairs is where the map-key rule lives: sorted, so two documents that
// mean the same thing render the same.
func writeSortedPairs(b *strings.Builder, pairs [][2]string) {
	sort.Slice(pairs, func(i, j int) bool { return pairs[i][0] < pairs[j][0] })
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(p[0]))
		b.WriteByte(':')
		b.WriteString(p[1])
	}
	b.WriteByte('}')
}

// sameValue is the whole of a field-level diff, when the declared document is the
// WHOLE truth about that field. A validator is like this: one that lost a clause
// out of band has really changed.
func sameValue(live, want any) bool {
	return canonicalValue(live) == canonicalValue(want)
}

// matchesDeclared is the diff for a field mongod NORMALIZES by filling in its own
// defaults — `collation`, `timeseries`, `clusteredIndex`. A declared
// `{ locale: "en" }` comes back as a document with eight more keys in it, and
// comparing those whole would report a difference on every apply and, worse, report
// it on an IMMUTABLE field, where this artifact's answer is to fail. So only the
// keys the operator wrote are compared, recursively.
//
// It is deliberately not the default: on a field the server does not fill in, a
// subset comparison would call a live document equal to a declared one that is
// missing half of it.
func matchesDeclared(live, want any) bool {
	wantMap, ok := asStringMap(want)
	if !ok {
		return sameValue(live, want)
	}
	liveMap, ok := asStringMap(live)
	if !ok {
		return false
	}
	for k, wv := range wantMap {
		lv, present := liveMap[k]
		if !present || !matchesDeclared(lv, wv) {
			return false
		}
	}
	return true
}

// asStringMap normalizes the map shapes either side can arrive in.
func asStringMap(v any) (map[string]any, bool) {
	switch t := v.(type) {
	case map[string]any:
		return t, true
	case bson.M:
		return t, true
	case bson.D:
		out := make(map[string]any, len(t))
		for _, e := range t {
			out[e.Key] = e.Value
		}
		return out, true
	default:
		return nil, false
	}
}

// orderedKeyString renders an ORDER-BEARING document — an index key — into a
// comparable string.
//
// [canonicalValue] must not be used here and this is the one place that matters:
// it sorts map keys, and an index on { a: 1, b: -1 } is a DIFFERENT index from one
// on { b: -1, a: 1 }. Sorting them would make the two compare equal and report a
// wrong index as converged.
func orderedKeyString(pairs [][2]any) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Quote(fmt.Sprint(p[0])))
		b.WriteByte(':')
		writeCanonical(&b, p[1])
	}
	b.WriteByte('}')
	return b.String()
}

// rawToNative decodes a bson sub-document or value into the plain Go shapes
// [canonicalValue] reads, so a live document and a declared one meet in one form.
func rawToNative(v bson.RawValue) any {
	var out any
	if err := v.Unmarshal(&out); err != nil {
		return nil
	}
	return out
}

// lookupNative reads one field out of a live document in native form, reporting
// whether it was there at all — which is not the same as it being null.
func lookupNative(doc bson.Raw, key string) (any, bool) {
	v, err := doc.LookupErr(key)
	if err != nil {
		return nil, false
	}
	return rawToNative(v), true
}
