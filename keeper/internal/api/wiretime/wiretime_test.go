package wiretime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

var saoPaulo = time.FixedZone("-03", -3*60*60)

// at is 2026-10-30T14:00:00-03:00, the schedule_at the NIM-1133 demo showed.
var at = time.Date(2026, 10, 30, 14, 0, 0, 0, saoPaulo)

const atUTC = `"2026-10-30T17:00:00Z"`

type inner struct {
	At time.Time `json:"at"`
}

type hidden struct {
	Seen   time.Time `json:"seen"`
	unseen time.Time
}

type selfMarshaled struct {
	At time.Time
}

func (s selfMarshaled) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]time.Time{"self": s.At})
}

type node struct {
	At       time.Time `json:"at"`
	Children []node    `json:"children"`
}

type shapes struct {
	Direct    time.Time            `json:"direct"`
	Ptr       *time.Time           `json:"ptr"`
	NilPtr    *time.Time           `json:"nil_ptr"`
	Inner     inner                `json:"inner"`
	InnerPtr  *inner               `json:"inner_ptr"`
	Slice     []time.Time          `json:"slice"`
	Array     [1]time.Time         `json:"array"`
	Map       map[string]time.Time `json:"map"`
	Any       any                  `json:"any"`
	AnyMap    map[string]any       `json:"any_map"`
	Self      selfMarshaled        `json:"self_marshaled"`
	Tree      node                 `json:"tree"`
	Raw       json.RawMessage      `json:"raw"`
	Zero      time.Time            `json:"zero"`
	Promoted  struct{ time.Time }  `json:"promoted"`
	hidden                         // fields promoted from an unexported embedded struct
	Untouched string               `json:"untouched"`
}

func filled() shapes {
	// Ptr and AnyMap's "deep" share one pointer: a pointer reached on two paths
	// is moved on both, not taken for a cycle.
	ptr := at
	return shapes{
		Direct:   at,
		Ptr:      &ptr,
		Inner:    inner{At: at},
		InnerPtr: &inner{At: at},
		Slice:    []time.Time{at, at},
		Array:    [1]time.Time{at},
		Map:      map[string]time.Time{"k": at},
		Any:      at,
		AnyMap:   map[string]any{"k": at, "nested": []any{at, map[string]any{"deep": &ptr}}},
		Self:     selfMarshaled{At: at},
		Tree:     node{At: at, Children: []node{{At: at}}},
		Raw:      json.RawMessage(`{"kept":"2026-10-30T14:00:00-03:00"}`),
		Promoted: struct{ time.Time }{at},
		hidden:   hidden{Seen: at, unseen: at},
	}
}

func TestUTC_EveryShapeEncodesInUTC(t *testing.T) {
	out, err := json.Marshal(UTC(filled()))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	// 17 reachable times: direct, ptr, inner, inner_ptr, slice×2, array, map,
	// any, any_map×3, self_marshaled, tree×2, promoted, seen.
	if n := strings.Count(got, atUTC); n != 17 {
		t.Errorf("UTC instants in the answer = %d, want 17:\n%s", n, got)
	}
	// The one offset left is inside the RawMessage, which was encoded before.
	if n := strings.Count(got, "-03:00"); n != 1 {
		t.Errorf("offsets left in the answer = %d, want 1 (the RawMessage):\n%s", n, got)
	}
	if !strings.Contains(got, `"zero":"0001-01-01T00:00:00Z"`) {
		t.Errorf("zero time changed:\n%s", got)
	}
}

func TestUTC_LeavesTheOriginalAlone(t *testing.T) {
	src := filled()
	before, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = UTC(src)
	_ = UTC(&src)
	after, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("UTC wrote into its argument:\nbefore %s\nafter  %s", before, after)
	}
	if src.Ptr.Location() != saoPaulo || src.hidden.unseen.Location() != saoPaulo {
		t.Fatal("UTC moved a time it does not own")
	}
}

func TestUTC_KeepsTheInstant(t *testing.T) {
	got, ok := UTC(at).(time.Time)
	if !ok || !got.Equal(at) || got.Location() != time.UTC {
		t.Fatalf("UTC(%v) = %v, want the same instant in UTC", at, got)
	}
}

