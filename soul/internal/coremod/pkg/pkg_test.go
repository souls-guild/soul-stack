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

// aptInstalled — детект apt + dpkg-query вернувший installed.
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
	// Не должно быть вызова apt-get install.
	for _, c := range r.Calls {
		if strings.HasPrefix(c, "apt-get install") {
			t.Fatalf("unexpected install call: %q", c)
		}
	}
}

func TestApply_Installed_VersionMismatch_Reinstalls(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("apt-get update", util.Result{ExitCode: 0})
	r.OnSeq("dpkg-query -W -f=${Status} ${Version} redis-server",
		util.Result{ExitCode: 0, Stdout: "install ok installed 7:6.0.0-1"},
		util.Result{ExitCode: 0, Stdout: "install ok installed 7:7.0.0-1"},
	)
	r.On("apt-get install -y redis-server=7:7.0.0-1", util.Result{ExitCode: 0})
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
	r.On("apt-get update", util.Result{ExitCode: 0})
	// dpkg-query exits 1 — пакет не установлен.
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On("apt-get install -y redis-server", util.Result{ExitCode: 0})
	// Post-install: pacackge present.
	// Перезаписываем dpkg-query результат. Здесь невозможно (map): поэтому
	// fake-runner повторяет одинаковый ответ на повторные вызовы. Для теста
	// «changed=true после install» этого достаточно — нам важен сам факт
	// changed, версия после может быть пустой.
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

// hasCall — был ли среди вызовов runner-а ровно такой (space-joined) аргумент-набор.
func hasCall(r *internaltest.Runner, want string) bool {
	return callIndex(r, want) >= 0
}

// callIndex — позиция первого ровно-совпадающего вызова в r.Calls; -1 если не было.
func callIndex(r *internaltest.Runner, want string) int {
	for i, c := range r.Calls {
		if c == want {
			return i
		}
	}
	return -1
}

// TestApply_Installed_NoVersion_InstallsWithoutPin — BUG-1: version пустой/не
// задан → install БЕЗ =version-пина (latest available из репо), не `name=`.
// Покрывает все четыре backend-а: рендер install-команды без пина.
func TestApply_Installed_NoVersion_InstallsWithoutPin(t *testing.T) {
	cases := []struct {
		name    string
		mgrCmd  string // command -v <mgrCmd>
		refresh string // ожидаемая refresh-команда индекса репо ("" = mgr без refresh)
		query   string // запрос «установлен ли пакет» (возвращает «не установлен»)
		want    string // ожидаемая install-команда БЕЗ пина
		notWant string // подстрока пина, которой быть НЕ должно
	}{
		{
			name:    "apt",
			mgrCmd:  "apt-get",
			refresh: "apt-get update",
			query:   "dpkg-query -W -f=${Status} ${Version} redis-server",
			want:    "apt-get install -y redis-server",
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
			r.On(tc.query, util.Result{ExitCode: 1}) // пакет не установлен
			r.On(tc.want, util.Result{ExitCode: 0})  // install без пина
			m := &pkg.Module{Runner: r}

			stream := &internaltest.ApplyStream{}
			// version не передан вовсе (required:false на уровне контракта).
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
			// refresh индекса репо (apt/apk) обязан предшествовать install;
			// для dnf/yum refresh-команды нет (metadata auto-refresh).
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

// TestApply_Installed_EmptyVersion_InstallsWithoutPin — version передан пустой
// строкой ("") трактуется идентично «не задан»: install без пина (BUG-1).
func TestApply_Installed_EmptyVersion_InstallsWithoutPin(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("apt-get update", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On("apt-get install -y redis-server", util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server", "version": ""}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(r, "apt-get install -y redis-server") {
		t.Fatalf("ожидался install без пина при version=\"\", calls=%v", r.Calls)
	}
	for _, c := range r.Calls {
		if strings.Contains(c, "redis-server=") {
			t.Fatalf("version=\"\" не должен давать пин, но получили: %q", c)
		}
	}
}

// TestApply_Installed_DistroNativeVersion_PinsExact — distro-native version
// (epoch + revision, Debian-форма) пинуется как есть: `name=<version>` (apt).
// BUG-1: pattern контракта теперь допускает такие строки, а модуль обязан
// прокинуть их в install дословно.
func TestApply_Installed_DistroNativeVersion_PinsExact(t *testing.T) {
	const ver = "5:7.0.15-1~deb12u7"
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("apt-get update", util.Result{ExitCode: 0})
	r.OnSeq("dpkg-query -W -f=${Status} ${Version} redis-server",
		util.Result{ExitCode: 1}, // до install — не установлен
		util.Result{ExitCode: 0, Stdout: "install ok installed " + ver}, // после
	)
	r.On("apt-get install -y redis-server="+ver, util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server", "version": ver}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !hasCall(r, "apt-get install -y redis-server="+ver) {
		t.Fatalf("ожидался точный пин distro-native версии, calls=%v", r.Calls)
	}
}

// countCalls — сколько раз ровно такая команда встретилась в r.Calls.
func countCalls(r *internaltest.Runner, want string) int {
	n := 0
	for _, c := range r.Calls {
		if c == want {
			n++
		}
	}
	return n
}

// TestApply_Apt_RefreshBeforeInstall — фикс прод-бага: на свежей VM install без
// предварительного `apt-get update` упирается в «Unable to locate package».
// Проверяем, что update вызывается и предшествует install.
func TestApply_Apt_RefreshBeforeInstall(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("apt-get update", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On("apt-get install -y redis-server", util.Result{ExitCode: 0})
	m := &pkg.Module{Runner: r}

	stream := &internaltest.ApplyStream{}
	if err := m.Apply(&pluginv1.ApplyRequest{
		State:  "installed",
		Params: mustStruct(t, map[string]any{"name": "redis-server"}),
	}, stream); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	ri, ii := callIndex(r, "apt-get update"), callIndex(r, "apt-get install -y redis-server")
	if ri < 0 {
		t.Fatalf("apt-get update не вызван, calls=%v", r.Calls)
	}
	if ri > ii {
		t.Fatalf("apt-get update должен идти ДО install, calls=%v", r.Calls)
	}
}

// TestApply_Apk_RefreshBeforeInstall — apk-аналог: `apk update` перед `apk add`,
// иначе на свежем образе пакет не находится.
func TestApply_Apk_RefreshBeforeInstall(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0})
	r.On("apk update", util.Result{ExitCode: 0})
	r.On("apk info -e redis", util.Result{ExitCode: 1}) // не установлен
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

// TestApply_Apt_RefreshOncePerProcess — refresh индекса выполняется один раз за
// жизнь процесса (вариант (б)): один Module обслуживает несколько install-шагов
// прогона, второй install НЕ должен повторять `apt-get update`.
func TestApply_Apt_RefreshOncePerProcess(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("apt-get update", util.Result{ExitCode: 0})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On("dpkg-query -W -f=${Status} ${Version} nginx", util.Result{ExitCode: 1})
	r.On("apt-get install -y redis-server", util.Result{ExitCode: 0})
	r.On("apt-get install -y nginx", util.Result{ExitCode: 0})
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
	if n := countCalls(r, "apt-get update"); n != 1 {
		t.Fatalf("apt-get update вызван %d раз, ожидался ровно 1 (refresh-once), calls=%v", n, r.Calls)
	}
}

// TestApply_Dnf_NoRefresh — dnf/yum auto-refresh metadata по expiration, явный
// update НЕ добавляем; install при «не установлен» вызывается сразу.
func TestApply_Dnf_NoRefresh(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.On("rpm -q --qf %{VERSION} redis", util.Result{ExitCode: 1}) // не установлен
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

// TestApply_Apt_RefreshFails_NoInstall — если `apt-get update` упал, install НЕ
// выполняется и Apply возвращает failed (не пытаемся ставить по устаревшему
// индексу). Флаг refresh-done при этом не ставится.
func TestApply_Apt_RefreshFails_NoInstall(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("apt-get update", util.Result{ExitCode: 100, Stderr: "Could not resolve host"})
	r.On("dpkg-query -W -f=${Status} ${Version} redis-server", util.Result{ExitCode: 1})
	r.On("apt-get install -y redis-server", util.Result{ExitCode: 0})
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
		if strings.HasPrefix(c, "apt-get install") {
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
		if strings.HasPrefix(c, "apt-get remove") {
			t.Fatalf("unexpected remove call: %q", c)
		}
	}
}

func TestApply_Absent_Present_Removes(t *testing.T) {
	r := aptInstalled("redis-server", "7:7.0.0-1")
	r.On("apt-get remove -y redis-server", util.Result{ExitCode: 0})
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
	// `apk info -ev <name>` → `<name>-<version>`; модуль срезает `redis-` префикс,
	// в register.version попадает чистый номер (MINOR-C).
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

// TestApply_PkgMgrFromFact_NoDetect — BUG-B: soulprint-факт pkg_mgr=apk primary,
// `command -v`/`which` не отвечают (fallback-детект провалился бы и модуль упал бы
// «no supported package manager»). С фактом модуль идёт прямо в apk-ветку.
func TestApply_PkgMgrFromFact_NoDetect(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 127} // ВСЕ detection-команды отсутствуют
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

// TestApply_PkgMgrFactEmpty_FallbackDetect — пустой факт → runtime-детект
// (factless-хост: push-режим / старый Keeper без soulprint).
func TestApply_PkgMgrFactEmpty_FallbackDetect(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0}) // детект → apk
	r.On("apk info -e redis", util.Result{ExitCode: 0, Stdout: "redis"})
	r.On("apk info -ev redis", util.Result{ExitCode: 0, Stdout: "redis-7.0.0-r0\n"})
	m := &pkg.Module{Runner: r}
	// SetHostFacts не вызываем — facts пуст.

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

// TestApply_ApkVersion_NameWithDash — MINOR-C critical: имя apk-пакета может
// содержать дефис (`py3-pip`); split по дефису дал бы неверную версию. Срез
// известного `<name>-` префикса даёт корректный номер.
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

// TestApply_Latest_AllBackends — state latest по каждому backend-у: правильная
// upgrade-команда, refresh-индекса для apt/apk перед ней, changed=true когда
// версия изменилась (или пакета не было).
func TestApply_Latest_AllBackends(t *testing.T) {
	cases := []struct {
		name       string
		mgrCmd     string // command -v <mgrCmd>
		refresh    string // refresh-команда индекса ("" = backend без refresh)
		queryCmd   string // запрос версии
		preQuery   util.Result
		postQuery  util.Result
		upgrade    string // ожидаемая upgrade-команда
		wantChange bool
		// дополнительный вызов для apk (apk info -v) при installed
		extraOn  string
		extraRes util.Result
	}{
		{
			name:       "apt_upgrades",
			mgrCmd:     "apt-get",
			refresh:    "apt-get update",
			queryCmd:   "dpkg-query -W -f=${Status} ${Version} nginx",
			preQuery:   util.Result{ExitCode: 0, Stdout: "install ok installed 1.0.0"},
			postQuery:  util.Result{ExitCode: 0, Stdout: "install ok installed 1.2.0"},
			upgrade:    "apt-get install -y nginx",
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
			// apk info -ev вызывается дважды (до и после); версия меняется через
			// OnSeq ниже (1.0.0-r0 → 1.2.0-r0 после среза имени) → changed=true.
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
				// apk: версия меняется между pre/post запросом → changed=true.
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

// TestApply_Latest_NotInstalled_Installs — latest при отсутствующем пакете
// устанавливает его (changed=true, т.к. !beforeInstalled).
func TestApply_Latest_NotInstalled_Installs(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.OnSeq("rpm -q --qf %{VERSION} nginx",
		util.Result{ExitCode: 1},                  // до: не установлен
		util.Result{ExitCode: 0, Stdout: "1.2.0"}, // после: установлен
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

// TestApply_Latest_NoChange — latest при уже-свежем пакете: установлен и версия
// та же до и после upgrade → changed=false.
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

// TestApply_Latest_RefreshFails — провал refresh перед latest-upgrade →
// failed, upgrade не выполняется.
func TestApply_Latest_RefreshFails(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("apt-get update", util.Result{ExitCode: 100, Stderr: "network down"})
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
		if c == "apt-get install -y nginx" {
			t.Fatalf("upgrade не должен вызываться после провала refresh: %q", c)
		}
	}
}

// TestApply_Latest_UpgradeFails — сам upgrade вернул non-zero → failed.
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
// install с version-пином: dnf / yum / apk (apt уже покрыт)
// ---------------------------------------------------------------------------

// TestApply_Installed_VersionPin_RpmAndApk — рендер пина для dnf/yum (`name-ver`)
// и apk (`name=ver`); apt-пин (`name=ver`) уже покрыт отдельным тестом.
func TestApply_Installed_VersionPin_RpmAndApk(t *testing.T) {
	cases := []struct {
		name    string
		mgrCmd  string
		refresh string
		preQ    string
		want    string // install-команда с пином
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
			r.On(tc.preQ, util.Result{ExitCode: 1}) // не установлен
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
// state: absent по всем backend-ам (apt уже покрыт)
// ---------------------------------------------------------------------------

// TestApply_Absent_RpmAndApk — удаление установленного пакета через dnf/yum/apk.
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

// TestApply_Absent_RemoveFails — remove вернул non-zero → failed.
func TestApply_Absent_RemoveFails(t *testing.T) {
	r := aptInstalled("redis-server", "7:7.0.0-1")
	r.On("apt-get remove -y redis-server", util.Result{ExitCode: 1, Stderr: "held package"})
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
// error-пути queryInstalled: Err (binary не запустился), а не non-zero exit
// ---------------------------------------------------------------------------

// TestApply_QueryError_PerBackend — если query-команда не запустилась (Err != nil),
// queryInstalled возвращает ошибку, Apply → failed. По каждому backend-у.
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

// TestApply_Installed_PostInstallQueryError — install прошёл, но повторный
// query (для возврата версии) упал с Err → Apply failed.
func TestApply_Installed_PostInstallQueryError(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apt-get", util.Result{ExitCode: 0})
	r.On("apt-get update", util.Result{ExitCode: 0})
	r.OnSeq("dpkg-query -W -f=${Status} ${Version} redis-server",
		util.Result{ExitCode: 1},                            // pre: не установлен
		util.Result{Err: errors.New("dpkg-query vanished")}, // post: упал запуск
	)
	r.On("apt-get install -y redis-server", util.Result{ExitCode: 0})
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

// TestApply_Absent_QueryError — query упал в absent-ветке → failed.
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

// TestApply_InstallCmdError — install-команда не запустилась (Err != nil) →
// must возвращает ошибку, Apply failed. Покрывает Err-ветку must.
func TestApply_InstallCmdError(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v dnf", util.Result{ExitCode: 0})
	r.On("rpm -q --qf %{VERSION} redis", util.Result{ExitCode: 1}) // не установлен
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
// dpkg status parse: установлен, но Status != "install ok installed"
// ---------------------------------------------------------------------------

// TestApply_DpkgStatus_RemovedButConfigFiles — dpkg-query exit 0, но Status
// "deinstall ok config-files" → пакет считается НЕ установленным (для absent
// это no-op). Покрывает not-installed-ветку parseDpkgStatus.
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
		if strings.HasPrefix(c, "apt-get remove") {
			t.Fatalf("remove не должен вызываться для config-files-only пакета: %q", c)
		}
	}
}

// ---------------------------------------------------------------------------
// must: stderr с CR/LF и хвостовыми пробелами схлопывается в одну строку
// (oneLine — \r и trailing-trim ветки)
// ---------------------------------------------------------------------------

// TestApply_InstallFails_MultilineStderr — install non-zero с многострочным
// stderr (\r\n + хвостовой пробел): message сводится в одну строку без
// переводов строк и без хвостовых пробелов.
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
// Apply: входная валидация и роутинг state
// ---------------------------------------------------------------------------

// TestApply_VersionWrongType_Fails — version передан числом, а не строкой →
// OptStringParam ошибка → Apply failed (до detect-pkg-mgr).
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
	// detect-pkg-mgr не должен запускаться: ошибка раньше.
	if len(r.Calls) != 0 {
		t.Fatalf("команды не должны выполняться при ошибке параметра, calls=%v", r.Calls)
	}
}

// TestApply_UnknownState_Fails — неизвестный state (прошедший detect) → failed
// с упоминанием state. Покрывает default-ветку switch в Apply.
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

// TestApply_DetectViaWhichFallback — `command -v` недоступен (exit !=0), но
// `which apt-get` отрабатывает → backend всё равно определяется (fallback-ветка
// DetectPkgMgr). Проверяем, что путь установки выбрал apt.
func TestApply_DetectViaWhichFallback(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1} // command -v <любой> → не найден
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

// TestApply_ApkInstalled_NoTrailingNewline — apk info -ev без хвостового \n:
// firstLine отдаёт всю строку, parseApkVersion срезает имя → чистый номер (MINOR-C).
func TestApply_ApkInstalled_NoTrailingNewline(t *testing.T) {
	r := internaltest.NewRunner()
	r.Fallback = util.Result{ExitCode: 1}
	r.On("command -v apk", util.Result{ExitCode: 0})
	r.On("apk info -e redis", util.Result{ExitCode: 0, Stdout: "redis"})
	r.On("apk info -ev redis", util.Result{ExitCode: 0, Stdout: "redis-7.0.0-r0"}) // без \n
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

// TestPlan_Installed_AlreadyPresent_Clean — Plan(dry_run) для уже
// установленного пакета без drift: changed=false, и НИ ОДНОЙ мутирующей команды
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

// TestPlan_Installed_NotPresent_Drift — Plan для отсутствующего пакета:
// changed=true (Apply установил бы), без мутаций.
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

// TestPlan_Absent_Present_Drift — Plan для state absent при установленном пакете:
// changed=true (Apply удалил бы), без мутаций.
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

// TestPlan_Latest_Unsupported — Plan для state latest возвращает явную ошибку
// (drift «есть ли новее» не выводится из pure-read), а НЕ false-clean.
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

// assertNoMutatingPkgCalls — фейлит, если runner получил install/remove/del/add/
// update команду (Plan обязан быть pure-read, ADR-031).
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

// planStream — fake grpc.ServerStreamingServer[PlanEvent] для теста Plan.
type planStream struct {
	grpc.ServerStreamingServer[pluginv1.PlanEvent]
	events []*pluginv1.PlanEvent
}

func (s *planStream) Send(e *pluginv1.PlanEvent) error {
	s.events = append(s.events, e)
	return nil
}

func (s *planStream) Context() context.Context { return context.Background() }
