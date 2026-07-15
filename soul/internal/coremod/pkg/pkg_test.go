package pkg_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/internaltest"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/pkg"
	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

// aptInstalled — apt detected + dpkg-query reporting installed.
func aptInstalled(name, version string) *internaltest.Runner {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} "+name, util.Result{
		ExitCode: 0,
		Stdout:   "install ok installed " + version,
	})
	return r
}

// Expected apt-command forms: install/remove/update are non-interactive
// (`env DEBIAN_FRONTEND=noninteractive apt-get …`), and install carries
// Dpkg::Options force-confdef/force-confold — conffile prompts ("keep or
// replace?") on re-apply are no longer possible, so the Soul agent's empty
// stdin doesn't hit EOF (live Debian 12 bug). Strings match how
// internaltest.Runner joins name+args with spaces.
const aptNonInteractive = "env DEBIAN_FRONTEND=noninteractive apt-get"

// aptInstallCmd — expected install command (target = name or `name=version`).
// confold keeps our rendered conffile, confdef is the default for the rest.
func aptInstallCmd(target string) string {
	return aptNonInteractive + " install -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold " + target
}

func aptRemoveCmd(name string) string { return aptNonInteractive + " remove -y " + name }

const aptUpdateCmd = aptNonInteractive + " update"

func TestValidate(t *testing.T) {
	m := pkg.New()
	reply, err := m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	})
	if err != nil || !reply.Ok {
		t.Fatalf("Validate ok: reply=%+v err=%v", reply, err)
	}

	reply, _ = m.Validate(context.Background(), &pluginv1.ValidateRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	})
	if reply.Ok {
		t.Fatalf("Validate bad state: ok unexpectedly")
	}
}

func TestApply_Installed_AlreadyPresent(t *testing.T) {
	r := aptInstalled("redis-server", "7:7.0.0-1")
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Changed || ev.Failed {
		t.Fatalf("changed=%v failed=%v want changed=false", ev.Changed, ev.Failed)
	}
	if got := ev.Output.Fields["version"].GetStringValue(); got != "7:7.0.0-1" {
		t.Fatalf("version=%q", got)
	}
	// No apt-get install call expected.
	for _, c := range r.Calls {
		if strings.Contains(c, "apt-get install") {
			t.Fatalf("unexpected install call: %q", c)
		}
	}
}

func TestApply_Installed_VersionMismatch_Reinstalls(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 0})
	r.OnSeq("dpkg-query -W -f=${Status} ${Version} redis-server",
		util.Result{ExitCode: 0, Stdout: "install ok installed 7:6.0.0-1"},
		util.Result{ExitCode: 0, Stdout: "install ok installed 7:7.0.0-1"},
	)
	r.On(aptInstallCmd("redis-server=7:7.0.0-1"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server", "version": "7:7.0.0-1"}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Changed {
		t.Fatalf("changed=false want true")
	}
}

func TestApply_Installed_NotPresent_Installs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 0})
	// dpkg-query exits 1 — package not installed.
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On(aptInstallCmd("redis-server"), util.Result{ExitCode: 0})
	// Post-install: package present. Can't override dpkg-query's result here
	// (map key), so fake-runner repeats the same response on repeat calls.
	// Fine for this test — we only care that changed=true, not the version.
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false, want true")
	}
}

// hasCall — whether the runner saw this exact (space-joined) call.
func hasCall(r *internaltest.Runner, want string) bool {
	return callIndex(r, want) >= 0
}

// callIndex — index of the first exact-matching call in r.Calls; -1 if none.
func callIndex(r *internaltest.Runner, want string) int {
	for i, c := range r.Calls {
		if c == want {
			return i
		}
	}
	return -1
}

