//go:build integration

// L2-исполнение Trial (ADR-023): дизайн «Вариант A» (без реального Keeper, без
// SSH). soul-trial рендерит план in-process тем же Keeper-side render-пайплайном,
// что и L0 (renderCase), сериализует ApplyRequest в protojson, доставляет soul-
// бинарь + ApplyRequest в эфемерный контейнер (testcontainers-go) и исполняет
// `soul apply` (push-oneshot, ADR-004) с редиректом protojson из файла на stdin.
// Из stdout читается NDJSON-поток TaskEvent + финальный RunResult. Реальные
// core-модули работают в контейнере; vault-ref резолвится Keeper-side (fixture-
// vault как L0) — на хост уходит готовый ApplyRequest без vault-ссылок.
//
// Build-tag integration: дефолтный `make test` L2 не запускает (тянет docker +
// testcontainers). Запуск: `go test -tags integration -run TestL2 ./internal/trial/...`.
package trial

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	render "github.com/souls-guild/soul-stack/keeper/internal/render"
	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
)

// containerSoulPath / containerReqPath — фиксированные пути в стенде, куда
// доставляются soul-бинарь и сериализованный ApplyRequest.
const (
	containerSoulPath = "/usr/local/bin/soul"
	containerReqPath  = "/tmp/apply-request.json"
)

// L2Stand — поднятый эфемерный стенд с доставленным soul-бинарём. Закрывается
// через Close (terminate контейнера). Применять план — Apply.
type L2Stand struct {
	ctr  testcontainers.Container
	soul string // путь soul-бинаря внутри контейнера
}

// applyOutcome — разобранный итог одного `soul apply`: финальный RunResult +
// per-task register-payload-ы (для idempotent-проверки changed==false и
// verify-expect). exitCode — код возврата процесса soul.
type applyOutcome struct {
	exitCode int
	result   *keeperv1.RunResult
	events   []*keeperv1.TaskEvent
	rawErr   string // содержимое stderr soul (для диагностики при фейле)
}

// StartL2Stand собирает soul под linux/<arch стенда> (= GOARCH хоста, контейнер
// под ту же платформу), поднимает контейнер из stand.Image и доставляет soul.
// Контейнер держится живым command-ом `sleep infinity` — exec-ы идут поверх него
// (soul apply каждый раз отдельным exec, как oneshot push-сессия).
func StartL2Stand(ctx context.Context, stand Stand) (*L2Stand, error) {
	soulBin, err := buildSoulLinux(ctx)
	if err != nil {
		return nil, err
	}
	defer os.Remove(soulBin)

	req := testcontainers.ContainerRequest{
		Image:      stand.Image,
		Entrypoint: []string{"sleep", "infinity"},
		WaitingFor: wait.ForExec([]string{"true"}),
	}
	ctr, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("trial L2: поднять стенд %s: %w", stand.Image, err)
	}

	if err := ctr.CopyFileToContainer(ctx, soulBin, containerSoulPath, 0o755); err != nil {
		_ = ctr.Terminate(ctx)
		return nil, fmt.Errorf("trial L2: доставить soul в стенд: %w", err)
	}

	return &L2Stand{ctr: ctr, soul: containerSoulPath}, nil
}

// Close уничтожает стенд.
func (s *L2Stand) Close(ctx context.Context) error {
	return s.ctr.Terminate(ctx)
}

// containerErrPath — файл внутри стенда, куда перенаправляется stderr soul apply.
// stdout оставляем чистым (только NDJSON), чтобы tcexec.Multiplexed combined-reader
// не смешал диагностику soul с NDJSON-потоком.
const containerErrPath = "/tmp/apply-stderr.log"

// Apply сериализует req в protojson, доставляет в стенд и исполняет
// `soul apply` с редиректом protojson из файла на stdin (testcontainers Exec не
// прокидывает stdin — редирект из файла внутри контейнера эквивалентен). stderr
// soul уводится в файл, чтобы stdout нёс только NDJSON. Парсит NDJSON в outcome.
func (s *L2Stand) Apply(ctx context.Context, req *keeperv1.ApplyRequest) (applyOutcome, error) {
	var out applyOutcome

	payload, err := protojson.Marshal(req)
	if err != nil {
		return out, fmt.Errorf("trial L2: marshal ApplyRequest: %w", err)
	}
	if err := s.copyBytes(ctx, payload, containerReqPath); err != nil {
		return out, err
	}

	// soul apply читает ApplyRequest со stdin; редиректим из доставленного файла —
	// поведение идентично push-сессии (Keeper пишет protojson в stdin SSH-exec).
	// stderr → файл: stdout остаётся чистым NDJSON для Multiplexed-reader.
	cmd := []string{"sh", "-c", fmt.Sprintf("%s apply < %s 2> %s", s.soul, containerReqPath, containerErrPath)}
	code, stdout, err := s.exec(ctx, cmd)
	if err != nil {
		return out, fmt.Errorf("trial L2: exec soul apply: %w", err)
	}
	out.exitCode = code
	out.rawErr = s.readFile(ctx, containerErrPath)

	if err := parseNDJSON(stdout, &out); err != nil {
		return out, fmt.Errorf("trial L2: разбор NDJSON soul apply (stderr: %s): %w", strings.TrimSpace(out.rawErr), err)
	}
	return out, nil
}