type skipped struct {
	At     time.Time `json:"at"`
	Parent *skipped  `json:"-"`
}

func TestUTC_DoesNotFollowWhatJSONSkips(t *testing.T) {
	c := &skipped{At: at, Parent: &skipped{At: at}}
	got, ok := UTC(c).(*skipped)
	if !ok || got.At.Location() != time.UTC {
		t.Fatalf("UTC(skipped) = %#v, want the time in UTC", got)
	}
	if got.Parent != c.Parent || c.Parent.At.Location() != saoPaulo {
		t.Fatal("UTC walked into a json:\"-\" field")
	}
}

type headed struct {
	At   time.Time  `json:"at"`
	Head *headed    `json:"head"`
	Self *time.Time `json:"self"`
}

// Two paths can share an address without being a cycle: a slice and a pointer
// to its first element, a slice and its prefix, a struct and its first field.
// encoding/json writes both, and both are moved.
func TestUTC_TellsAnAliasFromACycle(t *testing.T) {
	items := []headed{{At: at}, {At: at}}
	items[1].Head = &items[0]
	items[0].Self = &items[0].At
	s := []any{at, nil}
	s[1] = s[:1]
	for name, v := range map[string]any{"pointer into a slice": items, "prefix of a slice": s} {
		out, err := json.Marshal(UTC(v))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.Contains(string(out), "-03:00") {
			t.Errorf("%s: an offset survived: %s", name, out)
		}
	}
}

type twin struct {
	At   time.Time `json:"at"`
	A, B *twin
}

// A value that leads back into itself is one encoding/json refuses, so UTC
// hands it back unchanged for the encoder to refuse — promptly, even where
// every node has two ways forward.
func TestUTC_GivesUpOnACycle(t *testing.T) {
	ring := make([]*twin, 40)
	for i := range ring {
		ring[i] = &twin{At: at}
	}
	for i, n := range ring {
		n.A, n.B = ring[(i+1)%len(ring)], ring[(i+1)%len(ring)]
	}
	m := map[string]any{"at": at}
	m["self"] = m
	s := []any{at, nil}
	s[1] = s

	done := make(chan struct{})
	go func() {
		defer close(done)
		if got := UTC(ring[0]); got != any(ring[0]) {
			t.Errorf("UTC(a pointer cycle) = %v, want it back unchanged", got)
		}
		if got := UTC(m).(map[string]any); reflect.ValueOf(got).Pointer() != reflect.ValueOf(m).Pointer() {
			t.Error("UTC(a map cycle) was copied")
		}
		if got := UTC(s).([]any); &got[0] != &s[0] {
			t.Error("UTC(a slice cycle) was copied")
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("UTC did not return on a cyclic value")
	}
}

func TestUTC_ReturnsWhatHasNoOffsetAsIs(t *testing.T) {
	// A Keeper running in UTC: time.Local is not the time.UTC pointer, yet its
	// zero offset is already written as Z, so nothing is copied.
	inUTC := time.Date(2026, 10, 30, 17, 0, 0, 0, time.FixedZone("Local", 0))
	payload := map[string]any{"state": map[string]any{"port": 6379.0}, "at": inUTC}
	src := shapes{Direct: inUTC, AnyMap: payload}
	got, ok := UTC(src).(shapes)
	if !ok || reflect.ValueOf(got.AnyMap).Pointer() != reflect.ValueOf(payload).Pointer() {
		t.Fatal("a value with no offset to move was copied")
	}
	// The same map beside a time that does move is still not copied.
	src.Direct = at
	got = UTC(src).(shapes)
	if got.Direct.Location() != time.UTC || reflect.ValueOf(got.AnyMap).Pointer() != reflect.ValueOf(payload).Pointer() {
		t.Fatal("a part with nothing to move was copied along with one that had")
	}
}

func TestUTC_ReturnsATimelessValueAsIs(t *testing.T) {
	raw := json.RawMessage(`{"a":1}`)
	if got := UTC(raw); &got.(json.RawMessage)[0] != &raw[0] {
		t.Fatal("a value that cannot hold a time was copied")
	}
	if UTC(nil) != nil {
		t.Fatal("UTC(nil) != nil")
	}
}
