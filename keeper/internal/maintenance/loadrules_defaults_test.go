package maintenance_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/keeper/internal/maintenance"
	"github.com/souls-guild/soul-stack/keeper/internal/maintenance/maintenancetest"
)

// The default reaches every incarnation a load names, the ones with no row at
// all included, and only when it is set: without it the map is what it was, an
// incarnation with no rows absent.
func TestLoadRules_StampsTheDefaultOnEveryIncarnation(t *testing.T) {
	slot, _ := maintenancetest.Slot(time.Now())
	db := &maintenancetest.DB{Windows: []maintenance.Window{slot}}
	ids := []string{maintenancetest.Incarnation, "redis-stage"}
	modes := maintenancetest.Modes(slot)
	byDefault := modes[0]

	rules, err := maintenance.LoadRules(context.Background(), db, ids, byDefault.Defaults)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatalf("rules for %d incarnations, want both", len(rules))
	}
	for _, id := range ids {
		if len(rules[id].Defaults) != 1 {
			t.Errorf("%s: defaults %v, want the one slot", id, rules[id].Defaults)
		}
	}
	if got := rules["redis-stage"].DefaultsInEffect(); len(got) != 1 {
		t.Errorf("an incarnation without rows: defaults in effect %v, want the default", got)
	}
	if got := rules[maintenancetest.Incarnation].DefaultsInEffect(); got != nil {
		t.Errorf("an incarnation with its own weekly window: defaults in effect %v, want none", got)
	}

	for _, empty := range []maintenance.DefaultsSource{nil, func() []maintenance.Window { return nil }} {
		rules, err := maintenance.LoadRules(context.Background(), db, ids, empty)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]maintenance.Rules{maintenancetest.Incarnation: {Windows: []maintenance.Window{slot}}}
		if !reflect.DeepEqual(rules, want) {
			t.Errorf("without a default: %+v, want %+v", rules, want)
		}
	}
}
