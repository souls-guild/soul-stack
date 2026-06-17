package handlers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/keeper/internal/herald"
	"github.com/souls-guild/soul-stack/keeper/internal/voyage"
)

// TestNotifyEventTypes_MappingByKind — guard маппинга on→event_types (ADR-052(g)):
// terminals × kind дают корректный area-prefix и action.
func TestNotifyEventTypes_MappingByKind(t *testing.T) {
	cases := []struct {
		name string
		kind voyage.Kind
		on   []string
		want []string
	}{
		{
			name: "scenario default (пустой on) → все три терминала",
			kind: voyage.KindScenario,
			on:   nil,
			want: []string{"scenario_run.completed", "scenario_run.failed", "scenario_run.partial_failed"},
		},
		{
			name: "command default (пустой on) → все три терминала",
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
			name: "partial маппится в partial_failed (а не partial)",
			kind: voyage.KindScenario,
			on:   []string{"partial"},
			want: []string{"scenario_run.partial_failed"},
		},
		{
			name: "дубль в on дедуплицируется",
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
			// Каждый выведенный event_type обязан проходить ValidateEventTypes
			// (иначе InsertTiding отверг бы его CHECK-ом в tx).
			if err := herald.ValidateEventTypes(got); err != nil {
				t.Fatalf("выведенные event_types не проходят ValidateEventTypes: %v", err)
			}
		})
	}
}

// TestNotifyEventTypes_UnknownTerminal — неизвестное значение on → ошибка
// (закрытый enum completed/failed/partial).
func TestNotifyEventTypes_UnknownTerminal(t *testing.T) {
	_, errMsg := notifyEventTypes(voyage.KindScenario, []string{"started"})
	if errMsg == "" {
		t.Fatal("notifyEventTypes на неизвестном terminal должен вернуть ошибку")
	}
	if !strings.Contains(errMsg, "started") {
		t.Errorf("errMsg = %q, want упоминание невалидного значения", errMsg)
	}
}

// TestStampEphemeralTidings_NameAndVoyageID — стемп проставляет уникальные имена
// (валидные по NamePattern) и общий voyage_id всем шаблонам.
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
			t.Fatalf("tpls[%d].Name = %q не матчит NamePattern", i, tpls[i].Name)
		}
		if !strings.HasPrefix(tpls[i].Name, "eph-") {
			t.Errorf("tpls[%d].Name = %q, want префикс eph-", i, tpls[i].Name)
		}
		if _, dup := names[tpls[i].Name]; dup {
			t.Fatalf("имя %q повторилось — нарушена уникальность (коллизия имён ephemeral)", tpls[i].Name)
		}
		names[tpls[i].Name] = struct{}{}
		// Стемпленный шаблон обязан проходить domain-инвариант ephemeral⟺voyage_id.
		if !tpls[i].Ephemeral || tpls[i].VoyageID == nil {
			t.Errorf("tpls[%d] нарушает ephemeral⟺voyage_id", i)
		}
	}
}