// TestApply_Installed_NoVersion_InstallsWithoutPin — BUG-1: empty/unset
// version → install WITHOUT a version pin (latest available from repo), not
// `name=`. Covers all four backends: install-command rendering without a pin.
func TestApply_Installed_NoVersion_InstallsWithoutPin(t *testing.T) {
	cases := []struct {
		name    string
		mgrCmd  string // command -v <mgrCmd>
		refresh string // expected repo-index refresh command ("" = mgr has no refresh)
		query   string // "is package installed" query (returns "not installed")
		want    string // expected install command WITHOUT a pin
		notWant string // pin substring that must NOT appear
	}{
		{
			name:    "apt",
			mgrCmd:  "apt-get",
			refresh: aptUpdateCmd,
			query:   "dpkg-query -W -f=${Status} ${Version} redis-server",
			want:    aptInstallCmd("redis-server"),
			notWant: "redis-server=",
		},
		{
			name:    "dnf",
			mgrCmd:  "dnf",
			query:   "rpm -q --qf %{VERSION} redis-server",
			want:    "dnf install -y redis-server",
			notWant: "redis-server-",
		},
		{
			name:    "yum",
			mgrCmd:  "yum",
			query:   "rpm -q --qf %{VERSION} redis-server",
			want:    "yum install -y redis-server",
			notWant: "redis-server-",
		},
		{
			name:    "apk",
			mgrCmd:  "apk",
			refresh: "apk update",
			query:   "apk info -e redis-server",
			want:    "apk add --no-cache redis-server",
			notWant: "redis-server=",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := internaltest.NewRunner()
			r.Fallback = util.Result{ExitCode: 1}
			r.On("command -v "+tc.mgrCmd, util.Result{ExitCode: 0})
			if tc.refresh != "" {
				r.On(tc.refresh, util.Result{ExitCode: 0})
			}
			r.On(tc.query, util.Result{ExitCode: 1}) // package not installed
			r.On(tc.want, util.Result{ExitCode: 0})  // install without a pin
			m := &pkg.Module{Runner: r}

			stream := &internaltest.ApplyStream{}
			// version omitted entirely (required:false at the contract level).
			if err := m.Apply(&pluginv1.ApplyRequest{
				State:  "installed",
				Params: mustStruct(t, map[string]any{"name": "redis-server"}),
			}, stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !hasCall(r, tc.want) {
				t.Fatalf("ожидалась install-команда без пина %q, calls=%v", tc.want, r.Calls)
			}
			for _, c := range r.Calls {
				if strings.Contains(c, tc.notWant) {
					t.Fatalf("install получил version-пин %q (хотя version не задан): %q", tc.notWant, c)
				}
			}
			// repo-index refresh (apt/apk) must precede install;
			// dnf/yum have no refresh command (metadata auto-refresh).
			if tc.refresh != "" {
				ri, ii := callIndex(r, tc.refresh), callIndex(r, tc.want)
				if ri < 0 {
					t.Fatalf("ожидался refresh индекса %q перед install, calls=%v", tc.refresh, r.Calls)
				}
				if ri > ii {
					t.Fatalf("refresh %q должен идти ДО install %q, calls=%v", tc.refresh, tc.want, r.Calls)
				}
			}
		})
	}
}

// TestApply_Installed_EmptyVersion_InstallsWithoutPin — version passed as an
// empty string ("") is treated the same as unset: install without a pin (BUG-1).
func TestApply_Installed_EmptyVersion_InstallsWithoutPin(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On(aptInstallCmd("redis-server"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server", "version": ""}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(r, aptInstallCmd("redis-server")) {
		t.Fatalf("ожидался install без пина при version=\"\", calls=%v", r.Calls)
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "redis-server=") {
			t.Fatalf("version=\"\" не должен давать пин, но получили: %q", c)
		}
	}
}

// TestApply_Installed_DistroNativeVersion_PinsExact — distro-native version
// (epoch + revision, Debian form) is pinned verbatim: `name=<version>` (apt).
// BUG-1: the contract pattern now allows such strings, and the module must
// pass them through to install unchanged.
func TestApply_Installed_DistroNativeVersion_PinsExact(t *testing.T) {
	const ver = "5:7.0.15-1~deb12u7"
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 0})
	r.OnSeq("dpkg-query -W -f=${Status} ${Version} redis-server",
		util.Result{ExitCode: 1}, // before install — not installed
		util.Result{ExitCode: 0, Stdout: "install ok installed " + ver}, // after
	)
	r.On(aptInstallCmd("redis-server="+ver), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server", "version": ver}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(r, aptInstallCmd("redis-server="+ver)) {
		t.Fatalf("ожидался точный пин distro-native версии, calls=%v", r.Calls)
	}
}

// TestApply_Apt_Install_NonInteractive_ConffileSafe — guards the live Debian
// 12 bug: conffile prompts ("keep or replace?") on re-apply are now
// structurally impossible. Apply install must carry, in one command:
// DEBIAN_FRONTEND=noninteractive (debconf stays quiet) AND both
// Dpkg::Options force-confdef/force-confold (dpkg keeps our rendered
// conffile without asking). Missing any of the three, the Soul agent's
// empty stdin would hit EOF and fail the install.
//
// Checks the actual captured command fragment (not the helper form — that
// would be tautological): a refactor dropping non-interactivity gets caught.
func TestApply_Apt_Install_NonInteractive_ConffileSafe(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-sentinel", util.Result{ExitCode: 1})
	r.On(aptInstallCmd("redis-sentinel"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-sentinel"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var install string
	for _, c := range r.Calls {
		if strings.Contains(c, "apt-get install") {
			install = c
			break
		}
	}
	if install == "" {
		t.Fatalf("install-команда не вызвана, calls=%v", r.Calls)
	}
	for _, frag := range []string{
		"DEBIAN_FRONTEND=noninteractive",
		"Dpkg::Options::=--force-confdef",
		"Dpkg::Options::=--force-confold",
	} {
		if !strings.Contains(install, frag) {
			t.Fatalf("install-команда не несёт %q (conffile-prompt снова возможен): %q", frag, install)
		}
	}
}

// TestApply_Apt_RemoveAndUpdate_NonInteractive — remove and index update also
// run with DEBIAN_FRONTEND=noninteractive (package prerm scripts and debconf
// during update must not prompt interactively on empty stdin).
func TestApply_Apt_RemoveAndUpdate_NonInteractive(t *testing.T) {
	// remove an installed package.
	r := aptInstalled("redis-server", "7:7.0.0-1")
	r.On(aptRemoveCmd("redis-server"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply absent: %v", err)
	}
	assertAptNonInteractive(t, r, "apt-get remove")

	// index update (via install on an absent package).
	r2 := internaltest.NewRunner()
	r2.Fallback = util.Result{ExitCode: 1}
	r2.On("command -v apt-get", util.Result{ExitCode: 0})
	r2.On(aptUpdateCmd, util.Result{ExitCode: 0})
	r2.On("dpkg-query -W -f=${Status} ${Version} nginx", util.Result{ExitCode: 1})
	r2.On(aptInstallCmd("nginx"), util.Result{ExitCode: 0})
	m2 := &pkg.Module{Runner: r2}
	stream2 := &internaltest.ApplyStream{}
	if err := m2.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "nginx"}),
	}, stream2); err != nil {
		t.Fatalf("Apply installed: %v", err)
	}
	assertAptNonInteractive(t, r2, "apt-get update")
}

