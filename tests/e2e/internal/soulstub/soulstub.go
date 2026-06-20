//go:build e2e

// Package soulstub — fake-Soul helper для L3a E2E (ADR-039(2)).
//
// Открывает gRPC bidi-стрим к Keeper-у поверх mTLS (ровно как настоящий Soul),
// отвечает на ApplyRequest предзаписанным RunResult из YAML-scripts. НЕ
// запускает реальное apply, не мутирует filesystem, не парсит destiny —
// контракт-тест L3a проверяет лифт-цикл apply_runs / RBAC / audit / metrics
// на keeper-стороне, а не реализм apply (для этого L3b).
//
// НЕ бинарь (ADR-004): test-fixture без operator-lifecycle.
package soulstub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/types/known/structpb"

	keeperv1 "github.com/souls-guild/soul-stack/proto/gen/go/keeper/v1"
	"github.com/souls-guild/soul-stack/shared/config"
)

// ErrAlreadyOpen возвращается, если Open вызван второй раз без Close.
var ErrAlreadyOpen = errors.New("soulstub: stream already open")

// Stub — один fake-Soul, держащий долгоживущий EventStream к Keeper-у.
type Stub struct {
	SID            string
	KeeperGRPCAddr string

	// TLS-материал mTLS-handshake. cert/key — client-cert stub-а (Vault-issued
	// leaf под SID), caBundle — root CA Keeper-server-cert-а.
	cert     []byte
	key      []byte
	caBundle []byte

	// scripted — карта scenario-name → список ScriptEntry, заполняется из
	// fixtures/stub-responses.yaml. Matching по task_name (порядок задач в
	// ApplyRequest не гарантирован — ADR-027).
	scripted map[string][]ScriptEntry

	// errandStatus — статус, которым stub отвечает на ErrandRequest (ADR-033/041).
	// Дефолт SUCCESS (см. New). recvLoop на ErrandRequest шлёт ErrandResult с этим
	// статусом + эхо errand_id. Позволяет e2e-тесту ErrandRun-а прогнать цепочку
	// dispatch→terminal без реального exec (stub не запускает shell — L3a-контракт).
	errandStatus keeperv1.ErrandStatus

	// applyStatusBySID / errandStatusBySID — per-SID override поверх глобального
	// дефолта (applyDefaultSuccess / errandStatus). Нужны partial-failure-тестам
	// (Tide/ErrandRun abort/continue): один хост волны возвращает FAILED, остальные
	// — глобальный дефолт. Маршрутизация по соединению (один Stub = один SID), но
	// recvLoop явно сверяет req-SID (echo в ErrandRequest.sid) / s.SID для
	// читаемости и на случай нескольких Stub-ов в одном тесте. Пусто → дефолт.
	applyStatusBySID  map[string]bool
	errandStatusBySID map[string]keeperv1.ErrandStatus

	// applyDefaultSuccess — режим «success на любой ApplyRequest»: задача, не
	// покрытая scripted-таблицей, считается SUCCESS (а не FAILED). Удобно для
	// apply-e2e, которому важен lifecycle apply_runs (planned→…→success), а не
	// реализм per-task RunResult-а (L3a-контракт). Дефолт false (строгий режим —
	// unscripted task = FAILED, явный сигнал о дыре в fixture). Включается
	// SetApplyDefaultSuccess.
	applyDefaultSuccess bool

	// taskRegisterByName — scripted per-task register (staged-render, ADR-056):
	// task_name → per-SID register-payload (sid → register_data). На ApplyRequest
	// stub эмитит TaskEvent с RegisterData ДО агрегированного RunResult (как
	// настоящий Soul на probe-задаче), echo-я passage из запроса. Keeper-side
	// accumulateRegister складывает register per-(apply_id, sid, passage), откуда
	// render следующего Passage резолвит where: register.*. Без scripted-register
	// (дефолт) stub register не эмитит — обычный apply (L3a-контракт). Включается
	// [SetTaskRegister].
	taskRegisterByName map[string]map[string]map[string]any

	// dryRunPlanSet — включён ли Plan-ответ на dry_run-ApplyRequest (Scry,
	// ADR-031). Когда true, на ApplyRequest{dry_run:true} stub перед RunResult
	// шлёт по одному TaskEvent на каждую задачу со status=CHANGED|OK и
	// register_data{changed:dryRunChanged} — keeper-side accumulateRegister
	// складывает их в apply_task_register, откуда CheckDrift строит
	// per-task changed (drifted/clean). Дефолт false: без явного включения
	// dry_run-прогон ведёт себя как обычный (только RunResult), drift-report
	// собирается с host=clean (нет register-строк). Имитирует SoulModule.Plan
	// (mod.Apply на dry_run не вызывается — read-only гарантия ADR-031), НЕ
	// исполняет реальный Plan core-модуля (L3a-контракт, как и весь stub).
	dryRunPlanSet bool
	dryRunChanged bool

	mu       sync.Mutex
	conn     *grpc.ClientConn
	stream   keeperv1.Keeper_EventStreamClient
	recorded []Message
	cancel   context.CancelFunc
	done     chan struct{}
}

