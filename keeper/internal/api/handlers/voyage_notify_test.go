package handlers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// TestNotifyEventTypes_MappingByKind — guard for the on→event_types mapping (ADR-052(g)):
// terminals × kind yield the correct area prefix and action.
func TestNotifyEventTypes_MappingByKind(t *testing.T) {
	cases := []struct {
		name string
		kind voyage.Kind
		on   []string
		want []string
	}{
		{
			name: "scenario default (empty on) -> all three terminals",
			kind: voyage.KindScenario,
			on:   nil,
			want: []string{"scenario_run.completed", "scenario_run.failed", "scenario_run.partial_failed"},
		},
		{
			name: "command default (empty on) -> all three terminals",
			kind: voyage.KindCommand,
			on:   nil,
			want: []string{"command_run.completed", "command_run.failed", "command_run.partial_failed"},
		},
		{
			name: "scenario completed",
			kind: voyage.KindScenario,
			on:   []string{"completed"},
			want: []string{"scenario_run.completed"},
		},
		{
			name: "command failed+partial",
			kind: voyage.KindCommand,
			on:   []string{"failed", "partial"},
			want: []string{"command_run.failed", "command_run.partial_failed"},
		},
		{
			name: "partial maps to partial_failed (not partial)",
			kind: voyage.KindScenario,
			on:   []string{"partial"},
			want: []string{"scenario_run.partial_failed"},
		},
		{
			name: "duplicate in on is deduplicated",
			kind: voyage.KindCommand,
			on:   []string{"completed", "completed"},
			want: []string{"command_run.completed"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, errMsg := notifyEventTypes(tc.kind, tc.on)
			if errMsg != "" {
				t.Fatalf("notifyEventTypes errMsg = %q, want none", errMsg)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("notifyEventTypes = %v, want %v", got, tc.want)
			}
			// Every derived event_type must pass ValidateEventTypes
			// (otherwise InsertTiding would reject it via the CHECK in the tx).
			if err := herald.ValidateEventTypes(got); err != nil {
				t.Fatalf("derived event_types fail ValidateEventTypes: %v", err)
			}
		})
	}
}

// TestNotifyEventTypes_UnknownTerminal — an unknown on value → error
// (closed enum completed/failed/partial).
func TestNotifyEventTypes_UnknownTerminal(t *testing.T) {
	_, errMsg := notifyEventTypes(voyage.KindScenario, []string{"started"})
	if errMsg == "" {
		t.Fatal("notifyEventTypes on an unknown terminal should return an error")
	}
	if !strings.Contains(errMsg, "started") {
		t.Errorf("errMsg = %q, want mention of the invalid value", errMsg)
	}
}

// TestStampEphemeralTidings_NameAndVoyageID — the stamp assigns unique names
// (valid per NamePattern) and a shared voyage_id to all templates.
func TestStampEphemeralTidings_NameAndVoyageID(t *testing.T) {
	const vid = "01HZZZZZZZZZZZZZZZZZZZZZZZZ"
	tpls := []herald.Tiding{
		{Herald: "ops", EventTypes: []string{"scenario_run.completed"}, Ephemeral: true},
		{Herald: "ops", EventTypes: []string{"scenario_run.failed"}, Ephemeral: true},
	}
	stampEphemeralTidings(tpls, vid)

	names := map[string]struct{}{}
	for i := range tpls {
		if tpls[i].VoyageID == nil || *tpls[i].VoyageID != vid {
			t.Fatalf("tpls[%d].VoyageID = %v, want %s", i, tpls[i].VoyageID, vid)
		}
		if !herald.ValidName(tpls[i].Name) {
			t.Fatalf("tpls[%d].Name = %q does not match NamePattern", i, tpls[i].Name)
		}
		if !strings.HasPrefix(tpls[i].Name, "eph-") {
			t.Errorf("tpls[%d].Name = %q, want prefix eph-", i, tpls[i].Name)
		}
		if _, dup := names[tpls[i].Name]; dup {
			t.Fatalf("name %q repeated - uniqueness violated (ephemeral name collision)", tpls[i].Name)
		}
		names[tpls[i].Name] = struct{}{}
		// A stamped template must satisfy the domain invariant ephemeral⟺voyage_id.
		if !tpls[i].Ephemeral || tpls[i].VoyageID == nil {
			t.Errorf("tpls[%d] violates ephemeral<=>voyage_id", i)
		}
	}
}
