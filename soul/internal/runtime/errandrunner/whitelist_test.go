package errandrunner

import (
	"strings"
	"testing"

	sdkmodule "github.com/souls-guild/soul-stack/sdk/module"
)

// markerModule — пустой SoulModule с маркером ErrandReadSafe.
type markerModule struct{ sdkmodule.BaseModule }

func (markerModule) ErrandReadSafe() {}

// plainModule — без маркера. BaseModule сознательно НЕ реализует ErrandReadSafe.
type plainModule struct{ sdkmodule.BaseModule }

func TestIsAllowed_Hardcoded(t *testing.T) {
	t.Parallel()
	cases := []string{"core.cmd.shell", "core.exec.run"}
	for _, full := range cases {
		ok, reason := IsAllowed(full, &plainModule{})
		if !ok {
			t.Errorf("IsAllowed(%q, plain) = (false, %q); want true", full, reason)
		}
	}
}

func TestIsAllowed_Marker(t *testing.T) {
	t.Parallel()
	ok, reason := IsAllowed("core.http.probe", &markerModule{})
	if !ok {
		t.Errorf("IsAllowed(marker) = (false, %q); want true", reason)
	}
}

func TestIsAllowed_RejectPlain(t *testing.T) {
	t.Parallel()
	ok, reason := IsAllowed("core.pkg.installed", &plainModule{})
	if ok {
		t.Fatalf("IsAllowed(plain) = true; want false")
	}
	if !strings.HasPrefix(reason, "errand_module_not_allowed:") {
		t.Errorf("reason = %q; want errand_module_not_allowed prefix", reason)
	}
}

func TestIsAllowed_RejectHardcodedStateMismatch(t *testing.T) {
	t.Parallel()
	// core.cmd.foo не в hardcoded-списке (там только shell); без маркера — reject.
	ok, _ := IsAllowed("core.cmd.foo", &plainModule{})
	if ok {
		t.Errorf("IsAllowed(core.cmd.foo) = true; want false (only shell hardcoded)")
	}
}

func TestIsAllowed_NilModule(t *testing.T) {
	t.Parallel()
	// nil-mod: defensive reject, hardcoded-список всё равно срабатывает.
	if ok, _ := IsAllowed("core.cmd.shell", nil); !ok {
		t.Errorf("IsAllowed(core.cmd.shell, nil) = false; want true (hardcoded прежде marker-check)")
	}
	if ok, _ := IsAllowed("core.pkg.installed", nil); ok {
		t.Errorf("IsAllowed(core.pkg.installed, nil) = true; want false")
	}
}
