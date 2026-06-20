// Package validate реализует подкоманду `soul-lint validate-config`.
//
// Auto-detect kind (keeper vs soul) по top-level ключу `kid:` / `sid:`,
// делегирование в shared/config, форматирование вывода (human / JSON).
package validate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/souls-guild/soul-stack/shared/config"
	"github.com/souls-guild/soul-stack/shared/diag"
	sharedplugin "github.com/souls-guild/soul-stack/shared/plugin"
)

// Exit codes — стабильный контракт CLI (см. delegation.md).
const (
	ExitOK        = 0
	ExitHasErrors = 1
	ExitIOFatal   = 2
)

// Kind — какой документ валидируем. Прокидывается из CLI (своя подкоманда
// на каждый kind); внутри Run выбирается соответствующий Load*-вызов.
type Kind int

const (
	// KindConfig — keeper.yml / soul.yml (auto-detect между ними по
	// top-level ключу `kid:` / `sid:`).
	KindConfig Kind = iota
	// KindDestiny — destiny.yml (корневой манифест destiny).
	KindDestiny
	// KindService — service.yml (корневой манифест сервиса).
	KindService
	// KindScenario — scenario/<name>/main.yml.
	KindScenario
	// KindManifest — manifest.yaml плагина (kind: soul_module / cloud_driver /
	// ssh_provider). Парсер и валидатор — `shared/plugin`.
	KindManifest
)

// Options — параметры одного запуска validate-* подкоманды.
type Options struct {
	Path string
	JSON bool
	Kind Kind
}

// Run выполняет одну валидацию. Печатает диагностики в `out`, возвращает
// exit-code согласно контракту (0/1/2).
func Run(opts Options, out io.Writer, errOut io.Writer) int {
	src, ioErr := os.ReadFile(opts.Path)
	if ioErr != nil {
		fmt.Fprintf(errOut, "soul-lint: %s: %v\n", opts.Path, ioErr)
		return ExitIOFatal
	}

	var diags []diag.Diagnostic
	switch opts.Kind {
	case KindConfig:
		kind := detectKind(stripBOM(src))
		switch kind {
		case kindKeeper:
			_, _, diags, _ = config.LoadKeeperFromBytes(opts.Path, src, config.ValidateOptions{})
		case kindSoul:
			_, _, diags, _ = config.LoadSoulFromBytes(opts.Path, src, config.ValidateOptions{})
		case kindIndeterminate:
			diags = []diag.Diagnostic{{
				Level:   diag.LevelError,
				Phase:   diag.PhaseParse,
				File:    opts.Path,
				Code:    "config_kind_indeterminate",
				Message: "cannot auto-detect config kind: expected top-level `kid:` (keeper) or `sid:` (soul); neither or both found",
				Hint:    "ensure the file is either keeper.yml (kid:) or soul.yml (sid:)",
			}}
		}
	case KindDestiny:
		_, _, diags, _ = config.LoadDestinyManifestFromBytes(opts.Path, src, config.ValidateOptions{})
		// Кросс-файловая проверка коллизии destiny-локалов: соседний `vars.yml`
		// (file-level vars) против task-level `vars:` из `tasks/main.yml`. Вариант A
		// (vars.md) детерминирован, но коллизия имён — частый источник недоразумений
		// → warn. Отсутствие любого из соседей → проверка пропускается (vars.yml
		// опционален, tasks/main.yml может лежать иначе — линтер манифеста на это не
		// падает).
		diags = append(diags, destinyVarsCollisionDiags(opts.Path)...)
	case KindService:
		_, _, diags, _ = config.LoadServiceManifestFromBytes(opts.Path, src, config.ValidateOptions{})
	case KindScenario:
		var scn *config.ScenarioManifest
		scn, _, diags, _ = config.LoadScenarioManifestFromBytes(opts.Path, src, config.ValidateOptions{})
		// Stage-валидация (ADR-056 §S5): офлайн Passage-стратификация той же
		// функцией config.Stratify, что рантайм делает перед dispatch. Ловит
		// register-цикл и serial+staged ДО apply (config-валидатор уже поднял
		// unknown_register на парсе). Прогоняется даже при наличии диагностик парса
		// (stageDiagnostics сам решает по nil scn, надёжен ли граф).
		diags = append(diags, stageDiagnostics(opts.Path, scn)...)
	case KindManifest:
		_, diags = sharedplugin.LoadFromBytes(opts.Path, src)
	default:
		fmt.Fprintf(errOut, "soul-lint: unknown kind %d\n", opts.Kind)
		return ExitIOFatal
	}

	printDiagnostics(opts, diags, out)
	if diag.HasErrors(diags) {
		return ExitHasErrors
	}
	return ExitOK
}

