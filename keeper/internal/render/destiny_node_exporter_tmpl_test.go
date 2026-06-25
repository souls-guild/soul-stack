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

	// Корень text/template-контекста core.file.rendered (templating.md §3.2):
	// {vars, self, role, essence}. vars — суперсет всех ключей, которые поднимают
	// в params.vars шаги этой destiny (tasks/main.yml): шаблоны без vars (скрипты,
	// таймеры) лишних ключей просто не читают, шаблоны с vars читают свои.
	root := map[string]any{
		"vars": map[string]any{
			"bin_dir":      "/usr/local/bin",
			"user":         "node_exporter",
			"group":        "node_exporter",
			"listen":       "127.0.0.1:9100",
			"textfile_dir": "/var/lib/node_exporter",
		},
		"self": map[string]any{
			"os":      map[string]any{"family": "debian"},
			"network": map[string]any{"primary_ip": "10.0.0.1"},
		},
		"role":    "",
		"essence": map[string]any{},
	}

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

// nodeExporterRenderVars — vars-контекст рендера для content-ассертов: суперсет
// всех ключей, которые поднимают в params.vars шаги destiny (tasks/main.yml).
// Один на оба теста (ParseAndRender выше использует тот же набор).
var nodeExporterRenderVars = map[string]any{
	"vars": map[string]any{
		"bin_dir":      "/usr/local/bin",
		"user":         "node_exporter",
		"group":        "node_exporter",
		"listen":       "127.0.0.1:9100",
		"textfile_dir": "/var/lib/node_exporter",
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
