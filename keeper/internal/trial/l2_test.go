//go:build integration

// L2 integration-тест Trial (ADR-023, дизайн «Вариант A»). Прогоняет пилот-кейс
// node-exporter end-to-end на docker-стенде: render in-process → ApplyRequest →
// soul apply в контейнере → verify → expect_idempotent. Не входит в дефолтный
// `make test` (build-tag integration). Запуск:
//
//	cd keeper && go test -tags integration -run TestL2 ./internal/trial/...
//
// Требует docker. Без docker и без SOUL_STACK_INTEGRATION_REQUIRE_DOCKER — skip;
// с флагом — fatal (паттерн прочих integration-тестов keeper).
package trial

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// l2PilotCase — путь к пилот-кейсу относительно этого пакета (keeper/internal/trial).
// Раскладка ADR-023: <destiny>/_trial/scenario/<name>/tests/<case>/case.yml.
const l2PilotCase = "../../../examples/destiny/destiny-node-exporter/_trial/scenario/verify-l2/tests/run-and-probe"

// dockerAvailable — best-effort проба доступности docker через попытку короткого
// прогона: реальная проверка делается StartL2Stand. Здесь только grуб-фильтр по
// наличию docker-сокета, чтобы дать осмысленный skip без полного fail.
func dockerAvailable() bool {
	for _, p := range []string{
		os.Getenv("DOCKER_HOST"),
		"/var/run/docker.sock",
		filepath.Join(os.Getenv("HOME"), ".docker/run/docker.sock"),
		filepath.Join(os.Getenv("HOME"), ".colima/default/docker.sock"),
	} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(strings.TrimPrefix(p, "unix://")); err == nil {
			return true
		}
	}
	return false
}

func TestL2_NodeExporterPilot(t *testing.T) {
	if !dockerAvailable() && !requireDocker() {
		t.Skip("L2: docker недоступен, SOUL_STACK_INTEGRATION_REQUIRE_DOCKER не задан — skip")
	}

	// Абсолютный путь: template-резолвер (securejoin) отвергает root с '..'
	// (serviceRootFor выводит svcRoot из caseFile; относительный путь даёт '..').
	caseAbs, err := filepath.Abs(l2PilotCase)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}
	c, file, err := LoadL2Case(caseAbs)
	if err != nil {
		t.Fatalf("LoadL2Case: %v", err)
	}

	// go build soul (linux) + pull образа + apply×2 + verify×3 — щедрый таймаут.
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	res, err := RunL2Case(ctx, c, file)
	if err != nil {
		// docker недоступен по-настоящему (StartL2Stand упал на поднятии стенда):
		// при отсутствии REQUIRE_DOCKER — skip, иначе fail.
		if isDockerSetupErr(err) && !requireDocker() {
			t.Skipf("L2: стенд не поднялся (docker?): %v", err)
		}
		t.Fatalf("RunL2Case: %v", err)
	}

	if res.Level != LevelL2 {
		t.Errorf("Level = %v, ожидался LevelL2", res.Level)
	}
	if !res.Pass {
		t.Fatalf("пилот-кейс L2 не прошёл:\n  - %s", joinFailures(res.Failures))
	}
}

func isDockerSetupErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"поднять стенд", "docker", "cannot connect", "dial unix", "rootless"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func joinFailures(fs []string) string {
	return strings.Join(fs, "\n  - ")
}