// ScriptEntry — одна scripted-реплика stub-а.
//
// TaskName — имя задачи, на которую stub реагирует RunResult-ом.
// Status — RunStatus enum-значение.
// StateChanges — произвольный jsonb-payload, упаковывается в RunResult.state_changes.
type ScriptEntry struct {
	TaskName     string
	Status       keeperv1.RunStatus
	StateChanges map[string]any
}

// Message — зафиксированный stub-ом payload от Keeper-а.
type Message struct {
	Kind  string
	Frame *keeperv1.FromKeeper
}

// New конструирует Stub. cert/key — client-cert mTLS, caBundle — root CA
// Keeper-server-cert-а (для верификации server-cert-а на handshake-е).
func New(sid, keeperGRPCAddr string, cert, key, caBundle []byte) *Stub {
	return &Stub{
		SID:                sid,
		KeeperGRPCAddr:     keeperGRPCAddr,
		cert:               cert,
		key:                key,
		caBundle:           caBundle,
		scripted:           make(map[string][]ScriptEntry),
		errandStatus:       keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS,
		applyStatusBySID:   make(map[string]bool),
		errandStatusBySID:  make(map[string]keeperv1.ErrandStatus),
		taskRegisterByName: make(map[string]map[string]map[string]any),
		done:               make(chan struct{}),
	}
}

// SetTaskRegister задаёт scripted per-task register (staged-render, ADR-056):
// задача taskName на хосте s.SID эмитит TaskEvent с RegisterData=data ДО
// агрегированного RunResult (как настоящий Soul на probe-задаче, эхо-я passage
// из ApplyRequest). Keeper копит register per-(apply_id, sid, passage), и render
// следующего Passage резолвит `where: register.<name>.*` per-host этим фактом.
// Для 2-passage e2e probe→where: один хост даёт role='master', другой 'slave'.
func (s *Stub) SetTaskRegister(taskName string, data map[string]any) {
	s.mu.Lock()
	bySID := s.taskRegisterByName[taskName]
	if bySID == nil {
		bySID = make(map[string]map[string]any)
		s.taskRegisterByName[taskName] = bySID
	}
	bySID[s.SID] = data
	s.mu.Unlock()
}

// SetErrandStatus задаёт статус, которым stub ответит на ErrandRequest
// (по умолчанию SUCCESS). Для тестов failed/timeout-веток ErrandRun-а.
func (s *Stub) SetErrandStatus(st keeperv1.ErrandStatus) {
	s.mu.Lock()
	s.errandStatus = st
	s.mu.Unlock()
}

// SetApplyDefaultSuccess включает режим «success на любой ApplyRequest»:
// scripted-таблицей не покрытая задача даёт SUCCESS, а не FAILED. Для apply-e2e,
// которому важен lifecycle apply_runs, а не реализм per-task RunResult-а.
func (s *Stub) SetApplyDefaultSuccess(v bool) {
	s.mu.Lock()
	s.applyDefaultSuccess = v
	s.mu.Unlock()
}

