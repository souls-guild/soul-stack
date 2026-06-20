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
const l2PilotCase = "../../../examples/destiny/node-exporter/_trial/scenario/verify-l2/tests/run-and-probe"

// l2ReloadCases — daemon-reload L2-кейсы (systemd-PID1 стенд). auto-reload —
// основной guard (NeedDaemonReload-flag-flip надёжен на debian-12); always-reload —
// детерминированный дубль (reload→новое-определение независимо от флага).
var l2ReloadCases = []string{
	"../../../examples/destiny/service-reload/_trial/scenario/verify-l2/tests/auto-reload",
	"../../../examples/destiny/service-reload/_trial/scenario/verify-l2/tests/always-reload",
}

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

// TestL2_ServiceDaemonReload прогоняет daemon-reload L2-кейсы на systemd-PID1
// стенде (init: systemd). Доказывает фикс util.EnsureDaemonReloaded: после
// перезаписи unit-файла core.service.restarted сам делает daemon-reload и
// применяет НОВОЕ определение (ExecStart=…2000) без ручного reload.
//
// Skip при отсутствии docker ИЛИ при отказе privileged/cgroup (rootless/sandbox):
// isDockerSetupErr распознаёт privileged/cgroup/permission/timeout как
// setup-ошибку → t.Skip, не t.Fatal (ложного красного на окружении без privileged
// быть не должно).
func TestL2_ServiceDaemonReload(t *testing.T) {
	if !dockerAvailable() && !requireDocker() {
		t.Skip("L2: docker недоступен, SOUL_STACK_INTEGRATION_REQUIRE_DOCKER не задан — skip")
	}

	for _, casePath := range l2ReloadCases {
		casePath := casePath
		t.Run(filepath.Base(casePath), func(t *testing.T) {
			caseAbs, err := filepath.Abs(casePath)
			if err != nil {
				t.Fatalf("filepath.Abs: %v", err)
			}
			c, file, err := LoadL2Case(caseAbs)
			if err != nil {
				t.Fatalf("LoadL2Case: %v", err)
			}

			// systemd-build (~60s cold) + boot + apply×N + verify — щедрый таймаут.
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()

			res, err := RunL2Case(ctx, c, file)
			if err != nil {
				if isDockerSetupErr(err) && !requireDocker() {
					t.Skipf("L2: systemd-стенд не поднялся (docker/privileged?): %v", err)
				}
				t.Fatalf("RunL2Case: %v", err)
			}
			if !res.Pass {
				t.Fatalf("daemon-reload L2-кейс %q не прошёл:\n  - %s", c.Name, joinFailures(res.Failures))
			}
		})
	}
}

func isDockerSetupErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"поднять стенд", "docker", "cannot connect", "dial unix", "rootless",
		// systemd-стенд требует --privileged + CgroupnsMode=host: на rootless/
		// sandbox-окружении docker отказывает на этих опциях. Считаем это
		// setup-ошибкой (skip, не fatal): кейс не должен давать ложный красный там,
		// где privileged недоступен. WaitingFor по systemctl на не-systemd-host
		// упрётся в startup-timeout → DeadlineExceeded (ниже).
		"privileged", "cgroup", "permission denied", "operation not permitted",
		"is-system-running", "starting container", "/sbin/init",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func joinFailures(fs []string) string {
	return strings.Join(fs, "\n  - ")
}
