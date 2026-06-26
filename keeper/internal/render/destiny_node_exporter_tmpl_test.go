package render

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/souls-guild/soul-stack/shared/tmpl"
)

// nodeExporterTemplatesDir — каталог .tmpl destiny node-exporter относительно
// этого пакета (keeper/internal/render). Тест живёт здесь, потому что именно
// этот пакет владеет интеграцией с text/template-движком Soul-а (shared/tmpl)
// и уже держит TestRenderToSoulExecute_* — общий путь рендера .tmpl.
const nodeExporterTemplatesDir = "../../../examples/destiny/node-exporter/templates"

// TestNodeExporterTemplates_ParseAndRender — guard на инвариант «каждый .tmpl
// destiny node-exporter реально ПАРСИТСЯ и РЕНДЕРИТСЯ под text/template strict».
//
// Зачем: L0-trial этой destiny ассертит ПЛАН задач и гоняет только CEL-фазу, а
// text/template-фазу для шаблонов не исполняет. Из-за этого литеральный `{{ }}`
// в комментарии шаблона (Go text/template strict парсит его как пустое действие
// → «missing value for command» на Parse) проскользнул мимо тестов и хард-фейлил
// core.file.rendered на хосте. Тест прогоняет КАЖДЫЙ .tmpl через тот же движок,
// что Soul (shared/tmpl.Engine, strict missingkey=error), с корнем контекста
// §3.2 {vars,self,role,essence}. Падение на Parse/Execute ловит регресс «шаблон
// не парсится/не рендерится» на уровне unit-теста, до E2E.
func TestNodeExporterTemplates_ParseAndRender(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(nodeExporterTemplatesDir, "*.tmpl"))
	if err != nil {
		t.Fatalf("glob .tmpl: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("в %q не найдено ни одного .tmpl — путь к destiny сломан", nodeExporterTemplatesDir)
	}
	sort.Strings(files)

	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}

	// Корень text/template-контекста core.file.rendered (templating.md §3.2,
	// Вариант B): {vars, input, self, role, essence}. vars — destiny-локалы vars.yml
	// (bin_dir/bin_path), доступны шаблону `.vars.<file_var>` НАПРЯМУЮ (без
	// passthrough params.vars); input — operator-input прохода (user/group/listen/
	// textfile_dir + прод-параметры демона), читаются шаблоном `.input.<name>`
	// напрямую (Вариант B, ADR-010 §3.2 amendment). Шаблоны без input/vars (скрипты,
	// таймеры) лишних ключей просто не читают.
	root := nodeExporterRenderVars

	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			out, err := engine.Render(string(body), root)
			if err != nil {
				// Именно сюда падал блокер с литеральным `{{ }}` в комментарии
				// (ErrParse: «missing value for command»).
				t.Fatalf("text/template render %s упал: %v", name, err)
			}
			// Рендер не должен оставлять незакрытых маркеров действия — признак,
			// что что-то не подставилось/проглочено.
			if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
				t.Errorf("в рендере %s остались `{{`/`}}` — действие не выполнено:\n%s", name, out)
			}
		})
	}
}