// SetApplyStatusForSID задаёт per-SID override RunResult-статуса на ApplyRequest:
// success=true → RUN_STATUS_SUCCESS, success=false → RUN_STATUS_FAILED, независимо
// от scripted-таблицы и applyDefaultSuccess. Применяется, если sid совпадает с
// s.SID (один Stub = один SID, маршрутизация по соединению). Для partial-failure-
// тестов Tide (одна волна с упавшим хостом → on_failure=abort/continue). Без
// вызова поведение прежнее (глобальный дефолт).
func (s *Stub) SetApplyStatusForSID(sid string, success bool) {
	s.mu.Lock()
	s.applyStatusBySID[sid] = success
	s.mu.Unlock()
}

// SetErrandStatusForSID задаёт per-SID override статуса ErrandResult на
// ErrandRequest поверх глобального errandStatus. Применяется, если sid совпадает
// с s.SID. Для partial-failure-тестов ErrandRun (один host FAILED → on_failure=
// abort/continue). Без вызова поведение прежнее (глобальный SetErrandStatus/default).
func (s *Stub) SetErrandStatusForSID(sid string, status keeperv1.ErrandStatus) {
	s.mu.Lock()
	s.errandStatusBySID[sid] = status
	s.mu.Unlock()
}

// SetDryRunPlan включает Plan-ответ на dry_run-ApplyRequest (Scry, ADR-031):
// stub перед RunResult эмитит TaskEvent на каждую задачу с register_data{changed}
// = changed. changed=true → drift (per-task CHANGED), false → clean (per-task OK).
// Без вызова dry_run-прогон ведёт себя как обычный (только RunResult) и
// drift-report собирается host=clean без per-task register-строк.
func (s *Stub) SetDryRunPlan(changed bool) {
	s.mu.Lock()
	s.dryRunPlanSet = true
	s.dryRunChanged = changed
	s.mu.Unlock()
}

// LoadScript заполняет scripted-карту из готового парс-результата
// `stub-responses.yaml`. Валидация структуры RunResult — на caller-е (harness).
func (s *Stub) LoadScript(perScenario map[string][]ScriptEntry) {
	for k, v := range perScenario {
		s.scripted[k] = v
	}
}

// Open подключается к Keeper-у через mTLS, открывает EventStream, шлёт Hello,
// запускает recv-loop в фоновой goroutine. Блокирующего вызова нет — caller
// дальше делает ассерты и потом Close.
func (s *Stub) Open(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.stream != nil {
		return ErrAlreadyOpen
	}

	clientCert, err := tls.X509KeyPair(s.cert, s.key)
	if err != nil {
		return fmt.Errorf("soulstub(%s): X509KeyPair: %w", s.SID, err)
	}
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(s.caBundle) {
		return fmt.Errorf("soulstub(%s): не удалось добавить CA в pool", s.SID)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      caPool,
		ServerName:   "localhost",
		MinVersion:   tls.VersionTLS13,
	}

	conn, err := grpc.NewClient(s.KeeperGRPCAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		return fmt.Errorf("soulstub(%s): grpc.NewClient: %w", s.SID, err)
	}

	client := keeperv1.NewKeeperClient(conn)
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := client.EventStream(streamCtx)
	if err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("soulstub(%s): EventStream: %w", s.SID, err)
	}

	if err := stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_Hello{
			Hello: &keeperv1.Hello{
				SidEcho:     s.SID,
				SoulVersion: "soulstub-l3a",
				// Анонс фичей протокола (ADR-056 §S5) — тот же канон-список, что шлёт
				// реальный Soul (soul/internal/grpc/client.go). Без "passage" keeper
				// отвергает stub под staged-сценарием (N>1 Passage) fail-closed
				// (soul_passage_unsupported, run.go), хотя respondToApply эхает passage
				// в TaskEvent/RunResult (S3) — capability обязана совпасть с поведением.
				Capabilities: config.SoulCapabilities(),
			},
		},
	}); err != nil {
		cancel()
		_ = conn.Close()
		return fmt.Errorf("soulstub(%s): send Hello: %w", s.SID, err)
	}

	s.conn = conn
	s.stream = stream
	s.cancel = cancel

	go s.recvLoop()
	return nil
}

