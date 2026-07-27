package scenario

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

// Guard tests for the automatic half of the compat axis (ADR-0076(i)/(k)): the
// floor a definition actually needs, weighed against the floor its author
// declared. The pass runs AFTER the body is parsed and never aborts — the
// declared-window gate (compat.go) keeps its place ahead of the parse.

func floorWarnings(t *testing.T, window *config.VersionWindow, used []config.KeeperFeature) string {
	t.Helper()
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	warnCompatFloorTooLow(
		config.CompatEntity{Kind: config.CompatEntityDestiny, Name: "redis", Ref: "v2.1.0", Window: window},
		used, log)
	return buf.String()
}

// A stale declaration is reported with the feature and the version that
// introduced it — the operator has to learn which OTHER instance of the cluster
// would fail this same definition, not just that something is wrong.
func TestWarnCompatFloorTooLow_NamesFeatureAndVersion(t *testing.T) {
	got := floorWarnings(t,
		&config.VersionWindow{Min: "1.0.0", Max: "9.0.0"},
		[]config.KeeperFeature{{ID: "core.file.present.params.selinux", IntroducedIn: "2.5.0", Where: "$.tasks[2]"}})

	for _, want := range []string{"compat_floor_too_low", "2.5.0", "core.file.present.params.selinux", "redis"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q does not mention %q", got, want)
		}
	}
}

func TestWarnCompatFloorTooLow_Silent(t *testing.T) {
	cases := []struct {
		name   string
		window *config.VersionWindow
		used   []config.KeeperFeature
	}{
		{
			name:   "declaration honors the floor",
			window: &config.VersionWindow{Min: "2.5.0"},
			used:   []config.KeeperFeature{{ID: "core.file.present.params.selinux", IntroducedIn: "2.5.0"}},
		},
		{
			name:   "no window declared - unbounded is not a promise",
			window: nil,
			used:   []config.KeeperFeature{{ID: "core.file.present.params.selinux", IntroducedIn: "2.5.0"}},
		},
		{
			name:   "nothing above the baseline is used",
			window: &config.VersionWindow{Min: "0.1.0"},
			used:   []config.KeeperFeature{{ID: "destiny.compat", IntroducedIn: config.Unreleased}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := floorWarnings(t, tc.window, tc.used); got != "" {
				t.Fatalf("logged %q, want silence", got)
			}
		})
	}
}

// The floor is a diagnostic, never a block (ADR-0076(k)): a destiny that
// declares a window and uses registered features still resolves — on a versioned
// build and on a version-less one alike, since the floor pass looks at the
// definition, not at who is rendering it.
func TestDestinyFloorPass_NeverAbortsAResolve(t *testing.T) {
	gitURL := compatDestinyRepo(t, "redis", "compat:\n  keeper: {min: \"0.1.0\", max: \"9.0.0\"}\n")
	for _, v := range []string{"v0.2.0", config.DevVersionSentinel} {
		if err := resolveDestinyAt(t, gitURL, "redis", v); err != nil {
			t.Fatalf("keeper %s: floor cross-check must not reject a resolve: %v", v, err)
		}
	}
}