// destinyVarsCollisionDiags поднимает warn на каждое имя, объявленное И в
// соседнем `vars.yml` (file-level destiny-локалы), И в task-level `vars:` хотя бы
// одной задачи `tasks/main.yml`. Вариант A детерминирован (task переопределяет
// file, vars.md), но коллизия — частый источник недоразумений.
//
// manifestPath — путь к destiny.yml; соседи берутся из его каталога. Любая I/O-
// или parse-ошибка соседа → пропуск (vars.yml опционален; ошибки самих задач
// ловит validate-scenario/рантайм — здесь только коллизия). Не падает, если
// соседей нет.
func destinyVarsCollisionDiags(manifestPath string) []diag.Diagnostic {
	dir := filepath.Dir(manifestPath)

	fileVars, err := config.LoadDestinyVars(filepath.Join(dir, "vars.yml"))
	if err != nil || len(fileVars) == 0 {
		return nil
	}

	tasksPath := filepath.Join(dir, "tasks", "main.yml")
	tasksData, rerr := os.ReadFile(tasksPath)
	if rerr != nil {
		return nil
	}
	tasks, _, terr := config.LoadDestinyTasksFromBytes(tasksPath, tasksData, config.ValidateOptions{})
	if terr != nil {
		return nil
	}

	var out []diag.Diagnostic
	for _, name := range config.DestinyVarsCollisions(fileVars, tasks) {
		out = append(out, diag.Diagnostic{
			Level:    diag.LevelWarning,
			Phase:    diag.PhaseSemanticValidate,
			File:     filepath.Join(dir, "vars.yml"),
			Code:     "vars_collision",
			Message:  fmt.Sprintf("vars.%s declared in both vars.yml and a task-level vars:; task-level wins (Variant A)", name),
			Hint:     "rename one, or rely on task-level override intentionally (docs/destiny/vars.md)",
			YAMLPath: "$." + name,
		})
	}
	return out
}

// printDiagnostics форматирует и пишет диагностики в `w` в выбранном режиме.
// JSON-mode: одна строка JSON на диагностику (JSON-Lines); 0 диагностик —
// пустой stdout. Human-mode: gcc-style + при 0 ошибках одна строка `OK: <path>`.
func printDiagnostics(opts Options, diags []diag.Diagnostic, w io.Writer) {
	if opts.JSON {
		bw := bufio.NewWriter(w)
		defer bw.Flush()
		enc := json.NewEncoder(bw)
		for _, d := range diags {
			_ = enc.Encode(d)
		}
		return
	}
	for _, d := range diags {
		writeHumanDiag(w, d)
	}
	if !diag.HasErrors(diags) {
		fmt.Fprintf(w, "OK: %s\n", opts.Path)
	}
}

func writeHumanDiag(w io.Writer, d diag.Diagnostic) {
	file := d.File
	if file == "" {
		file = "<input>"
	}
	// gcc-style `file:line:col: level: [code] message`. Когда line/col
	// неизвестны (cross-field invariant) — секции просто опускаются,
	// без пустого ":" → исчезает двойной пробел в `path: error:`.
	prefix := file + ":"
	if d.Line > 0 {
		if d.Column > 0 {
			prefix += fmt.Sprintf("%d:%d:", d.Line, d.Column)
		} else {
			prefix += fmt.Sprintf("%d:", d.Line)
		}
	}
	fmt.Fprintf(w, "%s %s: [%s] %s\n", prefix, d.Level, d.Code, d.Message)
	if d.YAMLPath != "" {
		fmt.Fprintf(w, "  yaml_path: %s\n", d.YAMLPath)
	}
	if d.Hint != "" {
		fmt.Fprintf(w, "  hint: %s\n", d.Hint)
	}
}

// detectKind — auto-detect по составу top-level ключей.
//
// Приоритет 1: `kid:` → keeper, `sid:` → soul (одно из двух явно есть).
// Приоритет 2: если ни `kid:`, ни `sid:` — голосование по уникальным
// top-level ключам (`postgres`/`vault`/`plugins`/… → keeper; `keeper`/`paths`/
// `soulprint`/`cleanup`/`metrics` → soul). Это нужно, потому что `sid:` в
// soul.yml опционален (вычисляется из FQDN хоста по умолчанию).
//
// Возвращает `kindIndeterminate`, если оба явных ключа присутствуют либо ни
// один признак не найден / голосование неоднозначно.
func detectKind(src []byte) configKind {
	keys := readTopLevelKeys(src)
	hasKID, hasSID := keys["kid"], keys["sid"]
	switch {
	case hasKID && hasSID:
		return kindIndeterminate
	case hasKID:
		return kindKeeper
	case hasSID:
		return kindSoul
	}
	keeperVotes := 0
	soulVotes := 0
	for k := range keys {
		switch k {
		// `services`/`default_destiny_source`/`default_module_source` исключены из
		// голосования: реестр Service-ов и скаляры перенесены в Postgres (ADR-029
		// hard-cut), в keeper.yml их больше нет — детектировать keeper по ним
		// нельзя. `rbac` исключён ранее (ADR-028). Сигнатура keeper остаётся
		// сильной (postgres/vault/auth/plugins/reaper).
		case "postgres", "vault", "auth", "plugins", "reaper":
			keeperVotes++
		case "keeper", "paths", "soulprint", "cleanup", "metrics":
			soulVotes++
		}
	}
	switch {
	case keeperVotes > 0 && soulVotes == 0:
		return kindKeeper
	case soulVotes > 0 && keeperVotes == 0:
		return kindSoul
	default:
		return kindIndeterminate
	}
}

func readTopLevelKeys(src []byte) map[string]bool {
	out := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(src))
	for sc.Scan() {
		line := sc.Text()
		if len(line) == 0 {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.IndexByte(trimmed, ':')
		if idx <= 0 {
			continue
		}
		key := trimmed[:idx]
		out[key] = true
	}
	return out
}

type configKind int

const (
	kindIndeterminate configKind = iota
	kindKeeper
	kindSoul
)

// stripBOM — локальный helper для синхронизации с shared/config (тот же strip
// делает goccy-парсер через LoadKeeperFromBytes/LoadSoulFromBytes). Без него
// detectKind не нашёл бы `kid:`/`sid:` под ведущим BOM.
func stripBOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}