// recvLoop читает сообщения от Keeper-а и реагирует scripted-RunResult-ом на
// ApplyRequest. Любое другое сообщение записывается в recorded и игнорируется
// (L3a-контракт: stub не отвечает на CancelApply / SigilTrustAnchors кроме
// записи).
func (s *Stub) recvLoop() {
	defer close(s.done)
	for {
		frame, err := s.stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
				// Тест читает Messages() для ассерта — здесь логировать нечем
				// (нет testing.TB).
			}
			return
		}

		s.mu.Lock()
		s.recorded = append(s.recorded, Message{
			Kind:  payloadKind(frame),
			Frame: frame,
		})
		s.mu.Unlock()

		if req := frame.GetApplyRequest(); req != nil {
			s.respondToApply(req)
		}
		if er := frame.GetErrandRequest(); er != nil {
			s.respondToErrand(er)
		}
	}
}

// respondToErrand отвечает на ErrandRequest (ADR-033/041) ErrandResult-ом с
// настроенным статусом (default SUCCESS). НЕ запускает реальный shell/exec —
// L3a-контракт проверяет keeper-side цепочку dispatch→applybus→Dispatcher-
// terminal→errands-row (вкл. FK started_by_aid), а не реализм исполнения модуля.
// errand_id эхо-проксируется из запроса. exit_code=0 для SUCCESS.
func (s *Stub) respondToErrand(req *keeperv1.ErrandRequest) {
	s.mu.Lock()
	status := s.errandStatus
	if override, ok := s.errandStatusBySID[s.SID]; ok {
		status = override
	}
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return
	}
	var exitCode int32
	if status != keeperv1.ErrandStatus_ERRAND_STATUS_SUCCESS {
		exitCode = 1
	}
	_ = stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_ErrandResult{
			ErrandResult: &keeperv1.ErrandResult{
				ErrandId:   req.GetErrandId(),
				Status:     status,
				ExitCode:   exitCode,
				Stdout:     "ok\n",
				DurationMs: 1,
			},
		},
	})
}