// assertAptNonInteractive — fails if an apt command with this subcommand was
// called WITHOUT DEBIAN_FRONTEND=noninteractive.
func assertAptNonInteractive(t *testing.T, r *internaltest.Runner, subcmd string) {
	t.Helper()
	for _, c := range r.Calls {
		if strings.Contains(c, subcmd) && !strings.Contains(c, "DEBIAN_FRONTEND=noninteractive") {
			t.Fatalf("%q вызван без DEBIAN_FRONTEND=noninteractive: %q", subcmd, c)
		}
	}
}

// countCalls — how many times this exact command appears in r.Calls.
func countCalls(r *internaltest.Runner, want string) int {
	n := 0
	for _, c := range r.Calls {
		if c == want {
			n++
		}
	}
	return n
}

// TestApply_Apt_RefreshBeforeInstall — fix for a prod bug: on a fresh VM,
// install without a prior `apt-get update` hits "Unable to locate package".
// Checks that update is called and precedes install.
func TestApply_Apt_RefreshBeforeInstall(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On(aptInstallCmd("redis-server"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ri, ii := callIndex(r, aptUpdateCmd), callIndex(r, aptInstallCmd("redis-server"))
	if ri < 0 {
		t.Fatalf("apt-get update не вызван, calls=%v", r.Calls)
	}
	if ri > ii {
		t.Fatalf("apt-get update должен идти ДО install, calls=%v", r.Calls)
	}
}

// TestApply_Apk_RefreshBeforeInstall — apk equivalent: `apk update` before
// `apk add`, otherwise the package isn't found on a fresh image.
func TestApply_Apk_RefreshBeforeInstall(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0})
	r.On("apk update", util.Result{ExitCode: 0})
	r.On("apk info -e redis", util.Result{ExitCode: 1}) // not installed
	r.On("apk add --no-cache redis", util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ri, ii := callIndex(r, "apk update"), callIndex(r, "apk add --no-cache redis")
	if ri < 0 {
		t.Fatalf("apk update не вызван, calls=%v", r.Calls)
	}
	if ri > ii {
		t.Fatalf("apk update должен идти ДО install, calls=%v", r.Calls)
	}
}

// TestApply_Apt_RefreshOncePerProcess — index refresh runs once per process
// lifetime (option (b)): one Module serves multiple install steps in a run,
// the second install must NOT repeat `apt-get update`.
func TestApply_Apt_RefreshOncePerProcess(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On("dpkg-query -W -f=${Status} ${Version} nginx", util.Result{ExitCode: 1})
	r.On(aptInstallCmd("redis-server"), util.Result{ExitCode: 0})
	r.On(aptInstallCmd("nginx"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	for _, name := range []string{"redis-server", "nginx"} {
		stream := &internaltest.ApplyStream{}
		if err := m.Apply(&pluginv1.ApplyRequest{
			State:  "installed",
			Params: mustStruct(t, map[string]any{"name": name}),
		}, stream); err != nil {
			t.Fatalf("Apply %s: %v", name, err)
		}
	}
	if n := countCalls(r, aptUpdateCmd); n != 1 {
		t.Fatalf("apt-get update вызван %d раз, ожидался ровно 1 (refresh-once), calls=%v", n, r.Calls)
	}
}

// TestApply_Dnf_NoRefresh — dnf/yum auto-refresh metadata on expiration, we
// don't add an explicit update; install runs immediately when not installed.
func TestApply_Dnf_NoRefresh(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.On("rpm -q --qf %{VERSION} redis", util.Result{ExitCode: 1}) // not installed
	r.On("dnf install -y redis", util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "update") || strings.Contains(c, "makecache") || strings.Contains(c, "check-update") {
			t.Fatalf("dnf не должен делать refresh, но был вызов: %q", c)
		}
	}
}

// TestApply_Apt_RefreshFails_NoInstall — if `apt-get update` fails, install is
// NOT run and Apply returns failed (we don't install against a stale index).
// The refresh-done flag is not set in this case.
func TestApply_Apt_RefreshFails_NoInstall(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 100, Stderr: "Could not resolve host"})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On(aptInstallCmd("redis-server"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true (refresh упал)")
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "apt-get install") {
			t.Fatalf("install не должен вызываться после провала refresh: %q", c)
		}
	}
}

func TestApply_Absent_NotPresent_NoOp(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true for not-installed absent")
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "apt-get remove") {
			t.Fatalf("unexpected remove call: %q", c)
		}
	}
}

