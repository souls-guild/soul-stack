package api

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/api/handlers"
	"github.com/souls-guild/soul-stack/keeper/internal/applyrun"
	"github.com/souls-guild/soul-stack/shared/api/wire"
)

func runStatusNames() []string {
	var out []string
	for _, s := range applyrun.RunStatuses() {
		out = append(out, string(s))
	}
	return out
}

// TestRunStatusWireEnumsAreTheClosedSet — the run status reaches the public schema
// through three `enum:` tags, written by hand. check-openapi only proves the
// committed spec matches those tags; nothing proved the tags match RunStatuses, so
// dropping partial_failed from one of them regenerated a clean spec that would
// reject the value the moment its writer (NIM-948) lands.
func TestRunStatusWireEnumsAreTheClosedSet(t *testing.T) {
	want := runStatusNames()
	for _, typ := range []reflect.Type{
		reflect.TypeFor[wire.RunSummaryEntry](),
		reflect.TypeFor[wire.RunDetailReply](),
		reflect.TypeFor[GlobalRunEntry](),
	} {
		f, ok := typ.FieldByName("Status")
		if !ok {
			t.Errorf("%s: no Status field", typ.Name())
			continue
		}
		if got := strings.Split(f.Tag.Get("enum"), ","); !slices.Equal(got, want) {
			t.Errorf("%s.Status enum = %v, want %v", typ.Name(), got, want)
		}
	}
}

// TestRunsStatsBucketCarriesEveryRunStatus — /v1/runs/stats has one counter per
// run status, and each survives the handler view → wire projection (store → view
// is TestRunsStatsTyped_OK in handlers). A distinct value per field catches a
// counter copied into the wrong key as well as one not copied at all.
func TestRunsStatsBucketCarriesEveryRunStatus(t *testing.T) {
	var keys []string
	b := reflect.TypeFor[RunsStatsBucket]()
	for i := range b.NumField() {
		keys = append(keys, strings.Split(b.Field(i).Tag.Get("json"), ",")[0])
	}
	if want := append([]string{"total"}, runStatusNames()...); !slices.Equal(keys, want) {
		t.Fatalf("RunsStatsBucket keys = %v, want %v", keys, want)
	}

	view := handlers.RunsStatsBucketView{}
	vv := reflect.ValueOf(&view).Elem()
	if vv.NumField() != len(keys) {
		t.Fatalf("RunsStatsBucketView has %d fields, the wire bucket %d", vv.NumField(), len(keys))
	}
	for i := range vv.NumField() {
		vv.Field(i).SetInt(int64(i + 1))
	}
	raw, err := json.Marshal(newRunsStatsBucket(view))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]int
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for i := range vv.NumField() {
		name := vv.Type().Field(i).Name
		key := keys[i]
		if got[key] != i+1 {
			t.Errorf("view.%s = %d reached wire %q as %d", name, i+1, key, got[key])
		}
	}
}