// copyBytes пишет data во временный файл хоста и доставляет в стенд по dst.
func (s *L2Stand) copyBytes(ctx context.Context, data []byte, dst string) error {
	tmp, err := os.CreateTemp("", "l2-apply-*.json")
	if err != nil {
		return fmt.Errorf("trial L2: temp-файл: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("trial L2: запись temp: %w", err)
	}
	tmp.Close()
	if err := s.ctr.CopyFileToContainer(ctx, tmp.Name(), dst, 0o644); err != nil {
		return fmt.Errorf("trial L2: доставить %s в стенд: %w", dst, err)
	}
	return nil
}

// exec гоняет cmd в стенде и возвращает combined-output (tcexec.Multiplexed
// сам снимает docker-stream-заголовки). Вызывающий редиректит stderr команды в
// файл, поэтому combined здесь = чистый stdout команды.
func (s *L2Stand) exec(ctx context.Context, cmd []string) (int, string, error) {
	code, reader, err := s.ctr.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return 0, "", err
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(reader); err != nil {
		return code, buf.String(), fmt.Errorf("чтение output: %w", err)
	}
	return code, buf.String(), nil
}

// readFile дочитывает файл из стенда через cat (best-effort, для диагностики).
func (s *L2Stand) readFile(ctx context.Context, path string) string {
	_, out, err := s.exec(ctx, []string{"cat", path})
	if err != nil {
		return ""
	}
	return out
}

// RunL2Case прогоняет один L2-кейс end-to-end (дизайн Вариант A): render
// in-process → ApplyRequest → стенд → soul apply → verify → expect_idempotent.
// caseFile — путь к L2 case.yml (рядом с scenario/<name>/main.yml). Возвращает
// Result (LevelL2, Pass + Failures). Стенд поднимается и уничтожается внутри.
func RunL2Case(ctx context.Context, c *L2Case, caseFile string) (Result, error) {
	res := Result{Case: c.Name, Level: LevelL2}

	// 1. Render in-process тем же Keeper-side путём, что L0. L2-кейс несёт input:
	//    (а не fixtures:), поэтому маппим его в герметичный Fixtures.Input;
	//    остальной контекст L2-пилота пуст (один хост, без essence/vault).
	l0 := &Case{Name: c.Name, Fixtures: Fixtures{Input: c.Input}}
	rc, err := renderCase(ctx, l0, caseFile)
	if err != nil {
		return res, err
	}

	stand, err := StartL2Stand(ctx, c.Stand)
	if err != nil {
		return res, err
	}
	defer func() { _ = stand.Close(ctx) }()

	// 2. План → ApplyRequest → стенд.
	req := &keeperv1.ApplyRequest{
		ApplyId: "trial-l2-" + sanitizeID(c.Name),
		Tasks:   render.ToProtoTasks(rc.tasks),
	}
	first, err := stand.Apply(ctx, req)
	if err != nil {
		return res, err
	}
	if fail := assertRunSuccess("apply", first); fail != "" {
		res.Failures = append(res.Failures, fail)
		res.Pass = false
		return res, nil
	}

	// 3. verify-блок: каждая проверка — однозадачный ApplyRequest на том же стенде.
	for _, v := range c.Verify {
		fails, err := stand.runVerify(ctx, req.ApplyId, v)
		if err != nil {
			return res, err
		}
		res.Failures = append(res.Failures, fails...)
	}

	// 4. expect_idempotent: повторный прогон того же ApplyRequest → все
	//    register.changed==false (state хоста уже сошёлся, второй apply — no-op).
	if c.expectIdempotent() {
		second, err := stand.Apply(ctx, req)
		if err != nil {
			return res, err
		}
		if fail := assertRunSuccess("idempotent-apply", second); fail != "" {
			res.Failures = append(res.Failures, fail)
		}
		res.Failures = append(res.Failures, assertNoChanges(second)...)
	}

	res.Pass = len(res.Failures) == 0
	return res, nil
}

// runVerify исполняет один verify-шаг как однозадачный ApplyRequest и сверяет
// register-output задачи с Expect. apply_id наследуется от основного прогона для
// трассируемости (verify — продолжение той же сессии стенда). Module задаётся
// полным именем `<namespace>.<module>.<state>` (например core.cmd.shell) — soul
// сам отделяет state суффиксом (splitModuleAddr).
func (s *L2Stand) runVerify(ctx context.Context, applyID string, v Verify) ([]string, error) {
	params, err := structpb.NewStruct(v.Apply.Params)
	if err != nil {
		return nil, fmt.Errorf("trial L2: verify %q params: %w", v.Name, err)
	}
	req := &keeperv1.ApplyRequest{
		ApplyId: applyID + "-verify-" + sanitizeID(v.Name),
		Tasks: []*keeperv1.RenderedTask{{
			Name:   v.Name,
			Module: v.Apply.Module,
			Params: params,
		}},
	}
	out, err := s.Apply(ctx, req)
	if err != nil {
		return nil, err
	}
	return compareExpect(v, out), nil
}

// Конвертация render-плана в wire-форму ApplyRequest.tasks — общий
// render.ToProtoTasks (keeper/internal/render/prototask.go), тот же, что зовёт
// scenario-orchestrator. Index — orchestrator-only, в proto не идёт; Module
// несёт полное имя со state-суффиксом (soul splits state из RenderedTask).

// parseNDJSON разбирает stdout `soul apply` (NDJSON: по строке на TaskEvent, в
// конце — RunResult). Различает по наличию поля apply_id+status: RunResult несёт
// RunStatus, TaskEvent — TaskStatus+task_idx. Простой разбор: пробуем TaskEvent;
// строка без task-полей, но со status — RunResult. Надёжнее — по эксклюзивному
// полю: RunResult имеет state_changes/только status; TaskEvent — task_idx.
func parseNDJSON(stdout string, out *applyOutcome) error {
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// RunResult и TaskEvent оба несут apply_id+status; различаем по task_idx
		// (только TaskEvent). protojson игнорирует неизвестные поля при
		// DiscardUnknown=false → парсим строго в правильный тип, выбирая по ключу.
		if strings.Contains(line, "\"taskIdx\"") || strings.Contains(line, "\"task_idx\"") || strings.Contains(line, "\"registerData\"") || strings.Contains(line, "\"register_data\"") {
			ev := &keeperv1.TaskEvent{}
			if err := protojson.Unmarshal([]byte(line), ev); err != nil {
				return fmt.Errorf("TaskEvent %q: %w", line, err)
			}
			out.events = append(out.events, ev)
			continue
		}
		rr := &keeperv1.RunResult{}
		if err := protojson.Unmarshal([]byte(line), rr); err != nil {
			return fmt.Errorf("RunResult %q: %w", line, err)
		}
		out.result = rr
	}
	if out.result == nil {
		return fmt.Errorf("в stdout нет RunResult")
	}
	return nil
}