func TestApply_Absent_Present_Removes(t *testing.T) {
	r := aptInstalled("redis-server", "7:7.0.0-1")
	r.On(aptRemoveCmd("redis-server"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false, want true")
	}
}

func TestApply_NoPkgMgrDetected_Fails(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	m := &pkg.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true")
	}
}

func TestApply_MissingName_Fails(t *testing.T) {
	r := internaltest.NewRunner()
	m := &pkg.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	_ = m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{}),
	}, stream)
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true (missing name)")
	}
}

func TestApply_DnfInstalled(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.On("rpm -q --qf %{VERSION} redis", util.Result{ExitCode: 0, Stdout: "7.0.0"})
	m := &pkg.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true, want false (already installed)")
	}
}

func TestApply_ApkInstalled(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0})
	r.On("apk info -e redis", util.Result{ExitCode: 0, Stdout: "redis"})
	// `apk info -ev <name>` → `<name>-<version>`; the module strips the
	// `redis-` prefix, register.version gets the bare number (MINOR-C).
	r.On("apk info -ev redis", util.Result{ExitCode: 0, Stdout: "redis-7.0.0-r0\n"})
	m := &pkg.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true, want false")
	}
	if got := stream.Last().Output.Fields["version"].GetStringValue(); got != "7.0.0-r0" {
		t.Fatalf("version=%q, want 7.0.0-r0 (номер без имени пакета, MINOR-C)", got)
	}
}

// TestApply_PkgMgrFromFact_NoDetect — BUG-B: soulprint fact pkg_mgr=apk is
// primary; `command -v`/`which` don't respond (fallback detection would fail
// and the module would error "no supported package manager"). With the fact,
// the module goes straight to the apk branch.
func TestApply_PkgMgrFromFact_NoDetect(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127} // ALL detection commands are absent
	r.On("apk info -e redis", util.Result{ExitCode: 0, Stdout: "redis"})
	r.On("apk info -ev redis", util.Result{ExitCode: 0, Stdout: "redis-7.0.0-r0\n"})
	m := &pkg.Module{Runner: r}
	m.SetHostFacts(util.HostFacts{PkgMgr: util.PkgMgrApk})

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true: факт pkg_mgr=apk должен миновать детект, msg=%q", ev.Message)
	}
	if got := ev.Output.Fields["version"].GetStringValue(); got != "7.0.0-r0" {
		t.Fatalf("version=%q, want 7.0.0-r0", got)
	}
	for _, c := range r.Calls {
		if strings.HasPrefix(c, "command -v") || strings.HasPrefix(c, "which") {
			t.Fatalf("факт primary: detection не должен вызываться, call=%q", c)
		}
	}
}

// TestApply_PkgMgrFactEmpty_FallbackDetect — empty fact → runtime detection
// (factless host: push mode / old Keeper without soulprint).
func TestApply_PkgMgrFactEmpty_FallbackDetect(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0}) // detect → apk
	r.On("apk info -e redis", util.Result{ExitCode: 0, Stdout: "redis"})
	r.On("apk info -ev redis", util.Result{ExitCode: 0, Stdout: "redis-7.0.0-r0\n"})
	m := &pkg.Module{Runner: r}
	// SetHostFacts not called — facts stay empty.

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Failed {
		t.Fatalf("failed=true: fallback-детект apk должен сработать, msg=%q", stream.Last().Message)
	}
	if !hasCall(r, "command -v apk") {
		t.Fatalf("пустой факт: ожидался fallback-детект, calls=%v", r.Calls)
	}
}

// TestApply_ApkVersion_NameWithDash — MINOR-C critical: an apk package name
// can contain a dash (`py3-pip`); splitting on dash would give a wrong
// version. Stripping the known `<name>-` prefix gives the correct number.
func TestApply_ApkVersion_NameWithDash(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0})
	r.On("apk info -e py3-pip", util.Result{ExitCode: 0, Stdout: "py3-pip"})
	r.On("apk info -ev py3-pip", util.Result{ExitCode: 0, Stdout: "py3-pip-23.3.1-r0\n"})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "py3-pip"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := stream.Last().Output.Fields["version"].GetStringValue(); got != "23.3.1-r0" {
		t.Fatalf("version=%q, want 23.3.1-r0 (имя с дефисом, MINOR-C)", got)
	}
}

// ---------------------------------------------------------------------------
// state: latest
// ---------------------------------------------------------------------------

