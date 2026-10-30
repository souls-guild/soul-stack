package maintenance

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/souls-guild/soul-stack/shared/config"
)

// The cluster default of these tests: every day 22:00–23:00 UTC. Asked on
// Monday 2026-11-02 at 10:00, it next opens at 22:00 the same day.
var nightly = Window{ID: "maintenance.default_windows[0]", Kind: KindWeekly, Days: everyDay,
	StartTime: "22:00", DurationMinutes: 60, Timezone: "UTC"}

func onceWindow(id string, from, to time.Time) Window {
	return Window{ID: id, Kind: KindOnce, StartsAt: from, EndsAt: to}
}

func mustStatus(t *testing.T, r Rules, t0 time.Time) Status {
	t.Helper()
	s, err := r.Status(t0)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return s
}

func mustRulesCalc(t *testing.T, r Rules) *Calculator {
	t.Helper()
	c, err := r.Calculator()
	if err != nil {
		t.Fatalf("Calculator: %v", err)
	}
	return c
}

// ★ One weekly window of its own and the default is gone entirely: not unioned
// with the incarnation's windows, not shown, not the source.
func TestRules_OwnWeeklyWindowReplacesTheDefault(t *testing.T) {
	own := utcWindow0("own", []string{"tue"}, "03:00", 60)
	r := Rules{Windows: []Window{own}, Defaults: []Window{nightly}}
	now := at(2, 10, 0)

	c := mustRulesCalc(t, r)
	if got := c.NextStart(now); !got.Equal(at(3, 3, 0)) {
		t.Errorf("NextStart = %v, want %v: the default's 22:00 must not count beside an own window", got, at(3, 3, 0))
	}
	if c.OpenAt(at(2, 22, 30)) {
		t.Error("open inside the default's slot although the incarnation has its own weekly window")
	}
	if txt := c.Text(now); strings.Contains(txt, "22:00") {
		t.Errorf("Text = %q, want the own window only", txt)
	}
	if d := r.DefaultsInEffect(); d != nil {
		t.Errorf("DefaultsInEffect = %v, want none", d)
	}
	if s := mustStatus(t, r, now); s.Source != SourceOwn {
		t.Errorf("Source = %q, want %q", s.Source, SourceOwn)
	}

	// A once window beside the own weekly one changes nothing about that.
	r.Windows = append(r.Windows, onceWindow("o", at(2, 12, 0), at(2, 13, 0)))
	if got := mustRulesCalc(t, r).NextStart(at(2, 14, 0)); !got.Equal(at(3, 3, 0)) {
		t.Errorf("NextStart after the once window = %v, want %v", got, at(3, 3, 0))
	}
}

// ★ A once window under the default adds to it: the run may start in it, and
// after it ends the default still holds — a once window does not switch the
// default off, an ended one no more than a future one.
func TestRules_OnceWindowAddsToTheDefault(t *testing.T) {
	once := onceWindow("o", at(2, 12, 0), at(2, 14, 0))
	r := Rules{Windows: []Window{once}, Defaults: []Window{nightly}}
	c := mustRulesCalc(t, r)

	if got := c.NextStart(at(2, 10, 0)); !got.Equal(at(2, 12, 0)) {
		t.Errorf("NextStart before the once window = %v, want its start %v", got, at(2, 12, 0))
	}
	if got := c.NextStart(at(2, 15, 0)); !got.Equal(at(2, 22, 0)) {
		t.Errorf("NextStart after the once window = %v, want the default's %v", got, at(2, 22, 0))
	}
	if c.OpenAt(at(2, 15, 0)) {
		t.Error("open between the once window and the default's slot")
	}
	if txt := c.Text(at(2, 10, 0)); !strings.Contains(txt, "22:00") || !strings.Contains(txt, "once 2026-11-02T12:00:00Z") {
		t.Errorf("Text = %q, want the default and the once window", txt)
	}
	if s := mustStatus(t, r, at(2, 10, 0)); s.Source != SourceDefault {
		t.Errorf("Source = %q, want %q", s.Source, SourceDefault)
	}
	if got := r.DefaultsInEffect(); !reflect.DeepEqual(got, []Window{nightly}) {
		t.Errorf("DefaultsInEffect = %v, want the default", got)
	}

	ended := Rules{Windows: []Window{onceWindow("past", at(1, 1, 0), at(1, 2, 0))}, Defaults: []Window{nightly}}
	if got := mustRulesCalc(t, ended).NextStart(at(2, 10, 0)); !got.Equal(at(2, 22, 0)) {
		t.Errorf("an ended once window: NextStart = %v, want the default's %v", got, at(2, 22, 0))
	}
}