// assertRunSuccess проверяет, что прогон завершился RUN_STATUS_SUCCESS и exit 0.
func assertRunSuccess(phase string, out applyOutcome) string {
	if out.result == nil {
		return fmt.Sprintf("%s: нет RunResult (exit=%d, stderr: %s)", phase, out.exitCode, strings.TrimSpace(out.rawErr))
	}
	if out.result.GetStatus() != keeperv1.RunStatus_RUN_STATUS_SUCCESS {
		return fmt.Sprintf("%s: статус %s, ожидался SUCCESS (exit=%d, stderr: %s)",
			phase, out.result.GetStatus(), out.exitCode, strings.TrimSpace(out.rawErr))
	}
	if out.exitCode != 0 {
		return fmt.Sprintf("%s: exit %d при SUCCESS-статусе", phase, out.exitCode)
	}
	return ""
}

// assertNoChanges проверяет, что во втором прогоне ни одна задача не пометила
// changed==true (идемпотентность). Пропущенные (skipped) задачи changed==false —
// это и есть ожидаемый no-op повторного применения.
func assertNoChanges(out applyOutcome) []string {
	var fails []string
	for _, ev := range out.events {
		if registerBool(ev.GetRegisterData(), "changed") {
			fails = append(fails, fmt.Sprintf(
				"idempotent: задача idx=%d (%s) во втором прогоне changed=true — план не идемпотентен",
				ev.GetTaskIdx(), ev.GetStatus()))
		}
	}
	return fails
}

