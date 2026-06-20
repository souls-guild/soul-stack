//go:build e2e_live

package harness

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Destiny-fixture L3b: материализация standalone-destiny из examples/destiny/<name>
// в per-test file://-git-репо под git-тегом + установка
// keeper_settings[default_destiny_source].
//
// Зачем: service-композиция через apply:destiny (ADR-009) — например
// examples/service/redis — ссылается на standalone-destiny по имени; keeper-side
// scenario-резолвер тянет git-URL каждой из keeper_settings[default_destiny_source]
// с подстановкой {name} (ADR-029). Service-fixture-репо (git.go) этих destiny НЕ
// содержит — они живут отдельными каталогами examples/destiny/<name>. Без
// materialize+seed apply:destiny падает «default_destiny_source не задан» либо
// «ref не резолвится».
//
// keeper_settings — direct SQL upsert (harness уже работает через pgxpool, как
// AddSoulToCoven): OpenAPI-эндпоинта для этого скаляра нет (ADR-029). Вызывать ДО
// registerExampleService — invalidate от POST /v1/services тем же снимком
// подтянет уже-записанную настройку без ожидания 10s TTL-poll-а Holder-а.

const settingDefaultDestinySource = "default_destiny_source"

const destinyURLPlaceholder = "{name}"

// MaterializeDestinies материализует каждую destiny из examples/destiny/<name> в
// отдельный file://-git-репо ($TMP/destiny-repos/<name>) под git-тегом ref и
// ставит keeper_settings[default_destiny_source] = file://$TMP/destiny-repos/{name}.
//
// ref — git-тег из service.yml::destiny[] (ADR-007: версия зависимости = git-ref);
// резолвер артефактов чекаутит destiny именно по ref (НЕ main). Для
// examples/service/redis все три destiny объявлены ref:v1.0.0.
//
// Вызывать ДО registerExampleService (NewStack делает регистрацию сам — на L3b
// MaterializeDestinies вызывается тестом ПОСЛЕ NewStack, поэтому полагается на
// TTL-poll/последующий invalidate; первый CreateIncarnation ретраит транзиентный
// «not registered», а резолв default_destiny_source происходит на render-фазе
// create-прогона — к тому моменту Holder снимок уже свежий).
func (s *Stack) MaterializeDestinies(t *testing.T, ref string, names ...string) {
	t.Helper()
	if len(names) == 0 {
		t.Fatal("MaterializeDestinies: пустой список имён destiny")
	}

	destinyRoot := filepath.Join(s.tmpDir, "destiny-repos")
	for _, name := range names {
		relPath := filepath.Join("examples", "destiny", name)
		s.materializeRepoAt(t, filepath.Join(destinyRoot, name), relPath, ref)
	}

	tmpl := "file://" + filepath.Join(destinyRoot, destinyURLPlaceholder)
	s.SeedDefaultDestinySource(t, tmpl)
}

// holderRefreshGrace — окно ожидания TTL-перечита снимка serviceregistry.Holder
// после SQL-записи keeper_settings. = serviceregistry.DefaultRefreshInterval(10s)
// + буфер. На L3b registerExampleService уже отработал в NewStack (его invalidate
// прошёл ДО seed-а), а из harness нет доступа к Redis-pub/sub-каналу
// `service:invalidate` (harness ходит только в PG/Vault). Поэтому полагаемся на
// TTL-poll: ждём, пока Holder гарантированно перечитал снимок с новым
// default_destiny_source. Для L3b (медленный тир, тест и так минуты) приемлемо.
const holderRefreshGrace = 12 * time.Second

// SeedDefaultDestinySource записывает keeper_settings[default_destiny_source]
// прямым SQL-upsert-ом и блокируется на holderRefreshGrace, чтобы Holder
// гарантированно подхватил значение через TTL-poll ДО первого create-render-а
// (apply:destiny резолвит шаблон на render-фазе). updated_by_aid = NULL
// (системная установка fixture-а).
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
	time.Sleep(holderRefreshGrace)
}

// materializeRepoAt — как materializeServiceRepo (git.go), но в произвольный
// repoDir и с git-тегом ref: destiny-репо живут под destiny-repos/<name>, чтобы
// {name}-подстановка в default_destiny_source указывала на них, и резолвятся по
// ref из service.yml::destiny[] (а не main).
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
		"commit", "-q", "-m", "e2e-live destiny snapshot from "+relativePath)
	runGit(t, repoDir, "-c", "tag.gpgsign=false",
		"tag", "-a", ref, "-m", "e2e-live destiny ref "+ref)
}