// TestApply_Latest_AllBackends — state latest for each backend: correct
// upgrade command, index refresh for apt/apk before it, changed=true when the
// version changed (or the package was absent).
func TestApply_Latest_AllBackends(t *testing.T) {
	cases := []struct {
		name       string
		mgrCmd     string // command -v <mgrCmd>
		refresh    string // index refresh command ("" = backend has no refresh)
		queryCmd   string // version query
		preQuery   util.Result
		postQuery  util.Result
		upgrade    string // expected upgrade command
		wantChange bool
		// extra call for apk (apk info -v) when installed
		extraOn  string
		extraRes util.Result
	}{
		{
			name:       "apt_upgrades",
			mgrCmd:     "apt-get",
			refresh:    aptUpdateCmd,
			queryCmd:   "dpkg-query -W -f=${Status} ${Version} nginx",
			preQuery:   util.Result{ExitCode: 0, Stdout: "install ok installed 1.0.0"},
			postQuery:  util.Result{ExitCode: 0, Stdout: "install ok installed 1.2.0"},
			upgrade:    aptInstallCmd("nginx"),
			wantChange: true,
		},
		{
			name:       "dnf_upgrades",
			mgrCmd:     "dnf",
			queryCmd:   "rpm -q --qf %{VERSION} nginx",
			preQuery:   util.Result{ExitCode: 0, Stdout: "1.0.0"},
			postQuery:  util.Result{ExitCode: 0, Stdout: "1.2.0"},
			upgrade:    "dnf install -y nginx",
			wantChange: true,
		},
		{
			name:       "yum_upgrades",
			mgrCmd:     "yum",
			queryCmd:   "rpm -q --qf %{VERSION} nginx",
			preQuery:   util.Result{ExitCode: 0, Stdout: "1.0.0"},
			postQuery:  util.Result{ExitCode: 0, Stdout: "1.2.0"},
			upgrade:    "yum install -y nginx",
			wantChange: true,
		},
		{
			name:       "apk_upgrades",
			mgrCmd:     "apk",
			refresh:    "apk update",
			queryCmd:   "apk info -e nginx",
			preQuery:   util.Result{ExitCode: 0, Stdout: "nginx"},
			postQuery:  util.Result{ExitCode: 0, Stdout: "nginx"},
			upgrade:    "apk add --upgrade nginx",
			wantChange: true,
			// apk info -ev is called twice (before and after); version changes via
			// OnSeq below (1.0.0-r0 → 1.2.0-r0 after name-stripping) → changed=true.
			extraOn: "apk info -ev nginx",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := internaltest.NewRunner()
			r.Fallback = util.Result{ExitCode: 1}
			r.On("command -v "+tc.mgrCmd, util.Result{ExitCode: 0})
			if tc.refresh != "" {
				r.On(tc.refresh, util.Result{ExitCode: 0})
			}
			r.OnSeq(tc.queryCmd, tc.preQuery, tc.postQuery)
			r.On(tc.upgrade, util.Result{ExitCode: 0})
			if tc.extraOn != "" {
				// apk: version changes between pre/post query → changed=true.
				r.OnSeq(tc.extraOn,
					util.Result{ExitCode: 0, Stdout: "nginx-1.0.0-r0\n"},
					util.Result{ExitCode: 0, Stdout: "nginx-1.2.0-r0\n"},
				)
			}
			m := &pkg.Module{Runner: r}

			stream := &internaltest.ApplyStream{}
			if err := m.Apply(&pluginv1.ApplyRequest{
				State:  "latest",
				Params: mustStruct(t, map[string]any{"name": "nginx"}),
			}, stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			ev := stream.Last()
			if ev.Failed {
				t.Fatalf("failed=true unexpectedly: %s", ev.Message)
			}
			if !hasCall(r, tc.upgrade) {
				t.Fatalf("ожидалась upgrade-команда %q, calls=%v", tc.upgrade, r.Calls)
			}
			if ev.Changed != tc.wantChange {
				t.Fatalf("changed=%v want %v", ev.Changed, tc.wantChange)
			}
			if tc.refresh != "" {
				ri, ui := callIndex(r, tc.refresh), callIndex(r, tc.upgrade)
				if ri < 0 || ri > ui {
					t.Fatalf("refresh %q должен предшествовать upgrade %q, calls=%v", tc.refresh, tc.upgrade, r.Calls)
				}
			}
		})
	}
}