// compareExpect сверяет register-output verify-задачи с Expect (exit_code /
// stdout / stdout_contains). Один TaskEvent на verify-шаг (однозадачный req).
func compareExpect(v Verify, out applyOutcome) []string {
	var fails []string
	if len(out.events) == 0 {
		return []string{fmt.Sprintf("verify %q: нет TaskEvent (stderr: %s)", v.Name, strings.TrimSpace(out.rawErr))}
	}
	rd := out.events[len(out.events)-1].GetRegisterData()

	if v.Expect.ExitCode != nil {
		got := registerInt(rd, "exit_code")
		if got != *v.Expect.ExitCode {
			fails = append(fails, fmt.Sprintf("verify %q: exit_code=%d, ожидался %d", v.Name, got, *v.Expect.ExitCode))
		}
	}
	if v.Expect.Stdout != nil {
		got := strings.TrimRight(registerString(rd, "stdout"), "\n")
		want := strings.TrimRight(*v.Expect.Stdout, "\n")
		if got != want {
			fails = append(fails, fmt.Sprintf("verify %q: stdout=%q, ожидался %q", v.Name, got, want))
		}
	}
	if v.Expect.StdoutContains != "" {
		got := registerString(rd, "stdout")
		if !strings.Contains(got, v.Expect.StdoutContains) {
			fails = append(fails, fmt.Sprintf("verify %q: stdout не содержит %q (получено %q)", v.Name, v.Expect.StdoutContains, got))
		}
	}
	return fails
}

func registerBool(s *structpb.Struct, key string) bool {
	if s == nil {
		return false
	}
	v, ok := s.GetFields()[key]
	return ok && v.GetBoolValue()
}

func registerInt(s *structpb.Struct, key string) int {
	if s == nil {
		return 0
	}
	if v, ok := s.GetFields()[key]; ok {
		return int(v.GetNumberValue())
	}
	return 0
}

func registerString(s *structpb.Struct, key string) string {
	if s == nil {
		return ""
	}
	if v, ok := s.GetFields()[key]; ok {
		return v.GetStringValue()
	}
	return ""
}

// sanitizeID приводит произвольное имя кейса/шага к безопасному id-фрагменту для
// apply_id (буквы/цифры/дефис).
func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		return "case"
	}
	return out
}

// buildSoulLinux собирает soul-бинарь под linux/<GOARCH хоста> во временный файл,
// путь к нему возвращает (caller обязан удалить). Контейнер поднимается под ту же
// платформу, что хост (docker desktop), поэтому GOARCH хоста = arch стенда.
// CGO_ENABLED=0 — статический бинарь без glibc-зависимостей (работает в любом
// базовом образе, вкл. alpine).
func buildSoulLinux(ctx context.Context) (string, error) {
	soulMod, err := soulModuleDir()
	if err != nil {
		return "", err
	}
	bin, err := os.CreateTemp("", "soul-l2-*")
	if err != nil {
		return "", fmt.Errorf("trial L2: temp soul: %w", err)
	}
	bin.Close()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin.Name(), "./cmd/soul")
	cmd.Dir = soulMod
	cmd.Env = append(os.Environ(),
		"GOOS=linux",
		"GOARCH="+runtime.GOARCH,
		"CGO_ENABLED=0",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		os.Remove(bin.Name())
		return "", fmt.Errorf("trial L2: go build soul (linux/%s): %w\n%s", runtime.GOARCH, err, stderr.String())
	}
	return bin.Name(), nil
}

// soulModuleDir находит корень модуля soul/ относительно этого пакета
// (keeper/internal/trial). go.work-раскладка ADR-011: soul/ — sibling keeper/.
func soulModuleDir() (string, error) {
	_, self, _, ok := runtimeCaller()
	if !ok {
		return "", fmt.Errorf("trial L2: не удалось определить путь пакета")
	}
	// self = .../keeper/internal/trial/l2_run.go → repo-root = ../../../..
	trialDir := filepath.Dir(self)
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(trialDir)))
	soulMod := filepath.Join(repoRoot, "soul")
	if _, err := os.Stat(filepath.Join(soulMod, "go.mod")); err != nil {
		return "", fmt.Errorf("trial L2: модуль soul/ не найден по %s: %w", soulMod, err)
	}
	return soulMod, nil
}

// runtimeCaller — тонкая обёртка над runtime.Caller (изоляция импорта для теста).
func runtimeCaller() (uintptr, string, int, bool) {
	return runtime.Caller(0)
}