// respondToApply шлёт агрегированный RunResult по scripted-таблице. Если
// какая-то задача в ApplyRequest scripted-таблицей не покрыта — статус
// результата = FAILED (явный сигнал тесту о дыре в fixture).
func (s *Stub) respondToApply(req *keeperv1.ApplyRequest) {
	s.mu.Lock()
	defaultSuccess := s.applyDefaultSuccess
	dryRunPlanSet := s.dryRunPlanSet
	dryRunChanged := s.dryRunChanged
	sidOverride, hasSidOverride := s.applyStatusBySID[s.SID]
	s.mu.Unlock()

	// Per-SID override (partial-failure-тесты Tide): этот хост возвращает
	// детерминированный RunResult{SUCCESS|FAILED} вне зависимости от scripted-
	// таблицы и applyDefaultSuccess — один host волны падает, остальные нет.
	if hasSidOverride {
		status := keeperv1.RunStatus_RUN_STATUS_SUCCESS
		if !sidOverride {
			status = keeperv1.RunStatus_RUN_STATUS_FAILED
		}
		_ = s.stream.Send(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_RunResult{
				RunResult: &keeperv1.RunResult{
					ApplyId: req.GetApplyId(),
					Status:  status,
					Attempt: req.GetAttempt(),
					Passage: req.GetPassage(),
				},
			},
		})
		return
	}

	// Scripted per-task register (staged-render, ADR-056): для probe-задачи (с
	// заданным SetTaskRegister) эмитим TaskEvent с RegisterData ДО RunResult,
	// echo-я passage из запроса — как настоящий Soul на register-задаче. Keeper
	// копит register per-(apply_id, sid, passage); render следующего Passage
	// резолвит `where: register.<name>.*` этим фактом. Без scripted-register —
	// no-op (обычный apply).
	s.emitTaskRegisters(req)

	// dry_run + включённый Plan-режим (Scry, ADR-031): эмитим per-task TaskEvent
	// с register_data{changed}, как сделал бы Soul после mod.Plan. Это наполняет
	// apply_task_register, откуда CheckDrift собирает per-task drifted/clean.
	// RunResult ниже закрывает host-терминал (driftBarrier ждёт его).
	if req.GetDryRun() && dryRunPlanSet {
		s.emitPlanTaskEvents(req, dryRunChanged)
	}

	worst := keeperv1.RunStatus_RUN_STATUS_SUCCESS
	merged := map[string]any{}

	for _, task := range req.GetTasks() {
		entries := s.findEntriesByTask(task.GetName())
		if len(entries) == 0 {
			if defaultSuccess {
				// Apply-default-success режим: unscripted task = SUCCESS (lifecycle
				// apply_runs важнее реализма per-task RunResult-а, L3a-контракт).
				continue
			}
			worst = keeperv1.RunStatus_RUN_STATUS_FAILED
			merged["_unscripted_task"] = task.GetName()
			continue
		}
		e := entries[0]
		if e.Status == keeperv1.RunStatus_RUN_STATUS_FAILED {
			worst = keeperv1.RunStatus_RUN_STATUS_FAILED
		}
		for k, v := range e.StateChanges {
			merged[k] = v
		}
	}

	stateStruct, err := structpb.NewStruct(merged)
	if err != nil {
		stateStruct = nil
	}

	_ = s.stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_RunResult{
			RunResult: &keeperv1.RunResult{
				ApplyId:      req.GetApplyId(),
				Status:       worst,
				StateChanges: stateStruct,
				Attempt:      req.GetAttempt(),
				// passage (ADR-056): echo из ApplyRequest — Keeper коррелирует
				// терминал per-(apply_id, sid, passage) и барьерит срез Passage.
				Passage: req.GetPassage(),
			},
		},
	})
}

// emitTaskRegisters эмитит TaskEvent с RegisterData для задач ApplyRequest, у
// которых задан scripted per-task register этого SID (SetTaskRegister). task_idx
// — позиция задачи в req.Tasks[] (так же его проставляет настоящий Soul); passage
// — echo ApplyRequest.passage. Keeper-side accumulateRegister складывает
// register_data в apply_task_register по (apply_id, sid, task_idx) с этим passage.
// no-op, если scripted-register не задан.
func (s *Stub) emitTaskRegisters(req *keeperv1.ApplyRequest) {
	s.mu.Lock()
	stream := s.stream
	byName := s.taskRegisterByName
	sid := s.SID
	s.mu.Unlock()
	if stream == nil || len(byName) == 0 {
		return
	}
	for idx, task := range req.GetTasks() {
		bySID, ok := byName[task.GetName()]
		if !ok {
			continue
		}
		data, ok := bySID[sid]
		if !ok {
			continue
		}
		reg, err := structpb.NewStruct(data)
		if err != nil {
			continue
		}
		_ = stream.Send(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_TaskEvent{
				TaskEvent: &keeperv1.TaskEvent{
					ApplyId:      req.GetApplyId(),
					TaskIdx:      int32(idx),
					Status:       keeperv1.TaskStatus_TASK_STATUS_OK,
					RegisterData: reg,
					Passage:      req.GetPassage(),
				},
			},
		})
	}
}

// emitPlanTaskEvents шлёт по одному TaskEvent на каждую задачу dry_run-
// ApplyRequest со status=CHANGED|OK и register_data{changed}. task_idx —
// позиция задачи в req.Tasks[] (так же его проставляет настоящий Soul:
// applyrunner.go TaskIdx=int32(idx)), что совпадает с RenderedTask.Index для
// linear-сценария вроде converge. Keeper-side accumulateRegister складывает
// register_data в apply_task_register по task_idx.
func (s *Stub) emitPlanTaskEvents(req *keeperv1.ApplyRequest, changed bool) {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return
	}
	status := keeperv1.TaskStatus_TASK_STATUS_OK
	if changed {
		status = keeperv1.TaskStatus_TASK_STATUS_CHANGED
	}
	for idx := range req.GetTasks() {
		reg, err := structpb.NewStruct(map[string]any{"changed": changed})
		if err != nil {
			continue
		}
		_ = stream.Send(&keeperv1.FromSoul{
			Payload: &keeperv1.FromSoul_TaskEvent{
				TaskEvent: &keeperv1.TaskEvent{
					ApplyId:      req.GetApplyId(),
					TaskIdx:      int32(idx),
					Status:       status,
					RegisterData: reg,
					Passage:      req.GetPassage(),
				},
			},
		})
	}
}