// TestApply_Latest_NotInstalled_Installs — latest with an absent package
// installs it (changed=true, since !beforeInstalled).
func TestApply_Latest_NotInstalled_Installs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.OnSeq("rpm -q --qf %{VERSION} nginx",
		util.Result{ExitCode: 1},                  // before: not installed
		util.Result{ExitCode: 0, Stdout: "1.2.0"}, // after: installed
	)
	r.On("dnf install -y nginx", util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "latest",
		Params: mustStruct(t, map[string]any{"name": "nginx"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Changed {
		t.Fatal("changed=false, want true (пакета не было)")
	}
}

// TestApply_Latest_NoChange — latest with an already up-to-date package:
// installed and same version before/after upgrade → changed=false.
func TestApply_Latest_NoChange(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.On("rpm -q --qf %{VERSION} nginx", util.Result{ExitCode: 0, Stdout: "1.2.0"})
	r.On("dnf install -y nginx", util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "latest",
		Params: mustStruct(t, map[string]any{"name": "nginx"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if stream.Last().Changed {
		t.Fatal("changed=true, want false (версия не изменилась)")
	}
}

// TestApply_Latest_RefreshFails — refresh failure before latest upgrade →
// failed, upgrade is not run.
func TestApply_Latest_RefreshFails(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 100, Stderr: "network down"})
	r.On("dpkg-query -W -f=${Status} ${Version} nginx", util.Result{ExitCode: 1})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "latest",
		Params: mustStruct(t, map[string]any{"name": "nginx"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true (refresh упал)")
	}
	for _, c := range r.Calls {
		if c == aptInstallCmd("nginx") {
			t.Fatalf("upgrade не должен вызываться после провала refresh: %q", c)
		}
	}
}

// TestApply_Latest_UpgradeFails — upgrade itself returns non-zero → failed.
func TestApply_Latest_UpgradeFails(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.On("rpm -q --qf %{VERSION} nginx", util.Result{ExitCode: 0, Stdout: "1.0.0"})
	r.On("dnf install -y nginx", util.Result{ExitCode: 1, Stderr: "no such package"})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "latest",
		Params: mustStruct(t, map[string]any{"name": "nginx"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true (upgrade fail)")
	}
}

// ---------------------------------------------------------------------------
// install with a version pin: dnf / yum / apk (apt already covered)
// ---------------------------------------------------------------------------

// TestApply_Installed_VersionPin_RpmAndApk — pin rendering for dnf/yum
// (`name-ver`) and apk (`name=ver`); the apt pin (`name=ver`) is already
// covered by a separate test.
func TestApply_Installed_VersionPin_RpmAndApk(t *testing.T) {
	cases := []struct {
		name    string
		mgrCmd  string
		refresh string
		preQ    string
		want    string // install command with a pin
	}{
		{
			name:   "dnf",
			mgrCmd: "dnf",
			preQ:   "rpm -q --qf %{VERSION} nginx",
			want:   "dnf install -y nginx-1.2.0",
		},
		{
			name:   "yum",
			mgrCmd: "yum",
			preQ:   "rpm -q --qf %{VERSION} nginx",
			want:   "yum install -y nginx-1.2.0",
		},
		{
			name:    "apk",
			mgrCmd:  "apk",
			refresh: "apk update",
			preQ:    "apk info -e nginx",
			want:    "apk add --no-cache nginx=1.2.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := internaltest.NewRunner()
			r.Fallback = util.Result{ExitCode: 1}
			r.On("command -v "+tc.mgrCmd, util.Result{ExitCode: 0})
			if tc.refresh != "" {
				r.On(tc.refresh, util.Result{ExitCode: 0})
			}
			r.On(tc.preQ, util.Result{ExitCode: 1}) // not installed
			r.On(tc.want, util.Result{ExitCode: 0})
			m := &pkg.Module{Runner: r}

			stream := &internaltest.ApplyStream{}
			if err := m.Apply(&pluginv1.ApplyRequest{
				State:  "installed",
				Params: mustStruct(t, map[string]any{"name": "nginx", "version": "1.2.0"}),
			}, stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !hasCall(r, tc.want) {
				t.Fatalf("ожидался install с пином %q, calls=%v", tc.want, r.Calls)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// state: absent across all backends (apt already covered)
// ---------------------------------------------------------------------------

// TestApply_Absent_RpmAndApk — removing an installed package via dnf/yum/apk.
func TestApply_Absent_RpmAndApk(t *testing.T) {
	cases := []struct {
		name    string
		mgrCmd  string
		queries []struct {
			cmd string
			res util.Result
		}
		remove string
	}{
		{
			name:   "dnf",
			mgrCmd: "dnf",
			queries: []struct {
				cmd string
				res util.Result
			}{{"rpm -q --qf %{VERSION} nginx", util.Result{ExitCode: 0, Stdout: "1.2.0"}}},
			remove: "dnf remove -y nginx",
		},
		{
			name:   "yum",
			mgrCmd: "yum",
			queries: []struct {
				cmd string
				res util.Result
			}{{"rpm -q --qf %{VERSION} nginx", util.Result{ExitCode: 0, Stdout: "1.2.0"}}},
			remove: "yum remove -y nginx",
		},
		{
			name:   "apk",
			mgrCmd: "apk",
			queries: []struct {
				cmd string
				res util.Result
			}{
				{"apk info -e nginx", util.Result{ExitCode: 0, Stdout: "nginx"}},
				{"apk info -ev nginx", util.Result{ExitCode: 0, Stdout: "nginx-1.2.0-r0\n"}},
			},
			remove: "apk del nginx",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := internaltest.NewRunner()
			r.Fallback = util.Result{ExitCode: 1}
			r.On("command -v "+tc.mgrCmd, util.Result{ExitCode: 0})
			for _, q := range tc.queries {
				r.On(q.cmd, q.res)
			}
			r.On(tc.remove, util.Result{ExitCode: 0})
			m := &pkg.Module{Runner: r}

			stream := &internaltest.ApplyStream{}
			if err := m.Apply(&pluginv1.ApplyRequest{
				State:  "absent",
				Params: mustStruct(t, map[string]any{"name": "nginx"}),
			}, stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !stream.Last().Changed {
				t.Fatalf("changed=false, want true (%s remove)", tc.name)
			}
			if !hasCall(r, tc.remove) {
				t.Fatalf("ожидалась remove-команда %q, calls=%v", tc.remove, r.Calls)
			}
		})
	}
}

// TestApply_Absent_RemoveFails — remove returns non-zero → failed.
func TestApply_Absent_RemoveFails(t *testing.T) {
	r := aptInstalled("redis-server", "7:7.0.0-1")
	r.On(aptRemoveCmd("redis-server"), util.Result{ExitCode: 1, Stderr: "held package"})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true (remove fail)")
	}
}

// ---------------------------------------------------------------------------
// queryInstalled error paths: Err (binary failed to start), not non-zero exit
// ---------------------------------------------------------------------------

// TestApply_QueryError_PerBackend — if the query command fails to start
// (Err != nil), queryInstalled returns an error, Apply → failed. Per backend.
func TestApply_QueryError_PerBackend(t *testing.T) {
	runErr := errors.New("fork/exec: permission denied")
	cases := []struct {
		name   string
		mgrCmd string
		query  string
	}{
		{"apt", "apt-get", "dpkg-query -W -f=${Status} ${Version} redis"},
		{"dnf", "dnf", "rpm -q --qf %{VERSION} redis"},
		{"yum", "yum", "rpm -q --qf %{VERSION} redis"},
		{"apk", "apk", "apk info -e redis"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := internaltest.NewRunner()
			r.Fallback = util.Result{ExitCode: 1}
			r.On("command -v "+tc.mgrCmd, util.Result{ExitCode: 0})
			r.On(tc.query, util.Result{Err: runErr})
			m := &pkg.Module{Runner: r}

			stream := &internaltest.ApplyStream{}
			if err := m.Apply(&pluginv1.ApplyRequest{
				State:  "installed",
				Params: mustStruct(t, map[string]any{"name": "redis"}),
			}, stream); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			ev := stream.Last()
			if !ev.Failed {
				t.Fatalf("failed=false, want true (%s query Err)", tc.name)
			}
			if !strings.Contains(ev.Message, "permission denied") {
				t.Fatalf("message не несёт причину запуска: %q", ev.Message)
			}
		})
	}
}

// TestApply_Installed_PostInstallQueryError — install succeeded, but the
// follow-up query (to return the version) fails with Err → Apply failed.
func TestApply_Installed_PostInstallQueryError(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On(aptUpdateCmd, util.Result{ExitCode: 0})
	r.OnSeq("dpkg-query -W -f=${Status} ${Version} redis-server",
		util.Result{ExitCode: 1},                            // pre: not installed
		util.Result{Err: errors.New("dpkg-query vanished")}, // post: failed to start
	)
	r.On(aptInstallCmd("redis-server"), util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true (post-install query Err)")
	}
}

// TestApply_Absent_QueryError — query fails in the absent branch → failed.
func TestApply_Absent_QueryError(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{Err: errors.New("boom")})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true (absent query Err)")
	}
}

// TestApply_InstallCmdError — install command fails to start (Err != nil) →
// must returns an error, Apply failed. Covers the Err branch of must.
func TestApply_InstallCmdError(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.On("rpm -q --qf %{VERSION} redis", util.Result{ExitCode: 1}) // not installed
	r.On("dnf install -y redis", util.Result{Err: errors.New("exec format error")})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("failed=false, want true (install Err)")
	}
	if !strings.Contains(ev.Message, "exec format error") {
		t.Fatalf("message не несёт причину: %q", ev.Message)
	}
}

// ---------------------------------------------------------------------------
// dpkg status parse: installed, but Status != "install ok installed"
// ---------------------------------------------------------------------------

// TestApply_DpkgStatus_RemovedButConfigFiles — dpkg-query exits 0, but Status
// "deinstall ok config-files" → package considered NOT installed (a no-op for
// absent). Covers the not-installed branch of parseDpkgStatus.
func TestApply_DpkgStatus_RemovedButConfigFiles(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server",
		util.Result{ExitCode: 0, Stdout: "deinstall ok config-files 7:6.0.0-1"})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Changed {
		t.Fatal("changed=true, want false (config-files != installed → absent no-op)")
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "apt-get remove") {
			t.Fatalf("remove не должен вызываться для config-files-only пакета: %q", c)
		}
	}
}

// ---------------------------------------------------------------------------
// must: stderr with CR/LF and trailing whitespace collapses to one line
// (oneLine — \r and trailing-trim branches)
// ---------------------------------------------------------------------------

// TestApply_InstallFails_MultilineStderr — install non-zero with multiline
// stderr (\r\n + trailing space): message collapses to one line, no
// newlines and no trailing whitespace.
func TestApply_InstallFails_MultilineStderr(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.On("rpm -q --qf %{VERSION} redis", util.Result{ExitCode: 1})
	r.On("dnf install -y redis", util.Result{
		ExitCode: 1,
		Stderr:   "Error: Unable to find a match\r\nNothing to do  \n",
	})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("failed=false, want true")
	}
	if strings.ContainsAny(ev.Message, "\r\n") {
		t.Fatalf("message содержит CR/LF, oneLine не схлопнул: %q", ev.Message)
	}
	if strings.HasSuffix(ev.Message, " ") {
		t.Fatalf("message с хвостовым пробелом, trailing-trim не сработал: %q", ev.Message)
	}
	if !strings.Contains(ev.Message, "Unable to find a match") {
		t.Fatalf("message потерял текст stderr: %q", ev.Message)
	}
}

// ---------------------------------------------------------------------------
// Apply: input validation and state routing
// ---------------------------------------------------------------------------

// TestApply_VersionWrongType_Fails — version passed as a number, not a
// string → OptStringParam error → Apply failed (before detect-pkg-mgr).
func TestApply_VersionWrongType_Fails(t *testing.T) {
	r := internaltest.NewRunner()
	m := &pkg.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis", "version": 7.0}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !stream.Last().Failed {
		t.Fatal("failed=false, want true (version не строка)")
	}
	// detect-pkg-mgr must not run: the error happens earlier.
	if len(r.Calls) != 0 {
		t.Fatalf("команды не должны выполняться при ошибке параметра, calls=%v", r.Calls)
	}
}

// TestApply_UnknownState_Fails — unknown state (past detect) → failed,
// mentioning the state. Covers the default branch of the switch in Apply.
func TestApply_UnknownState_Fails(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}
	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "frobnicate",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if !ev.Failed {
		t.Fatal("failed=false, want true (unknown state)")
	}
	if !strings.Contains(ev.Message, "frobnicate") {
		t.Fatalf("message не называет неизвестный state: %q", ev.Message)
	}
}

