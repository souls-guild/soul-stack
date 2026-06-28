package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// dragonflyTemplatesDir — каталог .tmpl destiny dragonfly относительно этого пакета.
// L0-trial этой destiny ассертит ПЛАН и гоняет только CEL-фазу — text/template-фазу
// (рендер flagfile/units) НЕ исполняет. Эти L1-тесты закрывают text/template-фазу:
// доказывают, что flagfile несёт КОРРЕКТНЫЕ DF-флаги (--flag=value, absl), а не
// redis.conf-директивы. Регресс формы flagfile (дефис вместо underscore, redis-style
// `port 6379` вместо `--port=6379`) на L0 не виден — ловится здесь.
const dragonflyTemplatesDir = "../../../examples/destiny/dragonfly/templates"

// renderDragonflyTmpl рендерит один .tmpl destiny dragonfly через тот же shared/tmpl.Engine,
// что и Soul (strict, missingkey=error). Падение Parse/Execute = провал теста.
func renderDragonflyTmpl(t *testing.T, name string, root map[string]any) string {
	t.Helper()
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dragonflyTemplatesDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	out, err := engine.Render(string(body), root)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return out
}

// TestDragonflyFlagfile_AbslFlagForm — guard на ФОРМУ flagfile DragonFly (absl-флаги
// --flag=value, НЕ redis.conf). Корень риска: DragonFly читает flagfile (key=value с
// двойным дефисом и знаком `=`), а НЕ redis.conf-синтаксис (`directive value`). Регресс
// «отрендерили redis.conf вместо flagfile» (нет `--`, нет `=`, дефис в имени флага)
// сделал бы DF неспособным распарсить конфиг → отказ старта на хосте, невидимый на L0.
//
// Тест рендерит РЕАЛЬНЫЙ dragonfly.flags.tmpl с контекстом, который собирает scenario
// (vars + .self), и доказывает: (а) базовые директивы в absl-форме --<flag>=<value>;
// (б) host-layout (--dir/--aclfile/--pidfile/--log_dir) выводится из переданных каталогов;
// (в) merged config range-ится в --<key>=<value> с DF-флагами (underscore); (г) bind —
// из .self.network.primary_ip (per-host).
func TestDragonflyFlagfile_AbslFlagForm(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"password": "s3cr3t-dragonfly-pass",
			"conf_dir": "/etc/dragonfly",
			"data_dir": "/var/lib/dragonfly",
			"port":     6379,
			"run_dir":  "/var/run/dragonfly",
			"log_dir":  "/var/log/dragonfly",
			// merged config от scenario — DF-флаги (underscore-форма). TLS-блок здесь:
			// доказывает, что bool-флаг tls рендерится как --tls=true (валидно для absl).
			// ★ maxmemory_policy НЕ кладём: у DF нет такого флага (absl FATAL на неизвестном).
			// ★ tls_port НЕ кладём: у DF нет tls_port — TLS встаёт на основной --port
			// (TestDragonflyFlagfile_TLSOnMainPort гейтит отсутствие --tls_port).
			"config": map[string]any{
				"maxmemory":     "256mb",
				"maxclients":    10000,
				"tls":           "true",
				"tls_cert_file": "/etc/dragonfly/tls/dragonfly.crt",
			},
		},
		"self": map[string]any{
			"network": map[string]any{"primary_ip": "10.0.0.1"},
		},
	}

	out := renderDragonflyFlagfileNormalized(t, root)

	// (а) базовые директивы — absl-форма --<flag>=<value>, host-layout из каталогов.
	mustContainLine(t, out, "--bind=10.0.0.1")
	mustContainLine(t, out, "--port=6379")
	// unixsocket — локальный listener (DF при --bind=primary_ip НЕ слушает 127.0.0.1;
	// локальные вызовы плагина идут через сокет). Путь выводится из run_dir.
	mustContainLine(t, out, "--unixsocket=/var/run/dragonfly/dragonfly.sock")
	mustContainLine(t, out, "--dir=/var/lib/dragonfly")
	mustContainLine(t, out, "--dbfilename=dump")
	mustContainLine(t, out, "--requirepass=s3cr3t-dragonfly-pass")
	mustContainLine(t, out, "--aclfile=/etc/dragonfly/users.acl")
	mustContainLine(t, out, "--pidfile=/var/run/dragonfly/dragonfly.pid")
	mustContainLine(t, out, "--log_dir=/var/log/dragonfly")

	// (в) merged config — --<key>=<value> с DF-флагами (underscore). bool tls=true.
	mustContainLine(t, out, "--maxmemory=256mb")
	mustContainLine(t, out, "--maxclients=10000")
	mustContainLine(t, out, "--tls=true")
	mustContainLine(t, out, "--tls_cert_file=/etc/dragonfly/tls/dragonfly.crt")

	// НЕ redis.conf: ни одной строки без ведущего `--` (кроме пустых) — каждая
	// непустая строка обязана быть absl-флагом. Ловит регресс «redis.conf-синтаксис».
	for _, line := range strings.Split(out, "\n") {
		ln := strings.TrimSpace(line)
		if ln == "" {
			continue
		}
		if !strings.HasPrefix(ln, "--") {
			t.Fatalf("flagfile содержит не-absl строку (ожидался --flag=value): %q", ln)
		}
	}
}

