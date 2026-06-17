// Package internaltest — общие test-helper-ы для unit-тестов core-модулей
// Soul-стороны (pkg/file/service/user/group/…). Сам пакет НЕ имеет суффикса
// _test, потому что test-файлы разных пакетов (pkg_test, file_test, …) не
// могут импортировать xxx_test-пакеты друг друга.
//
// Содержимое — только тестовая инфраструктура, не используется в проде.
// Файл попадает в production-сборку как dead-code, но никогда не
// инстанцируется (нет init-ов, нет registry-сторон).
package internaltest

import (
	"context"
	"sort"
	"strings"

	"github.com/souls-guild/soul-stack/soul/internal/coremod/util"

	pluginv1 "github.com/souls-guild/soul-stack/proto/plugin/gen/go/v1"
	"google.golang.org/grpc"
)

// ApplyStream — fake grpc.ServerStreamingServer[ApplyEvent] для unit-тестов.
// Захватывает все Send-события в Events; единственное финальное событие
// доступно как Last().
type ApplyStream struct {
	grpc.ServerStreamingServer[pluginv1.ApplyEvent]
	Events []*pluginv1.ApplyEvent
}

func (s *ApplyStream) Send(e *pluginv1.ApplyEvent) error {
	s.Events = append(s.Events, e)
	return nil
}

func (s *ApplyStream) Context() context.Context { return context.Background() }

// Last — последнее отправленное событие; nil если ничего не было.
func (s *ApplyStream) Last() *pluginv1.ApplyEvent {
	if len(s.Events) == 0 {
		return nil
	}
	return s.Events[len(s.Events)-1]
}

// Runner — детерминированный fake util.Runner. Сборка ключа — name + " " +
// args (через space). Команды без явной настройки возвращают Fallback.
//
// Каждому ключу можно прописать очередь ответов (через OnSeq) — последовательные
// вызовы одной и той же команды получают разные результаты. Это нужно для
// сценариев типа «pre-install: not installed; post-install: installed».
// Когда очередь исчерпана, отдаётся последний элемент очереди (sticky).
// Простой On — синтаксический сахар для одно-элементной очереди.
type Runner struct {
	Calls    []string
	Results  map[string][]util.Result
	Fallback util.Result
}

// NewRunner — конструктор с пустым Results и Fallback {ExitCode: 127}
// (имитация «command not found», подходит как default для DetectPkgMgr,
// DetectInitSystem-проверок).
func NewRunner() *Runner {
	return &Runner{Results: map[string][]util.Result{}, Fallback: util.Result{ExitCode: 127}}
}

// On — fluent-настройка одного ответа на команду. Перезаписывает текущую очередь.
func (r *Runner) On(cmd string, res util.Result) *Runner {
	r.Results[cmd] = []util.Result{res}
	return r
}

// OnSeq — настраивает последовательность ответов на повторные вызовы команды.
// Последний элемент после исчерпания остаётся sticky.
func (r *Runner) OnSeq(cmd string, results ...util.Result) *Runner {
	r.Results[cmd] = append([]util.Result(nil), results...)
	return r
}

func (r *Runner) Run(_ context.Context, name string, args ...string) util.Result {
	return r.dispatch(name, args)
}

// RunOpts — fake-вариант с поддержкой cwd/env: ключ обогащается префиксом
// `[cwd=<dir>] ` и/или `[env=KEY=VAL,…] `, чтобы тест мог проверить, что
// модуль действительно передал ожидаемые опции. Sorted-порядок env-ключей
// обеспечивает детерминизм (map iteration в Go недетерминирован).
func (r *Runner) RunOpts(_ context.Context, opts util.RunOptions) util.Result {
	var prefix string
	if opts.Cwd != "" {
		prefix += "[cwd=" + opts.Cwd + "] "
	}
	if len(opts.Env) > 0 {
		env := append([]string(nil), opts.Env...)
		sort.Strings(env)
		prefix += "[env=" + strings.Join(env, ",") + "] "
	}
	key := prefix + opts.Name
	for _, a := range opts.Args {
		key += " " + a
	}
	r.Calls = append(r.Calls, key)
	seq, ok := r.Results[key]
	if !ok || len(seq) == 0 {
		return r.Fallback
	}
	res := seq[0]
	if len(seq) > 1 {
		r.Results[key] = seq[1:]
	}
	return res
}

func (r *Runner) dispatch(name string, args []string) util.Result {
	key := name
	for _, a := range args {
		key += " " + a
	}
	r.Calls = append(r.Calls, key)
	seq, ok := r.Results[key]
	if !ok || len(seq) == 0 {
		return r.Fallback
	}
	res := seq[0]
	if len(seq) > 1 {
		r.Results[key] = seq[1:]
	}
	// если seq.len==1 — оставляем последний sticky
	return res
}