// TestApply_DetectViaWhichFallback — `command -v` unavailable (exit != 0),
// but `which apt-get` succeeds → backend still resolves (DetectPkgMgr
// fallback branch). Checks the install path picked apt.
func TestApply_DetectViaWhichFallback(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1} // command -v <any> → not found
	r.On("which apt-get", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis", util.Result{ExitCode: 0, Stdout: "install ok installed 1.0"})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ev := stream.Last()
	if ev.Failed {
		t.Fatalf("failed=true, want false (which-fallback должен дать apt): %s", ev.Message)
	}
	if !hasCall(r, "dpkg-query -W -f=${Status} ${Version} redis") {
		t.Fatalf("apt-путь не выбран через which-fallback, calls=%v", r.Calls)
	}
}

// TestApply_ApkInstalled_NoTrailingNewline — apk info -ev without a trailing
// \n: firstLine returns the whole line, parseApkVersion strips the name →
// bare number (MINOR-C).
func TestApply_ApkInstalled_NoTrailingNewline(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0})
	r.On("apk info -e redis", util.Result{ExitCode: 0, Stdout: "redis"})
	r.On("apk info -ev redis", util.Result{ExitCode: 0, Stdout: "redis-7.0.0-r0"}) // no \n
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := stream.Last().Output.Fields["version"].GetStringValue(); got != "7.0.0-r0" {
		t.Fatalf("version=%q, want 7.0.0-r0 (номер без имени, MINOR-C)", got)
	}
}