// TestDragonflyFlagfile_TLSOnMainPort — guard: TLS DragonFly встаёт на ОСНОВНОЙ --port,
// БЕЗ отдельного tls_port. Корень риска: у DragonFly НЕТ флага tls_port (разведка src:
// facade/dragonfly_listener.cc ABSL_FLAG(bool, tls, ...) переводит ОСНОВНОЙ listener в TLS,
// tls_helpers.cc несёт tls_cert_file/tls_key_file/tls_ca_cert_file; tls_port отсутствует).
// Регресс «вернули tls_port в config» → absl FATAL на неизвестном флаге → DF не стартует,
// невидимо на L0. Тест рендерит TLS-блок БЕЗ tls_port и доказывает: --tls=true +
// --tls_cert_file присутствуют, --unixsocket присутствует, а строки --tls_port НЕТ.
func TestDragonflyFlagfile_TLSOnMainPort(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"password": "s3cr3t-dragonfly-pass",
			"conf_dir": "/etc/dragonfly",
			"data_dir": "/var/lib/dragonfly",
			"port":     6379,
			"run_dir":  "/var/run/dragonfly",
			"log_dir":  "/var/log/dragonfly",
			// TLS-блок в DF-форме БЕЗ tls_port (TLS на основном --port).
			"config": map[string]any{
				"tls":              "true",
				"tls_cert_file":    "/etc/dragonfly/tls/dragonfly.crt",
				"tls_key_file":     "/etc/dragonfly/tls/dragonfly.key",
				"tls_ca_cert_file": "/etc/dragonfly/tls/ca.crt",
				"tls_replication":  "true",
			},
		},
		"self": map[string]any{
			"network": map[string]any{"primary_ip": "10.0.0.1"},
		},
	}

	out := renderDragonflyFlagfileNormalized(t, root)

	mustContainLine(t, out, "--tls=true")
	mustContainLine(t, out, "--tls_cert_file=/etc/dragonfly/tls/dragonfly.crt")
	mustContainLine(t, out, "--tls_key_file=/etc/dragonfly/tls/dragonfly.key")
	mustContainLine(t, out, "--tls_ca_cert_file=/etc/dragonfly/tls/ca.crt")
	mustContainLine(t, out, "--unixsocket=/var/run/dragonfly/dragonfly.sock")

	// ★ АНТИ-GUARD: ни одной строки с --tls_port. У DragonFly нет такого флага — его
	// присутствие = absl FATAL на старте. Ловит регресс возврата tls_port в df_config.
	if strings.Contains(out, "--tls_port") {
		t.Fatalf("flagfile содержит --tls_port: у DragonFly нет такого флага (TLS встаёт на основной --port). Рендер:\n%s", out)
	}
}

// TestDragonflyFlagfile_HostLayoutOverride — guard директивы B (override conf_dir/data_dir
// доезжает до flagfile). Регресс «--dir/--aclfile хардкодят /var/lib/dragonfly,/etc/dragonfly
// игнорируя override» сломал бы кастомный storage-layout оператора (snapshot в чужой
// каталог → отказ записи под systemd).
func TestDragonflyFlagfile_HostLayoutOverride(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"password": "pw0000000000000000",
			"conf_dir": "/opt/df/conf",
			"data_dir": "/mnt/df/data",
			"port":     6379,
			"run_dir":  "/var/run/dragonfly",
			"log_dir":  "/var/log/dragonfly",
			"config":   map[string]any{},
		},
		"self": map[string]any{
			"network": map[string]any{"primary_ip": "10.0.0.5"},
		},
	}
	out := renderDragonflyFlagfileNormalized(t, root)
	mustContainLine(t, out, "--dir=/mnt/df/data")
	mustContainLine(t, out, "--aclfile=/opt/df/conf/users.acl")
}

// TestDragonflyServiceUnit_TypeSimpleAndExecStart — guard юнита DragonFly (binary).
// DragonFly — foreground БЕЗ sd_notify → Type=simple (НЕ notify как redis). ExecStart —
// --flagfile-форма из bin_dir/conf_dir. Регресс Type=notify подвесил бы старт (systemd
// ждал бы READY-нотификацию, которой DF не шлёт).
func TestDragonflyServiceUnit_TypeSimpleAndExecStart(t *testing.T) {
	root := map[string]any{
		"vars": map[string]any{
			"bin_dir":         "/usr/local/bin",
			"conf_dir":        "/etc/dragonfly",
			"dragonfly_user":  "dragonfly",
			"dragonfly_group": "dragonfly",
			"run_dir":         "/var/run/dragonfly",
			"data_dir":        "/var/lib/dragonfly",
			"log_dir":         "/var/log/dragonfly",
		},
	}
	out := renderDragonflyTmpl(t, "dragonfly.service.tmpl", root)
	mustContainLine(t, out, "Type=simple")
	mustContainLine(t, out, "ExecStart=/usr/local/bin/dragonfly --flagfile=/etc/dragonfly/dragonfly.conf")
	mustContainLine(t, out, "User=dragonfly")
	mustContainLine(t, out, "Group=dragonfly")
	if strings.Contains(out, "Type=notify") {
		t.Fatalf("юнит DragonFly не должен быть Type=notify (DF без sd_notify): %s", out)
	}
}

// renderDragonflyFlagfileNormalized рендерит dragonfly.flags.tmpl и нормализует
// возможные хвостовые пробелы/строки. Вынесено: оба flagfile-теста читают тот же шаблон.
func renderDragonflyFlagfileNormalized(t *testing.T, root map[string]any) string {
	t.Helper()
	return renderDragonflyTmpl(t, "dragonfly.flags.tmpl", root)
}

// mustContainLine падает, если среди строк out нет точного (после trim) совпадения с want.
// Сверка по ЦЕЛОЙ строке (не подстроке): `--port=6379` не должен пройти за счёт
// `--tls_port=6379` и т.п.
func mustContainLine(t *testing.T, out, want string) {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return
		}
	}
	t.Fatalf("ожидалась строка %q в рендере, не найдена. Рендер:\n%s", want, out)
}