// SendPortent шлёт Soul-stub-ом PortentEvent в EventStream от имени live
// Soul-producer-а (V5-1 ADR-030 amendment 2026-05-26). Используется L3a-тестами
// Oracle-loop-а: stub эмитит typed/legacy Portent через настоящий gRPC-mTLS,
// Keeper-handler принимает и проходит весь pipeline (match/where/cooldown/
// enqueue → apply_runs). Возвращает ошибку send-а (stream закрыт / транспорт).
//
// stub НЕ имитирует scheduler: caller сам собирает PortentEvent с нужным
// beacon_name / payload (typed oneof либо legacy Data). collected_at /sid
// заполняются автоматически из stub.SID и time.Now, если caller их не выставил.
func (s *Stub) SendPortent(ev *keeperv1.PortentEvent) error {
	s.mu.Lock()
	stream := s.stream
	s.mu.Unlock()
	if stream == nil {
		return errors.New("soulstub: stream is not open")
	}
	if ev.GetSid() == "" {
		ev.Sid = s.SID
	}
	return stream.Send(&keeperv1.FromSoul{
		Payload: &keeperv1.FromSoul_PortentEvent{PortentEvent: ev},
	})
}

// findEntriesByTask ищет scripted-entry по task_name по всем сценариям. Stub
// не знает текущего scenario-имени (Keeper присылает только task_name в
// RenderedTask), поэтому плоский поиск по объединению карт. Коллизия имён
// задач между сценариями в test-окружении исключена (одно stub-responses.yaml
// на смок-кейс).
func (s *Stub) findEntriesByTask(taskName string) []ScriptEntry {
	var out []ScriptEntry
	for _, entries := range s.scripted {
		for _, e := range entries {
			if e.TaskName == taskName {
				out = append(out, e)
			}
		}
	}
	return out
}

// payloadKind возвращает имя oneof-варианта для Message.Kind.
func payloadKind(frame *keeperv1.FromKeeper) string {
	switch frame.GetPayload().(type) {
	case *keeperv1.FromKeeper_HelloReply:
		return "HelloReply"
	case *keeperv1.FromKeeper_ApplyRequest:
		return "ApplyRequest"
	case *keeperv1.FromKeeper_CancelApply:
		return "CancelApply"
	case *keeperv1.FromKeeper_SeedRotationReply:
		return "SeedRotationReply"
	case *keeperv1.FromKeeper_PluginSigil:
		return "PluginSigil"
	case *keeperv1.FromKeeper_AugurReply:
		return "AugurReply"
	case *keeperv1.FromKeeper_SigilSnapshot:
		return "SigilSnapshot"
	case *keeperv1.FromKeeper_SigilTrustAnchors:
		return "SigilTrustAnchors"
	case *keeperv1.FromKeeper_VigilSnapshot:
		return "VigilSnapshot"
	case *keeperv1.FromKeeper_ErrandRequest:
		return "ErrandRequest"
	default:
		return "unknown"
	}
}

// Close — graceful-shutdown стрима. Безопасен к повторному вызову.
func (s *Stub) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	conn := s.conn
	stream := s.stream
	s.stream = nil
	s.conn = nil
	s.cancel = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stream != nil {
		_ = stream.CloseSend()
	}
	if conn != nil {
		return conn.Close()
	}
	return nil
}

// Messages возвращает копию принятых от Keeper-а сообщений за время жизни стрима.
func (s *Stub) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.recorded))
	copy(out, s.recorded)
	return out
}