// TestPlan_Installed_AlreadyPresent_Clean — Plan(dry_run) for an already
// installed package with no drift: changed=false, and NO mutating commands
// (install/remove/update) — pure-read (ADR-031 Scry).
func TestPlan_Installed_AlreadyPresent_Clean(t *testing.T) {
	r := aptInstalled("redis-server", "7:7.0.0-1")
	m := &pkg.Module{Runner: r}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := lastPlan(stream); got == nil || got.GetChanged() {
		t.Fatalf("changed=%v, want false (clean)", got.GetChanged())
	}
	assertNoMutatingPkgCalls(t, r)
}

// TestPlan_Installed_NotPresent_Drift — Plan for an absent package:
// changed=true (Apply would install it), no mutations.
func TestPlan_Installed_NotPresent_Drift(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	m := &pkg.Module{Runner: r}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := lastPlan(stream); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingPkgCalls(t, r)
}

// TestPlan_Absent_Present_Drift — Plan for state absent with an installed
// package: changed=true (Apply would remove it), no mutations.
func TestPlan_Absent_Present_Drift(t *testing.T) {
	r := aptInstalled("redis-server", "7:7.0.0-1")
	m := &pkg.Module{Runner: r}

	stream := &planStream{}
	if err := m.Plan(&pluginv1.PlanRequest{
		State:  "absent",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := lastPlan(stream); got == nil || !got.GetChanged() {
		t.Fatalf("changed=false, want true (drift)")
	}
	assertNoMutatingPkgCalls(t, r)
}

// TestPlan_Latest_Unsupported — Plan for state latest returns an explicit
// error ("is there a newer version" drift can't be derived from pure-read),
// not a false-clean.
func TestPlan_Latest_Unsupported(t *testing.T) {
	r := aptInstalled("redis-server", "7:7.0.0-1")
	m := &pkg.Module{Runner: r}

	err := m.Plan(&pluginv1.PlanRequest{
		State:  "latest",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, &planStream{})
	if err == nil {
		t.Fatal("Plan(latest) вернул nil, ожидалась явная ошибка не-поддержки")
	}
}

func lastPlan(s *planStream) *pluginv1.PlanEvent {
	if len(s.events) == 0 {
		return nil
	}
	return s.events[len(s.events)-1]
}

// assertNoMutatingPkgCalls — fails if the runner received an
// install/remove/del/add/update command (Plan must be pure-read, ADR-031).
func assertNoMutatingPkgCalls(t *testing.T, r *internaltest.Runner) {
	t.Helper()
	for _, c := range r.Calls {
		for _, bad := range []string{"install", "remove", " del ", " add ", "update", "upgrade"} {
			if strings.Contains(c, bad) {
				t.Fatalf("Plan вызвал мутирующую команду %q (должен быть pure-read)", c)
			}
		}
	}
}

// planStream — fake grpc.ServerStreamingServer[PlanEvent] for the Plan test.
type planStream struct {
	grpc.ServerStreamingServer[pluginv1.PlanEvent]
	events []*pluginv1.PlanEvent
}

func (s *planStream) Send(e *pluginv1.PlanEvent) error {
	s.events = append(s.events, e)
	return nil
}

func (s *planStream) Context() context.Context { return context.Background() }