// nodeExporterRenderVars — корень рендера для content-ассертов (Вариант B,
// templating.md §3.2): {vars, input, self, role, essence}. vars — destiny-локалы
// vars.yml (bin_dir/bin_path), доступные шаблону `.vars.<file_var>` НАПРЯМУЮ;
// input — operator-input прохода (user/group/listen/textfile_dir + прод-параметры
// демона), читаемый шаблоном `.input.<name>`. Один на оба теста (ParseAndRender
// использует тот же набор).
var nodeExporterRenderVars = map[string]any{
	// vars — суперсет file-vars и values, поднятых в params.vars ШАГАМИ destiny:
	// основной unit (service.yml, Вариант B) НЕ поднимает params.vars вовсе —
	// читает file-var `.vars.bin_path` напрямую; collector-шаги (collectors.yml —
	// ВНЕ скоупа Варианта B) всё ещё пробрасывают user/textfile_dir/bin_dir через
	// свои params.vars, поэтому collector-.tmpl читают `.vars.user`/`.vars.
	// textfile_dir`. Синтетический корень даёт оба слоя.
	"vars": map[string]any{
		"bin_dir":      "/usr/local/bin",
		"bin_path":     "/usr/local/bin/node_exporter",
		"user":         "node_exporter",
		"textfile_dir": "/var/lib/node_exporter",
	},
	"input": map[string]any{
		"user":         "node_exporter",
		"group":        "node_exporter",
		"listen":       "127.0.0.1:9100",
		"textfile_dir": "/var/lib/node_exporter",
		// Прод-параметры демона (node_exporter.service.tmpl §коммент): дефолты,
		// при которых опц. флаги опускаются, а log.*/web.telemetry-path
		// подставляются всегда.
		"gomaxprocs":              int64(0),
		"disabled_collectors":     []any{},
		"enabled_collectors":      []any{},
		"collector_options":       map[string]any{},
		"log_level":               "info",
		"log_format":              "logfmt",
		"web_telemetry_path":      "/metrics",
		"fs_mount_points_exclude": "",
		"netdev_device_exclude":   "",
	},
	"self": map[string]any{
		"os":      map[string]any{"family": "debian"},
		"network": map[string]any{"primary_ip": "10.0.0.1"},
	},
	"role":    "",
	"essence": map[string]any{},
}