// ★ With the default empty — never set, or set to [] — every answer is the
// release before NIM-1138's: the incarnation's own windows compiled alone, and a
// source that is none whenever nothing of its own counts.
func TestRules_EmptyDefaultIsTheReleaseBefore(t *testing.T) {
	owns := map[string][]Window{
		"none":        nil,
		"weekly":      {utcWindow0("w", []string{"mon"}, "13:00", 60)},
		"future once": {onceWindow("o", at(2, 12, 0), at(2, 14, 0))},
		"ended once":  {onceWindow("o", at(1, 12, 0), at(1, 14, 0))},
	}
	freezes := []Freeze{{ID: "f", StartsAt: at(2, 11, 0), EndsAt: at(2, 12, 30)}}
	for name, own := range owns {
		for _, empty := range [][]Window{nil, {}} {
			r := Rules{Windows: own, Freezes: freezes, Defaults: empty}
			before := mustCalc(t, own, freezes)
			after := mustRulesCalc(t, r)
			for _, t0 := range []time.Time{at(2, 10, 0), at(2, 11, 30), at(2, 13, 0), at(2, 15, 0)} {
				if a, b := after.Status(t0), before.Status(t0); !reflect.DeepEqual(a, b) {
					t.Errorf("%s, default %#v, at %v: status %+v, the release before answered %+v", name, empty, t0, a, b)
				}
				if a, b := after.Text(t0), before.Text(t0); a != b {
					t.Errorf("%s, default %#v, at %v: text %q, the release before rendered %q", name, empty, t0, a, b)
				}
			}
			if d := r.DefaultsInEffect(); d != nil {
				t.Errorf("%s, default %#v: DefaultsInEffect = %v, want none", name, empty, d)
			}
			want := SourceNone
			if before.HasWindows(at(2, 10, 0)) {
				want = SourceOwn
			}
			if got := r.Source(at(2, 10, 0)); got != want {
				t.Errorf("%s, default %#v: Source = %q, want %q", name, empty, got, want)
			}
		}
	}
}

// The default seen as the incarnation's own windows answers alike: Effective is
// the only difference between the two, and it puts the same slots in.
func TestRules_DefaultAnswersAsTheSameOwnWindows(t *testing.T) {
	byDefault := mustRulesCalc(t, Rules{Defaults: []Window{nightly}})
	asOwn := mustRulesCalc(t, Rules{Windows: []Window{nightly}})
	for h := 0; h < 48; h += 3 {
		t0 := at(2, 0, 0).Add(time.Duration(h) * time.Hour)
		if a, b := byDefault.Status(t0), asOwn.Status(t0); !reflect.DeepEqual(a, b) {
			t.Errorf("at %v: by default %+v, as own %+v", t0, a, b)
		}
	}
}

// The configured list becomes weekly windows with the fields it holds, and
// ConfigDefaults reads the config again on every call.
func TestConfigDefaults_ReadsTheLiveConfigEachCall(t *testing.T) {
	cfg := &config.KeeperConfig{}
	src := ConfigDefaults(func() *config.KeeperConfig { return cfg })
	if got := src.windows(); got != nil {
		t.Fatalf("no maintenance block: %v, want none", got)
	}
	cfg = &config.KeeperConfig{Maintenance: &config.KeeperMaintenance{DefaultWindows: []config.KeeperMaintenanceWindow{
		{Days: []string{"sat", "mon"}, StartTime: "04:00", DurationMinutes: 30, Timezone: "Europe/Moscow", Comment: "c"},
	}}}
	want := []Window{{ID: "maintenance.default_windows[0]", Kind: KindWeekly, Days: []string{"mon", "sat"},
		StartTime: "04:00", DurationMinutes: 30, Timezone: "Europe/Moscow", Comment: "c"}}
	if got := src.windows(); !reflect.DeepEqual(got, want) {
		t.Errorf("after the swap: %+v, want %+v", got, want)
	}
	if got := ConfigDefaults(func() *config.KeeperConfig { return nil }).windows(); got != nil {
		t.Errorf("nil config: %v, want none", got)
	}
	if got := DefaultsSource(nil).Unrecorded(); !reflect.DeepEqual(got, Rules{}) {
		t.Errorf("nil source: %+v, want the zero rules", got)
	}
}

// ★ A run that did not ask for the window is held to no window, the default's no
// more than the incarnation's own: only the freezes it was not asked about hold it.
func TestRules_ForcedRunIsHeldToNoDefault(t *testing.T) {
	r := Rules{Defaults: []Window{nightly}, Freezes: []Freeze{{ID: "f", StartsAt: at(2, 11, 0), EndsAt: at(2, 12, 0)}}}
	c, err := r.Gate(false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.OpenAt(at(2, 10, 0)) || c.HasWindows(at(2, 10, 0)) {
		t.Error("a forced run is held to the default's windows")
	}
	if got := c.NextStart(at(2, 11, 30)); !got.Equal(at(2, 12, 0)) {
		t.Errorf("NextStart in the freeze = %v, want its end %v", got, at(2, 12, 0))
	}
}
