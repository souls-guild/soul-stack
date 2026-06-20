//go:build e2e

package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Destiny-fixture: материализация standalone-destiny из examples/destiny/<name> в
// per-test file://-git-репо + установка keeper_settings[default_destiny_source].
//
// Зачем отдельно от RegisterService (git.go): service-композиция через
// apply:destiny (ADR-009) — например examples/service/redis — ссылается на
// standalone-destiny по имени (`apply: { destiny: redis-single }`). Keeper-side
// scenario-резолвер тянет git-URL такой destiny из keeper_settings
// [default_destiny_source] с подстановкой {name} (ADR-029,
// keeper/internal/scenario/destiny.go::resolveGitURL). Service-fixture-репо этих
// destiny НЕ содержит — они живут отдельными каталогами в examples/destiny/.
// Без materialize+seed резолв падает «default_destiny_source не задан».
//
// keeper_settings — direct SQL upsert (harness уже работает через pgxpool, как
// AddSoulToCoven): OpenAPI-эндпоинта для этого скаляра нет (управляется только
// через keeper_settings, ADR-029). Holder перечитывает снимок по TTL-poll (10s)
// ИЛИ по pub/sub-инвалидации; чтобы не ждать poll, harness вызывает
// SeedDefaultDestinySource ДО RegisterService — invalidate от POST /v1/services
// тем же снимком подтянет уже-записанную настройку (см. holder.go).

// settingDefaultDestinySource — ключ скаляра в keeper_settings. Дублирует
// serviceregistry.SettingDefaultDestinySource литералом: tests/e2e — отдельный
// go-модуль без зависимости на keeper/internal/* (Go internal-rules, ADR-039).
const settingDefaultDestinySource = "default_destiny_source"

// destinyURLPlaceholder — маркер {name} в шаблоне default_destiny_source,
// заменяемый именем destiny при keeper-side-резолве.
const destinyURLPlaceholder = "{name}"

// MaterializeDestinies материализует каждую destiny из examples/destiny/<name> в
// отдельный file://-git-репо ($TMP/destiny-repos/<name>) под git-тегом ref и
// устанавливает keeper_settings[default_destiny_source] =
// file://$TMP/destiny-repos/{name}.
//
// ref — git-тег, под которым каждая destiny объявлена в service.yml::destiny[]
// (ADR-007: версия зависимости = git-ref). Для examples/service/redis все три
// destiny объявлены ref:v1.0.0 — резолвер артефактов берёт ref из записи
// service.yml и чекаутит именно его (НЕ main). Без тега резолв падает «ref
// <ref> не резолвится: reference not found».
//
// Вызывать ДО RegisterService: invalidate от регистрации сервиса подтянет
// keeper_settings[default_destiny_source] в Holder без ожидания 10s TTL-poll-а
// (см. doc выше). Имена destiny — ровно те, что объявлены в service.yml::destiny[]
// и используются в apply:destiny.
func (s *Stack) MaterializeDestinies(t *testing.T, ref string, names ...string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("MaterializeDestinies: пустой список имён destiny")
	}

	destinyRoot := filepath.Join(s.tmpDir, "destiny-repos")
	for _, name := range names {
		relPath := filepath.Join("examples", "destiny", name)
		// materializeRepoAt: init working-tree-репо + детерминированный commit +
		// тег ref. Каталог репо — destiny-repos/<name>, чтобы {name}-подстановка
		// в шаблоне резолвилась в этот путь.
		s.materializeRepoAt(t, filepath.Join(destinyRoot, name), relPath, ref)
	}

	tmpl := "file://" + filepath.Join(destinyRoot, destinyURLPlaceholder)
	s.SeedDefaultDestinySource(t, tmpl)
}

// SeedDefaultDestinySource записывает keeper_settings[default_destiny_source]
// прямым SQL-upsert-ом. updated_by_aid = NULL (системная установка fixture-а; FK
// допускает NULL, как у первого Архонта). Holder подхватит при следующем
// перечитe снимка (TTL-poll либо invalidate).
func (s *Stack) SeedDefaultDestinySource(t *testing.T, template string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx, `
		INSERT INTO keeper_settings (key, value, updated_by_aid, updated_at)
		VALUES ($1, $2, NULL, NOW())
		ON CONFLICT (key) DO UPDATE
		SET value = EXCLUDED.value, updated_at = NOW()
	`, settingDefaultDestinySource, template)
	if err != nil {
		t.Fatalf("SeedDefaultDestinySource(%q): %v", template, err)
	}
}

// materializeRepoAt — как materializeServiceRepo (git.go), но в произвольный
// repoDir (а не $TMP/repos/<name>) и с git-тегом ref: destiny-репо живут под
// destiny-repos/<name>, чтобы {name}-подстановка в default_destiny_source
// указывала на них, и резолвятся по ref из service.yml::destiny[] (а не main).
func (s *Stack) materializeRepoAt(t *testing.T, repoDir, relativePath, ref string) {
	t.Helper()
	srcDir := filepath.Join(repoRoot(t), relativePath)
	if _, err := os.Stat(srcDir); err != nil {
		t.Fatalf("materializeRepoAt: source %s: %v", srcDir, err)
	}
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("materializeRepoAt: mkdir %s: %v", repoDir, err)
	}
	runGit(t, "", "init", "-q", "-b", "main", repoDir)
	copyTree(t, srcDir, repoDir)
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "-c", "commit.gpgsign=false",
		"commit", "-q", "-m", "e2e destiny snapshot from "+relativePath)
	// Аннотированный тег ref (-m обязателен): резолвер артефактов чекаутит destiny
	// по этому ref. gpgsign=false — герметичность fixture-а (см. commit выше).
	runGit(t, repoDir, "-c", "tag.gpgsign=false",
		"tag", "-a", ref, "-m", "e2e destiny ref "+ref)
}