// renderNodeExporterTmpl рендерит один .tmpl destiny node-exporter через тот же
// shared/tmpl.Engine, что и Soul (strict, missingkey=error), с vars-контекстом
// выше. Падение Parse/Execute = провал теста (шаблон сломан).
func renderNodeExporterTmpl(t *testing.T, name string) string {
	t.Helper()
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(nodeExporterTemplatesDir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	out, err := engine.Render(string(body), nodeExporterRenderVars)
	if err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return out
}

// TestNodeExporterTemplates_HardeningContent — guard на СОДЕРЖИМОЕ отрендеренных
// systemd-юнитов: проверяет, что критичные для функциональности и безопасности
// директивы реально присутствуют в выводе. ParseAndRender выше ловит только
// «шаблон не парсится» — регресс, удаливший hardening-строку (например
// RestrictAddressFamilies, без которой node_exporter не слушает TCP, или User=
// → демон под root), прошёл бы молча. Тест рендерит каждый юнит и ассертит
// набор обязательных подстрок; удаление любой из них валит тест.
//
// Подстроки — не полная сверка файла (хрупка), а точечные инварианты: то, без
// чего юнит функционально/секьюрно неверен. Источник набора — ТЗ §коллекторы и
// прод-конвенция §3 (hardening), задокументированные в комментариях .tmpl.
func TestNodeExporterTemplates_HardeningContent(t *testing.T) {
	// Каждый кейс — один .tmpl и список подстрок, обязанных быть в рендере.
	cases := []struct {
		file string
		want []string
	}{
		{
			// Основной демон: без RestrictAddressFamilies=AF_INET[6] node_exporter
			// не слушает TCP; User=node_exporter (НЕ root); NoNewPrivileges; флаги
			// listen-address и textfile.directory в ExecStart.
			file: "node_exporter.service.tmpl",
			want: []string{
				"RestrictAddressFamilies=AF_INET AF_INET6",
				"User=node_exporter",
				"NoNewPrivileges=yes",
				"--web.listen-address=",
				"--collector.textfile.directory=",
				// web.telemetry-path рендерится ВСЕГДА (дефолт непустой /metrics).
				"--web.telemetry-path=/metrics",
			},
		},
		{
			// smartmon .service: Condition (на VM не стартует), привилегированный
			// (User=root, PrivateDevices=no, DeviceAllow/CapabilityBoundingSet
			// непустые, PrivateNetwork=yes, ReadWritePaths с textfile-каталогом).
			file: "node-exporter-smartmon.service.tmpl",
			want: []string{
				"ConditionVirtualization=no",
				"User=root",
				"PrivateDevices=no",
				"DeviceAllow=block-* r",
				"CapabilityBoundingSet=CAP_SYS_RAWIO CAP_SYS_ADMIN",
				"PrivateNetwork=yes",
				"ReadWritePaths=/var/lib/node_exporter",
			},
		},
		{
			// smartmon .timer несёт ту же Condition, что и .service.
			file: "node-exporter-smartmon.timer.tmpl",
			want: []string{"ConditionVirtualization=no"},
		},
		{
			// nvme .service: Condition по /dev/nvme*, привилегированный sandbox.
			file: "node-exporter-nvme.service.tmpl",
			want: []string{
				"ConditionPathExistsGlob=/dev/nvme[0-9]*",
				"User=root",
				"PrivateDevices=no",
				"DeviceAllow=char-nvme rw",
				"CapabilityBoundingSet=CAP_SYS_ADMIN CAP_SYS_RAWIO",
				"PrivateNetwork=yes",
				"ReadWritePaths=/var/lib/node_exporter",
			},
		},
		{
			file: "node-exporter-nvme.timer.tmpl",
			want: []string{"ConditionPathExistsGlob=/dev/nvme[0-9]*"},
		},
		{
			// ipmi .service: Condition по /dev/ipmi0, привилегированный sandbox.
			// ProtectKernelModules=no — драйвер ipmi подгружается лениво (ТЗ).
			file: "node-exporter-ipmitool.service.tmpl",
			want: []string{
				"ConditionPathExists=/dev/ipmi0",
				"User=root",
				"PrivateDevices=no",
				"DeviceAllow=char-ipmidev rw",
				"CapabilityBoundingSet=CAP_SYS_RAWIO CAP_DAC_OVERRIDE",
				"PrivateNetwork=yes",
				"ReadWritePaths=/var/lib/node_exporter",
				"ProtectKernelModules=no",
			},
		},
		{
			file: "node-exporter-ipmitool.timer.tmpl",
			want: []string{"ConditionPathExists=/dev/ipmi0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			out := renderNodeExporterTmpl(t, tc.file)
			for _, sub := range tc.want {
				if !strings.Contains(out, sub) {
					t.Errorf("в рендере %s отсутствует обязательная директива %q\n--- рендер ---\n%s", tc.file, sub, out)
				}
			}
		})
	}
}

// renderNodeExporterServiceWithVars рендерит основной node_exporter.service.tmpl
// с подменой части .input (поверх дефолтного набора): нужен content-тестам,
// варьирующим collector_options/web_telemetry_path. Вариант B: эти поля —
// operator-input, шаблон читает их `.input.<name>`, поэтому overrides мёрджатся в
// input-подмапу. Базовый набор — nodeExporterRenderVars.
func renderNodeExporterServiceWithVars(t *testing.T, overrides map[string]any) string {
	t.Helper()
	engine, err := tmpl.New()
	if err != nil {
		t.Fatalf("tmpl.New: %v", err)
	}
	base := nodeExporterRenderVars["input"].(map[string]any)
	input := make(map[string]any, len(base)+len(overrides))
	for k, v := range base {
		input[k] = v
	}
	for k, v := range overrides {
		input[k] = v
	}
	root := map[string]any{
		"vars":    nodeExporterRenderVars["vars"],
		"input":   input,
		"self":    nodeExporterRenderVars["self"],
		"role":    nodeExporterRenderVars["role"],
		"essence": nodeExporterRenderVars["essence"],
	}
	body, err := os.ReadFile(filepath.Join(nodeExporterTemplatesDir, "node_exporter.service.tmpl"))
	if err != nil {
		t.Fatalf("read node_exporter.service.tmpl: %v", err)
	}
	out, err := engine.Render(string(body), root)
	if err != nil {
		t.Fatalf("render node_exporter.service.tmpl: %v", err)
	}
	return out
}

// TestNodeExporterService_CollectorOptionsDeterministic — ★двойная проверка ТЗ:
// двойной range по map<string,map<string,string>> collector_options в ExecStart
// ДЕТЕРМИНИРОВАН (Go text/template range по map обходит ключи в ОТСОРТИРОВАННОМ
// порядке — инвариант, на который опираются и users.acl.tmpl/redis). Тест задаёт
// заведомо не-в-лексикографическом insertion-порядке набор коллекторов и опций и
// требует, чтобы рендер выдавал флаги строго по возрастанию имён коллекторов, а
// внутри коллектора — по возрастанию ключей опций. Прогон повторяется, чтобы
// исключить случайное совпадение с порядком итерации одной хеш-таблицы.
func TestNodeExporterService_CollectorOptionsDeterministic(t *testing.T) {
	// Insertion-порядок намеренно перемешан (systemd до cpu; внутри systemd
	// unit-allowlist до enable-restart). Ожидаемый рендер — отсортированный.
	collectorOptions := map[string]any{
		"systemd": map[string]any{
			"unit-include":            ".+\\.service",
			"enable-restarts-metrics": "true",
		},
		"cpu": map[string]any{
			"info": "true",
		},
	}

	// Ожидаемый порядок флагов: коллекторы cpu<systemd, внутри systemd ключи
	// enable-restarts-metrics<unit-include (лексикографически).
	wantOrder := []string{
		"--collector.cpu.info=true",
		"--collector.systemd.enable-restarts-metrics=true",
		"--collector.systemd.unit-include=.+\\.service",
	}

	// Несколько прогонов: range по разным экземплярам map не должен менять
	// порядок (если бы он зависел от итерации хеш-таблицы — расходился бы).
	for run := 0; run < 8; run++ {
		out := renderNodeExporterServiceWithVars(t, map[string]any{"collector_options": collectorOptions})

		prev := -1
		for _, flag := range wantOrder {
			idx := strings.Index(out, flag)
			if idx < 0 {
				t.Fatalf("прогон %d: флаг %q отсутствует в ExecStart\n--- рендер ---\n%s", run, flag, out)
			}
			if idx <= prev {
				t.Fatalf("прогон %d: флаг %q идёт не по возрастанию (idx=%d, prev=%d) — порядок range НЕ детерминирован\n--- рендер ---\n%s", run, flag, idx, prev, out)
			}
			prev = idx
		}

		// collector_options-флаги стоят ПОСЛЕ enabled/disabled-коллекторов и
		// ПЕРЕД gomaxprocs (позиция блока в ExecStart, ТЗ §1c). gomaxprocs=0 →
		// флага нет, поэтому проверяем относительно --log.format (предыдущий
		// безусловный флаг) и отсутствия gomaxprocs.
		logIdx := strings.Index(out, "--log.format=")
		firstOptIdx := strings.Index(out, "--collector.cpu.info=")
		if firstOptIdx < logIdx {
			t.Fatalf("прогон %d: collector_options отрендерены до --log.format — нарушена позиция блока\n--- рендер ---\n%s", run, out)
		}
	}
}

// TestNodeExporterService_ExecStartBinPath — ★регресс на «убран последний
// vars-passthrough»: ExecStart берёт путь бинаря из file-var `.vars.bin_path`
// (vars.yml), а не из снятого passthrough `.vars.exporter_bin`. Путь обязан
// остаться тем же (/usr/local/bin/node_exporter) — поведение ExecStart не
// изменилось, изменился только канал доставки (file-var напрямую).
func TestNodeExporterService_ExecStartBinPath(t *testing.T) {
	out := renderNodeExporterTmpl(t, "node_exporter.service.tmpl")
	if !strings.Contains(out, "ExecStart=/usr/local/bin/node_exporter ") {
		t.Errorf("ExecStart не берёт путь бинаря из .vars.bin_path:\n%s", out)
	}
}

// TestNodeExporterService_TelemetryPathOverride — web_telemetry_path != /metrics
// рендерится в ExecStart как --web.telemetry-path=<override> (флаг безусловный).
func TestNodeExporterService_TelemetryPathOverride(t *testing.T) {
	out := renderNodeExporterServiceWithVars(t, map[string]any{"web_telemetry_path": "/node/metrics"})
	if !strings.Contains(out, "--web.telemetry-path=/node/metrics") {
		t.Errorf("web_telemetry_path override не отрендерен\n--- рендер ---\n%s", out)
	}
	if strings.Contains(out, "--web.telemetry-path=/metrics") {
		t.Errorf("остался дефолтный --web.telemetry-path=/metrics при override\n--- рендер ---\n%s", out)
	}
}
